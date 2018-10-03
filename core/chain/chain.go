package chain

import (
	"errors"
	"github.com/theQRL/go-qrl/core/addressstate"
	"github.com/theQRL/go-qrl/core/state"
	"math/big"
	"reflect"
	"sync"

	"github.com/syndtr/goleveldb/leveldb"

	c "github.com/theQRL/go-qrl/config"
	"github.com/theQRL/go-qrl/core/block"
	"github.com/theQRL/go-qrl/core/metadata"
	"github.com/theQRL/go-qrl/core/pool"
	"github.com/theQRL/go-qrl/core/transactions"
	"github.com/theQRL/go-qrl/generated"
	"github.com/theQRL/go-qrl/genesis"
	"github.com/theQRL/go-qrl/log"
	"github.com/theQRL/go-qrl/misc"
	"github.com/theQRL/qryptonight/goqryptonight"

	"github.com/theQRL/go-qrl/pow"
)

type Chain struct {
	lock sync.Mutex

	log log.Logger
	config *c.Config

	triggerMiner bool

	state *state.State

	txPool *pool.TransactionPool

	lastBlock *block.Block
	currentDifficulty []byte

}

func CreateChain(log *log.Logger, config *c.Config) (*Chain, error) {
	s, err := state.CreateState(log, config)
	if err != nil {
		return nil, err
	}

	txPool := &pool.TransactionPool{}

	chain := &Chain {
		log: *log,
		config: config,

		state: s,
		txPool: txPool,
	}

	return chain, err
}

func (c *Chain) Height() uint64 {
	return c.lastBlock.BlockNumber()
}

func (c *Chain) GetLastBlock() *block.Block {
	return c.lastBlock
}

func (c *Chain) Load(genesisBlock *block.Block) error {
	// load() has the following tasks:
	// Write Genesis Block into State immediately
	// Register block_number <-> blockhash mapping
	// Calculate difficulty Metadata for Genesis Block
	// Generate AddressStates from Genesis Block balances
	// Apply Genesis Block's transactions to the state
	// Detect if we are forked from genesis block and if so initiate recovery.
	h, err := c.state.GetChainHeight()

	if err != nil {
		c.state.PutBlock(genesisBlock, nil)
		blockNumberMapping := &generated.BlockNumberMapping{Headerhash:genesisBlock.HeaderHash(),
			PrevHeaderhash:genesisBlock.PrevHeaderHash()}

		c.state.PutBlockNumberMapping(genesisBlock.BlockNumber(), blockNumberMapping, nil)
		parentDifficulty := goqryptonight.StringToUInt256(string(c.config.Dev.Genesis.GenesisDifficulty))

		dt := pow.DifficultyTracker{}
		currentDifficulty, _ := dt.Get(uint64(c.config.Dev.MiningSetpointBlocktime),
			misc.UCharVectorToBytes(parentDifficulty))

		blockMetaData := metadata.CreateBlockMetadata(currentDifficulty, currentDifficulty, nil)

		c.state.PutBlockMetaData(genesisBlock.HeaderHash(), blockMetaData, nil)

		addressesState := make(map[string]*addressstate.AddressState)
		gen := &genesis.Genesis{}
		for _, genesisBalance := range gen.GenesisBalance() {
			addrState := addressstate.GetDefaultAddressState(genesisBalance.Address)
			addressesState[string(addrState.Address())] = addrState
			addrState.SetBalance(genesisBalance.Balance)
		}

		txs := gen.Transactions()
		for i := 1; i < len(txs); i++ {
			for _, addr := range txs[i].GetTransfer().AddrsTo {
				addressesState[string(addr)] = addressstate.GetDefaultAddressState(addr)
			}
		}

		coinBase := &transactions.CoinBase{}
		coinBase.SetPBData(txs[0])
		addressesState[string(coinBase.AddrTo())] = addressstate.GetDefaultAddressState(coinBase.AddrTo())

		if !coinBase.ValidateExtendedCoinbase(gen.BlockNumber()) {
			return errors.New("coinbase validation failed")
		}

		coinBase.ApplyStateChanges(addressesState)

		for i := 1; i < len(txs); i++ {
			tx := transactions.TransferTransaction{}
			tx.SetPBData(txs[i])
			tx.ApplyStateChanges(addressesState)
		}

		c.state.PutAddressesState(addressesState, nil)
		c.state.UpdateTxMetadata(genesisBlock, nil)
		c.state.PutChainHeight(0, nil)
	} else {
		c.lastBlock, err = c.state.GetBlockByNumber(h)
		var blockMetadata *metadata.BlockMetaData
		blockMetadata, err := c.state.GetBlockMetadata(c.lastBlock.HeaderHash())

		if err != nil {
			return err
		}

		c.currentDifficulty = blockMetadata.BlockDifficulty()
		forkState, err := c.state.GetForkState()
		if err == nil {
			b, err := c.state.GetBlock(forkState.InitiatorHeaderhash)
			if err != nil {
				return err
			}
			c.forkRecovery(b, forkState)
		}
	}
	return nil
}

func (c *Chain) addBlock(block *block.Block, batch *leveldb.Batch) (bool, bool) {
	blockSizeLimit, err := c.state.GetBlockSizeLimit(block)

	if err == nil && block.Size() > blockSizeLimit {
		c.log.Warn("Block Size greater than threshold limit %s > %s", block.Size(), blockSizeLimit)
		return false, false
	}

	if reflect.DeepEqual(c.lastBlock.HeaderHash(), block.PrevHeaderHash()) {
		if !c.applyBlock(block, batch) {
			return false, false
		}
	}

	err = c.state.PutBlock(block, batch)

	if err != nil {
		return false, false
	}

	lastBlockMetadata, err := c.state.GetBlockMetadata(c.lastBlock.HeaderHash())
	newBlockMetadata, err := c.state.GetBlockMetadata(block.HeaderHash())

	lastBlockDifficulty := big.NewInt(0)
	lastBlockDifficulty.SetString(goqryptonight.UInt256ToString(misc.BytesToUCharVector(lastBlockMetadata.TotalDifficulty())), 10)
	newBlockDifficulty := big.NewInt(0)
	newBlockDifficulty.SetString(goqryptonight.UInt256ToString(misc.BytesToUCharVector(newBlockMetadata.TotalDifficulty())), 10)

	if newBlockDifficulty.Cmp(lastBlockDifficulty) == 1 {
		if !reflect.DeepEqual(c.lastBlock.HeaderHash(), block.PrevHeaderHash()) {
			forkState := &generated.ForkState{InitiatorHeaderhash:block.HeaderHash()}
			err = c.state.PutForkState(forkState, batch)
			if err != nil {
				c.log.Info("PutForkState Error %s", err.Error())
				return false, true
			}
			c.state.WriteBatch(batch)
			return c.forkRecovery(block, forkState), true
		}
		c.updateChainState(block, batch)
		c.txPool.CheckStale(block.BlockNumber())
		c.triggerMiner = true
	}
	return true, false
}

func (c *Chain) AddBlock(block *block.Block) bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	if block.BlockNumber() < c.Height() - c.config.Dev.ReorgLimit {
		c.log.Debug("Skipping block #%s as beyond re-org limit", block.BlockNumber())
		return false
	}

	_, err := c.state.GetBlock(block.HeaderHash())

	if err == nil {
		c.log.Debug("Skipping block #%s is duplicate block", block.BlockNumber())
		return false
	}

	batch := c.state.GetBatch()
	blockFlag, forkFlag := c.addBlock(block, batch)
	if blockFlag {
		if !forkFlag {
			c.state.WriteBatch(batch)
		}
		c.log.Info("Added Block #%s %s", block.BlockNumber(), string(block.HeaderHash()))
		return true
	}

	return false

}

func (c *Chain) applyBlock(block *block.Block, batch *leveldb.Batch) bool {
	addressesState := block.PrepareAddressesList()
	c.state.GetAddressesState(addressesState)
	if !block.ApplyStateChanges(addressesState) {
		return false
	}

	err := c.state.PutAddressesState(addressesState, batch)
	if err != nil {
		c.log.Warn("Failed to apply Block %s", err.Error())
		return false
	}

	return true
}

func (c *Chain) updateChainState(block *block.Block, batch *leveldb.Batch) {
	c.lastBlock = block
	c.updateBlockNumberMapping(block, batch)
	c.txPool.RemoveTxInBlock(block)
	c.state.PutChainHeight(block.BlockNumber(), batch)
	c.state.UpdateTxMetadata(block, batch)
}

func (c *Chain) updateBlockNumberMapping(block *block.Block, batch *leveldb.Batch) {
	blockNumberMapping := &generated.BlockNumberMapping{Headerhash:block.HeaderHash(), PrevHeaderhash:block.PrevHeaderHash()}
	c.state.PutBlockNumberMapping(block.BlockNumber(), blockNumberMapping, batch)
}

func (c *Chain) RemoveBlockFromMainchain(block *block.Block, blockNumber uint64, batch *leveldb.Batch) {
	addressesState := block.PrepareAddressesList()
	c.state.GetAddressesState(addressesState)
	for i := len(block.Transactions()) - 1; i <= 0; i-- {
		tx := transactions.ProtoToTransaction(block.Transactions()[i])
		tx.RevertStateChanges(addressesState)
		c.state.UnsetOTSKey(*addressesState[tx.AddrFromPK()], uint64(tx.OtsKey()))
	}

	c.txPool.AddTxFromBlock(block, blockNumber)
	c.state.PutChainHeight(block.BlockNumber() - 1, batch)
	c.state.RollbackTxMetadata(block, batch)
	c.state.RemoveBlockNumberMapping(block.BlockNumber())
	c.state.PutAddressesState(addressesState, batch)
}

func (c *Chain) Rollback(forkedHeaderHash []byte, forkState *generated.ForkState) [][]byte {
	var hashPath [][]byte

	for  ;!reflect.DeepEqual(c.lastBlock.HeaderHash(), forkedHeaderHash); {
		b, err := c.state.GetBlock(c.lastBlock.HeaderHash())

		if err != nil {
			c.log.Info("self.state.get_block(self.last_block.headerhash) returned None")
		}

		mainchainBlock, err := c.state.GetBlockByNumber(b.BlockNumber())

		if err != nil {
			c.log.Info("self.get_block_by_number(b.block_number) returned None")
		}

		if reflect.DeepEqual(b.HeaderHash(), mainchainBlock.HeaderHash()) {
			break
		}
		hashPath = append(hashPath, c.lastBlock.HeaderHash())

		batch := c.state.GetBatch()
		c.RemoveBlockFromMainchain(c.lastBlock, b.BlockNumber(), batch)

		if forkState != nil {
			forkState.OldMainchainHashPath = append(forkState.OldMainchainHashPath, c.lastBlock.HeaderHash())
			c.state.PutForkState(forkState, batch)
		}

		c.state.WriteBatch(batch)

		c.lastBlock, err = c.state.GetBlock(c.lastBlock.PrevHeaderHash())

		if err != nil {

		}
	}

	return hashPath
}

func (c *Chain) GetForkPoint(block *block.Block) ([]byte, [][]byte, error) {
	tmpBlock := block
	var err error
	var hashPath [][]byte
	for ;; {
		if block == nil {
			return nil, nil, errors.New("No Block Found " + string(block.HeaderHash()) +", Initiator " +
				string(tmpBlock.HeaderHash()))
		}
		mainchainBlock, err := c.state.GetBlockByNumber(block.BlockNumber())
		if err == nil && reflect.DeepEqual(mainchainBlock.HeaderHash(), block.HeaderHash()) {
			break
		}
		if block.BlockNumber() == 0 {
			return nil, nil, errors.New("Alternate chain genesis is different, Initiator " +
				string(tmpBlock.HeaderHash()))
		}
		hashPath = append(hashPath, block.HeaderHash())
		block, err = c.state.GetBlock(block.PrevHeaderHash())
		if err != nil {
			return nil, nil, err
		}
	}

	return block.HeaderHash(), hashPath, err
}

func (c *Chain) AddChain(hashPath [][]byte, forkState *generated.ForkState) bool {
	var start int

	for i := 0; i < len(hashPath); i++ {
		if reflect.DeepEqual(hashPath[i], c.lastBlock.HeaderHash()) {
			start = i + 1
			break
		}
	}

	for i := start; i < len(hashPath); i++ {
		headerHash := hashPath[i]
		block, err := c.state.GetBlock(headerHash)

		if err != nil {

		}

		batch := c.state.GetBatch()

		if !c.applyBlock(block, batch) {
			return false
		}

		c.updateChainState(block, batch)

		c.log.Debug("Apply block #%d - [batch %d | %s]", block.BlockNumber(), i, hashPath[i])
		c.state.WriteBatch(batch)
	}

	c.state.DeleteForkState()

	return true
}

func (c *Chain) forkRecovery(block *block.Block, forkState *generated.ForkState) bool {
	c.log.Info("Triggered Fork Recovery")

	var forkHeaderHash []byte
	var hashPath, oldHashPath [][]byte

	if len(forkState.ForkPointHeaderhash) > 0 {
		c.log.Info("Recovering from last fork recovery interruption")
		forkHeaderHash = forkState.ForkPointHeaderhash
		hashPath = forkState.NewMainchainHashPath
	} else {
		forkHeaderHash, hashPath, err := c.GetForkPoint(block)
		if err != nil {

		}
		forkState.ForkPointHeaderhash = forkHeaderHash
		forkState.NewMainchainHashPath = hashPath
		c.state.PutForkState(forkState, nil)
	}

	rollbackDone := false
	if len(forkState.OldMainchainHashPath) > 0 {
		b, err := c.state.GetBlock(forkState.OldMainchainHashPath[len(forkState.OldMainchainHashPath) - 1])
		if err == nil && reflect.DeepEqual(b.PrevHeaderHash(), forkState.ForkPointHeaderhash) {
			rollbackDone = true
		}
	}

	if !rollbackDone {
		c.log.Info("Rolling back")
		oldHashPath = c.Rollback(forkHeaderHash, forkState)
	} else {
		oldHashPath = forkState.OldMainchainHashPath
	}

	if !c.AddChain(misc.Reverse(hashPath), forkState) {
		c.log.Warn("Fork Recovery Failed... Recovering back to old mainchain")
		// Above condition is true, when the node failed to add_chain
		// Thus old chain state, must be retrieved
		c.Rollback(forkHeaderHash, nil)
		c.AddChain(misc.Reverse(oldHashPath), forkState)
		return false
	}

	c.log.Info("Fork Recovery Finished")

	c.triggerMiner = true
	return true
}

func (c *Chain) GetBlock(headerhash []byte) (*block.Block, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.state.GetBlock(headerhash)
}
