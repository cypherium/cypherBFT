package core

import (
	//	"bytes"

	"sync"
	"sync/atomic"

	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/cypherium/cypherBFT/common"
	"github.com/cypherium/cypherBFT/core/rawdb"
	"github.com/cypherium/cypherBFT/core/types"
	"github.com/cypherium/cypherBFT/cphdb"
	"github.com/cypherium/cypherBFT/event"
	"github.com/cypherium/cypherBFT/log"
	"github.com/cypherium/cypherBFT/params"
	"github.com/cypherium/cypherBFT/pow"
	"github.com/cypherium/cypherBFT/reconfig/bftview"
	"github.com/cypherium/cypherBFT/rlp"
	lru "github.com/hashicorp/golang-lru"
)

var (
	ErrNoKeyBlock = errors.New("Can't find KeyBlock")
)

type KeyBlockChain struct {
	chainConfig *params.ChainConfig // Chain & network configuration
	db          cphdb.Database      // Low level persistent database to store final content in

	khc *KeyHeaderChain
	//chainFeed     event.Feed
	chainHeadFeed event.Feed
	scope         event.SubscriptionScope
	genesisBlock  *types.KeyBlock

	mu      sync.RWMutex // global mutex for locking chain operations
	chainmu sync.RWMutex // insertion lock
	procmu  sync.RWMutex // block processor lock

	currentBlock  atomic.Value // Current head of the block chain
	currentBlockN uint64

	blockCache    *lru.Cache // Cache for the most recent entire blocks
	futureBlocks  *lru.Cache // future blocks are blocks added for later processing
	blockRLPCache *lru.Cache // Cache for the most recent entire blocks in rlp format

	quit    chan struct{} // blockchain quit channel
	running int32         // running must be called atomically

	// procInterrupt must be atomically called
	procInterrupt int32          // interrupt signaler for block processing
	wg            sync.WaitGroup // chain processing wait group for shutting down

	Validator *KeyBlockValidator // block and state validator interface
	badBlocks *lru.Cache         // Bad block cache

	engine pow.Engine
	mux    *event.TypeMux

	candidatePool *CandidatePool

	futureTxBlocks map[common.Hash][]*types.Block // future txblocks that is fetched before its keyblock arrives
	backend        Backend
	ProcInsertDone func(*types.KeyBlock)
}

// NewKeyBlockChain returns a fully initialised key block chain using information
// available in the database.
func NewKeyBlockChain(cph Backend, db cphdb.Database, cacheConfig *CacheConfig, chainConfig *params.ChainConfig, engine pow.Engine, mux *event.TypeMux) (*KeyBlockChain, error) {
	blockCache, _ := lru.New(blockCacheLimit)
	futureBlocks, _ := lru.New(maxFutureBlocks)
	badBlocks, _ := lru.New(badBlockLimit)
	blockRLPCache, _ := lru.New(bodyCacheLimit)

	kbc := &KeyBlockChain{
		chainConfig:    chainConfig,
		db:             db,
		quit:           make(chan struct{}),
		blockCache:     blockCache,
		futureBlocks:   futureBlocks,
		badBlocks:      badBlocks,
		blockRLPCache:  blockRLPCache,
		engine:         engine,
		mux:            mux,
		futureTxBlocks: make(map[common.Hash][]*types.Block),
		backend:        cph,
	}
	kbc.Validator = NewKeyBlockValidator(chainConfig, kbc)

	var err error
	kbc.khc, err = NewKeyHeaderChain(db, chainConfig, kbc.getProcInterrupt)
	if err != nil {
		return nil, err
	}

	h := kbc.GetHeaderByNumber(0)
	if h == nil {
		return nil, ErrNoGenesis
	}
	committee0 := bftview.LoadMember(0, h.Hash(), false)
	if committee0 == nil {
		return nil, ErrNoGenCommittee
	}

	kbc.genesisBlock = kbc.GetBlockByNumber(0)
	if kbc.genesisBlock == nil {
		return nil, ErrNoKeyGenesis
	}

	if err := kbc.loadLastState(); err != nil {
		return nil, err
	}
	//kbc.RollbackKeyChainFrom(params.KeyChainRollBackHash)
	go kbc.update()

	return kbc, nil
}

func (kbc *KeyBlockChain) Genesis() *types.KeyBlock {
	return kbc.genesisBlock
}
func (kbc *KeyBlockChain) getProcInterrupt() bool {
	return atomic.LoadInt32(&kbc.procInterrupt) == 1
}
func (kbc *KeyBlockChain) GetBlockByNumber(number uint64) *types.KeyBlock {
	hash := rawdb.ReadKeyBlockHash(kbc.db, number)
	if hash == (common.Hash{}) {
		return nil
	}
	return kbc.GetBlock(hash, number)
}

// GetBlockByHash retrieves a block from the database by hash, caching it if found.
func (kbc *KeyBlockChain) GetBlockByHash(hash common.Hash) *types.KeyBlock {
	number := kbc.khc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	return kbc.GetBlock(hash, *number)
}
func (kbc *KeyBlockChain) HasBlock(hash common.Hash, number uint64) bool {
	if kbc.blockCache.Contains(hash) {
		return true
	}
	return rawdb.HasKeyBlockBody(kbc.db, hash, number)
}

// GetBlock retrieves a block from the database by hash and number,
// caching it if found.
func (kbc *KeyBlockChain) GetBlock(hash common.Hash, number uint64) *types.KeyBlock {
	// Short circuit if the block's already in the cache, retrieve otherwise
	if block, ok := kbc.blockCache.Get(hash); ok {
		return block.(*types.KeyBlock)
	}
	block := rawdb.ReadKeyBlock(kbc.db, hash, number)
	if block == nil {
		return nil
	}

	// Cache the found block for next time and return
	kbc.blockCache.Add(block.Hash(), block)
	return block
}

// GetTd retrieves a block's total difficulty in the canonical chain from the
// database by hash and number, caching it if found.
func (kbc *KeyBlockChain) GetTd(hash common.Hash, number uint64) *big.Int {
	return kbc.khc.GetTd(hash, number)
}
func (kbc *KeyBlockChain) CurrentBlock() *types.KeyBlock {
	return kbc.currentBlock.Load().(*types.KeyBlock)
}
func (kbc *KeyBlockChain) CurrentBlockN() uint64 {
	return atomic.LoadUint64(&kbc.currentBlockN)
}
func (kbc *KeyBlockChain) CurrentBlockStore(block *types.KeyBlock) {
	kbc.currentBlock.Store(block)
	atomic.StoreUint64(&kbc.currentBlockN, block.NumberU64())
}

// GetKeyHeaderByHash retrieves a block header from the database by hash, caching it if
// found.
func (kbc *KeyBlockChain) GetHeaderByHash(hash common.Hash) *types.KeyBlockHeader {
	return kbc.khc.GetHeaderByHash(hash)
}

// GetKeyHeaderByNumber retrieves a block header from the database by number,
// caching it (associated with its hash) if found.
func (kbc *KeyBlockChain) GetHeaderByNumber(number uint64) *types.KeyBlockHeader {
	return kbc.khc.GetHeaderByNumber(number)
}
func (kbc *KeyBlockChain) CurrentHeader() *types.KeyBlockHeader {
	return kbc.khc.CurrentHeader()
}
func (kbc *KeyBlockChain) GetHeader(hash common.Hash, number uint64) *types.KeyBlockHeader {
	return kbc.khc.GetHeader(hash, number)
}

// SetHead rewinds the local key blockchain to a new head. In the case of headers, everything
// above the new head will be deleted and the new one set. In the case of blocks
// though, the head may be further rewound if block bodies are missing (non-archive
// nodes after a fast sync).
func (kbc *KeyBlockChain) SetHead(head uint64) error {
	log.Warn("Rewinding key blockchain", "target", head)

	kbc.mu.Lock()
	defer kbc.mu.Unlock()

	// Rewind the header chain, deleting all block bodies until then
	delFn := func(db rawdb.DatabaseDeleter, hash common.Hash, num uint64) {
		rawdb.DeleteKeyBlock(db, hash, num)
	}
	kbc.khc.SetHead(head, delFn)
	currentHeader := kbc.khc.CurrentHeader()

	// Clear out any stale content from the caches
	kbc.blockCache.Purge()
	kbc.futureBlocks.Purge()
	kbc.blockRLPCache.Purge()

	// Rewind the block chain, ensuring we don't end up with a stateless head block
	if currentBlock := kbc.CurrentBlock(); currentBlock != nil && currentHeader.Number.Uint64() < currentBlock.NumberU64() {
		kbc.currentBlock.Store(kbc.GetBlock(currentHeader.Hash(), currentHeader.Number.Uint64()))
		atomic.StoreUint64(&kbc.currentBlockN, currentHeader.Number.Uint64())
	}
	// If either blocks reached nil, reset to the genesis state
	if currentBlock := kbc.CurrentBlock(); currentBlock == nil {
		kbc.currentBlock.Store(kbc.genesisBlock)
		atomic.StoreUint64(&kbc.currentBlockN, 0)
	}

	currentBlock := kbc.CurrentBlock()
	rawdb.WriteHeadKeyBlockHash(kbc.db, currentBlock.Hash())

	return kbc.loadLastState()
}
func (kbc *KeyBlockChain) SetCurrent(hash common.Hash) error {
	log.Info("KeyBlockChain SetCurrent", "hash", hash)
	block := kbc.GetBlockByHash(hash)
	if block == nil {
		return ErrNoKeyBlock
	}
	kbc.mu.Lock()
	defer kbc.mu.Unlock()

	kbc.khc.SetCurrentHeader(block.Header())
	kbc.currentBlock.Store(block)
	atomic.StoreUint64(&kbc.currentBlockN, block.NumberU64())
	rawdb.WriteHeadBlockHash(kbc.db, block.Hash())
	return nil
}

func (kbc *KeyBlockChain) RollbackKeyChainFrom(hash common.Hash) error {
	if hash == (common.Hash{}) {
		return nil
	}

	log.Info("RollbackKeyChainFrom", "hash", hash)
	return kbc.SetCurrent(hash)
}

// Reset purges the entire blockchain, restoring it to its genesis state.
func (kbc *KeyBlockChain) Reset() error {
	return kbc.ResetWithGenesisBlock(kbc.genesisBlock)
}

// ResetWithGenesisBlock purges the entire blockchain, restoring it to the
// specified genesis state.
func (kbc *KeyBlockChain) ResetWithGenesisBlock(genesis *types.KeyBlock) error {
	// Dump the entire block chain and purge the caches
	if err := kbc.SetHead(0); err != nil {
		return err
	}
	kbc.mu.Lock()
	defer kbc.mu.Unlock()

	if err := kbc.khc.WriteTd(genesis.Hash(), genesis.NumberU64(), genesis.Difficulty()); err != nil {
		log.Crit("Failed to write genesis block TD", "err", err)
	}
	rawdb.WriteKeyBlock(kbc.db, genesis)

	kbc.genesisBlock = genesis
	kbc.insert(kbc.genesisBlock)
	committee0 := bftview.LoadMember(0, genesis.Hash(), true)
	kbc.genesisBlock.SetCommitteeHash(committee0.RlpHash())
	committee0.Store(kbc.genesisBlock)
	kbc.khc.SetGenesis(kbc.genesisBlock.Header())

	return nil
}

// loadLastState loads the last known chain state from the database. This method
// assumes that the chain manager mutex is held.
func (kbc *KeyBlockChain) loadLastState() error {
	// Restore the last known head block
	head := rawdb.ReadHeadKeyBlockHash(kbc.db)
	if head == (common.Hash{}) {
		// Corrupt or empty database, init from scratch
		log.Warn("Empty database, resetting chain")
		return kbc.Reset()
	}
	// Make sure the entire head block is available
	currentBlock := kbc.GetBlockByHash(head)
	if currentBlock == nil {
		// Corrupt or empty database, init from scratch
		log.Warn("Head block missing, resetting chain", "hash", head)
		return kbc.Reset()
	}

	// Everything seems to be fine, set as the head block
	kbc.currentBlock.Store(currentBlock)
	atomic.StoreUint64(&kbc.currentBlockN, currentBlock.NumberU64())
	// Restore the last known head header
	currentHeader := currentBlock.Header()
	if head := rawdb.ReadHeadHeaderHash(kbc.db); head != (common.Hash{}) {
		if header := kbc.GetHeaderByHash(head); header != nil {
			currentHeader = header
		}
	}
	kbc.khc.SetCurrentHeader(currentHeader)

	headerTd := kbc.GetTd(currentHeader.Hash(), currentHeader.Number.Uint64())
	blockTd := kbc.GetTd(currentBlock.Hash(), currentBlock.NumberU64())

	log.Info("Loaded most recent local keyblock header", "number", currentHeader.Number, "hash", currentHeader.Hash(), "td", headerTd)
	log.Info("Loaded most recent local full keyblock", "number", currentBlock.Number(), "hash", currentBlock.Hash(), "td", blockTd)

	return nil
}

// insert injects a new head keyblock into the current keyblock chain.
// Note, this function assumes that the `mu` mutex is held!
func (kbc *KeyBlockChain) insert(block *types.KeyBlock) error {
	if err := kbc.khc.WriteTd(block.Hash(), block.NumberU64(), block.Difficulty()); err != nil {
		return err
	}
	rawdb.WriteKeyBlock(kbc.db, block)
	rawdb.WriteKeyBlockHash(kbc.db, block.Hash(), block.NumberU64())
	rawdb.WriteHeadKeyBlockHash(kbc.db, block.Hash())

	kbc.currentBlock.Store(block)
	atomic.StoreUint64(&kbc.currentBlockN, block.NumberU64())
	kbc.khc.SetCurrentHeader(block.Header())

	return nil
}

func (kbc *KeyBlockChain) InsertBlock(block *types.KeyBlock) error {
	log.Trace("kbc.InsertBlock..", "number", block.NumberU64())
	_, err := kbc.insert_Chain(types.KeyBlocks{block})
	log.Trace("kbc.InsertBlock..end", "number", block.NumberU64())
	return err
}
func (kbc *KeyBlockChain) InsertChain(chain types.KeyBlocks) (int, error) {
	log.Trace("kbc.InsertChain..", "number", chain[0].NumberU64())
	i, err := kbc.insert_Chain(chain)
	log.Trace("kbc.InsertChain..end", "number", chain[0].NumberU64())
	return i, err
}
func (kbc *KeyBlockChain) PostRosterConfigEvent(data interface{}) error {
	kbc.mux.Post(RosterConfigEvent{RosterConfigData: data})
	return nil
}

// InsertChain attempts to insert the given batch of key blocks in to the keyblock
// chain. If an error is returned it will return the index number of the failing block
// as well an error describing what went wrong.
func (kbc *KeyBlockChain) insert_Chain(chain types.KeyBlocks) (int, error) {
	// Sanity check that we have something meaningful to import
	if len(chain) == 0 {
		return 0, nil
	}
	// Do a sanity check that the provided chain is actually ordered and linked
	for i := 1; i < len(chain); i++ {
		if chain[i].NumberU64() != chain[i-1].NumberU64()+1 || chain[i].ParentHash() != chain[i-1].Hash() {
			// Chain broke ancestry, log a messge (programming error) and skip insertion
			log.Error("Non contiguous key block insert", "number", chain[i].Number(), "hash", chain[i].Hash(),
				"parent", chain[i].ParentHash(), "prevnumber", chain[i-1].Number(), "prevhash", chain[i-1].Hash())

			return 0, fmt.Errorf("non contiguous insert: item %d is #%d [%x…], item %d is #%d [%x…] (parent [%x…])", i-1, chain[i-1].NumberU64(),
				chain[i-1].Hash().Bytes()[:4], i, chain[i].NumberU64(), chain[i].Hash().Bytes()[:4], chain[i].ParentHash().Bytes()[:4])
		}
	}
	// Pre-checks passed, start the full block imports
	kbc.wg.Add(1)
	defer kbc.wg.Done()

	kbc.chainmu.Lock()
	defer kbc.chainmu.Unlock()

	currentBlock := kbc.CurrentBlock()

	var lastBlock *types.KeyBlock
	// Iterate over the blocks and insert when the verifier permits
	for i, block := range chain {
		// If the chain is terminating, stop processing blocks
		if atomic.LoadInt32(&kbc.procInterrupt) == 1 {
			log.Debug("Premature abort during key blocks processing")
			break
		}

		err := kbc.Validator.ValidateKeyBlock(block)
		switch {
		case err == types.ErrKnownBlock:
			// Block and state both already known. However if the current block is below
			// this number we did a rollback and we should reimport it nonetheless.
			if kbc.CurrentBlockN() >= block.NumberU64() {
				continue
			}
		case err == types.ErrFutureBlock:
			// Allow up to MaxFuture second in the future blocks. If this limit is exceeded
			// the chain is discarded and processed at a later time if given.
			max := big.NewInt(time.Now().Unix() + maxTimeFutureBlocks)
			if block.Time().Cmp(max) > 0 {
				return i, fmt.Errorf("future block: %v > %v", block.Time(), max)
			}
			kbc.futureBlocks.Add(block.Hash(), block)
			continue

		case err == types.ErrUnknownAncestor && kbc.futureBlocks.Contains(block.ParentHash()):
			kbc.futureBlocks.Add(block.Hash(), block)
			continue

		case err != nil:
			kbc.reportBlock(block, err)
			return i, err

			continue
		}

		if err := kbc.insert(block); err != nil {
			return i, err
		}
		if kbc.ProcInsertDone != nil {
			kbc.ProcInsertDone(block)
		}

		lastBlock = block
	}

	// Append a single chain head event if we've progressed the chain
	if lastBlock != nil && currentBlock.Hash() != lastBlock.Hash() {
		//log.Info("Key Chain Head event", "last block hash", lastBlock.Hash(), "number", lastBlock.Number(), "current block hash", currentBlock.Hash(), "number", currentBlock.Number())

		go kbc.mux.Post(KeyChainHeadEvent{KeyBlock: lastBlock})
		//??kbc.chainHeadFeed.Send(KeyChainHeadEvent{KeyBlock: lastBlock})
	}

	return 0, nil
}

// addBadBlock adds a bad block to the bad-block LRU cache
func (kbc *KeyBlockChain) addBadBlock(block *types.KeyBlock) {
	kbc.badBlocks.Add(block.Hash(), block)
}

func (kbc *KeyBlockChain) reportBlock(block *types.KeyBlock, err error) {
	kbc.addBadBlock(block)

	log.Warn(fmt.Sprintf(`
########## KEY BLOCK #########
Number: %v
Hash: 0x%x

Error: %v
##############################
`, block.Number(), block.Hash(), err))
}

func (kbc *KeyBlockChain) update() {
	futureTimer := time.NewTicker(5 * time.Second)
	defer futureTimer.Stop()
	for {
		select {
		case <-futureTimer.C:
			kbc.procFutureBlocks()
		case <-kbc.quit:
			return
		}
	}
}

func (kbc *KeyBlockChain) procFutureBlocks() {
	blocks := make([]*types.KeyBlock, 0, kbc.futureBlocks.Len())
	for _, hash := range kbc.futureBlocks.Keys() {
		if block, exist := kbc.futureBlocks.Peek(hash); exist {
			blocks = append(blocks, block.(*types.KeyBlock))
		}
	}
	if len(blocks) > 0 {
		sort.Sort(types.SortKeyBlocksByNumber(blocks))

		// Insert one by one as chain insertion needs contiguous ancestry between blocks
		for i := range blocks {
			kbc.InsertChain(blocks[i : i+1])
		}
	}
}

// Stop stops the key blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
func (kbc *KeyBlockChain) Stop() {
	if !atomic.CompareAndSwapInt32(&kbc.running, 0, 1) {
		return
	}
	// Unsubscribe all subscriptions registered from blockchain
	kbc.scope.Close()
	close(kbc.quit)
	atomic.StoreInt32(&kbc.procInterrupt, 1)

	kbc.wg.Wait()

	log.Info("key blockchain manager stopped")
}

func (kbc *KeyBlockChain) FinalizeKeyBlock(header *types.KeyBlockHeader) (*types.KeyBlock, error) {
	return types.NewKeyBlock(header), nil
}

// Config retrieves the blockchain's chain configuration.
func (kbc *KeyBlockChain) Config() *params.ChainConfig { return kbc.chainConfig }

func (kbc *KeyBlockChain) MockBlock(amount int64) {
	genKeyBlock := func(i int, parent *types.KeyBlock) *types.KeyBlock {
		b := types.NewKeyBlock(makeKeyHeader(nil, parent, kbc.engine))

		return b.WithSignatrue([]byte("I am signatrue"), nil)
	}

	blocks := make([]*types.KeyBlock, 0, amount)
	parent := kbc.CurrentBlock()

	for i := 0; i < int(amount); i++ {
		block := genKeyBlock(1, parent)
		log.Trace("Mock key block", "number", block.NumberU64(), "parentNumber", parent.NumberU64())
		blocks = append(blocks, block)

		parent = block
	}

	log.Info("Mock key block", "amount", amount)

	kbc.InsertChain(blocks)
}

func (kbc *KeyBlockChain) AnnounceBlock(number uint64) {
	block := kbc.GetBlockByNumber(number)

	kbc.mux.Post(KeyChainHeadEvent{KeyBlock: block})
}

// GetBlockRLPByHash retrieves a block in RLP encoding from the database by hash,
// caching it if found.
func (kbc *KeyBlockChain) GetBlockRLPByHash(hash common.Hash) rlp.RawValue {
	// Short circuit if the blocks's already in the cache, retrieve otherwise
	if cached, ok := kbc.blockRLPCache.Get(hash); ok {
		return cached.([]uint8)
	}
	number := kbc.khc.GetBlockNumber(hash)
	if number == nil {
		log.Trace("Get block number by hash returns err", "hash", hash.Hex())
		return nil
	}
	block := rawdb.ReadKeyBlock(kbc.db, hash, *number)
	if block == nil {
		log.Trace("Read key block returns error", "hash", hash.Hex(), "number", *number)
		return nil
	}

	rlpBlock, err := rlp.EncodeToBytes(block)
	if err == nil {
		kbc.blockRLPCache.Add(hash, rlpBlock)
		return rlpBlock
	} else {
		return nil
	}
}
func (kbc *KeyBlockChain) GetBlockRLPByNumber(number uint64) rlp.RawValue {
	hash := rawdb.ReadKeyBlockHash(kbc.db, number)
	if hash == (common.Hash{}) {
		return nil
	}

	return kbc.GetBlockRLPByHash(hash)
}
func (kbc *KeyBlockChain) EncodeBlockToBytes(hash common.Hash, block *types.KeyBlock) rlp.RawValue {
	// Short circuit if the blocks's already in the cache, retrieve otherwise
	if cached, ok := kbc.blockRLPCache.Get(hash); ok {
		return cached.([]uint8)
	}

	rlpBlock, err := rlp.EncodeToBytes(block)
	if err == nil {
		kbc.blockRLPCache.Add(hash, rlpBlock)
		return rlpBlock
	} else {
		return nil
	}
}

// SubscribeChainEvent registers a subscription of ChainEvent.
func (kbc *KeyBlockChain) SubscribeChainEvent(ch chan<- KeyChainHeadEvent) event.Subscription {
	return kbc.scope.Track(kbc.chainHeadFeed.Subscribe(ch))
}

func (kbc *KeyBlockChain) AddFutureTxBlock(block *types.Block) {
	keyHash := block.GetKeyHash()

	if _, ok := kbc.futureTxBlocks[keyHash]; !ok {
		kbc.futureTxBlocks[keyHash] = make([]*types.Block, 0)
	}

	log.Trace("keyblockchain: add future tx block", "keyHash", keyHash, "txhash", block.Hash())
	kbc.futureTxBlocks[keyHash] = append(kbc.futureTxBlocks[keyHash], block)
}

func (kbc *KeyBlockChain) GetCommitteeByHash(hash common.Hash) []*common.Cnode {
	number := kbc.khc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	return kbc.GetCommitteeByNumber(*number)
}

// CurrentBlock retrieves the current committee of the canonical chain. The
// block is retrieved from the blockchain's internal cache.
func (kbc *KeyBlockChain) CurrentCommittee() []*common.Cnode {
	keyblock := kbc.CurrentBlock()
	c := bftview.LoadMember(keyblock.NumberU64(), keyblock.Hash(), false)
	if c != nil {
		return c.List
	}
	log.Warn("CurrentCommittee not found committee", "number", keyblock.NumberU64())
	return nil
}
func (kbc *KeyBlockChain) GetCommitteeByNumber(kNumber uint64) []*common.Cnode {
	blockSrc := kbc.GetBlockByNumber(kNumber)
	if blockSrc == nil {
		return nil
	}
	c := bftview.LoadMember(kNumber, blockSrc.Hash(), false)
	if c != nil {
		return c.List
	}
	log.Warn("GetCommitteeByNumber not found committee", "number", kNumber)
	return nil
}
