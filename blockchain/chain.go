// Copyright (c) 2013-2016 The btcsuite developers
// Copyright (c) 2015-2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"container/list"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/decred/dcrd/blockchain/stake"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/database"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/txscript"
	"github.com/decred/dcrd/wire"
)

const (
	// maxOrphanBlocks is the maximum number of orphan blocks that can be
	// queued.
	maxOrphanBlocks = 500

	// minMemoryNodes is the minimum number of consecutive nodes needed
	// in memory in order to perform all necessary validation.  It is used
	// to determine when it's safe to prune nodes from memory without
	// causing constant dynamic reloading.  This value should be larger than
	// that for minMemoryStakeNodes.
	minMemoryNodes = 2880

	// minMemoryStakeNodes is the maximum height to keep stake nodes
	// in memory for in their respective nodes.  Beyond this height,
	// they will need to be manually recalculated.  This value should
	// be at least the stake retarget interval.
	minMemoryStakeNodes = 288

	// mainchainBlockCacheSize is the number of mainchain blocks to
	// keep in memory, by height from the tip of the mainchain.
	mainchainBlockCacheSize = 12
)

// panicf is a convenience function that formats according to the given format
// specifier and arguments and then logs the result at the critical level and
// panics with it.
func panicf(format string, args ...interface{}) {
	str := fmt.Sprintf(format, args...)
	log.Critical(str)
	panic(str)
}

// BlockLocator is used to help locate a specific block.  The algorithm for
// building the block locator is to add the hashes in reverse order until
// the genesis block is reached.  In order to keep the list of locator hashes
// to a reasonable number of entries, first the most recent previous 12 block
// hashes are added, then the step is doubled each loop iteration to
// exponentially decrease the number of hashes as a function of the distance
// from the block being located.
//
// For example, assume a block chain with a side chain as depicted below:
// 	genesis -> 1 -> 2 -> ... -> 15 -> 16  -> 17  -> 18
// 	                              \-> 16a -> 17a
//
// The block locator for block 17a would be the hashes of blocks:
// [17a 16a 15 14 13 12 11 10 9 8 7 6 4 genesis]
type BlockLocator []*chainhash.Hash

// orphanBlock represents a block that we don't yet have the parent for.  It
// is a normal block plus an expiration time to prevent caching the orphan
// forever.
type orphanBlock struct {
	block      *dcrutil.Block
	expiration time.Time
}

// BestState houses information about the current best block and other info
// related to the state of the main chain as it exists from the point of view of
// the current best block.
//
// The BestSnapshot method can be used to obtain access to this information
// in a concurrent safe manner and the data will not be changed out from under
// the caller when chain state changes occur as the function name implies.
// However, the returned snapshot must be treated as immutable since it is
// shared by all callers.
type BestState struct {
	Hash               chainhash.Hash   // The hash of the block.
	PrevHash           chainhash.Hash   // The previous block hash.
	Height             int64            // The height of the block.
	Bits               uint32           // The difficulty bits of the block.
	NextPoolSize       uint32           // The ticket pool size.
	NextStakeDiff      int64            // The next stake difficulty.
	BlockSize          uint64           // The size of the block.
	NumTxns            uint64           // The number of txns in the block.
	TotalTxns          uint64           // The total number of txns in the chain.
	MedianTime         time.Time        // Median time as per CalcPastMedianTime.
	TotalSubsidy       int64            // The total subsidy for the chain.
	NextWinningTickets []chainhash.Hash // The eligible tickets to vote on the next block.
	MissedTickets      []chainhash.Hash // The missed tickets set to be revoked.
	NextFinalState     [6]byte          // The calculated state of the lottery for the next block.
}

// newBestState returns a new best stats instance for the given parameters.
func newBestState(node *blockNode, blockSize, numTxns, totalTxns uint64,
	medianTime time.Time, totalSubsidy int64, nextPoolSize uint32,
	nextStakeDiff int64, nextWinners, missed []chainhash.Hash,
	nextFinalState [6]byte) *BestState {
	prevHash := *zeroHash
	if node.parent != nil {
		prevHash = node.parent.hash
	}
	return &BestState{
		Hash:               node.hash,
		PrevHash:           prevHash,
		Height:             node.height,
		Bits:               node.bits,
		NextPoolSize:       nextPoolSize,
		NextStakeDiff:      nextStakeDiff,
		BlockSize:          blockSize,
		NumTxns:            numTxns,
		TotalTxns:          totalTxns,
		MedianTime:         medianTime,
		TotalSubsidy:       totalSubsidy,
		NextWinningTickets: nextWinners,
		MissedTickets:      missed,
		NextFinalState:     nextFinalState,
	}
}

// BlockChain provides functions for working with the Decred block chain.
// It includes functionality such as rejecting duplicate blocks, ensuring blocks
// follow all rules, orphan handling, checkpoint handling, and best chain
// selection with reorganization.
type BlockChain struct {
	// The following fields are set when the instance is created and can't
	// be changed afterwards, so there is no need to protect them with a
	// separate mutex.
	checkpointsByHeight map[int64]*chaincfg.Checkpoint
	db                  database.DB
	dbInfo              *databaseInfo
	chainParams         *chaincfg.Params
	timeSource          MedianTimeSource
	notifications       NotificationCallback
	sigCache            *txscript.SigCache
	indexManager        IndexManager
	interrupt           <-chan struct{}

	// subsidyCache is the cache that provides quick lookup of subsidy
	// values.
	subsidyCache *SubsidyCache

	// chainLock protects concurrent access to the vast majority of the
	// fields in this struct below this point.
	chainLock sync.RWMutex

	// These fields are configuration parameters that can be toggled at
	// runtime.  They are protected by the chain lock.
	noVerify      bool
	noCheckpoints bool

	// These fields are related to the memory block index.  They both have
	// their own locks, however they are often also protected by the chain
	// lock to help prevent logic races when blocks are being processed.
	//
	// index houses the entire block index in memory.  The block index is
	// a tree-shaped structure.
	//
	// bestChain tracks the current active chain by making use of an
	// efficient chain view into the block index.
	index     *blockIndex
	bestChain *chainView

	// These fields are related to handling of orphan blocks.  They are
	// protected by a combination of the chain lock and the orphan lock.
	orphanLock   sync.RWMutex
	orphans      map[chainhash.Hash]*orphanBlock
	prevOrphans  map[chainhash.Hash][]*orphanBlock
	oldestOrphan *orphanBlock

	// The block cache for mainchain blocks, to facilitate faster
	// reorganizations.
	mainchainBlockCacheLock sync.RWMutex
	mainchainBlockCache     map[chainhash.Hash]*dcrutil.Block
	mainchainBlockCacheSize int

	// These fields are related to checkpoint handling.  They are protected
	// by the chain lock.
	nextCheckpoint *chaincfg.Checkpoint
	checkpointNode *blockNode

	// The state is used as a fairly efficient way to cache information
	// about the current best chain state that is returned to callers when
	// requested.  It operates on the principle of MVCC such that any time a
	// new block becomes the best block, the state pointer is replaced with
	// a new struct and the old state is left untouched.  In this way,
	// multiple callers can be pointing to different best chain states.
	// This is acceptable for most callers because the state is only being
	// queried at a specific point in time.
	//
	// In addition, some of the fields are stored in the database so the
	// chain state can be quickly reconstructed on load.
	stateLock     sync.RWMutex
	stateSnapshot *BestState

	// The following caches are used to efficiently keep track of the
	// current deployment threshold state of each rule change deployment.
	//
	// This information is stored in the database so it can be quickly
	// reconstructed on load.
	//
	// deploymentCaches caches the current deployment threshold state for
	// blocks in each of the actively defined deployments.
	deploymentCaches map[uint32][]thresholdStateCache

	// pruner is the automatic pruner for block nodes and stake nodes,
	// so that the memory may be restored by the garbage collector if
	// it is unlikely to be referenced in the future.
	pruner *chainPruner

	// The following maps are various caches for the stake version/voting
	// system.  The goal of these is to reduce disk access to load blocks
	// from disk.  Measurements indicate that it is slightly more expensive
	// so setup the cache (<10%) vs doing a straight chain walk.  Every
	// other subsequent call is >10x faster.
	isVoterMajorityVersionCache   map[[stakeMajorityCacheKeySize]byte]bool
	isStakeMajorityVersionCache   map[[stakeMajorityCacheKeySize]byte]bool
	calcPriorStakeVersionCache    map[[chainhash.HashSize]byte]uint32
	calcVoterVersionIntervalCache map[[chainhash.HashSize]byte]uint32
	calcStakeVersionCache         map[[chainhash.HashSize]byte]uint32
}

const (
	// stakeMajorityCacheKeySize is comprised of the stake version and the
	// hash size.  The stake version is a little endian uint32, hence we
	// add 4 to the overall size.
	stakeMajorityCacheKeySize = 4 + chainhash.HashSize
)

// StakeVersions is a condensed form of a dcrutil.Block that is used to prevent
// using gigabytes of memory.
type StakeVersions struct {
	Hash         chainhash.Hash
	Height       int64
	BlockVersion int32
	StakeVersion uint32
	Votes        []stake.VoteVersionTuple
}

// GetStakeVersions returns a cooked array of StakeVersions.  We do this in
// order to not bloat memory by returning raw blocks.
func (b *BlockChain) GetStakeVersions(hash *chainhash.Hash, count int32) ([]StakeVersions, error) {
	// NOTE: The requirement for the node being fully validated here is strictly
	// stronger than what is actually required.  In reality, all that is needed
	// is for the block data for the node and all of its ancestors to be
	// available, but there is not currently any tracking to be able to
	// efficiently determine that state.
	startNode := b.index.LookupNode(hash)
	if startNode == nil || !b.index.NodeStatus(startNode).KnownValid() {
		return nil, fmt.Errorf("block %s is not known", hash)
	}

	// Nothing to do if no count requested.
	if count == 0 {
		return nil, nil
	}

	if count < 0 {
		return nil, fmt.Errorf("count must not be less than zero - "+
			"got %d", count)
	}

	// Limit the requested count to the max possible for the requested block.
	if count > int32(startNode.height+1) {
		count = int32(startNode.height + 1)
	}

	result := make([]StakeVersions, 0, count)
	prevNode := startNode
	for i := int32(0); prevNode != nil && i < count; i++ {
		sv := StakeVersions{
			Hash:         prevNode.hash,
			Height:       prevNode.height,
			BlockVersion: prevNode.blockVersion,
			StakeVersion: prevNode.stakeVersion,
			Votes:        prevNode.votes,
		}

		result = append(result, sv)

		prevNode = prevNode.parent
	}

	return result, nil
}

// VoteInfo represents information on agendas and their respective states for
// a consensus deployment.
type VoteInfo struct {
	Agendas      []chaincfg.ConsensusDeployment
	AgendaStatus []ThresholdStateTuple
}

// GetVoteInfo returns information on consensus deployment agendas
// and their respective states at the provided hash, for the provided
// deployment version.
func (b *BlockChain) GetVoteInfo(hash *chainhash.Hash, version uint32) (*VoteInfo, error) {
	deployments, ok := b.chainParams.Deployments[version]
	if !ok {
		return nil, VoteVersionError(version)
	}

	if !ok {
		return nil, HashError(hash.String())
	}

	vi := VoteInfo{
		Agendas: make([]chaincfg.ConsensusDeployment,
			0, len(deployments)),
		AgendaStatus: make([]ThresholdStateTuple, 0, len(deployments)),
	}
	for _, deployment := range deployments {
		vi.Agendas = append(vi.Agendas, deployment)
		status, err := b.NextThresholdState(hash, version, deployment.Vote.Id)
		if err != nil {
			return nil, err
		}
		vi.AgendaStatus = append(vi.AgendaStatus, status)
	}

	return &vi, nil
}

// DisableVerify provides a mechanism to disable transaction script validation
// which you DO NOT want to do in production as it could allow double spends
// and other undesirable things.  It is provided only for debug purposes since
// script validation is extremely intensive and when debugging it is sometimes
// nice to quickly get the chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) DisableVerify(disable bool) {
	b.chainLock.Lock()
	b.noVerify = disable
	b.chainLock.Unlock()
}

// TotalSubsidy returns the total subsidy mined so far in the best chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) TotalSubsidy() int64 {
	b.chainLock.RLock()
	ts := b.BestSnapshot().TotalSubsidy
	b.chainLock.RUnlock()

	return ts
}

// FetchSubsidyCache returns the current subsidy cache from the blockchain.
//
// This function is safe for concurrent access.
func (b *BlockChain) FetchSubsidyCache() *SubsidyCache {
	return b.subsidyCache
}

// HaveBlock returns whether or not the chain instance has the block represented
// by the passed hash.  This includes checking the various places a block can
// be like part of the main chain, on a side chain, or in the orphan pool.
//
// This function is safe for concurrent access.
func (b *BlockChain) HaveBlock(hash *chainhash.Hash) (bool, error) {
	return b.index.HaveBlock(hash) || b.IsKnownOrphan(hash), nil
}

// ChainWork returns the total work up to and including the block of the
// provided block hash.
func (b *BlockChain) ChainWork(hash *chainhash.Hash) (*big.Int, error) {
	node := b.index.LookupNode(hash)
	if node == nil {
		return nil, fmt.Errorf("block %s is not known", hash)
	}

	return node.workSum, nil
}

// IsKnownOrphan returns whether the passed hash is currently a known orphan.
// Keep in mind that only a limited number of orphans are held onto for a
// limited amount of time, so this function must not be used as an absolute
// way to test if a block is an orphan block.  A full block (as opposed to just
// its hash) must be passed to ProcessBlock for that purpose.  However, calling
// ProcessBlock with an orphan that already exists results in an error, so this
// function provides a mechanism for a caller to intelligently detect *recent*
// duplicate orphans and react accordingly.
//
// This function is safe for concurrent access.
func (b *BlockChain) IsKnownOrphan(hash *chainhash.Hash) bool {
	// Protect concurrent access.  Using a read lock only so multiple
	// readers can query without blocking each other.
	b.orphanLock.RLock()
	_, exists := b.orphans[*hash]
	b.orphanLock.RUnlock()

	return exists
}

// GetOrphanRoot returns the head of the chain for the provided hash from the
// map of orphan blocks.
//
// This function is safe for concurrent access.
func (b *BlockChain) GetOrphanRoot(hash *chainhash.Hash) *chainhash.Hash {
	// Protect concurrent access.  Using a read lock only so multiple
	// readers can query without blocking each other.
	b.orphanLock.RLock()
	defer b.orphanLock.RUnlock()

	// Keep looping while the parent of each orphaned block is
	// known and is an orphan itself.
	orphanRoot := hash
	prevHash := hash
	for {
		orphan, exists := b.orphans[*prevHash]
		if !exists {
			break
		}
		orphanRoot = prevHash
		prevHash = &orphan.block.MsgBlock().Header.PrevBlock
	}

	return orphanRoot
}

// removeOrphanBlock removes the passed orphan block from the orphan pool and
// previous orphan index.
func (b *BlockChain) removeOrphanBlock(orphan *orphanBlock) {
	// Protect concurrent access.
	b.orphanLock.Lock()
	defer b.orphanLock.Unlock()

	// Remove the orphan block from the orphan pool.
	orphanHash := orphan.block.Hash()
	delete(b.orphans, *orphanHash)

	// Remove the reference from the previous orphan index too.  An indexing
	// for loop is intentionally used over a range here as range does not
	// reevaluate the slice on each iteration nor does it adjust the index
	// for the modified slice.
	prevHash := &orphan.block.MsgBlock().Header.PrevBlock
	orphans := b.prevOrphans[*prevHash]
	for i := 0; i < len(orphans); i++ {
		hash := orphans[i].block.Hash()
		if hash.IsEqual(orphanHash) {
			copy(orphans[i:], orphans[i+1:])
			orphans[len(orphans)-1] = nil
			orphans = orphans[:len(orphans)-1]
			i--
		}
	}
	b.prevOrphans[*prevHash] = orphans

	// Remove the map entry altogether if there are no longer any orphans
	// which depend on the parent hash.
	if len(b.prevOrphans[*prevHash]) == 0 {
		delete(b.prevOrphans, *prevHash)
	}
}

// addOrphanBlock adds the passed block (which is already determined to be
// an orphan prior calling this function) to the orphan pool.  It lazily cleans
// up any expired blocks so a separate cleanup poller doesn't need to be run.
// It also imposes a maximum limit on the number of outstanding orphan
// blocks and will remove the oldest received orphan block if the limit is
// exceeded.
func (b *BlockChain) addOrphanBlock(block *dcrutil.Block) {
	// Remove expired orphan blocks.
	for _, oBlock := range b.orphans {
		if time.Now().After(oBlock.expiration) {
			b.removeOrphanBlock(oBlock)
			continue
		}

		// Update the oldest orphan block pointer so it can be discarded
		// in case the orphan pool fills up.
		if b.oldestOrphan == nil ||
			oBlock.expiration.Before(b.oldestOrphan.expiration) {
			b.oldestOrphan = oBlock
		}
	}

	// Limit orphan blocks to prevent memory exhaustion.
	if len(b.orphans)+1 > maxOrphanBlocks {
		// Remove the oldest orphan to make room for the new one.
		b.removeOrphanBlock(b.oldestOrphan)
		b.oldestOrphan = nil
	}

	// Protect concurrent access.  This is intentionally done here instead
	// of near the top since removeOrphanBlock does its own locking and
	// the range iterator is not invalidated by removing map entries.
	b.orphanLock.Lock()
	defer b.orphanLock.Unlock()

	// Insert the block into the orphan map with an expiration time
	// 1 hour from now.
	expiration := time.Now().Add(time.Hour)
	oBlock := &orphanBlock{
		block:      block,
		expiration: expiration,
	}
	b.orphans[*block.Hash()] = oBlock

	// Add to previous hash lookup index for faster dependency lookups.
	prevHash := &block.MsgBlock().Header.PrevBlock
	b.prevOrphans[*prevHash] = append(b.prevOrphans[*prevHash], oBlock)
}

// TipGeneration returns the entire generation of blocks stemming from the
// parent of the current tip.
//
// The function is safe for concurrent access.
func (b *BlockChain) TipGeneration() ([]chainhash.Hash, error) {
	b.chainLock.Lock()
	b.index.RLock()
	nodes := b.index.chainTips[b.bestChain.Tip().height]
	nodeHashes := make([]chainhash.Hash, len(nodes))
	for i, n := range nodes {
		nodeHashes[i] = n.hash
	}
	b.index.RUnlock()
	b.chainLock.Unlock()
	return nodeHashes, nil
}

// fetchMainChainBlockByNode returns the block from the main chain associated
// with the given node.  It first attempts to use cache and then falls back to
// loading it from the database.
//
// An error is returned if the block is either not found or not in the main
// chain.
//
// This function MUST be called with the chain lock held (for reads).
func (b *BlockChain) fetchMainChainBlockByNode(node *blockNode) (*dcrutil.Block, error) {
	// Ensure the block is in the main chain.
	if !b.bestChain.Contains(node) {
		str := fmt.Sprintf("block %s is not in the main chain", node.hash)
		return nil, errNotInMainChain(str)
	}

	b.mainchainBlockCacheLock.RLock()
	block, ok := b.mainchainBlockCache[node.hash]
	b.mainchainBlockCacheLock.RUnlock()
	if ok {
		return block, nil
	}

	// Load the block from the database.
	err := b.db.View(func(dbTx database.Tx) error {
		var err error
		block, err = dbFetchBlockByNode(dbTx, node)
		return err
	})
	return block, err
}

// fetchBlockByNode returns the block associated with the given node all known
// sources such as the internal caches and the database.  This function returns
// blocks regardless or whether or not they are part of the main chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) fetchBlockByNode(node *blockNode) (*dcrutil.Block, error) {
	// Check main chain cache.
	b.mainchainBlockCacheLock.RLock()
	block, ok := b.mainchainBlockCache[node.hash]
	b.mainchainBlockCacheLock.RUnlock()
	if ok {
		return block, nil
	}

	// Check orphan cache.
	b.orphanLock.RLock()
	orphan, existsOrphans := b.orphans[node.hash]
	b.orphanLock.RUnlock()
	if existsOrphans {
		return orphan.block, nil
	}

	// Load the block from the database.
	err := b.db.View(func(dbTx database.Tx) error {
		var err error
		block, err = dbFetchBlockByNode(dbTx, node)
		return err
	})
	return block, err
}

// pruneStakeNodes removes references to old stake nodes which should no
// longer be held in memory so as to keep the maximum memory usage down.
// It proceeds from the bestNode back to the determined minimum height node,
// finds all the relevant children, and then drops the the stake nodes from
// them by assigning nil and allowing the memory to be recovered by GC.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) pruneStakeNodes() {
	// Find the height to prune to.
	pruneToNode := b.bestChain.Tip()
	for i := int64(0); i < minMemoryStakeNodes-1 && pruneToNode != nil; i++ {
		pruneToNode = pruneToNode.parent
	}

	// Nothing to do if there are not enough nodes.
	if pruneToNode == nil || pruneToNode.parent == nil {
		return
	}

	// Push the nodes to delete on a list in reverse order since it's easier
	// to prune them going forwards than it is backwards.  This will
	// typically end up being a single node since pruning is currently done
	// just before each new node is created.  However, that might be tuned
	// later to only prune at intervals, so the code needs to account for
	// the possibility of multiple nodes.
	deleteNodes := list.New()
	for node := pruneToNode.parent; node != nil; node = node.parent {
		deleteNodes.PushFront(node)
	}

	// Loop through each node to prune, unlink its children, remove it from
	// the dependency index, and remove it from the node index.
	for e := deleteNodes.Front(); e != nil; e = e.Next() {
		node := e.Value.(*blockNode)
		// Do not attempt to prune if the node should already have been pruned,
		// for example if you're adding an old side chain block.
		if node.height > b.bestChain.Tip().height-minMemoryNodes {
			node.stakeNode = nil
			node.newTickets = nil
			node.ticketsVoted = nil
			node.ticketsRevoked = nil
		}
	}
}

// BestPrevHash returns the hash of the previous block of the block at HEAD.
//
// This function is safe for concurrent access.
func (b *BlockChain) BestPrevHash() chainhash.Hash {
	b.chainLock.Lock()
	defer b.chainLock.Unlock()

	var prevHash chainhash.Hash
	tip := b.bestChain.Tip()
	if tip.parent != nil {
		prevHash = tip.parent.hash
	}
	return prevHash
}

// isMajorityVersion determines if a previous number of blocks in the chain
// starting with startNode are at least the minimum passed version.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) isMajorityVersion(minVer int32, startNode *blockNode, numRequired uint64) bool {
	numFound := uint64(0)
	iterNode := startNode
	for i := uint64(0); i < b.chainParams.BlockUpgradeNumToCheck &&
		numFound < numRequired && iterNode != nil; i++ {

		// This node has a version that is at least the minimum version.
		if iterNode.blockVersion >= minVer {
			numFound++
		}

		iterNode = iterNode.parent
	}

	return numFound >= numRequired
}

// getReorganizeNodes finds the fork point between the main chain and the passed
// node and returns a list of block nodes that would need to be detached from
// the main chain and a list of block nodes that would need to be attached to
// the fork point (which will be the end of the main chain after detaching the
// returned list of block nodes) in order to reorganize the chain such that the
// passed node is the new end of the main chain.  The lists will be empty if the
// passed node is not on a side chain or if the reorganize would involve
// reorganizing to a known invalid chain.
//
// This function may modify the validation state of nodes in the block index
// without flushing.
//
// This function MUST be called with the chain state lock held (for reads).
func (b *BlockChain) getReorganizeNodes(node *blockNode) (*list.List, *list.List) {
	// Nothing to detach or attach if there is no node.
	attachNodes := list.New()
	detachNodes := list.New()
	if node == nil {
		return detachNodes, attachNodes
	}

	// Do not allow a reorganize to a known invalid chain.  Note that all
	// intermediate ancestors other than the direct parent are also checked
	// below, however, this check allows extra to work to be avoided in the
	// majority of cases since reorgs across multiple unvalidated blocks are
	// not very common.
	if b.index.NodeStatus(node.parent).KnownInvalid() {
		b.index.SetStatusFlags(node, statusInvalidAncestor)
		return detachNodes, attachNodes
	}

	// Find the fork point (if any) adding each block to the list of nodes
	// to attach to the main tree.  Push them onto the list in reverse order
	// so they are attached in the appropriate order when iterating the list
	// later.
	//
	// In the case a known invalid block is detected while constructing this
	// list, mark all of its descendants as having an invalid ancestor and
	// prevent the reorganize by not returning any nodes.
	forkNode := b.bestChain.FindFork(node)
	for n := node; n != nil && n != forkNode; n = n.parent {
		if b.index.NodeStatus(n).KnownInvalid() {
			for e := attachNodes.Front(); e != nil; e = e.Next() {
				dn := e.Value.(*blockNode)
				b.index.SetStatusFlags(dn, statusInvalidAncestor)
			}

			attachNodes.Init()
			return detachNodes, attachNodes
		}

		attachNodes.PushFront(n)
	}

	// Start from the end of the main chain and work backwards until the
	// common ancestor adding each block to the list of nodes to detach from
	// the main chain.
	for n := b.bestChain.Tip(); n != nil && n != forkNode; n = n.parent {
		detachNodes.PushBack(n)
	}

	return detachNodes, attachNodes
}

// pushMainChainBlockCache pushes a block onto the main chain block cache,
// and removes any old blocks from the cache that might be present.
func (b *BlockChain) pushMainChainBlockCache(block *dcrutil.Block) {
	curHeight := block.Height()
	curHash := block.Hash()
	b.mainchainBlockCacheLock.Lock()
	b.mainchainBlockCache[*curHash] = block
	for hash, bl := range b.mainchainBlockCache {
		if bl.Height() <= curHeight-int64(b.mainchainBlockCacheSize) {
			delete(b.mainchainBlockCache, hash)
		}
	}
	b.mainchainBlockCacheLock.Unlock()
}

// connectBlock handles connecting the passed node/block to the end of the main
// (best) chain.
//
// This passed utxo view must have all referenced txos the block spends marked
// as spent and all of the new txos the block creates added to it.  In addition,
// the passed stxos slice must be populated with all of the information for the
// spent txos.  This approach is used because the connection validation that
// must happen prior to calling this function requires the same details, so
// it would be inefficient to repeat it.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) connectBlock(node *blockNode, block, parent *dcrutil.Block, view *UtxoViewpoint, stxos []spentTxOut) error {
	// Make sure it's extending the end of the best chain.
	prevHash := block.MsgBlock().Header.PrevBlock
	tip := b.bestChain.Tip()
	if prevHash != tip.hash {
		panicf("block %v (height %v) connects to block %v instead of "+
			"extending the best chain (hash %v, height %v)", node.hash,
			node.height, prevHash, tip.hash, tip.height)
	}

	// Sanity check the correct number of stxos are provided.
	if len(stxos) != countSpentOutputs(block, parent) {
		panicf("provided %v stxos for block %v (height %v), but counted %v "+
			"spent utxos", len(stxos), node.hash, node.height,
			countSpentOutputs(block, parent))
	}

	// Write any modified block index entries to the database before
	// updating the best state.
	if err := b.flushBlockIndex(); err != nil {
		return err
	}

	// Get the stake node for this node, filling in any data that
	// may have yet to have been filled in.  In all cases this
	// should simply give a pointer to data already prepared, but
	// run this anyway to be safe.
	stakeNode, err := b.fetchStakeNode(node)
	if err != nil {
		return err
	}

	// Generate a new best state snapshot that will be used to update the
	// database and later memory if all database updates are successful.
	b.stateLock.RLock()
	curTotalTxns := b.stateSnapshot.TotalTxns
	curTotalSubsidy := b.stateSnapshot.TotalSubsidy
	b.stateLock.RUnlock()

	// Calculate the number of transactions that would be added by adding
	// this block.
	numTxns := countNumberOfTransactions(block, parent)

	// Calculate the exact subsidy produced by adding the block.
	subsidy := CalculateAddedSubsidy(block, parent)

	// Calcultate the next stake difficulty.
	nextStakeDiff, err := b.calcNextRequiredStakeDifficulty(node)
	if err != nil {
		return err
	}

	blockSize := uint64(block.MsgBlock().Header.Size)
	state := newBestState(node, blockSize, numTxns, curTotalTxns+numTxns,
		node.CalcPastMedianTime(), curTotalSubsidy+subsidy,
		uint32(node.stakeNode.PoolSize()), nextStakeDiff,
		node.stakeNode.Winners(), node.stakeNode.MissedTickets(),
		node.stakeNode.FinalState())

	// Atomically insert info into the database.
	err = b.db.Update(func(dbTx database.Tx) error {
		// Update best block state.
		err := dbPutBestState(dbTx, state, node.workSum)
		if err != nil {
			return err
		}

		// Update the utxo set using the state of the utxo view.  This
		// entails removing all of the utxos spent and adding the new
		// ones created by the block.
		err = dbPutUtxoView(dbTx, view)
		if err != nil {
			return err
		}

		// Update the transaction spend journal by adding a record for
		// the block that contains all txos spent by it.
		err = dbPutSpendJournalEntry(dbTx, block.Hash(), stxos)
		if err != nil {
			return err
		}

		// Insert the block into the stake database.
		err = stake.WriteConnectedBestNode(dbTx, stakeNode, node.hash)
		if err != nil {
			return err
		}

		// Allow the index manager to call each of the currently active
		// optional indexes with the block being connected so they can
		// update themselves accordingly.
		if b.indexManager != nil {
			err := b.indexManager.ConnectBlock(dbTx, block, parent, view)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Prune fully spent entries and mark all entries in the view unmodified
	// now that the modifications have been committed to the database.
	view.commit()

	// This node is now the end of the best chain.
	b.bestChain.SetTip(node)

	// Update the state for the best block.  Notice how this replaces the
	// entire struct instead of updating the existing one.  This effectively
	// allows the old version to act as a snapshot which callers can use
	// freely without needing to hold a lock for the duration.  See the
	// comments on the state variable for more details.
	b.stateLock.Lock()
	b.stateSnapshot = state
	b.stateLock.Unlock()

	// Assemble the current block and the parent into a slice.
	blockAndParent := []*dcrutil.Block{block, parent}

	// Notify the caller that the block was connected to the main chain.
	// The caller would typically want to react with actions such as
	// updating wallets.
	b.chainLock.Unlock()
	b.sendNotification(NTBlockConnected, blockAndParent)
	b.chainLock.Lock()

	// Send stake notifications about the new block.
	if node.height >= b.chainParams.StakeEnabledHeight {
		nextStakeDiff, err := b.calcNextRequiredStakeDifficulty(node)
		if err != nil {
			return err
		}

		// Notify of spent and missed tickets
		b.sendNotification(NTSpentAndMissedTickets,
			&TicketNotificationsData{
				Hash:            node.hash,
				Height:          node.height,
				StakeDifficulty: nextStakeDiff,
				TicketsSpent:    node.stakeNode.SpentByBlock(),
				TicketsMissed:   node.stakeNode.MissedByBlock(),
				TicketsNew:      []chainhash.Hash{},
			})
		// Notify of new tickets
		b.sendNotification(NTNewTickets,
			&TicketNotificationsData{
				Hash:            node.hash,
				Height:          node.height,
				StakeDifficulty: nextStakeDiff,
				TicketsSpent:    []chainhash.Hash{},
				TicketsMissed:   []chainhash.Hash{},
				TicketsNew:      node.stakeNode.NewTickets(),
			})
	}

	// Optimization: Before checkpoints, immediately dump the parent's stake
	// node because we no longer need it.
	if node.height < b.chainParams.LatestCheckpointHeight() {
		parent := b.bestChain.Tip().parent
		parent.stakeNode = nil
		parent.newTickets = nil
		parent.ticketsVoted = nil
		parent.ticketsRevoked = nil
	}

	b.pushMainChainBlockCache(block)

	return nil
}

// dropMainChainBlockCache drops a block from the main chain block cache.
func (b *BlockChain) dropMainChainBlockCache(block *dcrutil.Block) {
	curHash := block.Hash()
	b.mainchainBlockCacheLock.Lock()
	delete(b.mainchainBlockCache, *curHash)
	b.mainchainBlockCacheLock.Unlock()
}

// disconnectBlock handles disconnecting the passed node/block from the end of
// the main (best) chain.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) disconnectBlock(node *blockNode, block, parent *dcrutil.Block, view *UtxoViewpoint) error {
	// Make sure the node being disconnected is the end of the best chain.
	tip := b.bestChain.Tip()
	if node.hash != tip.hash {
		panicf("block %v (height %v) is not the end of the best chain "+
			"(hash %v, height %v)", node.hash, node.height, tip.hash,
			tip.height)
	}

	// Write any modified block index entries to the database before
	// updating the best state.
	if err := b.flushBlockIndex(); err != nil {
		return err
	}

	// Prepare the information required to update the stake database
	// contents.
	childStakeNode, err := b.fetchStakeNode(node)
	if err != nil {
		return err
	}
	parentStakeNode, err := b.fetchStakeNode(node.parent)
	if err != nil {
		return err
	}

	// Generate a new best state snapshot that will be used to update the
	// database and later memory if all database updates are successful.
	b.stateLock.RLock()
	curTotalTxns := b.stateSnapshot.TotalTxns
	curTotalSubsidy := b.stateSnapshot.TotalSubsidy
	b.stateLock.RUnlock()
	parentBlockSize := uint64(parent.MsgBlock().Header.Size)

	// Calculate the number of transactions that would be added by adding
	// this block.
	numTxns := countNumberOfTransactions(block, parent)
	newTotalTxns := curTotalTxns - numTxns

	// Calculate the exact subsidy produced by adding the block.
	subsidy := CalculateAddedSubsidy(block, parent)
	newTotalSubsidy := curTotalSubsidy - subsidy

	prevNode := node.parent
	state := newBestState(prevNode, parentBlockSize, numTxns, newTotalTxns,
		prevNode.CalcPastMedianTime(), newTotalSubsidy,
		uint32(prevNode.stakeNode.PoolSize()), node.sbits,
		prevNode.stakeNode.Winners(), prevNode.stakeNode.MissedTickets(),
		prevNode.stakeNode.FinalState())

	err = b.db.Update(func(dbTx database.Tx) error {
		// Update best block state.
		err := dbPutBestState(dbTx, state, node.workSum)
		if err != nil {
			return err
		}

		// Update the utxo set using the state of the utxo view.  This
		// entails restoring all of the utxos spent and removing the new
		// ones created by the block.
		err = dbPutUtxoView(dbTx, view)
		if err != nil {
			return err
		}

		// Update the transaction spend journal by removing the record
		// that contains all txos spent by the block .
		err = dbRemoveSpendJournalEntry(dbTx, block.Hash())
		if err != nil {
			return err
		}

		err = stake.WriteDisconnectedBestNode(dbTx, parentStakeNode,
			node.parent.hash, childStakeNode.UndoData())
		if err != nil {
			return err
		}

		// Allow the index manager to call each of the currently active
		// optional indexes with the block being disconnected so they
		// can update themselves accordingly.
		if b.indexManager != nil {
			err := b.indexManager.DisconnectBlock(dbTx, block, parent, view)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Prune fully spent entries and mark all entries in the view unmodified
	// now that the modifications have been committed to the database.
	view.commit()

	// This node's parent is now the end of the best chain.
	b.bestChain.SetTip(node.parent)

	// Update the state for the best block.  Notice how this replaces the
	// entire struct instead of updating the existing one.  This effectively
	// allows the old version to act as a snapshot which callers can use
	// freely without needing to hold a lock for the duration.  See the
	// comments on the state variable for more details.
	b.stateLock.Lock()
	b.stateSnapshot = state
	b.stateLock.Unlock()

	// Assemble the current block and the parent into a slice.
	blockAndParent := []*dcrutil.Block{block, parent}

	// Notify the caller that the block was disconnected from the main
	// chain.  The caller would typically want to react with actions such as
	// updating wallets.
	b.chainLock.Unlock()
	b.sendNotification(NTBlockDisconnected, blockAndParent)
	b.chainLock.Lock()

	b.dropMainChainBlockCache(block)

	return nil
}

// countSpentOutputs returns the number of utxos the passed block spends.
func countSpentOutputs(block *dcrutil.Block, parent *dcrutil.Block) int {
	// We need to skip the regular tx tree if it's not valid.
	// We also exclude the coinbase transaction since it can't
	// spend anything.
	var numSpent int
	if headerApprovesParent(&block.MsgBlock().Header) {
		for _, tx := range parent.Transactions()[1:] {
			numSpent += len(tx.MsgTx().TxIn)
		}
	}
	for _, stx := range block.MsgBlock().STransactions {
		txType := stake.DetermineTxType(stx)
		if txType == stake.TxTypeSSGen || txType == stake.TxTypeSSRtx {
			numSpent++
			continue
		}
		numSpent += len(stx.TxIn)
	}

	return numSpent
}

// countNumberOfTransactions returns the number of transactions inserted by
// adding the block.
func countNumberOfTransactions(block, parent *dcrutil.Block) uint64 {
	var numTxns uint64
	if headerApprovesParent(&block.MsgBlock().Header) {
		numTxns += uint64(len(parent.Transactions()))
	}
	numTxns += uint64(len(block.STransactions()))

	return numTxns
}

// reorganizeChain reorganizes the block chain by disconnecting the nodes in the
// detachNodes list and connecting the nodes in the attach list.  It expects
// that the lists are already in the correct order and are in sync with the
// end of the current best chain.  Specifically, nodes that are being
// disconnected must be in reverse order (think of popping them off the end of
// the chain) and nodes the are being attached must be in forwards order
// (think pushing them onto the end of the chain).
//
// This function may modify the validation state of nodes in the block index
// without flushing in the case the chain is not able to reorganize due to a
// block failing to connect.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) reorganizeChain(detachNodes, attachNodes *list.List) error {
	// Nothing to do if no reorganize nodes were provided.
	if detachNodes.Len() == 0 && attachNodes.Len() == 0 {
		return nil
	}

	// Ensure the provided nodes match the current best chain.
	tip := b.bestChain.Tip()
	if detachNodes.Len() != 0 {
		firstDetachNode := detachNodes.Front().Value.(*blockNode)
		if firstDetachNode.hash != tip.hash {
			panicf("reorganize nodes to detach are not for the current best "+
				"chain -- first detach node %v, current chain %v",
				&firstDetachNode.hash, &tip.hash)
		}
	}

	// Ensure the provided nodes are for the same fork point.
	if attachNodes.Len() != 0 && detachNodes.Len() != 0 {
		firstAttachNode := attachNodes.Front().Value.(*blockNode)
		lastDetachNode := detachNodes.Back().Value.(*blockNode)
		if firstAttachNode.parent.hash != lastDetachNode.parent.hash {
			panicf("reorganize nodes do not have the same fork point -- first "+
				"attach parent %v, last detach parent %v",
				&firstAttachNode.parent.hash, &lastDetachNode.parent.hash)
		}
	}

	// Track the old and new best chains heads.
	oldBest := tip
	newBest := tip

	// All of the blocks to detach and related spend journal entries needed
	// to unspend transaction outputs in the blocks being disconnected must
	// be loaded from the database during the reorg check phase below and
	// then they are needed again when doing the actual database updates.
	// Rather than doing two loads, cache the loaded data into these slices.
	detachBlocks := make([]*dcrutil.Block, 0, detachNodes.Len())
	detachSpentTxOuts := make([][]spentTxOut, 0, detachNodes.Len())
	attachBlocks := make([]*dcrutil.Block, 0, attachNodes.Len())

	// Disconnect all of the blocks back to the point of the fork.  This
	// entails loading the blocks and their associated spent txos from the
	// database and using that information to unspend all of the spent txos
	// and remove the utxos created by the blocks.
	view := NewUtxoViewpoint()
	view.SetBestHash(&oldBest.hash)
	view.SetStakeViewpoint(ViewpointPrevValidInitial)
	var nextBlockToDetach *dcrutil.Block
	for e := detachNodes.Front(); e != nil; e = e.Next() {
		// Grab the block to detach based on the node.  Use the fact that the
		// blocks are being detached in reverse order, so the parent of the
		// current block being detached is the next one being detached.
		n := e.Value.(*blockNode)
		block := nextBlockToDetach
		if block == nil {
			var err error
			block, err = b.fetchMainChainBlockByNode(n)
			if err != nil {
				return err
			}
		}
		if n.hash != *block.Hash() {
			panicf("detach block node hash %v (height %v) does not match "+
				"previous parent block hash %v", &n.hash, n.height,
				block.Hash())
		}

		// Grab the parent of the current block and also save a reference to it
		// as the next block to detach so it doesn't need to be loaded again on
		// the next iteration.
		parent, err := b.fetchMainChainBlockByNode(n.parent)
		if err != nil {
			return err
		}
		nextBlockToDetach = parent

		// Load all of the spent txos for the block from the spend
		// journal.
		var stxos []spentTxOut
		err = b.db.View(func(dbTx database.Tx) error {
			stxos, err = dbFetchSpendJournalEntry(dbTx, block, parent)
			return err
		})
		if err != nil {
			return err
		}

		// Quick sanity test.
		if len(stxos) != countSpentOutputs(block, parent) {
			panicf("retrieved %v stxos when trying to disconnect block %v "+
				"(height %v), yet counted %v many spent utxos", len(stxos),
				block.Hash(), block.Height(), countSpentOutputs(block, parent))
		}

		// Store the loaded block and spend journal entry for later.
		detachBlocks = append(detachBlocks, block)
		detachSpentTxOuts = append(detachSpentTxOuts, stxos)

		err = b.disconnectTransactions(view, block, parent, stxos)
		if err != nil {
			return err
		}

		newBest = n.parent
	}

	// Set the fork point and grab the fork block when there are nodes to be
	// attached.  The fork block is used as the parent to the first node to be
	// attached below.
	var forkNode *blockNode
	var forkBlock *dcrutil.Block
	if attachNodes.Len() > 0 {
		forkNode = newBest

		var err error
		forkBlock, err = b.fetchMainChainBlockByNode(forkNode)
		if err != nil {
			return err
		}
	}

	// Perform several checks to verify each block that needs to be attached
	// to the main chain can be connected without violating any rules and
	// without actually connecting the block.
	//
	// NOTE: These checks could be done directly when connecting a block,
	// however the downside to that approach is that if any of these checks
	// fail after disconnecting some blocks or attaching others, all of the
	// operations have to be rolled back to get the chain back into the
	// state it was before the rule violation (or other failure).  There are
	// at least a couple of ways accomplish that rollback, but both involve
	// tweaking the chain and/or database.  This approach catches these
	// issues before ever modifying the chain.
	for i, e := 0, attachNodes.Front(); e != nil; i, e = i+1, e.Next() {
		// Grab the block to attach based on the node.  Use the fact that the
		// parent of the block is either the fork point for the first node being
		// attached or the previous one that was attached for subsequent blocks
		// to optimize.
		n := e.Value.(*blockNode)
		block, err := b.fetchBlockByNode(n)
		if err != nil {
			return err
		}
		parent := forkBlock
		if i > 0 {
			parent = attachBlocks[i-1]
		}
		if n.parent.hash != *parent.Hash() {
			panicf("attach block node hash %v (height %v) parent hash %v does "+
				"not match previous parent block hash %v", &n.hash, n.height,
				&n.parent.hash, parent.Hash())
		}

		// Store the loaded block for later.
		attachBlocks = append(attachBlocks, block)

		// Skip validation if the block is already known to be valid.
		// However, the UTXO view still needs to be updated.
		if b.index.NodeStatus(n).KnownValid() {
			err = b.connectTransactions(view, block, parent, nil)
			if err != nil {
				return err
			}

			newBest = n
			continue
		}

		// Notice the spent txout details are not requested here and
		// thus will not be generated.  This is done because the state
		// is not being immediately written to the database, so it is
		// not needed.
		//
		// In the case the block is determined to be invalid due to a
		// rule violation, mark it as invalid and mark all of its
		// descendants as having an invalid ancestor.
		err = b.checkConnectBlock(n, block, parent, view, nil)
		if err != nil {
			if _, ok := err.(RuleError); ok {
				b.index.SetStatusFlags(n, statusValidateFailed)
				for de := e.Next(); de != nil; de = de.Next() {
					dn := de.Value.(*blockNode)
					b.index.SetStatusFlags(dn, statusInvalidAncestor)
				}
			}
			return err
		}
		b.index.SetStatusFlags(n, statusValid)

		newBest = n
	}
	log.Debugf("New best chain validation completed successfully, " +
		"commencing with the reorganization.")

	// Send a notification that a blockchain reorganization is in progress.
	reorgData := &ReorganizationNtfnsData{
		oldBest.hash,
		oldBest.height,
		newBest.hash,
		newBest.height,
	}
	b.chainLock.Unlock()
	b.sendNotification(NTReorganization, reorgData)
	b.chainLock.Lock()

	// Send a notification announcing the start of the chain reorganization.
	b.chainLock.Unlock()
	b.sendNotification(NTChainReorgStarted, nil)
	b.chainLock.Lock()

	defer func() {
		// Send a notification announcing the end of the chain reorganization.
		b.chainLock.Unlock()
		b.sendNotification(NTChainReorgDone, nil)
		b.chainLock.Lock()
	}()

	// Reset the view for the actual connection code below.  This is
	// required because the view was previously modified when checking if
	// the reorg would be successful and the connection code requires the
	// view to be valid from the viewpoint of each block being connected or
	// disconnected.
	view = NewUtxoViewpoint()
	view.SetBestHash(&oldBest.hash)
	view.SetStakeViewpoint(ViewpointPrevValidInitial)

	// Disconnect blocks from the main chain.
	for i, e := 0, detachNodes.Front(); e != nil; i, e = i+1, e.Next() {
		// Since the blocks are being detached in reverse order, the parent of
		// current block being detached is the next one being detached up to
		// the final one at which point it's the block that is already saved
		// from the next block to detach above.
		n := e.Value.(*blockNode)
		block := detachBlocks[i]
		parent := nextBlockToDetach
		if i < len(detachBlocks)-1 {
			parent = detachBlocks[i+1]
		}
		if n.parent.hash != *parent.Hash() {
			panicf("detach block node hash %v (height %v) parent hash %v does "+
				"not match previous parent block hash %v", &n.hash, n.height,
				&n.parent.hash, parent.Hash())
		}

		// Load all of the utxos referenced by the block that aren't
		// already in the view.
		err := view.fetchInputUtxos(b.db, block, parent)
		if err != nil {
			return err
		}

		// Update the view to unspend all of the spent txos and remove
		// the utxos created by the block.
		err = b.disconnectTransactions(view, block, parent,
			detachSpentTxOuts[i])
		if err != nil {
			return err
		}

		// Update the database and chain state.
		err = b.disconnectBlock(n, block, parent, view)
		if err != nil {
			return err
		}
	}

	// Connect the new best chain blocks.
	for i, e := 0, attachNodes.Front(); e != nil; i, e = i+1, e.Next() {
		// Grab the block to attach based on the node.  Use the fact that the
		// parent of the block is either the fork point for the first node being
		// attached or the previous one that was attached for subsequent blocks
		// to optimize.
		n := e.Value.(*blockNode)
		block := attachBlocks[i]
		parent := forkBlock
		if i > 0 {
			parent = attachBlocks[i-1]
		}
		if n.parent.hash != *parent.Hash() {
			panicf("attach block node hash %v (height %v) parent hash %v does "+
				"not match previous parent block hash %v", &n.hash, n.height,
				&n.parent.hash, parent.Hash())
		}

		// Update the view to mark all utxos referenced by the block
		// as spent and add all transactions being created by this block
		// to it.  Also, provide an stxo slice so the spent txout
		// details are generated.
		stxos := make([]spentTxOut, 0, countSpentOutputs(block, parent))
		err := b.connectTransactions(view, block, parent, &stxos)
		if err != nil {
			return err
		}

		// Update the database and chain state.
		err = b.connectBlock(n, block, parent, view, stxos)
		if err != nil {
			return err
		}
	}

	// Log the point where the chain forked and old and new best chain
	// heads.
	if forkNode != nil {
		log.Infof("REORGANIZE: Chain forks at %v (height %v)",
			forkNode.hash, forkNode.height)
	}
	log.Infof("REORGANIZE: Old best chain head was %v (height %v)",
		&oldBest.hash, oldBest.height)
	log.Infof("REORGANIZE: New best chain head is %v (height %v)",
		newBest.hash, newBest.height)

	return nil
}

// forceReorganizationToBlock forces a reorganization of the block chain to the
// block hash requested, so long as it matches up with the current organization
// of the best chain.
//
// This function may modify the validation state of nodes in the block index
// without flushing.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) forceHeadReorganization(formerBest chainhash.Hash, newBest chainhash.Hash) error {
	if formerBest.IsEqual(&newBest) {
		return fmt.Errorf("can't reorganize to the same block")
	}
	formerBestNode := b.bestChain.Tip()

	// We can't reorganize the chain unless our head block matches up with
	// b.bestChain.
	if !formerBestNode.hash.IsEqual(&formerBest) {
		return ruleError(ErrForceReorgWrongChain, "tried to force reorg "+
			"on wrong chain")
	}

	// Child to reorganize to is missing.
	newBestNode := b.index.LookupNode(&newBest)
	if newBestNode == nil || newBestNode.parent != formerBestNode.parent {
		return ruleError(ErrForceReorgMissingChild, "missing child of "+
			"common parent for forced reorg")
	}

	// Don't allow a reorganize to a known invalid chain.
	newBestNodeStatus := b.index.NodeStatus(newBestNode)
	if newBestNodeStatus.KnownInvalid() {
		return ruleError(ErrKnownInvalidBlock, "block is known to be invalid")
	}

	// Only validate the block if it is not already known valid.
	if !newBestNodeStatus.KnownValid() {
		newBestBlock, err := b.fetchBlockByNode(newBestNode)
		if err != nil {
			return err
		}

		// Check to make sure our forced-in node validates correctly.
		view := NewUtxoViewpoint()
		view.SetBestHash(&formerBestNode.parent.hash)
		view.SetStakeViewpoint(ViewpointPrevValidInitial)

		formerBestBlock, err := b.fetchBlockByNode(formerBestNode)
		if err != nil {
			return err
		}
		commonParentBlock, err := b.fetchMainChainBlockByNode(formerBestNode.parent)
		if err != nil {
			return err
		}
		var stxos []spentTxOut
		err = b.db.View(func(dbTx database.Tx) error {
			stxos, err = dbFetchSpendJournalEntry(dbTx, formerBestBlock,
				commonParentBlock)
			return err
		})
		if err != nil {
			return err
		}

		// Quick sanity test.
		if len(stxos) != countSpentOutputs(formerBestBlock, commonParentBlock) {
			panicf("retrieved %v stxos when trying to disconnect block %v "+
				"(height %v), yet counted %v many spent utxos when trying to "+
				"force head reorg", len(stxos), formerBestBlock.Hash(),
				formerBestBlock.Height(),
				countSpentOutputs(formerBestBlock, commonParentBlock))
		}

		err = b.disconnectTransactions(view, formerBestBlock, commonParentBlock,
			stxos)
		if err != nil {
			return err
		}

		err = checkBlockSanity(newBestBlock, b.timeSource, BFNone, b.chainParams)
		if err != nil {
			return err
		}

		err = b.checkBlockContext(newBestBlock, newBestNode.parent, BFNone)
		if err != nil {
			return err
		}

		err = b.checkConnectBlock(newBestNode, newBestBlock, commonParentBlock,
			view, nil)
		if err != nil {
			if _, ok := err.(RuleError); ok {
				b.index.SetStatusFlags(newBestNode, statusValidateFailed)
			}
			return err
		}
		b.index.SetStatusFlags(newBestNode, statusValid)
	}

	// Reorganize the chain and flush any potential unsaved changes to the
	// block index to the database.  It is safe to ignore any flushing
	// errors here as the only time the index will be modified is if the
	// block failed to connect.
	attach, detach := b.getReorganizeNodes(newBestNode)
	err := b.reorganizeChain(attach, detach)
	b.flushBlockIndexWarnOnly()
	return err
}

// ForceHeadReorganization is the exported version of forceHeadReorganization.
func (b *BlockChain) ForceHeadReorganization(formerBest chainhash.Hash, newBest chainhash.Hash) error {
	b.chainLock.Lock()
	err := b.forceHeadReorganization(formerBest, newBest)
	b.chainLock.Unlock()
	return err
}

// flushBlockIndex populates any ticket data that has been pruned from modified
// block nodes, writes those nodes to the database and clears the set of
// modified nodes if it succeeds.
func (b *BlockChain) flushBlockIndex() error {
	b.index.RLock()
	for node := range b.index.modified {
		if err := b.maybeFetchTicketInfo(node); err != nil {
			b.index.RUnlock()
			return err
		}
	}
	b.index.RUnlock()

	return b.index.flush()
}

// flushBlockIndexWarnOnly attempts to flush and modified block index nodes to
// the database and will log a warning if it fails.
//
// NOTE: This MUST only be used in the specific circumstances where failure to
// flush only results in a worst case scenario of requiring one or more blocks
// to be validated again.  All other cases must directly call the function on
// the block index and check the error return accordingly.
func (b *BlockChain) flushBlockIndexWarnOnly() {
	if err := b.flushBlockIndex(); err != nil {
		log.Warnf("Unable to flush block index changes to db: %v", err)
	}
}

// connectBestChain handles connecting the passed block to the chain while
// respecting proper chain selection according to the chain with the most
// proof of work.  In the typical case, the new block simply extends the main
// chain.  However, it may also be extending (or creating) a side chain (fork)
// which may or may not end up becoming the main chain depending on which fork
// cumulatively has the most proof of work.  It returns the resulting fork
// length, that is to say the number of blocks to the fork point from the main
// chain, which will be zero if the block ends up on the main chain (either
// due to extending the main chain or causing a reorganization to become the
// main chain).
//
// The flags modify the behavior of this function as follows:
//  - BFFastAdd: Avoids several expensive transaction validation operations.
//    This is useful when using checkpoints.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) connectBestChain(node *blockNode, block, parent *dcrutil.Block, flags BehaviorFlags) (int64, error) {
	fastAdd := flags&BFFastAdd == BFFastAdd

	// Ensure the passed parent is actually the parent of the block.
	if *parent.Hash() != node.parent.hash {
		panicf("parent block %v (height %v) does not match expected parent %v "+
			"(height %v)", parent.Hash(), parent.MsgBlock().Header.Height,
			node.parent.hash, node.height-1)
	}

	// We are extending the main (best) chain with a new block.  This is the
	// most common case.
	parentHash := &block.MsgBlock().Header.PrevBlock
	tip := b.bestChain.Tip()
	if *parentHash == tip.hash {
		// Skip expensive checks if the block has already been fully
		// validated.
		isKnownValid := b.index.NodeStatus(node).KnownValid()
		fastAdd = fastAdd || isKnownValid

		// Perform several checks to verify the block can be connected
		// to the main chain without violating any rules and without
		// actually connecting the block.
		//
		// Also, set the applicable status result in the block index,
		// and flush the status changes to the database.  It is safe to
		// ignore any errors when flushing here as the changes will be
		// flushed when a valid block is connected, and the worst case
		// scenario if a block a invalid is it would need to be
		// revalidated after a restart.
		view := NewUtxoViewpoint()
		view.SetBestHash(parentHash)
		view.SetStakeViewpoint(ViewpointPrevValidInitial)
		var stxos []spentTxOut
		if !fastAdd {
			err := b.checkConnectBlock(node, block, parent, view,
				&stxos)
			if err != nil {
				if _, ok := err.(RuleError); ok {
					b.index.SetStatusFlags(node, statusValidateFailed)
					b.flushBlockIndexWarnOnly()
				}
				return 0, err
			}
		}
		if !isKnownValid {
			b.index.SetStatusFlags(node, statusValid)
			b.flushBlockIndexWarnOnly()
		}

		// In the fast add case the code to check the block connection
		// was skipped, so the utxo view needs to load the referenced
		// utxos, spend them, and add the new utxos being created by
		// this block.
		if fastAdd {
			err := view.fetchInputUtxos(b.db, block, parent)
			if err != nil {
				return 0, err
			}
			err = b.connectTransactions(view, block, parent, &stxos)
			if err != nil {
				return 0, err
			}
		}

		// Connect the block to the main chain.
		err := b.connectBlock(node, block, parent, view, stxos)
		if err != nil {
			return 0, err
		}

		validateStr := "validating"
		if !voteBitsApproveParent(node.voteBits) {
			validateStr = "invalidating"
		}

		log.Debugf("Block %v (height %v) connected to the main chain, "+
			"%v the previous block", node.hash, node.height,
			validateStr)

		// The fork length is zero since the block is now the tip of the
		// best chain.
		return 0, nil
	}
	if fastAdd {
		log.Warnf("fastAdd set in the side chain case? %v\n",
			block.Hash())
	}

	// We're extending (or creating) a side chain, but the cumulative
	// work for this new side chain is not enough to make it the new chain.
	if node.workSum.Cmp(tip.workSum) <= 0 {
		// Log information about how the block is forking the chain.
		fork := b.bestChain.FindFork(node)
		if fork.hash == *parentHash {
			log.Infof("FORK: Block %v (height %v) forks the chain at height "+
				"%d/block %v, but does not cause a reorganize",
				node.hash, node.height, fork.height, fork.hash)
		} else {
			log.Infof("EXTEND FORK: Block %v (height %v) extends a side chain "+
				"which forks the chain at height %d/block %v", node.hash,
				node.height, fork.height, fork.hash)
		}

		forkLen := node.height - fork.height
		return forkLen, nil
	}

	// We're extending (or creating) a side chain and the cumulative work
	// for this new side chain is more than the old best chain, so this side
	// chain needs to become the main chain.  In order to accomplish that,
	// find the common ancestor of both sides of the fork, disconnect the
	// blocks that form the (now) old fork from the main chain, and attach
	// the blocks that form the new chain to the main chain starting at the
	// common ancenstor (the point where the chain forked).
	detachNodes, attachNodes := b.getReorganizeNodes(node)

	// Reorganize the chain and flush any potential unsaved changes to the
	// block index to the database.  It is safe to ignore any flushing
	// errors here as the only time the index will be modified is if the
	// block failed to connect.
	log.Infof("REORGANIZE: Block %v is causing a reorganize.", node.hash)
	err := b.reorganizeChain(detachNodes, attachNodes)
	b.flushBlockIndexWarnOnly()
	if err != nil {
		return 0, err
	}

	// The fork length is zero since the block is now the tip of the best
	// chain.
	return 0, nil
}

// isCurrent returns whether or not the chain believes it is current.  Several
// factors are used to guess, but the key factors that allow the chain to
// believe it is current are:
//  - Latest block height is after the latest checkpoint (if enabled)
//  - Latest block has a timestamp newer than 24 hours ago
//
// This function MUST be called with the chain state lock held (for reads).
func (b *BlockChain) isCurrent() bool {
	// Not current if the latest main (best) chain height is before the
	// latest known good checkpoint (when checkpoints are enabled).
	tip := b.bestChain.Tip()
	checkpoint := b.latestCheckpoint()
	if checkpoint != nil && tip.height < checkpoint.Height {
		return false
	}

	// Not current if the latest best block has a timestamp before 24 hours
	// ago.
	//
	// The chain appears to be current if none of the checks reported
	// otherwise.
	minus24Hours := b.timeSource.AdjustedTime().Add(-24 * time.Hour).Unix()
	return tip.timestamp >= minus24Hours
}

// IsCurrent returns whether or not the chain believes it is current.  Several
// factors are used to guess, but the key factors that allow the chain to
// believe it is current are:
//  - Latest block height is after the latest checkpoint (if enabled)
//  - Latest block has a timestamp newer than 24 hours ago
//
// This function is safe for concurrent access.
func (b *BlockChain) IsCurrent() bool {
	b.chainLock.RLock()
	defer b.chainLock.RUnlock()

	return b.isCurrent()
}

// BestSnapshot returns information about the current best chain block and
// related state as of the current point in time.  The returned instance must be
// treated as immutable since it is shared by all callers.
//
// This function is safe for concurrent access.
func (b *BlockChain) BestSnapshot() *BestState {
	b.stateLock.RLock()
	snapshot := b.stateSnapshot
	b.stateLock.RUnlock()
	return snapshot
}

// MaximumBlockSize returns the maximum permitted block size for the block
// AFTER the given node.
//
// This function MUST be called with the chain state lock held (for reads).
func (b *BlockChain) maxBlockSize(prevNode *blockNode) (int64, error) {
	// Hard fork voting on block size is only enabled on testnet v1 (removed
	// from code) and regnet.
	if b.chainParams.Net != wire.RegNet {
		return int64(b.chainParams.MaximumBlockSizes[0]), nil
	}

	// Return the larger block size if the version 4 stake vote for the max
	// block size increase agenda is active.
	//
	// NOTE: The choice field of the return threshold state is not examined
	// here because there is only one possible choice that can be active
	// for the agenda, which is yes, so there is no need to check it.
	maxSize := int64(b.chainParams.MaximumBlockSizes[0])
	state, err := b.deploymentState(prevNode, 4, chaincfg.VoteIDMaxBlockSize)
	if err != nil {
		return maxSize, err
	}
	if state.State == ThresholdActive {
		return int64(b.chainParams.MaximumBlockSizes[1]), nil
	}

	// The max block size is not changed in any other cases.
	return maxSize, nil
}

// MaxBlockSize returns the maximum permitted block size for the block AFTER
// the end of the current best chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) MaxBlockSize() (int64, error) {
	b.chainLock.Lock()
	maxSize, err := b.maxBlockSize(b.bestChain.Tip())
	b.chainLock.Unlock()
	return maxSize, err
}

// HeaderByHash returns the block header identified by the given hash or an
// error if it doesn't exist.  Note that this will return headers from both the
// main chain and any side chains.
//
// This function is safe for concurrent access.
func (b *BlockChain) HeaderByHash(hash *chainhash.Hash) (wire.BlockHeader, error) {
	node := b.index.LookupNode(hash)
	if node == nil {
		return wire.BlockHeader{}, fmt.Errorf("block %s is not known", hash)
	}

	return node.Header(), nil
}

// HeaderByHeight returns the block header at the given height in the main
// chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) HeaderByHeight(height int64) (wire.BlockHeader, error) {
	node := b.bestChain.NodeByHeight(height)
	if node == nil {
		str := fmt.Sprintf("no block at height %d exists", height)
		return wire.BlockHeader{}, errNotInMainChain(str)
	}

	return node.Header(), nil
}

// BlockByHash searches the internal chain block stores and the database in an
// attempt to find the requested block and returns it.  This function returns
// blocks regardless of whether or not they are part of the main chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) BlockByHash(hash *chainhash.Hash) (*dcrutil.Block, error) {
	node := b.index.LookupNode(hash)
	if node == nil || !b.index.NodeStatus(node).HaveData() {
		return nil, fmt.Errorf("block %s is not known", hash)
	}

	// Return the block from either cache or the database.
	return b.fetchBlockByNode(node)
}

// BlockByHeight returns the block at the given height in the main chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) BlockByHeight(height int64) (*dcrutil.Block, error) {
	// Lookup the block height in the best chain.
	node := b.bestChain.NodeByHeight(height)
	if node == nil {
		str := fmt.Sprintf("no block at height %d exists", height)
		return nil, errNotInMainChain(str)
	}

	// Return the block from either cache or the database.  Note that this is
	// not using fetchMainChainBlockByNode since the main chain check has
	// already been done.
	return b.fetchBlockByNode(node)
}

// MainChainHasBlock returns whether or not the block with the given hash is in
// the main chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) MainChainHasBlock(hash *chainhash.Hash) bool {
	node := b.index.LookupNode(hash)
	return node != nil && b.bestChain.Contains(node)
}

// BlockHeightByHash returns the height of the block with the given hash in the
// main chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) BlockHeightByHash(hash *chainhash.Hash) (int64, error) {
	node := b.index.LookupNode(hash)
	if node == nil || !b.bestChain.Contains(node) {
		str := fmt.Sprintf("block %s is not in the main chain", hash)
		return 0, errNotInMainChain(str)
	}

	return node.height, nil
}

// BlockHashByHeight returns the hash of the block at the given height in the
// main chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) BlockHashByHeight(height int64) (*chainhash.Hash, error) {
	node := b.bestChain.NodeByHeight(height)
	if node == nil {
		str := fmt.Sprintf("no block at height %d exists", height)
		return nil, errNotInMainChain(str)
	}

	return &node.hash, nil
}

// HeightRange returns a range of block hashes for the given start and end
// heights.  It is inclusive of the start height and exclusive of the end
// height.  In other words, it is the half open range [startHeight, endHeight).
//
// The end height will be limited to the current main chain height.
//
// This function is safe for concurrent access.
func (b *BlockChain) HeightRange(startHeight, endHeight int64) ([]chainhash.Hash, error) {
	// Ensure requested heights are sane.
	if startHeight < 0 {
		return nil, fmt.Errorf("start height of fetch range must not "+
			"be less than zero - got %d", startHeight)
	}
	if endHeight < startHeight {
		return nil, fmt.Errorf("end height of fetch range must not "+
			"be less than the start height - got start %d, end %d",
			startHeight, endHeight)
	}

	// There is nothing to do when the start and end heights are the same,
	// so return now to avoid extra work.
	if startHeight == endHeight {
		return nil, nil
	}

	// When the requested start height is after the most recent best chain
	// height, there is nothing to do.
	latestHeight := b.bestChain.Tip().height
	if startHeight > latestHeight {
		return nil, nil
	}

	// Limit the ending height to the latest height of the chain.
	if endHeight > latestHeight+1 {
		endHeight = latestHeight + 1
	}

	// Fetch as many as are available within the specified range.
	hashes := make([]chainhash.Hash, endHeight-startHeight)
	iterNode := b.bestChain.NodeByHeight(endHeight - 1)
	for i := startHeight; i < endHeight; i++ {
		// Since the desired result is from the starting node to the
		// ending node in forward order, but they are iterated in
		// reverse, add them in reverse order.
		hashes[endHeight-i-1] = iterNode.hash
		iterNode = iterNode.parent
	}
	return hashes, nil
}

// locateInventory returns the node of the block after the first known block in
// the locator along with the number of subsequent nodes needed to either reach
// the provided stop hash or the provided max number of entries.
//
// In addition, there are two special cases:
//
// - When no locators are provided, the stop hash is treated as a request for
//   that block, so it will either return the node associated with the stop hash
//   if it is known, or nil if it is unknown
// - When locators are provided, but none of them are known, nodes starting
//   after the genesis block will be returned
//
// This is primarily a helper function for the locateBlocks and locateHeaders
// functions.
//
// This function MUST be called with the chain state lock held (for reads).
func (b *BlockChain) locateInventory(locator BlockLocator, hashStop *chainhash.Hash, maxEntries uint32) (*blockNode, uint32) {
	// There are no block locators so a specific block is being requested
	// as identified by the stop hash.
	stopNode := b.index.LookupNode(hashStop)
	if len(locator) == 0 {
		if stopNode == nil {
			// No blocks with the stop hash were found so there is
			// nothing to do.
			return nil, 0
		}
		return stopNode, 1
	}

	// Find the most recent locator block hash in the main chain.  In the
	// case none of the hashes in the locator are in the main chain, fall
	// back to the genesis block.
	startNode := b.bestChain.Genesis()
	for _, hash := range locator {
		node := b.index.LookupNode(hash)
		if node != nil && b.bestChain.Contains(node) {
			startNode = node
			break
		}
	}

	// Start at the block after the most recently known block.  When there
	// is no next block it means the most recently known block is the tip of
	// the best chain, so there is nothing more to do.
	startNode = b.bestChain.Next(startNode)
	if startNode == nil {
		return nil, 0
	}

	// Calculate how many entries are needed.
	total := uint32((b.bestChain.Tip().height - startNode.height) + 1)
	if stopNode != nil && b.bestChain.Contains(stopNode) &&
		stopNode.height >= startNode.height {

		total = uint32((stopNode.height - startNode.height) + 1)
	}
	if total > maxEntries {
		total = maxEntries
	}

	return startNode, total
}

// locateBlocks returns the hashes of the blocks after the first known block in
// the locator until the provided stop hash is reached, or up to the provided
// max number of block hashes.
//
// See the comment on the exported function for more details on special cases.
//
// This function MUST be called with the chain state lock held (for reads).
func (b *BlockChain) locateBlocks(locator BlockLocator, hashStop *chainhash.Hash, maxHashes uint32) []chainhash.Hash {
	// Find the node after the first known block in the locator and the
	// total number of nodes after it needed while respecting the stop hash
	// and max entries.
	node, total := b.locateInventory(locator, hashStop, maxHashes)
	if total == 0 {
		return nil
	}

	// Populate and return the found hashes.
	hashes := make([]chainhash.Hash, 0, total)
	for i := uint32(0); i < total; i++ {
		hashes = append(hashes, node.hash)
		node = b.bestChain.Next(node)
	}
	return hashes
}

// LocateBlocks returns the hashes of the blocks after the first known block in
// the locator until the provided stop hash is reached, or up to the provided
// max number of block hashes.
//
// In addition, there are two special cases:
//
// - When no locators are provided, the stop hash is treated as a request for
//   that block, so it will either return the stop hash itself if it is known,
//   or nil if it is unknown
// - When locators are provided, but none of them are known, hashes starting
//   after the genesis block will be returned
//
// This function is safe for concurrent access.
func (b *BlockChain) LocateBlocks(locator BlockLocator, hashStop *chainhash.Hash, maxHashes uint32) []chainhash.Hash {
	b.chainLock.RLock()
	hashes := b.locateBlocks(locator, hashStop, maxHashes)
	b.chainLock.RUnlock()
	return hashes
}

// locateHeaders returns the headers of the blocks after the first known block
// in the locator until the provided stop hash is reached, or up to the provided
// max number of block headers.
//
// See the comment on the exported function for more details on special cases.
//
// This function MUST be called with the chain state lock held (for reads).
func (b *BlockChain) locateHeaders(locator BlockLocator, hashStop *chainhash.Hash, maxHeaders uint32) []wire.BlockHeader {
	// Find the node after the first known block in the locator and the
	// total number of nodes after it needed while respecting the stop hash
	// and max entries.
	node, total := b.locateInventory(locator, hashStop, maxHeaders)
	if total == 0 {
		return nil
	}

	// Populate and return the found headers.
	headers := make([]wire.BlockHeader, 0, total)
	for i := uint32(0); i < total; i++ {
		headers = append(headers, node.Header())
		node = b.bestChain.Next(node)
	}
	return headers
}

// LocateHeaders returns the headers of the blocks after the first known block
// in the locator until the provided stop hash is reached, or up to a max of
// wire.MaxBlockHeadersPerMsg headers.
//
// In addition, there are two special cases:
//
// - When no locators are provided, the stop hash is treated as a request for
//   that header, so it will either return the header for the stop hash itself
//   if it is known, or nil if it is unknown
// - When locators are provided, but none of them are known, headers starting
//   after the genesis block will be returned
//
// This function is safe for concurrent access.
func (b *BlockChain) LocateHeaders(locator BlockLocator, hashStop *chainhash.Hash) []wire.BlockHeader {
	b.chainLock.RLock()
	headers := b.locateHeaders(locator, hashStop, wire.MaxBlockHeadersPerMsg)
	b.chainLock.RUnlock()
	return headers
}

// BlockLocatorFromHash returns a block locator for the passed block hash.
// See BlockLocator for details on the algorithm used to create a block locator.
//
// In addition to the general algorithm referenced above, this function will
// return the block locator for the latest known tip of the main (best) chain if
// the passed hash is not currently known.
//
// This function is safe for concurrent access.
func (b *BlockChain) BlockLocatorFromHash(hash *chainhash.Hash) BlockLocator {
	b.chainLock.RLock()
	node := b.index.LookupNode(hash)
	locator := b.bestChain.BlockLocator(node)
	b.chainLock.RUnlock()
	return locator
}

// LatestBlockLocator returns a block locator for the latest known tip of the
// main (best) chain.
//
// This function is safe for concurrent access.
func (b *BlockChain) LatestBlockLocator() (BlockLocator, error) {
	b.chainLock.RLock()
	locator := b.bestChain.BlockLocator(nil)
	b.chainLock.RUnlock()
	return locator, nil
}

// IndexManager provides a generic interface that the is called when blocks are
// connected and disconnected to and from the tip of the main chain for the
// purpose of supporting optional indexes.
type IndexManager interface {
	// Init is invoked during chain initialize in order to allow the index
	// manager to initialize itself and any indexes it is managing.  The
	// channel parameter specifies a channel the caller can close to signal
	// that the process should be interrupted.  It can be nil if that
	// behavior is not desired.
	Init(*BlockChain, <-chan struct{}) error

	// ConnectBlock is invoked when a new block has been connected to the
	// main chain.
	ConnectBlock(database.Tx, *dcrutil.Block, *dcrutil.Block, *UtxoViewpoint) error

	// DisconnectBlock is invoked when a block has been disconnected from
	// the main chain.
	DisconnectBlock(database.Tx, *dcrutil.Block, *dcrutil.Block, *UtxoViewpoint) error
}

// Config is a descriptor which specifies the blockchain instance configuration.
type Config struct {
	// DB defines the database which houses the blocks and will be used to
	// store all metadata created by this package such as the utxo set.
	//
	// This field is required.
	DB database.DB

	// Interrupt specifies a channel the caller can close to signal that
	// long running operations, such as catching up indexes or performing
	// database migrations, should be interrupted.
	//
	// This field can be nil if the caller does not desire the behavior.
	Interrupt <-chan struct{}

	// ChainParams identifies which chain parameters the chain is associated
	// with.
	//
	// This field is required.
	ChainParams *chaincfg.Params

	// TimeSource defines the median time source to use for things such as
	// block processing and determining whether or not the chain is current.
	//
	// The caller is expected to keep a reference to the time source as well
	// and add time samples from other peers on the network so the local
	// time is adjusted to be in agreement with other peers.
	TimeSource MedianTimeSource

	// Notifications defines a callback to which notifications will be sent
	// when various events take place.  See the documentation for
	// Notification and NotificationType for details on the types and
	// contents of notifications.
	//
	// This field can be nil if the caller is not interested in receiving
	// notifications.
	Notifications NotificationCallback

	// SigCache defines a signature cache to use when when validating
	// signatures.  This is typically most useful when individual
	// transactions are already being validated prior to their inclusion in
	// a block such as what is usually done via a transaction memory pool.
	//
	// This field can be nil if the caller is not interested in using a
	// signature cache.
	SigCache *txscript.SigCache

	// IndexManager defines an index manager to use when initializing the
	// chain and connecting and disconnecting blocks.
	//
	// This field can be nil if the caller does not wish to make use of an
	// index manager.
	IndexManager IndexManager
}

// New returns a BlockChain instance using the provided configuration details.
func New(config *Config) (*BlockChain, error) {
	// Enforce required config fields.
	if config.DB == nil {
		return nil, AssertError("blockchain.New database is nil")
	}
	if config.ChainParams == nil {
		return nil, AssertError("blockchain.New chain parameters nil")
	}

	// Generate a checkpoint by height map from the provided checkpoints.
	params := config.ChainParams
	var checkpointsByHeight map[int64]*chaincfg.Checkpoint
	if len(params.Checkpoints) > 0 {
		checkpointsByHeight = make(map[int64]*chaincfg.Checkpoint)
		for i := range params.Checkpoints {
			checkpoint := &params.Checkpoints[i]
			checkpointsByHeight[checkpoint.Height] = checkpoint
		}
	}

	b := BlockChain{
		checkpointsByHeight:           checkpointsByHeight,
		db:                            config.DB,
		chainParams:                   params,
		timeSource:                    config.TimeSource,
		notifications:                 config.Notifications,
		sigCache:                      config.SigCache,
		indexManager:                  config.IndexManager,
		interrupt:                     config.Interrupt,
		index:                         newBlockIndex(config.DB, params),
		bestChain:                     newChainView(nil),
		orphans:                       make(map[chainhash.Hash]*orphanBlock),
		prevOrphans:                   make(map[chainhash.Hash][]*orphanBlock),
		mainchainBlockCache:           make(map[chainhash.Hash]*dcrutil.Block),
		mainchainBlockCacheSize:       mainchainBlockCacheSize,
		deploymentCaches:              newThresholdCaches(params),
		isVoterMajorityVersionCache:   make(map[[stakeMajorityCacheKeySize]byte]bool),
		isStakeMajorityVersionCache:   make(map[[stakeMajorityCacheKeySize]byte]bool),
		calcPriorStakeVersionCache:    make(map[[chainhash.HashSize]byte]uint32),
		calcVoterVersionIntervalCache: make(map[[chainhash.HashSize]byte]uint32),
		calcStakeVersionCache:         make(map[[chainhash.HashSize]byte]uint32),
	}

	// Initialize the chain state from the passed database.  When the db
	// does not yet contain any chain state, both it and the chain state
	// will be initialized to contain only the genesis block.
	if err := b.initChainState(); err != nil {
		return nil, err
	}

	// Initialize and catch up all of the currently active optional indexes
	// as needed.
	if config.IndexManager != nil {
		err := config.IndexManager.Init(&b, config.Interrupt)
		if err != nil {
			return nil, err
		}
	}

	tip := b.bestChain.Tip()
	b.subsidyCache = NewSubsidyCache(tip.height, b.chainParams)
	b.pruner = newChainPruner(&b)

	log.Infof("Blockchain database version info: chain: %d, compression: "+
		"%d, block index: %d", b.dbInfo.version, b.dbInfo.compVer,
		b.dbInfo.bidxVer)

	log.Infof("Chain state: height %d, hash %v, total transactions %d, "+
		"work %v, stake version %v", tip.height, tip.hash,
		b.stateSnapshot.TotalTxns, tip.workSum, 0)

	return &b, nil
}
