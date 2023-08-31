// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/monitor"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/fetcher"
	"github.com/ethereum/go-ethereum/eth/protocols/bsc"
	"github.com/ethereum/go-ethereum/eth/protocols/diff"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/eth/protocols/trust"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// voteChanSize is the size of channel listening to NewVotesEvent.
	voteChanSize = 256

	// deltaTdThreshold is the threshold of TD difference for peers to broadcast votes.
	deltaTdThreshold = 20
)

var (
	syncChallengeTimeout = 15 * time.Second // Time allowance for a node to reply to the sync progress challenge
)

// txPool defines the methods needed from a transaction pool implementation to
// support all the operations needed by the Ethereum chain protocols.
type txPool interface {
	// Has returns an indicator whether txpool has a transaction
	// cached with the given hash.
	Has(hash common.Hash) bool

	// Get retrieves the transaction from local txpool with given
	// tx hash.
	Get(hash common.Hash) *types.Transaction

	// AddRemotes should add the given transactions to the pool.
	AddRemotes([]*types.Transaction) []error

	// Pending should return pending transactions.
	// The slice should be modifiable by the caller.
	Pending(enforceTips bool) map[common.Address]types.Transactions

	// SubscribeNewTxsEvent should return an event subscription of
	// NewTxsEvent and send events to the given channel.
	SubscribeNewTxsEvent(chan<- core.NewTxsEvent) event.Subscription

	// SubscribeReannoTxsEvent should return an event subscription of
	// ReannoTxsEvent and send events to the given channel.
	SubscribeReannoTxsEvent(chan<- core.ReannoTxsEvent) event.Subscription
}

// votePool defines the methods needed from a votes pool implementation to
// support all the operations needed by the Ethereum chain protocols.
type votePool interface {
	PutVote(vote *types.VoteEnvelope)
	GetVotes() []*types.VoteEnvelope

	// SubscribeNewVoteEvent should return an event subscription of
	// NewVotesEvent and send events to the given channel.
	SubscribeNewVoteEvent(ch chan<- core.NewVoteEvent) event.Subscription
}

// handlerConfig is the collection of initialization parameters to create a full
// node network handler.
type handlerConfig struct {
	Database               ethdb.Database   // Database for direct sync insertions
	Chain                  *core.BlockChain // Blockchain to serve data from
	TxPool                 txPool           // Transaction pool to propagate from
	VotePool               votePool
	Merger                 *consensus.Merger         // The manager for eth1/2 transition
	Network                uint64                    // Network identifier to adfvertise
	Sync                   downloader.SyncMode       // Whether to snap or full sync
	DiffSync               bool                      // Whether to diff sync
	BloomCache             uint64                    // Megabytes to alloc for snap sync bloom
	EventMux               *event.TypeMux            // Legacy event mux, deprecate for `feed`
	Checkpoint             *params.TrustedCheckpoint // Hard coded checkpoint for sync challenges
	Whitelist              map[uint64]common.Hash    // Hard coded whitelist for sync challenged
	DirectBroadcast        bool
	DisablePeerTxBroadcast bool
	PeerSet                *peerSet
}

type handler struct {
	networkID              uint64
	forkFilter             forkid.Filter // Fork ID filter, constant across the lifetime of the node
	disablePeerTxBroadcast bool

	snapSync        uint32 // Flag whether snap sync is enabled (gets disabled if we already have blocks)
	acceptTxs       uint32 // Flag whether we're considered synchronised (enables transaction processing)
	directBroadcast bool
	diffSync        bool // Flag whether diff sync should operate on top of the diff protocol

	checkpointNumber uint64      // Block number for the sync progress validator to cross reference
	checkpointHash   common.Hash // Block hash for the sync progress validator to cross reference

	database             ethdb.Database
	txpool               txPool
	votepool             votePool
	maliciousVoteMonitor *monitor.MaliciousVoteMonitor
	chain                *core.BlockChain
	maxPeers             int
	maxPeersPerIP        int
	peersPerIP           map[string]int
	peerPerIPLock        sync.Mutex

	downloader   *downloader.Downloader
	blockFetcher *fetcher.BlockFetcher
	txFetcher    *fetcher.TxFetcher
	peers        *peerSet
	merger       *consensus.Merger

	eventMux       *event.TypeMux
	txsCh          chan core.NewTxsEvent
	txsSub         event.Subscription
	reannoTxsCh    chan core.ReannoTxsEvent
	reannoTxsSub   event.Subscription
	minedBlockSub  *event.TypeMuxSubscription
	voteCh         chan core.NewVoteEvent
	votesSub       event.Subscription
	voteMonitorSub event.Subscription

	whitelist map[uint64]common.Hash

	// channels for fetcher, syncer, txsyncLoop
	quitSync chan struct{}

	chainSync *chainSyncer
	wg        sync.WaitGroup
	peerWG    sync.WaitGroup
}

// 👹👹👹👹👹👹👹👹👹 handler 是以太坊客户端中负责网络和交易处理的模块 👹👹👹👹👹👹👹👹👹

// newHandler returns a handler for all Ethereum chain management protocol.
// // newHandler 返回用于 Ethereum 链管理协议的处理程序。
func newHandler(config *handlerConfig) (*handler, error) {
	// Create the protocol manager with the base fields
	// 🔷 初始化协议管理器的基本字段，如网络 ID、事件多路复用器、数据库等
	if config.EventMux == nil {
		config.EventMux = new(event.TypeMux) // Nicety initialization for tests
	}
	if config.PeerSet == nil {
		config.PeerSet = newPeerSet() // Nicety initialization for tests
	}
	h := &handler{
		networkID:              config.Network,
		forkFilter:             forkid.NewFilter(config.Chain),
		disablePeerTxBroadcast: config.DisablePeerTxBroadcast,
		eventMux:               config.EventMux,
		database:               config.Database,
		txpool:                 config.TxPool,
		votepool:               config.VotePool,
		chain:                  config.Chain,
		peers:                  config.PeerSet,
		merger:                 config.Merger,
		peersPerIP:             make(map[string]int),
		whitelist:              config.Whitelist,
		directBroadcast:        config.DirectBroadcast,
		diffSync:               config.DiffSync,
		quitSync:               make(chan struct{}),
	}
	// 🔷 根据同步模式选择是否切换到快照同步，或者从快照同步切换到完整同步
	if config.Sync == downloader.FullSync {
		// The database seems empty as the current block is the genesis. Yet the snap
		// block is ahead, so snap sync was enabled for this node at a certain point.
		// The scenarios where this can happen is
		// * if the user manually (or via a bad block) rolled back a snap sync node
		//   below the sync point.
		// * the last snap sync is not finished while user specifies a full sync this
		//   time. But we don't have any recent state for full sync.
		// In these cases however it's safe to reenable snap sync.
		// 如果数据库为空且快照块已经生成，切换到快照同步模式
		fullBlock, fastBlock := h.chain.CurrentBlock(), h.chain.CurrentFastBlock()
		if fullBlock.NumberU64() == 0 && fastBlock.NumberU64() > 0 {
			if rawdb.ReadAncientType(h.database) == rawdb.PruneFreezerType {
				log.Crit("Fast Sync not finish, can't enable pruneancient mode")
			}
			h.snapSync = uint32(1)
			log.Warn("Switch sync mode from full sync to snap sync")
		}
	} else {
		// 如果数据库不为空，输出警告日志，表示从快照同步切换到完整同步模式
		if h.chain.CurrentBlock().NumberU64() > 0 {
			// Print warning log if database is not empty to run snap sync.
			log.Warn("Switch sync mode from snap sync to full sync")
		} else {
			// If snap sync was requested and our database is empty, grant it
			// 如果请求了快照同步，并且数据库为空，将快照同步标志设为 1
			h.snapSync = uint32(1)
		}
	}
	// If we have trusted checkpoints, enforce them on the chain
	// 🔷 如果有可信的检查点，将其应用到链上
	if config.Checkpoint != nil {
		h.checkpointNumber = (config.Checkpoint.SectionIndex+1)*params.CHTFrequency - 1
		h.checkpointHash = config.Checkpoint.SectionHead
	}
	// Construct the downloader (long sync) and its backing state bloom if snap
	// sync is requested. The downloader is responsible for deallocating the state
	// bloom when it's done.
	// 🔷 构建下载器（长同步），支持状态布隆过滤器，用于从网络中获取块数据
	var downloadOptions []downloader.DownloadOption
	if h.diffSync {
		downloadOptions = append(downloadOptions, downloader.EnableDiffFetchOp(h.peers))
	}
	h.downloader = downloader.New(h.checkpointNumber, config.Database, h.eventMux, h.chain, nil, h.removePeer, downloadOptions...)

	// Construct the fetcher (short sync)
	// 🔷 构建获取器（短同步），用于验证和插入块，以及从网络中获取交易数据
	validator := func(header *types.Header) error {
		// All the block fetcher activities should be disabled
		// after the transition. Print the warning log.
		// 在过渡期后，所有块获取活动都应该被禁用
		if h.merger.PoSFinalized() {
			log.Warn("Unexpected validation activity", "hash", header.Hash(), "number", header.Number)
			return errors.New("unexpected behavior after transition")
		}
		// Reject all the PoS style headers in the first place. No matter
		// the chain has finished the transition or not, the PoS headers
		// should only come from the trusted consensus layer instead of
		// p2p network.
		// 在首个过渡块后拒绝所有 PoS 风格的区块头。无论是否完成了过渡期，
		// PoS 区块头都应该来自受信任的共识层而不是 P2P 网络。
		if beacon, ok := h.chain.Engine().(*beacon.Beacon); ok {
			if beacon.IsPoSHeader(header) {
				return errors.New("unexpected post-merge header")
			}
		}
		return h.chain.Engine().VerifyHeader(h.chain, header, true)
	}
	heighter := func() uint64 {
		return h.chain.CurrentBlock().NumberU64()
	}
	inserter := func(blocks types.Blocks) (int, error) {
		// All the block fetcher activities should be disabled
		// after the transition. Print the warning log.
		// 在过渡期后，所有块获取活动都应该被禁用
		if h.merger.PoSFinalized() {
			var ctx []interface{}
			ctx = append(ctx, "blocks", len(blocks))
			if len(blocks) > 0 {
				ctx = append(ctx, "firsthash", blocks[0].Hash())
				ctx = append(ctx, "firstnumber", blocks[0].Number())
				ctx = append(ctx, "lasthash", blocks[len(blocks)-1].Hash())
				ctx = append(ctx, "lastnumber", blocks[len(blocks)-1].Number())
			}
			log.Warn("Unexpected insertion activity", ctx...)
			return 0, errors.New("unexpected behavior after transition")
		}
		// If sync hasn't reached the checkpoint yet, deny importing weird blocks.
		//
		// Ideally we would also compare the head block's timestamp and similarly reject
		// the propagated block if the head is too old. Unfortunately there is a corner
		// case when starting new networks, where the genesis might be ancient (0 unix)
		// which would prevent full nodes from accepting it.
		// 如果同步尚未达到检查点，拒绝导入异常的区块
		if h.chain.CurrentBlock().NumberU64() < h.checkpointNumber {
			log.Warn("Unsynced yet, discarded propagated block", "number", blocks[0].Number(), "hash", blocks[0].Hash())
			return 0, nil
		}
		// If snap sync is running, deny importing weird blocks. This is a problematic
		// clause when starting up a new network, because snap-syncing miners might not
		// accept each others' blocks until a restart. Unfortunately we haven't figured
		// out a way yet where nodes can decide unilaterally whether the network is new
		// or not. This should be fixed if we figure out a solution.
		// 如果正在进行快照同步，拒绝导入异常的区块
		if atomic.LoadUint32(&h.snapSync) == 1 {
			log.Warn("Fast syncing, discarded propagated block", "number", blocks[0].Number(), "hash", blocks[0].Hash())
			return 0, nil
		}
		// 如果已经达到 PoW 过渡期，只接受终端 PoS 区块
		if h.merger.TDDReached() {
			// The blocks from the p2p network is regarded as untrusted
			// after the transition. In theory block gossip should be disabled
			// entirely whenever the transition is started. But in order to
			// handle the transition boundary reorg in the consensus-layer,
			// the legacy blocks are still accepted, but only for the terminal
			// pow blocks. Spec: https://github.com/ethereum/EIPs/blob/master/EIPS/eip-3675.md#halt-the-importing-of-pow-blocks
			// P2P 网络中的区块在过渡期后被视为不受信任
			// 在理论上，块广播应该在启动过渡期时完全被禁用
			// 但是为了处理共识层中的过渡边界重组，仍然接受旧的块，但仅针对终端 PoW 块

			for i, block := range blocks {
				ptd := h.chain.GetTd(block.ParentHash(), block.NumberU64()-1)
				if ptd == nil {
					return 0, nil
				}
				td := new(big.Int).Add(ptd, block.Difficulty())
				if !h.chain.Config().IsTerminalPoWBlock(ptd, td) {
					log.Info("Filtered out non-termimal pow block", "number", block.NumberU64(), "hash", block.Hash())
					return 0, nil
				}
				if err := h.chain.InsertBlockWithoutSetHead(block); err != nil {
					return i, err
				}
			}
			return 0, nil
		}
		// 插入区块链中
		n, err := h.chain.InsertChain(blocks)
		if err == nil {
			atomic.StoreUint32(&h.acceptTxs, 1) // Mark initial sync done on any fetcher import 标记初始同步完成
		}
		return n, err
	}
	// 🔷 构建区块获取器
	h.blockFetcher = fetcher.NewBlockFetcher(false, nil, h.chain.GetBlockByHash, validator, h.BroadcastBlock, heighter, nil, inserter, h.removePeer)

	// 区块获取器函数，从远程节点获取交易
	fetchTx := func(peer string, hashes []common.Hash) error {
		p := h.peers.peer(peer)
		if p == nil {
			return errors.New("unknown peer")
		}
		return p.RequestTxs(hashes) // 生成按收到的tx hash向远程节点发送请求tx的请求
	}
	// 🔷 构建交易获取器
	h.txFetcher = fetcher.NewTxFetcher(h.txpool.Has, h.txpool.AddRemotes, fetchTx)
	// 🔷 构建链同步器
	h.chainSync = newChainSyncer(h)
	return h, nil
}

// runEthPeer registers an eth peer into the joint eth/snap peerset, adds it to
// various subsystems and starts handling messages.
func (h *handler) runEthPeer(peer *eth.Peer, handler eth.Handler) error {
	// If the peer has a `snap` extension, wait for it to connect so we can have
	// a uniform initialization/teardown mechanism
	snap, err := h.peers.waitSnapExtension(peer)
	if err != nil {
		peer.Log().Error("Snapshot extension barrier failed", "err", err)
		return err
	}
	diff, err := h.peers.waitDiffExtension(peer)
	if err != nil {
		peer.Log().Error("Diff extension barrier failed", "err", err)
		return err
	}
	trust, err := h.peers.waitTrustExtension(peer)
	if err != nil {
		peer.Log().Error("Trust extension barrier failed", "err", err)
		return err
	}
	bsc, err := h.peers.waitBscExtension(peer)
	if err != nil {
		peer.Log().Error("Bsc extension barrier failed", "err", err)
		return err
	}
	// TODO(karalabe): Not sure why this is needed
	if !h.chainSync.handlePeerEvent(peer) {
		return p2p.DiscQuitting
	}
	h.peerWG.Add(1)
	defer h.peerWG.Done()

	// Execute the Ethereum handshake
	var (
		genesis = h.chain.Genesis()
		head    = h.chain.CurrentHeader()
		hash    = head.Hash()
		number  = head.Number.Uint64()
		td      = h.chain.GetTd(hash, number)
	)
	forkID := forkid.NewID(h.chain.Config(), h.chain.Genesis().Hash(), h.chain.CurrentHeader().Number.Uint64())
	if err := peer.Handshake(h.networkID, td, hash, genesis.Hash(), forkID, h.forkFilter, &eth.UpgradeStatusExtension{DisablePeerTxBroadcast: h.disablePeerTxBroadcast}); err != nil {
		peer.Log().Debug("Ethereum handshake failed", "err", err)
		return err
	}
	reject := false // reserved peer slots
	if atomic.LoadUint32(&h.snapSync) == 1 {
		if snap == nil {
			// If we are running snap-sync, we want to reserve roughly half the peer
			// slots for peers supporting the snap protocol.
			// The logic here is; we only allow up to 5 more non-snap peers than snap-peers.
			if all, snp := h.peers.len(), h.peers.snapLen(); all-snp > snp+5 {
				reject = true
			}
		}
	}
	// Ignore maxPeers if this is a trusted peer
	peerInfo := peer.Peer.Info()
	if !peerInfo.Network.Trusted {
		if reject || h.peers.len() >= h.maxPeers {
			return p2p.DiscTooManyPeers
		}
	}

	remoteAddr := peerInfo.Network.RemoteAddress
	indexIP := strings.LastIndex(remoteAddr, ":")
	if indexIP == -1 {
		// there could be no IP address, such as a pipe
		peer.Log().Debug("runEthPeer", "no ip address, remoteAddress", remoteAddr)
	} else if !peerInfo.Network.Trusted {
		remoteIP := remoteAddr[:indexIP]
		h.peerPerIPLock.Lock()
		if num, ok := h.peersPerIP[remoteIP]; ok && num >= h.maxPeersPerIP {
			h.peerPerIPLock.Unlock()
			peer.Log().Info("The IP has too many peers", "ip", remoteIP, "maxPeersPerIP", h.maxPeersPerIP,
				"name", peerInfo.Name, "Enode", peerInfo.Enode)
			return p2p.DiscTooManyPeers
		}
		h.peersPerIP[remoteIP] = h.peersPerIP[remoteIP] + 1
		h.peerPerIPLock.Unlock()
	}
	peer.Log().Debug("Ethereum peer connected", "name", peer.Name())

	// Register the peer locally
	if err := h.peers.registerPeer(peer, snap, diff, trust, bsc); err != nil {
		peer.Log().Error("Ethereum peer registration failed", "err", err)
		return err
	}
	defer h.unregisterPeer(peer.ID())

	p := h.peers.peer(peer.ID())
	if p == nil {
		return errors.New("peer dropped during handling")
	}
	// Register the peer in the downloader. If the downloader considers it banned, we disconnect
	if err := h.downloader.RegisterPeer(peer.ID(), peer.Version(), peer); err != nil {
		peer.Log().Error("Failed to register peer in eth syncer", "err", err)
		return err
	}
	if snap != nil {
		if err := h.downloader.SnapSyncer.Register(snap); err != nil {
			peer.Log().Error("Failed to register peer in snap syncer", "err", err)
			return err
		}
	}
	h.chainSync.handlePeerEvent(peer)

	// Propagate existing transactions and votes. new transactions and votes appearing
	// after this will be sent via broadcasts.
	h.syncTransactions(peer)
	if h.votepool != nil && p.bscExt != nil {
		h.syncVotes(p.bscExt)
	}

	// Create a notification channel for pending requests if the peer goes down
	dead := make(chan struct{})
	defer close(dead)

	// If we have a trusted CHT, reject all peers below that (avoid fast sync eclipse)
	if h.checkpointHash != (common.Hash{}) {
		// Request the peer's checkpoint header for chain height/weight validation
		resCh := make(chan *eth.Response)

		req, err := peer.RequestHeadersByNumber(h.checkpointNumber, 1, 0, false, resCh)
		if err != nil {
			return err
		}
		// Start a timer to disconnect if the peer doesn't reply in time
		go func() {
			// Ensure the request gets cancelled in case of error/drop
			defer req.Close()

			timeout := time.NewTimer(syncChallengeTimeout)
			defer timeout.Stop()

			select {
			case res := <-resCh:
				headers := ([]*types.Header)(*res.Res.(*eth.BlockHeadersPacket))
				if len(headers) == 0 {
					// If we're doing a snap sync, we must enforce the checkpoint
					// block to avoid eclipse attacks. Unsynced nodes are welcome
					// to connect after we're done joining the network.
					if atomic.LoadUint32(&h.snapSync) == 1 {
						peer.Log().Warn("Dropping unsynced node during sync", "addr", peer.RemoteAddr(), "type", peer.Name())
						res.Done <- errors.New("unsynced node cannot serve sync")
						return
					}
					res.Done <- nil
					return
				}
				// Validate the header and either drop the peer or continue
				if len(headers) > 1 {
					res.Done <- errors.New("too many headers in checkpoint response")
					return
				}
				if headers[0].Hash() != h.checkpointHash {
					res.Done <- errors.New("checkpoint hash mismatch")
					return
				}
				res.Done <- nil

			case <-timeout.C:
				peer.Log().Warn("Checkpoint challenge timed out, dropping", "addr", peer.RemoteAddr(), "type", peer.Name())
				h.removePeer(peer.ID())

			case <-dead:
				// Peer handler terminated, abort all goroutines
			}
		}()
	}
	// If we have any explicit whitelist block hashes, request them
	for number, hash := range h.whitelist {
		resCh := make(chan *eth.Response)

		req, err := peer.RequestHeadersByNumber(number, 1, 0, false, resCh)
		if err != nil {
			return err
		}
		go func(number uint64, hash common.Hash, req *eth.Request) {
			// Ensure the request gets cancelled in case of error/drop
			defer req.Close()

			timeout := time.NewTimer(syncChallengeTimeout)
			defer timeout.Stop()

			select {
			case res := <-resCh:
				headers := ([]*types.Header)(*res.Res.(*eth.BlockHeadersPacket))
				if len(headers) == 0 {
					// Whitelisted blocks are allowed to be missing if the remote
					// node is not yet synced
					res.Done <- nil
					return
				}
				// Validate the header and either drop the peer or continue
				if len(headers) > 1 {
					res.Done <- errors.New("too many headers in whitelist response")
					return
				}
				if headers[0].Number.Uint64() != number || headers[0].Hash() != hash {
					peer.Log().Info("Whitelist mismatch, dropping peer", "number", number, "hash", headers[0].Hash(), "want", hash)
					res.Done <- errors.New("whitelist block mismatch")
					return
				}
				peer.Log().Debug("Whitelist block verified", "number", number, "hash", hash)
				res.Done <- nil
			case <-timeout.C:
				peer.Log().Warn("Whitelist challenge timed out, dropping", "addr", peer.RemoteAddr(), "type", peer.Name())
				h.removePeer(peer.ID())
			}
		}(number, hash, req)
	}
	// Handle incoming messages until the connection is torn down
	return handler(peer)
}

// runSnapExtension registers a `snap` peer into the joint eth/snap peerset and
// starts handling inbound messages. As `snap` is only a satellite protocol to
// `eth`, all subsystem registrations and lifecycle management will be done by
// the main `eth` handler to prevent strange races.
func (h *handler) runSnapExtension(peer *snap.Peer, handler snap.Handler) error {
	h.peerWG.Add(1)
	defer h.peerWG.Done()

	if err := h.peers.registerSnapExtension(peer); err != nil {
		peer.Log().Warn("Snapshot extension registration failed", "err", err)
		return err
	}
	return handler(peer)
}

// runDiffExtension registers a `diff` peer into the joint eth/diff peerset and
// starts handling inbound messages. As `diff` is only a satellite protocol to
// `eth`, all subsystem registrations and lifecycle management will be done by
// the main `eth` handler to prevent strange races.
func (h *handler) runDiffExtension(peer *diff.Peer, handler diff.Handler) error {
	h.peerWG.Add(1)
	defer h.peerWG.Done()

	if err := h.peers.registerDiffExtension(peer); err != nil {
		peer.Log().Error("Diff extension registration failed", "err", err)
		peer.Close()
		return err
	}
	return handler(peer)
}

// runTrustExtension registers a `trust` peer into the joint eth/trust peerset and
// starts handling inbound messages. As `trust` is only a satellite protocol to
// `eth`, all subsystem registrations and lifecycle management will be done by
// the main `eth` handler to prevent strange races.
func (h *handler) runTrustExtension(peer *trust.Peer, handler trust.Handler) error {
	h.peerWG.Add(1)
	defer h.peerWG.Done()

	if err := h.peers.registerTrustExtension(peer); err != nil {
		peer.Log().Error("Trust extension registration failed", "err", err)
		return err
	}
	return handler(peer)
}

// runBscExtension registers a `bsc` peer into the joint eth/bsc peerset and
// starts handling inbound messages. As `bsc` is only a satellite protocol to
// `eth`, all subsystem registrations and lifecycle management will be done by
// the main `eth` handler to prevent strange races.
func (h *handler) runBscExtension(peer *bsc.Peer, handler bsc.Handler) error {
	h.peerWG.Add(1)
	defer h.peerWG.Done()

	if err := h.peers.registerBscExtension(peer); err != nil {
		peer.Log().Error("Bsc extension registration failed", "err", err)
		return err
	}
	return handler(peer)
}

// removePeer requests disconnection of a peer.
func (h *handler) removePeer(id string) {
	peer := h.peers.peer(id)
	if peer != nil {
		// Hard disconnect at the networking layer. Handler will get an EOF and terminate the peer. defer unregisterPeer will do the cleanup task after then.
		peer.Peer.Disconnect(p2p.DiscUselessPeer)
	}
}

// unregisterPeer removes a peer from the downloader, fetchers and main peer set.
func (h *handler) unregisterPeer(id string) {
	// Create a custom logger to avoid printing the entire id
	var logger log.Logger
	if len(id) < 16 {
		// Tests use short IDs, don't choke on them
		logger = log.New("peer", id)
	} else {
		logger = log.New("peer", id[:8])
	}
	// Abort if the peer does not exist
	peer := h.peers.peer(id)
	if peer == nil {
		logger.Error("Ethereum peer removal failed", "err", errPeerNotRegistered)
		return
	}
	// Remove the `eth` peer if it exists
	logger.Debug("Removing Ethereum peer", "snap", peer.snapExt != nil)

	// Remove the `snap` extension if it exists
	if peer.snapExt != nil {
		h.downloader.SnapSyncer.Unregister(id)
	}
	h.downloader.UnregisterPeer(id)
	h.txFetcher.Drop(id)

	if err := h.peers.unregisterPeer(id); err != nil {
		logger.Error("Ethereum peer removal failed", "err", err)
	}

	peerInfo := peer.Peer.Info()
	remoteAddr := peerInfo.Network.RemoteAddress
	indexIP := strings.LastIndex(remoteAddr, ":")
	if indexIP == -1 {
		// there could be no IP address, such as a pipe
		peer.Log().Debug("unregisterPeer", "name", peerInfo.Name, "no ip address, remoteAddress", remoteAddr)
	} else if !peerInfo.Network.Trusted {
		remoteIP := remoteAddr[:indexIP]
		h.peerPerIPLock.Lock()
		if h.peersPerIP[remoteIP] <= 0 {
			peer.Log().Error("unregisterPeer without record", "name", peerInfo.Name, "remoteAddress", remoteAddr)
		} else {
			h.peersPerIP[remoteIP] = h.peersPerIP[remoteIP] - 1
			logger.Debug("unregisterPeer", "name", peerInfo.Name, "connectNum", h.peersPerIP[remoteIP])
			if h.peersPerIP[remoteIP] == 0 {
				delete(h.peersPerIP, remoteIP)
			}
		}
		h.peerPerIPLock.Unlock()
	}
}

// handler 是以太坊客户端中负责网络和交易处理的模块。该方法启动了多个不同的 goroutine 来处理交易和区块的广播、同步等操作
// 并发地处理交易的广播、投票的广播、区块的广播以及区块链的同步，以确保以太坊客户端能够与网络中的其他节点进行有效的交流和数据同步。这有助于保持节点在网络中的活跃性，并使其能够有效地参与共识过程。
func (h *handler) Start(maxPeers int, maxPeersPerIP int) {
	h.maxPeers = maxPeers
	h.maxPeersPerIP = maxPeersPerIP
	// broadcast transactions
	h.wg.Add(1)

	// 外部只需要订阅 SubscribeNewTxsEvent 新可执行交易事件，则可实时接受交易。在 geth 中网络层将订阅交易事件，以便实时广播。
	h.txsCh = make(chan core.NewTxsEvent, txChanSize)
	h.txsSub = h.txpool.SubscribeNewTxsEvent(h.txsCh)

	// 🔷 广播交易循环: 用于从交易池中获取新的可执行交易事件，并将它们广播到网络中，以便其他节点可以得到这些新交易。
	go h.txBroadcastLoop()

	// broadcast votes
	// 🔷 广播投票循环: 如果 votepool 不为 nil，表示该客户端支持投票共识
	if h.votepool != nil {
		h.wg.Add(1)
		h.voteCh = make(chan core.NewVoteEvent, voteChanSize)
		h.votesSub = h.votepool.SubscribeNewVoteEvent(h.voteCh)
		go h.voteBroadcastLoop() // 用于从投票池中获取新的投票事件，并将它们广播到网络中

		if h.maliciousVoteMonitor != nil {
			h.wg.Add(1)
			go h.startMaliciousVoteMonitor() // 用于启恶意投票监视
		}
	}

	// announce local pending transactions again
	// 🔷 重新公告本地挂起交易循环: 用于监听挂起交易的重新公告事件，并将这些交易再次广播到网络中。
	h.wg.Add(1)
	h.reannoTxsCh = make(chan core.ReannoTxsEvent, txChanSize)
	h.reannoTxsSub = h.txpool.SubscribeReannoTxsEvent(h.reannoTxsCh)
	go h.txReannounceLoop()

	// broadcast mined blocks
	// 🔷 广播挖矿块循环: 用于监听新挖出的区块事件，并将这些区块广播到网络中。
	h.wg.Add(1)
	h.minedBlockSub = h.eventMux.Subscribe(core.NewMinedBlockEvent{})
	go h.minedBroadcastLoop()

	// start sync handlers
	// 🔷 同步处理循环: 用于处理区块链的同步操作
	h.wg.Add(1)
	go h.chainSync.loop()
}

func (h *handler) startMaliciousVoteMonitor() {
	defer h.wg.Done()
	voteCh := make(chan core.NewVoteEvent, voteChanSize)
	h.voteMonitorSub = h.votepool.SubscribeNewVoteEvent(voteCh)
	for {
		select {
		case event := <-voteCh:
			pendingBlockNumber := h.chain.CurrentHeader().Number.Uint64() + 1
			h.maliciousVoteMonitor.ConflictDetect(event.Vote, pendingBlockNumber)
		case <-h.voteMonitorSub.Err():
			return
		}
	}
}

// 用于停止以太坊协议的处理程序
// 该方法用于优雅地停止以太坊协议的处理程序
// 它会取消订阅各种事件，关闭循环，断开与对等节点的连接，并等待所有任务完成，以确保协议停止的顺利进行。
func (h *handler) Stop() {
	// 取消订阅新交易事件，从而退出 txBroadcastLoop
	h.txsSub.Unsubscribe() // quits txBroadcastLoop
	// 取消订阅重新公布交易事件，从而退出 txReannounceLoop。
	h.reannoTxsSub.Unsubscribe() // quits txReannounceLoop
	// 取消订阅新挖矿的区块事件，从而退出 blockBroadcastLoop。
	h.minedBlockSub.Unsubscribe() // quits blockBroadcastLoop
	if h.votepool != nil {
		// 取消订阅新投票事件，从而退出 voteBroadcastLoop
		h.votesSub.Unsubscribe() // quits voteBroadcastLoop
		if h.maliciousVoteMonitor != nil {
			// 取消订阅恶意投票监视事件
			h.voteMonitorSub.Unsubscribe()
		}
	}

	// Quit chainSync and txsync64.
	// After this is done, no new peers will be accepted.
	// 关闭 h.quitSync 通道：
	// 通知 chainSync 和 txsync64 循环停止，从此之后不再接受新的对等节点。
	// 关闭 h.quitSync 后，新的对等节点无法加入。
	close(h.quitSync)
	// 等待 h.wg 等待组中的所有任务完成，这包括之前启动的各种循环和处理任务
	h.wg.Wait()

	// Disconnect existing sessions.
	// This also closes the gate for any new registrations on the peer set.
	// sessions which are already established but not added to h.peers yet
	// will exit when they try to register.
	// 关闭现有会话：断开与现有对等节点的连接，这也关闭了对等节点集合上的任何新注册。
	// 已经建立但尚未添加到 h.peers 的会话将在尝试注册时退出
	h.peers.close()
	h.peerWG.Wait()

	log.Info("Ethereum protocol stopped")
}

// BroadcastBlock will either propagate a block to a subset of its peers, or
// will only announce its availability (depending what's requested).
func (h *handler) BroadcastBlock(block *types.Block, propagate bool) {
	// Disable the block propagation if the chain has already entered the PoS
	// stage. The block propagation is delegated to the consensus layer.
	if h.merger.PoSFinalized() {
		return
	}
	// Disable the block propagation if it's the post-merge block.
	if beacon, ok := h.chain.Engine().(*beacon.Beacon); ok {
		if beacon.IsPoSHeader(block.Header()) {
			return
		}
	}
	hash := block.Hash()
	peers := h.peers.peersWithoutBlock(hash)

	// If propagation is requested, send to a subset of the peer
	if propagate {
		// Calculate the TD of the block (it's not imported yet, so block.Td is not valid)
		var td *big.Int
		if parent := h.chain.GetBlock(block.ParentHash(), block.NumberU64()-1); parent != nil {
			td = new(big.Int).Add(block.Difficulty(), h.chain.GetTd(block.ParentHash(), block.NumberU64()-1))
		} else {
			log.Error("Propagating dangling block", "number", block.Number(), "hash", hash)
			return
		}
		// Send the block to a subset of our peers
		var transfer []*ethPeer
		if h.directBroadcast {
			transfer = peers[:]
		} else {
			transfer = peers[:int(math.Sqrt(float64(len(peers))))]
		}
		diff := h.chain.GetDiffLayerRLP(block.Hash())
		for _, peer := range transfer {
			if len(diff) != 0 && peer.diffExt != nil {
				// difflayer should send before block
				peer.diffExt.SendDiffLayers([]rlp.RawValue{diff})
			}
			peer.AsyncSendNewBlock(block, td)
		}

		log.Trace("Propagated block", "hash", hash, "recipients", len(transfer), "duration", common.PrettyDuration(time.Since(block.ReceivedAt)))
		return
	}
	// Otherwise if the block is indeed in our own chain, announce it
	if h.chain.HasBlock(hash, block.NumberU64()) {
		for _, peer := range peers {
			peer.AsyncSendNewBlockHash(block)
		}
		log.Trace("Announced block", "hash", hash, "recipients", len(peers), "duration", common.PrettyDuration(time.Since(block.ReceivedAt)))
	}
}

// BroadcastTransactions will propagate a batch of transactions
// - To a square root of all peers
// - And, separately, as announcements to all peers which are not known to
// already have the given transaction.
func (h *handler) BroadcastTransactions(txs types.Transactions) {
	var (
		annoCount   int // Count of announcements made
		annoPeers   int
		directCount int // Count of the txs sent directly to peers
		directPeers int // Count of the peers that were sent transactions directly

		txset = make(map[*ethPeer][]common.Hash) // Set peer->hash to transfer directly
		annos = make(map[*ethPeer][]common.Hash) // Set peer->hash to announce

	)
	// Broadcast transactions to a batch of peers not knowing about it
	for _, tx := range txs {
		peers := h.peers.peersWithoutTransaction(tx.Hash())
		// Send the tx unconditionally to a subset of our peers
		numDirect := int(math.Sqrt(float64(len(peers))))
		for _, peer := range peers[:numDirect] {
			txset[peer] = append(txset[peer], tx.Hash())
		}
		// For the remaining peers, send announcement only
		for _, peer := range peers[numDirect:] {
			annos[peer] = append(annos[peer], tx.Hash())
		}
	}
	for peer, hashes := range txset {
		directPeers++
		directCount += len(hashes)
		peer.AsyncSendTransactions(hashes)
	}
	for peer, hashes := range annos {
		annoPeers++
		annoCount += len(hashes)
		peer.AsyncSendPooledTransactionHashes(hashes)
	}
	log.Debug("Transaction broadcast", "txs", len(txs),
		"announce packs", annoPeers, "announced hashes", annoCount,
		"tx packs", directPeers, "broadcast txs", directCount)
}

// ReannounceTransactions will announce a batch of local pending transactions
// to a square root of all peers.
func (h *handler) ReannounceTransactions(txs types.Transactions) {
	hashes := make([]common.Hash, 0, txs.Len())
	for _, tx := range txs {
		hashes = append(hashes, tx.Hash())
	}

	// Announce transactions hash to a batch of peers
	peersCount := uint(math.Sqrt(float64(h.peers.len())))
	peers := h.peers.headPeers(peersCount)
	for _, peer := range peers {
		peer.AsyncSendPooledTransactionHashes(hashes)
	}
	log.Debug("Transaction reannounce", "txs", len(txs),
		"announce packs", peersCount, "announced hashes", peersCount*uint(len(hashes)))
}

// BroadcastVote will propagate a batch of votes to all peers
// which are not known to already have the given vote.
func (h *handler) BroadcastVote(vote *types.VoteEnvelope) {
	var (
		directCount int // Count of announcements made
		directPeers int

		voteMap = make(map[*ethPeer]*types.VoteEnvelope) // Set peer->hash to transfer directly
	)

	// Broadcast vote to a batch of peers not knowing about it
	peers := h.peers.peersWithoutVote(vote.Hash())
	headBlock := h.chain.CurrentBlock()
	currentTD := h.chain.GetTd(headBlock.Hash(), headBlock.NumberU64())
	for _, peer := range peers {
		_, peerTD := peer.Head()
		deltaTD := new(big.Int).Abs(new(big.Int).Sub(currentTD, peerTD))
		if deltaTD.Cmp(big.NewInt(deltaTdThreshold)) < 1 && peer.bscExt != nil {
			voteMap[peer] = vote
		}
	}

	for peer, _vote := range voteMap {
		directPeers++
		directCount += 1
		votes := []*types.VoteEnvelope{_vote}
		peer.bscExt.AsyncSendVotes(votes)
	}
	log.Debug("Vote broadcast", "vote packs", directPeers, "broadcast vote", directCount)
}

// minedBroadcastLoop sends mined blocks to connected peers.
func (h *handler) minedBroadcastLoop() {
	defer h.wg.Done()

	for obj := range h.minedBlockSub.Chan() {
		if ev, ok := obj.Data.(core.NewMinedBlockEvent); ok {
			h.BroadcastBlock(ev.Block, true)  // First propagate block to peers
			h.BroadcastBlock(ev.Block, false) // Only then announce to the rest
		}
	}
}

// txBroadcastLoop announces new transactions to connected peers.
func (h *handler) txBroadcastLoop() {
	defer h.wg.Done()
	for {
		select {
		case event := <-h.txsCh:
			h.BroadcastTransactions(event.Txs) // 广播交易
		case <-h.txsSub.Err():
			return
		}
	}
}

// txReannounceLoop announces local pending transactions to connected peers again.
func (h *handler) txReannounceLoop() {
	defer h.wg.Done()
	for {
		select {
		case event := <-h.reannoTxsCh:
			h.ReannounceTransactions(event.Txs)
		case <-h.reannoTxsSub.Err():
			return
		}
	}
}

// voteBroadcastLoop announces new vote to connected peers.
func (h *handler) voteBroadcastLoop() {
	defer h.wg.Done()
	for {
		select {
		case event := <-h.voteCh:
			// The timeliness of votes is very important,
			// so one vote will be sent instantly without waiting for other votes for batch sending by design.
			h.BroadcastVote(event.Vote)
		case <-h.votesSub.Err():
			return
		}
	}
}
