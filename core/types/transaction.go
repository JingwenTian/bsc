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

package types

import (
	"bytes"
	"container/heap"
	"errors"
	"io"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

var (
	ErrInvalidSig           = errors.New("invalid transaction v, r, s values")
	ErrUnexpectedProtection = errors.New("transaction type does not supported EIP-155 protected signatures")
	ErrInvalidTxType        = errors.New("transaction type not valid in this context")
	ErrTxTypeNotSupported   = errors.New("transaction type not supported")
	ErrGasFeeCapTooLow      = errors.New("fee cap less than base fee")
	errEmptyTypedTx         = errors.New("empty typed transaction bytes")
)

// Transaction types.
const (
	LegacyTxType = iota
	AccessListTxType
	DynamicFeeTxType
)

// Transaction is an Ethereum transaction.
type Transaction struct {
	inner TxData    // Consensus contents of a transaction
	time  time.Time // Time first seen locally (spam avoidance)

	// caches
	// 三个缓存项：交易哈希值(hash)、交易大小(size)和交易发送方(from)。缓存的原因是使用频次高且CPU计算量大
	// atomic.Value原子操作对象, 这样可防止并发引起多次哈希计算
	hash atomic.Value
	size atomic.Value
	from atomic.Value
}

// NewTx creates a new transaction.
func NewTx(inner TxData) *Transaction {
	tx := new(Transaction)
	tx.setDecoded(inner.copy(), 0)
	return tx
}

// TxData is the underlying data of a transaction.
//
// This is implemented by DynamicFeeTx, LegacyTx and AccessListTx.
type TxData interface {
	txType() byte // returns the type ID
	copy() TxData // creates a deep copy and initializes all fields

	chainID() *big.Int
	accessList() AccessList
	data() []byte
	gas() uint64
	gasPrice() *big.Int
	gasTipCap() *big.Int
	gasFeeCap() *big.Int
	value() *big.Int
	nonce() uint64
	to() *common.Address

	rawSignatureValues() (v, r, s *big.Int)
	setSignatureValues(chainID, v, r, s *big.Int)
}

// Time returns transaction's time
func (tx *Transaction) Time() time.Time {
	return tx.time
}

// ---------------------------
// RLP接口实现方法

// EncodeRLP implements rlp.Encoder
func (tx *Transaction) EncodeRLP(w io.Writer) error {
	if tx.Type() == LegacyTxType {
		return rlp.Encode(w, tx.inner)
	}
	// It's an EIP-2718 typed TX envelope.
	buf := encodeBufferPool.Get().(*bytes.Buffer)
	defer encodeBufferPool.Put(buf)
	buf.Reset()
	if err := tx.encodeTyped(buf); err != nil {
		return err
	}
	return rlp.Encode(w, buf.Bytes())
}

// encodeTyped writes the canonical encoding of a typed transaction to w.
func (tx *Transaction) encodeTyped(w *bytes.Buffer) error {
	w.WriteByte(tx.Type())
	return rlp.Encode(w, tx.inner)
}

// MarshalBinary returns the canonical encoding of the transaction.
// For legacy transactions, it returns the RLP encoding. For EIP-2718 typed
// transactions, it returns the type and payload.
func (tx *Transaction) MarshalBinary() ([]byte, error) {
	if tx.Type() == LegacyTxType {
		return rlp.EncodeToBytes(tx.inner)
	}
	var buf bytes.Buffer
	err := tx.encodeTyped(&buf)
	return buf.Bytes(), err
}

// DecodeRLP implements rlp.Decoder
func (tx *Transaction) DecodeRLP(s *rlp.Stream) error {
	kind, size, err := s.Kind()
	switch {
	case err != nil:
		return err
	case kind == rlp.List:
		// It's a legacy transaction.
		var inner LegacyTx
		err := s.Decode(&inner)
		if err == nil {
			tx.setDecoded(&inner, int(rlp.ListSize(size)))
		}
		return err
	case kind == rlp.String:
		// It's an EIP-2718 typed TX envelope.
		var b []byte
		if b, err = s.Bytes(); err != nil {
			return err
		}
		inner, err := tx.decodeTyped(b)
		if err == nil {
			tx.setDecoded(inner, len(b))
		}
		return err
	default:
		return rlp.ErrExpectedList
	}
}

// UnmarshalBinary decodes the canonical encoding of transactions.
// It supports legacy RLP transactions and EIP2718 typed transactions.
func (tx *Transaction) UnmarshalBinary(b []byte) error {
	if len(b) > 0 && b[0] > 0x7f {
		// It's a legacy transaction.
		var data LegacyTx
		err := rlp.DecodeBytes(b, &data)
		if err != nil {
			return err
		}
		tx.setDecoded(&data, len(b))
		return nil
	}
	// It's an EIP2718 typed transaction envelope.
	inner, err := tx.decodeTyped(b)
	if err != nil {
		return err
	}
	tx.setDecoded(inner, len(b))
	return nil
}

// decodeTyped decodes a typed transaction from the canonical format.
func (tx *Transaction) decodeTyped(b []byte) (TxData, error) {
	if len(b) == 0 {
		return nil, errEmptyTypedTx
	}
	switch b[0] {
	case AccessListTxType:
		var inner AccessListTx
		err := rlp.DecodeBytes(b[1:], &inner)
		return &inner, err
	case DynamicFeeTxType:
		var inner DynamicFeeTx
		err := rlp.DecodeBytes(b[1:], &inner)
		return &inner, err
	default:
		return nil, ErrTxTypeNotSupported
	}
}

// setDecoded sets the inner transaction and size after decoding.
func (tx *Transaction) setDecoded(inner TxData, size int) {
	tx.inner = inner
	tx.time = time.Now()
	if size > 0 {
		tx.size.Store(common.StorageSize(size))
	}
}

func sanityCheckSignature(v *big.Int, r *big.Int, s *big.Int, maybeProtected bool) error {
	if isProtectedV(v) && !maybeProtected {
		return ErrUnexpectedProtection
	}

	var plainV byte
	if isProtectedV(v) {
		chainID := deriveChainId(v).Uint64()
		plainV = byte(v.Uint64() - 35 - 2*chainID)
	} else if maybeProtected {
		// Only EIP-155 signatures can be optionally protected. Since
		// we determined this v value is not protected, it must be a
		// raw 27 or 28.
		plainV = byte(v.Uint64() - 27)
	} else {
		// If the signature is not optionally protected, we assume it
		// must already be equal to the recovery id.
		plainV = byte(v.Uint64())
	}
	if !crypto.ValidateSignatureValues(plainV, r, s, false) {
		return ErrInvalidSig
	}

	return nil
}

func isProtectedV(V *big.Int) bool {
	if V.BitLen() <= 8 {
		v := V.Uint64()
		return v != 27 && v != 28 && v != 1 && v != 0
	}
	// anything not 27 or 28 is considered protected
	return true
}

// Protected says whether the transaction is replay-protected.
func (tx *Transaction) Protected() bool {
	switch tx := tx.inner.(type) {
	case *LegacyTx:
		return tx.V != nil && isProtectedV(tx.V)
	default:
		return true
	}
}

// Type returns the transaction type.
func (tx *Transaction) Type() uint8 {
	return tx.inner.txType()
}

// ChainId returns the EIP155 chain ID of the transaction. The return value will always be
// non-nil. For legacy transactions which are not replay-protected, the return value is
// zero.
// 从交易签名内容V中提取链ID
func (tx *Transaction) ChainId() *big.Int {
	return tx.inner.chainID()
}

// Data returns the input data of the transaction.
func (tx *Transaction) Data() []byte { return tx.inner.data() }

// AccessList returns the access list of the transaction.
func (tx *Transaction) AccessList() AccessList { return tx.inner.accessList() }

// Gas returns the gas limit of the transaction.
func (tx *Transaction) Gas() uint64 { return tx.inner.gas() }

// GasPrice returns the gas price of the transaction.
func (tx *Transaction) GasPrice() *big.Int { return new(big.Int).Set(tx.inner.gasPrice()) }

// GasTipCap returns the gasTipCap per gas of the transaction.
func (tx *Transaction) GasTipCap() *big.Int { return new(big.Int).Set(tx.inner.gasTipCap()) }

// GasFeeCap returns the fee cap per gas of the transaction.
func (tx *Transaction) GasFeeCap() *big.Int { return new(big.Int).Set(tx.inner.gasFeeCap()) }

// Value returns the ether amount of the transaction.
func (tx *Transaction) Value() *big.Int { return new(big.Int).Set(tx.inner.value()) }

// Nonce returns the sender account nonce of the transaction.
func (tx *Transaction) Nonce() uint64 { return tx.inner.nonce() }

// To returns the recipient address of the transaction.
// For contract-creation transactions, To returns nil.
func (tx *Transaction) To() *common.Address {
	return copyAddressPtr(tx.inner.to())
}

// Cost returns gas * gasPrice + value.
func (tx *Transaction) Cost() *big.Int {
	total := new(big.Int).Mul(tx.GasPrice(), new(big.Int).SetUint64(tx.Gas()))
	total.Add(total, tx.Value())
	return total
}

// RawSignatureValues returns the V, R, S signature values of the transaction.
// The return values should not be modified by the caller.
func (tx *Transaction) RawSignatureValues() (v, r, s *big.Int) {
	return tx.inner.rawSignatureValues()
}

// GasPriceCmp compares the gas prices of two transactions.
func (tx *Transaction) GasPriceCmp(other *Transaction) int {
	return tx.inner.gasPrice().Cmp(other.inner.gasPrice())
}

// GasPriceIntCmp compares the gas price of the transaction against the given price.
func (tx *Transaction) GasPriceIntCmp(other *big.Int) int {
	return tx.inner.gasPrice().Cmp(other)
}

// GasFeeCapCmp compares the fee cap of two transactions.
func (tx *Transaction) GasFeeCapCmp(other *Transaction) int {
	return tx.inner.gasFeeCap().Cmp(other.inner.gasFeeCap())
}

// GasFeeCapIntCmp compares the fee cap of the transaction against the given fee cap.
func (tx *Transaction) GasFeeCapIntCmp(other *big.Int) int {
	return tx.inner.gasFeeCap().Cmp(other)
}

// GasTipCapCmp compares the gasTipCap of two transactions.
func (tx *Transaction) GasTipCapCmp(other *Transaction) int {
	return tx.inner.gasTipCap().Cmp(other.inner.gasTipCap())
}

// GasTipCapIntCmp compares the gasTipCap of the transaction against the given gasTipCap.
func (tx *Transaction) GasTipCapIntCmp(other *big.Int) int {
	return tx.inner.gasTipCap().Cmp(other)
}

// EffectiveGasTip returns the effective miner gasTipCap for the given base fee.
// Note: if the effective gasTipCap is negative, this method returns both error
// the actual negative value, _and_ ErrGasFeeCapTooLow
func (tx *Transaction) EffectiveGasTip(baseFee *big.Int) (*big.Int, error) {
	if baseFee == nil {
		return tx.GasTipCap(), nil
	}
	var err error
	gasFeeCap := tx.GasFeeCap()
	if gasFeeCap.Cmp(baseFee) == -1 {
		err = ErrGasFeeCapTooLow
	}
	return math.BigMin(tx.GasTipCap(), gasFeeCap.Sub(gasFeeCap, baseFee)), err
}

// EffectiveGasTipValue is identical to EffectiveGasTip, but does not return an
// error in case the effective gasTipCap is negative
func (tx *Transaction) EffectiveGasTipValue(baseFee *big.Int) *big.Int {
	effectiveTip, _ := tx.EffectiveGasTip(baseFee)
	return effectiveTip
}

// EffectiveGasTipCmp compares the effective gasTipCap of two transactions assuming the given base fee.
func (tx *Transaction) EffectiveGasTipCmp(other *Transaction, baseFee *big.Int) int {
	if baseFee == nil {
		return tx.GasTipCapCmp(other)
	}
	return tx.EffectiveGasTipValue(baseFee).Cmp(other.EffectiveGasTipValue(baseFee))
}

// EffectiveGasTipIntCmp compares the effective gasTipCap of a transaction to the given gasTipCap.
func (tx *Transaction) EffectiveGasTipIntCmp(other *big.Int, baseFee *big.Int) int {
	if baseFee == nil {
		return tx.GasTipCapIntCmp(other)
	}
	return tx.EffectiveGasTipValue(baseFee).Cmp(other)
}

// Hash returns the transaction hash.
// 哈希运算
func (tx *Transaction) Hash() common.Hash {
	// 检查是否已经计算过哈希值，如果已经计算过，则直接返回已经计算过的哈希值
	if hash := tx.hash.Load(); hash != nil {
		return hash.(common.Hash)
	}

	var h common.Hash
	if tx.Type() == LegacyTxType {
		h = rlpHash(tx.inner)
	} else {
		h = prefixedRlpHash(tx.Type(), tx.inner)
	}
	tx.hash.Store(h) // 将计算得到的哈希值存储起来
	return h
}

// Size returns the true RLP encoded storage size of the transaction, either by
// encoding and returning it, or returning a previously cached value.
// 交易大小(size)
// 交易大小是指交易信息进行RLP编码后的数据大小、代表交易网络传输大小、代表交易占区块大小、代表交易存储大小。
// 每笔交易进入交易池都需要检查交易大小是否超过 32KB。
// 推送交易数据给其他节点时也需结合交易大小，在不超过网络消息最大限制(默认10MB)下分包推送数据。为避免重复计算开销，在第一次计算后便缓存。
func (tx *Transaction) Size() common.StorageSize {
	if size := tx.size.Load(); size != nil {
		return size.(common.StorageSize)
	}
	c := writeCounter(0)
	rlp.Encode(&c, &tx.inner) // 交易信息进行RLP编码后的数据大小
	tx.size.Store(common.StorageSize(c))
	return common.StorageSize(c)
}

// WithSignature returns a new transaction with the given signature.
// This signature needs to be in the [R || S || V] format where V is 0 or 1.
func (tx *Transaction) WithSignature(signer Signer, sig []byte) (*Transaction, error) {
	r, s, v, err := signer.SignatureValues(tx, sig)
	if err != nil {
		return nil, err
	}
	cpy := tx.inner.copy()
	cpy.setSignatureValues(signer.ChainID(), v, r, s)
	return &Transaction{inner: cpy, time: tx.time}, nil
}

// Transactions implements DerivableList for transactions.
type Transactions []*Transaction

// Len returns the length of s.
func (s Transactions) Len() int { return len(s) }

// EncodeIndex encodes the i'th transaction to w. Note that this does not check for errors
// because we assume that *Transaction will only ever contain valid txs that were either
// constructed by decoding or via public API in this package.
func (s Transactions) EncodeIndex(i int, w *bytes.Buffer) {
	tx := s[i]
	if tx.Type() == LegacyTxType {
		rlp.Encode(w, tx.inner)
	} else {
		tx.encodeTyped(w)
	}
}

// TxDifference returns a new set which is the difference between a and b.
func TxDifference(a, b Transactions) Transactions {
	keep := make(Transactions, 0, len(a))

	remove := make(map[common.Hash]struct{})
	for _, tx := range b {
		remove[tx.Hash()] = struct{}{}
	}

	for _, tx := range a {
		if _, ok := remove[tx.Hash()]; !ok {
			keep = append(keep, tx)
		}
	}

	return keep
}

// HashDifference returns a new set which is the difference between a and b.
func HashDifference(a, b []common.Hash) []common.Hash {
	keep := make([]common.Hash, 0, len(a))

	remove := make(map[common.Hash]struct{})
	for _, hash := range b {
		remove[hash] = struct{}{}
	}

	for _, hash := range a {
		if _, ok := remove[hash]; !ok {
			keep = append(keep, hash)
		}
	}

	return keep
}

// [Nonce排序规则]👇👇👇👇👇👇👇👇👇👇👇
// 按照交易的 nonce 对交易列表进行排序
// TxByNonce implements the sort interface to allow sorting a list of transactions
// by their nonces. This is usually only useful for sorting transactions from a
// single account, otherwise a nonce comparison doesn't make much sense.
type TxByNonce Transactions

func (s TxByNonce) Len() int { return len(s) }

// 比较两个交易在切片中的顺序。它根据交易的 nonce 值来判断它们的顺序
func (s TxByNonce) Less(i, j int) bool { return s[i].Nonce() < s[j].Nonce() }
func (s TxByNonce) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// TxWithMinerFee wraps a transaction with its gas price or effective miner gasTipCap
type TxWithMinerFee struct {
	tx       *Transaction
	minerFee *big.Int
}

// [Nonce排序规则]👆👆👆👆👆👆👆👆👆👆👆

// NewTxWithMinerFee creates a wrapped transaction, calculating the effective
// miner gasTipCap if a base fee is provided.
// Returns error in case of a negative effective miner gasTipCap.
func NewTxWithMinerFee(tx *Transaction, baseFee *big.Int) (*TxWithMinerFee, error) {
	minerFee, err := tx.EffectiveGasTip(baseFee)
	if err != nil {
		return nil, err
	}
	return &TxWithMinerFee{
		tx:       tx,
		minerFee: minerFee,
	}, nil
}

// [交易排序规则]👇👇👇👇👇👇👇👇👇👇👇👇👇👇👇👇👇👇👇👇👇👇👇
// TxByPriceAndTime implements both the sort and the heap interface, making it useful
// for all at once sorting as well as individually adding and removing elements.
// 实现了排序和堆接口，因此它适用于同时排序以及单独添加和删除元素
type TxByPriceAndTime []*TxWithMinerFee

// 获取切片长度
func (s TxByPriceAndTime) Len() int { return len(s) }

// ❗❗❗❗❗❗比较两个元素在切片中的顺序。❗❗❗❗❗❗
// 首先，它比较两个元素的矿工费用（minerFee）的大小，如果相等，则使用交易的时间戳来决定排序的顺序，以保持确定性。
func (s TxByPriceAndTime) Less(i, j int) bool {
	// If the prices are equal, use the time the transaction was first seen for
	// deterministic sorting
	cmp := s[i].minerFee.Cmp(s[j].minerFee) // ⭐
	if cmp == 0 {
		return s[i].tx.time.Before(s[j].tx.time) // ⭐
	}
	return cmp > 0
}

// 用于交换切片中的两个元素的位置
func (s TxByPriceAndTime) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// 向切片中添加一个元素
func (s *TxByPriceAndTime) Push(x interface{}) {
	*s = append(*s, x.(*TxWithMinerFee))
}

// 切片中删除最后一个元素，并返回删除的元素
func (s *TxByPriceAndTime) Pop() interface{} {
	old := *s
	n := len(old)
	x := old[n-1]
	*s = old[0 : n-1]
	return x
}

// [交易排序规则]👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆👆

// TransactionsByPriceAndNonce represents a set of transactions that can return
// transactions in a profit-maximizing sorted order, while supporting removing
// entire batches of transactions for non-executable accounts.
type TransactionsByPriceAndNonce struct {
	// 为每个账户维护了一个按 nonce 排序的交易列表
	txs map[common.Address]Transactions // Per account nonce-sorted list of transactions
	// 它保存了每个唯一账户（按价格堆排序）的下一个交易。
	heads TxByPriceAndTime // Next transaction for each unique account (price heap)
	// 用于交易的签名
	signer Signer // Signer for the set of transactions
	// 用于表示当前的基础费用
	baseFee *big.Int // Current base fee
}

// NewTransactionsByPriceAndNonce creates a transaction set that can retrieve
// price sorted transactions in a nonce-honouring way.
//
// Note, the input map is reowned so the caller should not interact any more with
// if after providing it to the constructor.
// 创建一个交易集合，该集合可以按照价格排序的方式、遵循 nonce 的规则来检索交易。
func NewTransactionsByPriceAndNonce(signer Signer, txs map[common.Address]Transactions, baseFee *big.Int) *TransactionsByPriceAndNonce {
	// Initialize a price and received time based heap with the head transactions
	// ⭐ 创建一个按Gas和时间排序的堆，并初始化为空堆。
	heads := make(TxByPriceAndTime, 0, len(txs))
	// 遍历输入的交易映射 txs，其中 from 是账户地址，accTxs 是该账户的交易列表。
	for from, accTxs := range txs {
		// 获取第一个交易的发送者地址。
		acc, _ := Sender(signer, accTxs[0])
		// 将第一个交易包装，并添加矿工费用，生成一个新的交易。
		wrapped, err := NewTxWithMinerFee(accTxs[0], baseFee)
		// Remove transaction if sender doesn't match from, or if wrapping fails.
		// 检查包装后的交易的发送者地址是否与账户地址相符，以及包装过程中是否出现错误。
		// 如果不匹配或者出现错误，则将该账户的交易从 txs 中删除，并继续处理下一个账户。
		if acc != from || err != nil {
			delete(txs, from)
			continue
		}
		// 将包装后的交易加入到堆中。
		heads = append(heads, wrapped)
		// 将已处理过的第一个交易从该账户的交易列表中移除。
		txs[from] = accTxs[1:]
	}
	// 对堆进行初始化。
	heap.Init(&heads)

	// Assemble and return the transaction set
	// 将构造好的 TransactionsByPriceAndNonce 结构返回。
	return &TransactionsByPriceAndNonce{
		txs:     txs,
		heads:   heads,
		signer:  signer,
		baseFee: baseFee,
	}
}

// Copy copys a new TransactionsPriceAndNonce with the same *transaction
func (t *TransactionsByPriceAndNonce) Copy() *TransactionsByPriceAndNonce {
	heads := make([]*TxWithMinerFee, len(t.heads))
	copy(heads, t.heads)
	txs := make(map[common.Address]Transactions, len(t.txs))
	for acc, txsTmp := range t.txs {
		txs[acc] = txsTmp
	}
	return &TransactionsByPriceAndNonce{
		heads:  heads,
		txs:    txs,
		signer: t.signer,
	}
}

// Peek returns the next transaction by price.
func (t *TransactionsByPriceAndNonce) Peek() *Transaction {
	if len(t.heads) == 0 {
		return nil
	}
	return t.heads[0].tx
}

// Shift replaces the current best head with the next one from the same account.
func (t *TransactionsByPriceAndNonce) Shift() {
	acc, _ := Sender(t.signer, t.heads[0].tx)
	if txs, ok := t.txs[acc]; ok && len(txs) > 0 {
		if wrapped, err := NewTxWithMinerFee(txs[0], t.baseFee); err == nil {
			t.heads[0], t.txs[acc] = wrapped, txs[1:]
			heap.Fix(&t.heads, 0)
			return
		}
	}
	heap.Pop(&t.heads)
}

// Pop removes the best transaction, *not* replacing it with the next one from
// the same account. This should be used when a transaction cannot be executed
// and hence all subsequent ones should be discarded from the same account.
func (t *TransactionsByPriceAndNonce) Pop() {
	heap.Pop(&t.heads)
}

func (t *TransactionsByPriceAndNonce) CurrentSize() int {
	return len(t.heads)
}

// Forward moves current transaction to be the one which is one index after tx
func (t *TransactionsByPriceAndNonce) Forward(tx *Transaction) {
	if tx == nil {
		if len(t.heads) > 0 {
			t.heads = t.heads[0:0]
		}
		return
	}
	//check whether target tx exists in t.heads
	for _, head := range t.heads {
		if tx == head.tx {
			//shift t to the position one after tx
			txTmp := t.Peek()
			for txTmp != tx {
				t.Shift()
				txTmp = t.Peek()
			}
			t.Shift()
			return
		}
	}
	//get the sender address of tx
	acc, _ := Sender(t.signer, tx)
	//check whether target tx exists in t.txs
	if txs, ok := t.txs[acc]; ok {
		for _, txTmp := range txs {
			//found the same pointer in t.txs as tx and then shift t to the position one after tx
			if txTmp == tx {
				txTmp = t.Peek()
				for txTmp != tx {
					t.Shift()
					txTmp = t.Peek()
				}
				t.Shift()
				return
			}
		}
	}
}

// Message is a fully derived transaction and implements core.Message
//
// NOTE: In a future PR this will be removed.
type Message struct {
	to         *common.Address
	from       common.Address
	nonce      uint64
	amount     *big.Int
	gasLimit   uint64
	gasPrice   *big.Int
	gasFeeCap  *big.Int
	gasTipCap  *big.Int
	data       []byte
	accessList AccessList
	isFake     bool
}

func NewMessage(from common.Address, to *common.Address, nonce uint64, amount *big.Int, gasLimit uint64, gasPrice, gasFeeCap, gasTipCap *big.Int, data []byte, accessList AccessList, isFake bool) Message {
	return Message{
		from:       from,
		to:         to,
		nonce:      nonce,
		amount:     amount,
		gasLimit:   gasLimit,
		gasPrice:   gasPrice,
		gasFeeCap:  gasFeeCap,
		gasTipCap:  gasTipCap,
		data:       data,
		accessList: accessList,
		isFake:     isFake,
	}
}

// AsMessage returns the transaction as a core.Message.
func (tx *Transaction) AsMessage(s Signer, baseFee *big.Int) (Message, error) {
	msg := Message{
		nonce:      tx.Nonce(),
		gasLimit:   tx.Gas(),
		gasPrice:   new(big.Int).Set(tx.GasPrice()),
		gasFeeCap:  new(big.Int).Set(tx.GasFeeCap()),
		gasTipCap:  new(big.Int).Set(tx.GasTipCap()),
		to:         tx.To(),
		amount:     tx.Value(),
		data:       tx.Data(),
		accessList: tx.AccessList(),
		isFake:     false,
	}
	// If baseFee provided, set gasPrice to effectiveGasPrice.
	if baseFee != nil {
		msg.gasPrice = math.BigMin(msg.gasPrice.Add(msg.gasTipCap, baseFee), msg.gasFeeCap)
	}
	var err error
	msg.from, err = Sender(s, tx)
	return msg, err
}

// AsMessageNoNonceCheck returns the transaction with checkNonce field set to be false.
func (tx *Transaction) AsMessageNoNonceCheck(s Signer) (Message, error) {
	msg, err := tx.AsMessage(s, nil)
	if err == nil {
		msg.isFake = true
	}
	return msg, err
}

func (m Message) From() common.Address   { return m.from }
func (m Message) To() *common.Address    { return m.to }
func (m Message) GasPrice() *big.Int     { return m.gasPrice }
func (m Message) GasFeeCap() *big.Int    { return m.gasFeeCap }
func (m Message) GasTipCap() *big.Int    { return m.gasTipCap }
func (m Message) Value() *big.Int        { return m.amount }
func (m Message) Gas() uint64            { return m.gasLimit }
func (m Message) Nonce() uint64          { return m.nonce }
func (m Message) Data() []byte           { return m.data }
func (m Message) AccessList() AccessList { return m.accessList }
func (m Message) IsFake() bool           { return m.isFake }

// copyAddressPtr copies an address.
func copyAddressPtr(a *common.Address) *common.Address {
	if a == nil {
		return nil
	}
	cpy := *a
	return &cpy
}
