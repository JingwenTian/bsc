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
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
)

const (
	// 同步时有peers最小连接数限制,在节点刚启动时连接数较少通过强制同步的定时器,保证每5秒就忽略一次连接数限制,加速冷启动周期
	forceSyncCycle      = 10 * time.Second // Time interval to force syncs, even if few peers are available
	defaultMinSyncPeers = 5                // Amount of peers desired to start syncing
)

// syncTransactions starts sending all currently pending transactions to the given peer.
func (h *handler) syncTransactions(p *eth.Peer) {
	// Assemble the set of transaction to broadcast or announce to the remote
	// peer. Fun fact, this is quite an expensive operation as it needs to sort
	// the transactions if the sorting is not cached yet. However, with a random
	// order, insertions could overflow the non-executable queues and get dropped.
	//
	// TODO(karalabe): Figure out if we could get away with random order somehow
	var txs types.Transactions
	pending := h.txpool.Pending(false)
	for _, batch := range pending {
		txs = append(txs, batch...)
	}
	if len(txs) == 0 {
		return
	}
	// The eth/65 protocol introduces proper transaction announcements, so instead
	// of dripping transactions across multiple peers, just send the entire list as
	// an announcement and let the remote side decide what they need (likely nothing).
	hashes := make([]common.Hash, len(txs))
	for i, tx := range txs {
		hashes[i] = tx.Hash()
	}
	p.AsyncSendPooledTransactionHashes(hashes)
}

// syncVotes starts sending all currently pending votes to the given peer.
func (h *handler) syncVotes(p *bscPeer) {
	votes := h.votepool.GetVotes()
	if len(votes) == 0 {
		return
	}
	p.AsyncSendVotes(votes)
}

// chainSyncer coordinates blockchain sync components.
type chainSyncer struct {
	handler     *handler
	force       *time.Timer
	forced      bool // true when force timer fired
	peerEventCh chan struct{}
	doneCh      chan error // non-nil when sync is running
}

// chainSyncOp is a scheduled sync operation.
type chainSyncOp struct {
	mode downloader.SyncMode
	peer *eth.Peer
	td   *big.Int
	head common.Hash
}

// newChainSyncer creates a chainSyncer.
func newChainSyncer(handler *handler) *chainSyncer {
	return &chainSyncer{
		handler:     handler,
		peerEventCh: make(chan struct{}),
	}
}

// handlePeerEvent notifies the syncer about a change in the peer set.
// This is called for new peers and every time a peer announces a new
// chain head.
func (cs *chainSyncer) handlePeerEvent(peer *eth.Peer) bool {
	select {
	case cs.peerEventCh <- struct{}{}:
		return true
	case <-cs.handler.quitSync:
		return false
	}
}

// loop runs in its own goroutine and launches the sync when necessary.
// 🔃 🔃 🔃 🔃 通过定时器和事件监听, 在一个循环中根据需要启动同步操作。
// 这个方法是以太坊客户端同步机制的核心, 负责保持本地区块链与网络中的其他节点同步。
func (cs *chainSyncer) loop() {
	defer cs.handler.wg.Done()

	// 🔷 启动 Block 和 Transaction Fetcher:
	// 在 loop 方法的开头，首先启动区块和交易获取器（blockFetcher 和 txFetcher），以便从远程节点获取所需的区块和交易数据。
	cs.handler.blockFetcher.Start()
	cs.handler.txFetcher.Start()
	defer cs.handler.blockFetcher.Stop()
	defer cs.handler.txFetcher.Stop()
	defer cs.handler.downloader.Terminate()

	// The force timer lowers the peer count threshold down to one when it fires.
	// This ensures we'll always start sync even if there aren't enough peers.
	// 🔷 定时强制同步:
	// 使用一个定时器 force，定期触发强制同步操作。当定时器触发时，将强制降低同步所需的对等节点数量的阈值，这样即使对等节点数量不足，也会启动同步。
	cs.force = time.NewTimer(forceSyncCycle)
	defer cs.force.Stop()

	// 🔷 循环同步操作:
	for {
		// 在一个无限循环中，不断检查是否存在下一个需要同步的操作 op。如果存在，则调用 startSync 方法开始同步操作
		if op := cs.nextSyncOp(); op != nil {
			cs.startSync(op)
		}
		// 同时，通过 select 语句监听多个 channel 的事件
		select {
		// 🔹 当对等节点信息发生变化时，重新检查同步操作
		case <-cs.peerEventCh:
			// Peer information changed, recheck.
		// 🔹 当同步完成时，重置强制同步定时器并标记为强制同步已完成
		case <-cs.doneCh:
			cs.doneCh = nil
			cs.force.Reset(forceSyncCycle)
			cs.forced = false
		// 🔹 强制同步定时器触发，标记为已强制同步
		case <-cs.force.C:
			cs.forced = true
		// 🔹 当收到退出同步的信号时，停止区块链的插入操作，终止下载器，并结束 loop 循环。
		case <-cs.handler.quitSync:
			// Disable all insertion on the blockchain. This needs to happen before
			// terminating the downloader because the downloader waits for blockchain
			// inserts, and these can take a long time to finish.
			cs.handler.chain.StopInsert()
			cs.handler.downloader.Terminate()
			if cs.doneCh != nil {
				<-cs.doneCh
			}
			return
		}
	}
}

// nextSyncOp determines whether sync is required at this time.
// 用于决定是否需要进行同步操作
// 根据一系列条件判断是否需要进行同步操作，如果需要同步操作，则生成一个同步操作以便在适当时机执行。
// 这个方法的作用是在正确的时机决定是否触发同步，以确保本地区块链与网络中的其他节点保持同步
func (cs *chainSyncer) nextSyncOp() *chainSyncOp {
	// 🔷 检查同步状态: 如果当前已经有同步操作正在进行中（cs.doneCh 不为空），则不需要进行新的同步操作
	if cs.doneCh != nil {
		return nil // Sync already running.
	}
	// Disable the td based sync trigger after the transition
	// 🔷 检查转换期间的 TD 触发: 如果合并器（merger）的终端总难度（Terminal Total Difficulty，TDD）已经达到
	// 说明当前正在进行区块链转换期间的同步操作，此时不需要进行新的同步操作
	if cs.handler.merger.TDDReached() {
		return nil
	}
	// Ensure we're at minimum peer count.
	// 🔷 检查对等节点数量: 确保当前连接的对等节点数量达到最小同步对等节点数量（defaultMinSyncPeers）。
	// 如果之前已经强制执行过同步操作（cs.forced 为 true），则最小同步对等节点数量可以为 1。
	// 如果最小同步对等节点数量大于当前连接的对等节点数量（cs.handler.peers.len()），也不进行同步操作
	minPeers := defaultMinSyncPeers
	if cs.forced {
		minPeers = 1
	} else if minPeers > cs.handler.maxPeers {
		minPeers = cs.handler.maxPeers
	}
	if cs.handler.peers.len() < minPeers {
		return nil
	}
	// We have enough peers, check TD
	// 🔷 检查最高总难度: 从当前已连接的对等节点中选择总难度最高的对等节点（peer），然后根据本地的总难度情况判断是否需要进行同步。
	// 如果本地总难度（ourTD）已经超过等于该对等节点的总难度，说明本地已经追上或超过该对等节点，不需要进行同步
	peer := cs.handler.peers.peerWithHighestTD()
	if peer == nil {
		return nil
	}
	mode, ourTD := cs.modeAndLocalHead()

	// 🔷 生成同步操作: 如果以上条件都满足，根据对等节点的同步模式和总难度，生成一个同步操作（chainSyncOp），该操作将被传递给 startSync 方法来执行实际的同步操作。
	op := peerToSyncOp(mode, peer)
	if op.td.Cmp(ourTD) <= 0 {
		return nil // We're in sync.
	}
	return op
}

func peerToSyncOp(mode downloader.SyncMode, p *eth.Peer) *chainSyncOp {
	peerHead, peerTD := p.Head()
	return &chainSyncOp{mode: mode, peer: p, td: peerTD, head: peerHead}
}

func (cs *chainSyncer) modeAndLocalHead() (downloader.SyncMode, *big.Int) {
	// If we're in snap sync mode, return that directly
	if atomic.LoadUint32(&cs.handler.snapSync) == 1 {
		block := cs.handler.chain.CurrentFastBlock()
		td := cs.handler.chain.GetTd(block.Hash(), block.NumberU64())
		return downloader.SnapSync, td
	}
	// We are probably in full sync, but we might have rewound to before the
	// snap sync pivot, check if we should reenable
	if pivot := rawdb.ReadLastPivotNumber(cs.handler.database); pivot != nil {
		if head := cs.handler.chain.CurrentBlock(); head.NumberU64() < *pivot {
			if rawdb.ReadAncientType(cs.handler.database) == rawdb.PruneFreezerType {
				log.Crit("Current rewound to before the fast sync pivot, can't enable pruneancient mode", "current block number", head.NumberU64(), "pivot", *pivot)
			}
			block := cs.handler.chain.CurrentFastBlock()
			td := cs.handler.chain.GetTd(block.Hash(), block.NumberU64())
			return downloader.SnapSync, td
		}
	}

	// Nope, we're really full syncing
	head := cs.handler.chain.CurrentBlock()
	td := cs.handler.chain.GetTd(head.Hash(), head.NumberU64())
	return downloader.FullSync, td
}

// startSync launches doSync in a new goroutine.
func (cs *chainSyncer) startSync(op *chainSyncOp) {
	cs.doneCh = make(chan error, 1)
	go func() { cs.doneCh <- cs.handler.doSync(op) }()
}

// doSync synchronizes the local blockchain with a remote peer.
// 🔄🔄🔄 执行远程节点的快照同步
// 实现了 handler 结构体的 doSync 方法，用于执行本地区块链与远程节点的同步操作
// 负责处理区块链与远程节点之间的同步过程，确保本地区块链与网络中的其他节点保持同步，并在同步完成后根据条件启用交易接收。这是以太坊客户端的核心功能之一，确保区块链网络的一致性和正确性。
func (h *handler) doSync(op *chainSyncOp) error {
	// 🔷 检查 Snap Sync:
	// 如果同步模式是 SnapSync（快照同步），则在启动快照同步之前，代码会检查是否使用相同的 txlookup 限制。
	// 这是为了确保在快照同步期间不会更改此限制，以避免可能的同步问题。
	if op.mode == downloader.SnapSync {
		// Before launch the snap sync, we have to ensure user uses the same
		// txlookup limit.
		// The main concern here is: during the snap sync Geth won't index the
		// block(generate tx indices) before the HEAD-limit. But if user changes
		// the limit in the next snap sync(e.g. user kill Geth manually and
		// restart) then it will be hard for Geth to figure out the oldest block
		// has been indexed. So here for the user-experience wise, it's non-optimal
		// that user can't change limit during the snap sync. If changed, Geth
		// will just blindly use the original one.
		limit := h.chain.TxLookupLimit()
		if stored := rawdb.ReadFastTxLookupLimit(h.database); stored == nil {
			rawdb.WriteFastTxLookupLimit(h.database, limit)
		} else if *stored != limit {
			h.chain.SetTxLookupLimit(*stored)
			log.Warn("Update txLookup limit", "provided", limit, "updated", *stored)
		}
	}
	// Run the sync cycle, and disable snap sync if we're past the pivot block
	// 🔷 运行同步周期: 开始同步历史区块快照
	// 调用下载器的 Synchronise 方法来执行同步操作，将本地区块链与远程节点的区块数据进行同步。
	// op.peer.ID() 表示要同步的远程节点的标识符，op.head 和 op.td 表示同步的目标头部和总难度。
	err := h.downloader.Synchronise(op.peer.ID(), op.head, op.td, op.mode)
	if err != nil {
		return err
	}
	// 🔷 处理 Snap Sync 完成:
	// 如果当前正在进行 Snap Sync，并且同步成功完成，将自动关闭 Snap Sync 模式，并将 snapSync 标志位设置为 0。
	if atomic.LoadUint32(&h.snapSync) == 1 {
		log.Info("Snap sync complete, auto disabling")
		atomic.StoreUint32(&h.snapSync, 0)
	}
	// If we've successfully finished a sync cycle and passed any required checkpoint,
	// enable accepting transactions from the network.
	// 🔷 检查是否启用交易接收:
	// 在同步完成后，根据当前区块的高度和是否通过了必要的检查点，决定是否启用接收网络中的交易。
	// 如果当前区块高度超过了指定的检查点区块高度，且区块的时间戳在过去一年内，将允许接收交易(表示可以接受来自网络的交易)
	head := h.chain.CurrentBlock()
	if head.NumberU64() >= h.checkpointNumber {
		// Checkpoint passed, sanity check the timestamp to have a fallback mechanism
		// for non-checkpointed (number = 0) private networks.
		if head.Time() >= uint64(time.Now().AddDate(0, -1, 0).Unix()) {
			atomic.StoreUint32(&h.acceptTxs, 1)
		}
	}
	// 🔷 通知同步完成:
	// 如果当前区块高度大于 0，表示已经完成了一轮同步，将通知所有对等节点关于新状态的变化。
	// 这对于在星型拓扑网络中需要通知所有过时的节点有新区块可用的情况非常重要。
	if head.NumberU64() > 0 {
		// We've completed a sync cycle, notify all peers of new state. This path is
		// essential in star-topology networks where a gateway node needs to notify
		// all its out-of-date peers of the availability of a new block. This failure
		// scenario will most often crop up in private and hackathon networks with
		// degenerate connectivity, but it should be healthy for the mainnet too to
		// more reliably update peers or the local TD state.
		h.BroadcastBlock(head, false)
	}
	return nil
}
