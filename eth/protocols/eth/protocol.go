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

package eth

import (
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// Constants to match up protocol versions and messages
// 当以太坊网络进行协议升级时, 会引入新的协议版本号来区分不同的协议规范.这些协议版本号用于标识节点所使用的协议版本, 并确保节点能够正确解析和处理收到的消息.
// ETH/66: 以太坊协议的较早版本, 引入了一些重要的消息类型, 如UpgradeStatusMsg, 用于在协议升级时进行握手和确认.ETH/66版本的协议规范定义了节点之间的通信协议和消息格式.
// ETH/67: 以太坊协议的更新版本, 引入了一些新的消息类型, 如 NewPooledTransactionHashesMsg, GetPooledTransactionsMsg 和 PooledTransactionsMsg.
// ........这些新的消息类型用于传输和处理未确认交易的信息.ETH/67版本的协议规范扩展了以太坊网络的功能和性能.
const (
	ETH66 = 66
	ETH67 = 67
)

// ProtocolName is the official short name of the `eth` protocol used during
// devp2p capability negotiation.
const ProtocolName = "eth"

// ProtocolVersions are the supported versions of the `eth` protocol (first
// is primary).
var ProtocolVersions = []uint{ETH66, ETH67}

// protocolLengths are the number of implemented message corresponding to
// different protocol versions.
var protocolLengths = map[uint]uint64{ETH67: 18, ETH66: 17}

// maxMessageSize is the maximum cap on the size of a protocol message.
const maxMessageSize = 10 * 1024 * 1024

// 👇 以太坊协议中定义的消息类型常量.这些常量用于区分不同类型的消息, 并在以太坊网络中进行通信
// 这些消息类型用于以太坊节点之间的通信, 以实现区块链同步、交易传输和其他协议功能.
const (
	StatusMsg                     = 0x00 // 握手消息, 用于建立网络连接和交换信息的初始阶段.
	NewBlockHashesMsg             = 0x01 // 用于传递新块的哈希列表以供同步其他节点的链.
	TransactionsMsg               = 0x02 // 用于传递未打包的交易, 以便其他节点可以执行和验证这些交易.
	GetBlockHeadersMsg            = 0x03 // 请求某个区块范围内的区块头列表.
	BlockHeadersMsg               = 0x04 // 响应请求, 包含某个区块范围内的区块头列表.
	GetBlockBodiesMsg             = 0x05 // 请求某个区块范围内的区块体列表.
	BlockBodiesMsg                = 0x06 // 响应请求, 包含某个区块范围内的区块体列表.
	NewBlockMsg                   = 0x07 // 用于广播新生成的块.
	GetNodeDataMsg                = 0x0d // 请求特定节点数据的消息.
	NodeDataMsg                   = 0x0e // 包含特定节点数据的响应消息.
	GetReceiptsMsg                = 0x0f // 请求交易收据的消息.
	ReceiptsMsg                   = 0x10 // 包含交易收据的响应消息.
	NewPooledTransactionHashesMsg = 0x08 // 用于传输一批新的未确认交易的哈希列表.
	GetPooledTransactionsMsg      = 0x09 // 请求未确认交易的完整信息.
	PooledTransactionsMsg         = 0x0a // 包含未确认交易的完整信息的响应消息.

	// Protocol messages overloaded in eth/66
	UpgradeStatusMsg = 0x0b // 这是以太坊协议的一部分, 用于协议升级时的握手消息.
)

var (
	errNoStatusMsg             = errors.New("no status message")
	errMsgTooLarge             = errors.New("message too long")
	errDecode                  = errors.New("invalid message")
	errInvalidMsgCode          = errors.New("invalid message code")
	errProtocolVersionMismatch = errors.New("protocol version mismatch")
	errNetworkIDMismatch       = errors.New("network ID mismatch")
	errGenesisMismatch         = errors.New("genesis mismatch")
	errForkIDRejected          = errors.New("fork ID rejected")
)

// Packet represents a p2p message in the `eth` protocol.
type Packet interface {
	Name() string // Name returns a string corresponding to the message type.
	Kind() byte   // Kind returns the message type.
}

// StatusPacket is the network packet for the status message for eth/64 and later.
// 网络数据包结构
// 通过传输这些状态信息，节点可以告知其他节点有关其协议版本、网络标识符、链的工作量证明难度、当前链头的哈希值、创世块的哈希值以及支持的分叉情况.
// 这些信息对于节点之间的连接和区块链同步非常重要，并有助于确保节点之间的一致性和正确性.
type StatusPacket struct {
	ProtocolVersion uint32      // 以太坊协议的版本号.它指示节点所使用的协议版本.
	NetworkID       uint64      // 以太坊网络的唯一标识符.不同的以太坊网络可以具有不同的网络标识符.
	TD              *big.Int    // 链的总难度（Total Difficulty）.它是一个大整数，表示当前链的工作量证明难度.
	Head            common.Hash // 链的头部块的哈希值.它指示当前链上的最新区块.
	Genesis         common.Hash // 以太坊区块链的创世块（Genesis Block）的哈希值.它标识了区块链的起始点.
	ForkID          forkid.ID   // 分叉标识符.它描述了节点支持的以太坊协议的分叉情况.
}

type UpgradeStatusExtension struct {
	DisablePeerTxBroadcast bool
}

func (e *UpgradeStatusExtension) Encode() (*rlp.RawValue, error) {
	rawBytes, err := rlp.EncodeToBytes(e)
	if err != nil {
		return nil, err
	}
	raw := rlp.RawValue(rawBytes)
	return &raw, nil
}

type UpgradeStatusPacket struct {
	Extension *rlp.RawValue `rlp:"nil"`
}

// 获取升级状态扩展的信息
// UpgradeStatusPacket 是以太坊协议中用于握手和确认升级的消息类型之一
func (p *UpgradeStatusPacket) GetExtension() (*UpgradeStatusExtension, error) {
	extension := &UpgradeStatusExtension{}
	if p.Extension == nil {
		return extension, nil
	}
	err := rlp.DecodeBytes(*p.Extension, extension)
	if err != nil {
		return nil, err
	}
	return extension, nil
}

// NewBlockHashesPacket is the network packet for the block announcements.
type NewBlockHashesPacket []struct {
	Hash   common.Hash // Hash of one particular block being announced
	Number uint64      // Number of one particular block being announced
}

// Unpack retrieves the block hashes and numbers from the announcement packet
// and returns them in a split flat format that's more consistent with the
// internal data structures.
func (p *NewBlockHashesPacket) Unpack() ([]common.Hash, []uint64) {
	var (
		hashes  = make([]common.Hash, len(*p))
		numbers = make([]uint64, len(*p))
	)
	for i, body := range *p {
		hashes[i], numbers[i] = body.Hash, body.Number
	}
	return hashes, numbers
}

// TransactionsPacket is the network packet for broadcasting new transactions.
type TransactionsPacket []*types.Transaction

// GetBlockHeadersPacket represents a block header query.
type GetBlockHeadersPacket struct {
	Origin  HashOrNumber // Block from which to retrieve headers
	Amount  uint64       // Maximum number of headers to retrieve
	Skip    uint64       // Blocks to skip between consecutive headers
	Reverse bool         // Query direction (false = rising towards latest, true = falling towards genesis)
}

// GetBlockHeadersPacket66 represents a block header query over eth/66
type GetBlockHeadersPacket66 struct {
	RequestId uint64
	*GetBlockHeadersPacket
}

// HashOrNumber is a combined field for specifying an origin block.
type HashOrNumber struct {
	Hash   common.Hash // Block hash from which to retrieve headers (excludes Number)
	Number uint64      // Block hash from which to retrieve headers (excludes Hash)
}

// EncodeRLP is a specialized encoder for HashOrNumber to encode only one of the
// two contained union fields.
func (hn *HashOrNumber) EncodeRLP(w io.Writer) error {
	if hn.Hash == (common.Hash{}) {
		return rlp.Encode(w, hn.Number)
	}
	if hn.Number != 0 {
		return fmt.Errorf("both origin hash (%x) and number (%d) provided", hn.Hash, hn.Number)
	}
	return rlp.Encode(w, hn.Hash)
}

// DecodeRLP is a specialized decoder for HashOrNumber to decode the contents
// into either a block hash or a block number.
func (hn *HashOrNumber) DecodeRLP(s *rlp.Stream) error {
	_, size, err := s.Kind()
	switch {
	case err != nil:
		return err
	case size == 32:
		hn.Number = 0
		return s.Decode(&hn.Hash)
	case size <= 8:
		hn.Hash = common.Hash{}
		return s.Decode(&hn.Number)
	default:
		return fmt.Errorf("invalid input size %d for origin", size)
	}
}

// BlockHeadersPacket represents a block header response.
type BlockHeadersPacket []*types.Header

// BlockHeadersPacket66 represents a block header response over eth/66.
type BlockHeadersPacket66 struct {
	RequestId uint64
	BlockHeadersPacket
}

// BlockHeadersRLPPacket represents a block header response, to use when we already
// have the headers rlp encoded.
type BlockHeadersRLPPacket []rlp.RawValue

// BlockHeadersPacket represents a block header response over eth/66.
type BlockHeadersRLPPacket66 struct {
	RequestId uint64
	BlockHeadersRLPPacket
}

// NewBlockPacket is the network packet for the block propagation message.
type NewBlockPacket struct {
	Block *types.Block
	TD    *big.Int
}

// sanityCheck verifies that the values are reasonable, as a DoS protection
func (request *NewBlockPacket) sanityCheck() error {
	if err := request.Block.SanityCheck(); err != nil {
		return err
	}
	//TD at mainnet block #7753254 is 76 bits. If it becomes 100 million times
	// larger, it will still fit within 100 bits
	if tdlen := request.TD.BitLen(); tdlen > 100 {
		return fmt.Errorf("too large block TD: bitlen %d", tdlen)
	}
	return nil
}

// GetBlockBodiesPacket represents a block body query.
type GetBlockBodiesPacket []common.Hash

// GetBlockBodiesPacket66 represents a block body query over eth/66.
type GetBlockBodiesPacket66 struct {
	RequestId uint64
	GetBlockBodiesPacket
}

// BlockBodiesPacket is the network packet for block content distribution.
type BlockBodiesPacket []*BlockBody

// BlockBodiesPacket66 is the network packet for block content distribution over eth/66.
type BlockBodiesPacket66 struct {
	RequestId uint64
	BlockBodiesPacket
}

// BlockBodiesRLPPacket is used for replying to block body requests, in cases
// where we already have them RLP-encoded, and thus can avoid the decode-encode
// roundtrip.
type BlockBodiesRLPPacket []rlp.RawValue

// BlockBodiesRLPPacket66 is the BlockBodiesRLPPacket over eth/66
type BlockBodiesRLPPacket66 struct {
	RequestId uint64
	BlockBodiesRLPPacket
}

// BlockBody represents the data content of a single block.
type BlockBody struct {
	Transactions []*types.Transaction // Transactions contained within a block
	Uncles       []*types.Header      // Uncles contained within a block
}

// Unpack retrieves the transactions and uncles from the range packet and returns
// them in a split flat format that's more consistent with the internal data structures.
func (p *BlockBodiesPacket) Unpack() ([][]*types.Transaction, [][]*types.Header) {
	var (
		txset    = make([][]*types.Transaction, len(*p))
		uncleset = make([][]*types.Header, len(*p))
	)
	for i, body := range *p {
		txset[i], uncleset[i] = body.Transactions, body.Uncles
	}
	return txset, uncleset
}

// GetNodeDataPacket represents a trie node data query.
type GetNodeDataPacket []common.Hash

// GetNodeDataPacket66 represents a trie node data query over eth/66.
type GetNodeDataPacket66 struct {
	RequestId uint64
	GetNodeDataPacket
}

// NodeDataPacket is the network packet for trie node data distribution.
type NodeDataPacket [][]byte

// NodeDataPacket66 is the network packet for trie node data distribution over eth/66.
type NodeDataPacket66 struct {
	RequestId uint64
	NodeDataPacket
}

// GetReceiptsPacket represents a block receipts query.
type GetReceiptsPacket []common.Hash

// GetReceiptsPacket66 represents a block receipts query over eth/66.
type GetReceiptsPacket66 struct {
	RequestId uint64
	GetReceiptsPacket
}

// ReceiptsPacket is the network packet for block receipts distribution.
type ReceiptsPacket [][]*types.Receipt

// ReceiptsPacket66 is the network packet for block receipts distribution over eth/66.
type ReceiptsPacket66 struct {
	RequestId uint64
	ReceiptsPacket
}

// ReceiptsRLPPacket is used for receipts, when we already have it encoded
type ReceiptsRLPPacket []rlp.RawValue

// ReceiptsRLPPacket66 is the eth-66 version of ReceiptsRLPPacket
type ReceiptsRLPPacket66 struct {
	RequestId uint64
	ReceiptsRLPPacket
}

// NewPooledTransactionHashesPacket represents a transaction announcement packet.
type NewPooledTransactionHashesPacket []common.Hash

// GetPooledTransactionsPacket represents a transaction query.
type GetPooledTransactionsPacket []common.Hash

type GetPooledTransactionsPacket66 struct {
	RequestId uint64
	GetPooledTransactionsPacket
}

// PooledTransactionsPacket is the network packet for transaction distribution.
type PooledTransactionsPacket []*types.Transaction

// PooledTransactionsPacket66 is the network packet for transaction distribution over eth/66.
type PooledTransactionsPacket66 struct {
	RequestId uint64
	PooledTransactionsPacket
}

// PooledTransactionsRLPPacket is the network packet for transaction distribution, used
// in the cases we already have them in rlp-encoded form
type PooledTransactionsRLPPacket []rlp.RawValue

// PooledTransactionsRLPPacket66 is the eth/66 form of PooledTransactionsRLPPacket
type PooledTransactionsRLPPacket66 struct {
	RequestId uint64
	PooledTransactionsRLPPacket
}

func (*StatusPacket) Name() string { return "Status" }
func (*StatusPacket) Kind() byte   { return StatusMsg }

func (*UpgradeStatusPacket) Name() string { return "UpgradeStatus" }
func (*UpgradeStatusPacket) Kind() byte   { return UpgradeStatusMsg }

func (*NewBlockHashesPacket) Name() string { return "NewBlockHashes" }
func (*NewBlockHashesPacket) Kind() byte   { return NewBlockHashesMsg }

func (*TransactionsPacket) Name() string { return "Transactions" }
func (*TransactionsPacket) Kind() byte   { return TransactionsMsg }

func (*GetBlockHeadersPacket) Name() string { return "GetBlockHeaders" }
func (*GetBlockHeadersPacket) Kind() byte   { return GetBlockHeadersMsg }

func (*BlockHeadersPacket) Name() string { return "BlockHeaders" }
func (*BlockHeadersPacket) Kind() byte   { return BlockHeadersMsg }

func (*GetBlockBodiesPacket) Name() string { return "GetBlockBodies" }
func (*GetBlockBodiesPacket) Kind() byte   { return GetBlockBodiesMsg }

func (*BlockBodiesPacket) Name() string { return "BlockBodies" }
func (*BlockBodiesPacket) Kind() byte   { return BlockBodiesMsg }

func (*NewBlockPacket) Name() string { return "NewBlock" }
func (*NewBlockPacket) Kind() byte   { return NewBlockMsg }

func (*GetNodeDataPacket) Name() string { return "GetNodeData" }
func (*GetNodeDataPacket) Kind() byte   { return GetNodeDataMsg }

func (*NodeDataPacket) Name() string { return "NodeData" }
func (*NodeDataPacket) Kind() byte   { return NodeDataMsg }

func (*GetReceiptsPacket) Name() string { return "GetReceipts" }
func (*GetReceiptsPacket) Kind() byte   { return GetReceiptsMsg }

func (*ReceiptsPacket) Name() string { return "Receipts" }
func (*ReceiptsPacket) Kind() byte   { return ReceiptsMsg }

func (*NewPooledTransactionHashesPacket) Name() string { return "NewPooledTransactionHashes" }
func (*NewPooledTransactionHashesPacket) Kind() byte   { return NewPooledTransactionHashesMsg }

func (*GetPooledTransactionsPacket) Name() string { return "GetPooledTransactions" }
func (*GetPooledTransactionsPacket) Kind() byte   { return GetPooledTransactionsMsg }

func (*PooledTransactionsPacket) Name() string { return "PooledTransactions" }
func (*PooledTransactionsPacket) Kind() byte   { return PooledTransactionsMsg }
