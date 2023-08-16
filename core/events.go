// Copyright 2014 The go-ethereum Authors
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

package core

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// NewTxsEvent is posted when a batch of transactions enters the transaction pool.
// 当一批交易进入交易池时,会发布此事件.它包含一个类型为types.Transaction的切片字段Txs,表示新的交易列表
type NewTxsEvent struct{ Txs []*types.Transaction }

// ReannoTxsEvent is posted when a batch of local pending transactions exceed a specified duration.
// 当一批本地待处理的交易超过指定的持续时间时,会发布此事件.它包含一个类型为types.Transaction的切片字段Txs,表示超时的交易列表.
type ReannoTxsEvent struct{ Txs []*types.Transaction }

// NewMinedBlockEvent is posted when a block has been imported.
// 当一个块被导入时,会发布此事件.它包含一个类型为types.Block的字段Block,表示导入的块.
type NewMinedBlockEvent struct{ Block *types.Block }

// RemovedLogsEvent is posted when a reorg happens
// 当发生重组时,会发布此事件.它包含一个类型为types.Log的切片字段Logs,表示被移除的日志列表.
type RemovedLogsEvent struct{ Logs []*types.Log }

// NewVoteEvent is posted when a batch of votes enters the vote pool.
// 当一批投票进入投票池时,会发布此事件.它包含一个类型为types.VoteEnvelope的字段Vote,表示新的投票.
type NewVoteEvent struct{ Vote *types.VoteEnvelope }

// FinalizedHeaderEvent is posted when a finalized header is reached.
// 当达到最终化的头部时,会发布此事件.它包含一个类型为types.Header的字段Header,表示最终化的头部.
type FinalizedHeaderEvent struct{ Header *types.Header }

// 表示链事件,当一个新的区块被添加到区块链时,或者当一个已经存在的区块被重新组织时,会触发该事件.
type ChainEvent struct {
	Block *types.Block
	Hash  common.Hash
	Logs  []*types.Log
}

// 表示链侧事件,当一个新的区块被添加到区块链的侧链上时,会触发该事件.
type ChainSideEvent struct {
	Block *types.Block
}

// 表示链头事件,当区块链的链头发生变化时,即新的区块被添加到区块链的主链上时,会触发该事件.
type ChainHeadEvent struct{ Block *types.Block }
