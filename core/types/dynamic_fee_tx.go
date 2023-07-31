// Copyright 2021 The go-ethereum Authors
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
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// DynamicFeeTx 相较于 LegacyTx 具有以下优势：
// 1. EIP-1559支持： DynamicFeeTx 是基于以太坊 EIP-1559 提案的新型交易类型。EIP-1559 提案旨在改进以太坊的交易费用机制，使燃料费用更加稳定和可预测。与传统的基于燃料价格的拍卖机制不同，EIP-1559 引入了基础燃料费用（Base Fee）和优先级燃料费用（Priority Fee）的概念，使交易费用更加合理和公平。
// 2. 动态燃料费用： DynamicFeeTx 允许发送方在交易中指定优先级燃料费用和最大燃料费用，从而更好地管理交易费用和优先级。矿工会根据发送方指定的最大燃料费用来决定是否打包交易，从而提高了矿工在交易费用市场中的收益。
// 3. 更好的交易优先级： DynamicFeeTx 的优先级由发送方通过设置优先级燃料费用来指定，而不再依赖于燃料价格的拍卖。这意味着发送方可以更精确地控制交易的优先级，从而更快地得到确认。
// 4. 更稳定的区块燃料限制： EIP-1559 引入的基础燃料费用机制有助于保持每个区块的平均燃料使用量接近区块的燃料限制。这样可以减少区块拥堵，提高整体网络的吞吐量和稳定性。
// 5. 更简化的交易费用计算： DynamicFeeTx 中的燃料费用计算更简单直观，只需指定优先级燃料费用和最大燃料费用即可，不再需要进行复杂的燃料价格拍卖。
// 总体而言，DynamicFeeTx 基于 EIP-1559 提案改进了以太坊交易的费用机制，使交易费用更加合理、稳定和可预测，同时提供更好的交易优先级控制，从而为用户和矿工带来更好的交易体验和经济激励。

// 以太坊中的动态手续费交易
// DynamicFeeTx 结构体包含了动态手续费交易的所有信息，这些信息用于发送和验证以太坊网络中的交易，并确保交易的有效性和安全性。
// 动态手续费交易是以太坊 EIP-1559 提案引入的新类型交易，其中燃料费用由发送方在交易中指定，以更好地管理网络拥堵和燃料费用市场。

type DynamicFeeTx struct {
	ChainID    *big.Int        // 交易所属的链ID
	Nonce      uint64          // 发送方账户的交易计数器，用于标识账户的交易顺序
	GasTipCap  *big.Int        // a.k.a. maxPriorityFeePerGas 交易发送方愿意支付的最高优先燃料费用（以太），用于提高交易的优先级，也称为 maxPriorityFeePerGas
	GasFeeCap  *big.Int        // a.k.a. maxFeePerGas 交易发送方愿意支付的最高燃料费用（以太），用于限制交易费用，也称为 maxFeePerGas。
	Gas        uint64          // 交易所需的Gas数量
	To         *common.Address `rlp:"nil"` // nil means contract creation 交易的接收地址，如果为 nil，则表示合约创建交易，否则为普通转账交易。
	Value      *big.Int        // 交易转移的以太币数量。
	Data       []byte          // 交易的数据字段，用于存储智能合约的调用数据。
	AccessList AccessList      // 交易的访问列表，指定了交易涉及的账户和存储槽。

	// Signature values
	V *big.Int `json:"v" gencodec:"required"`
	R *big.Int `json:"r" gencodec:"required"`
	S *big.Int `json:"s" gencodec:"required"`
}

// copy creates a deep copy of the transaction data and initializes all fields.
func (tx *DynamicFeeTx) copy() TxData {
	cpy := &DynamicFeeTx{
		Nonce: tx.Nonce,
		To:    copyAddressPtr(tx.To),
		Data:  common.CopyBytes(tx.Data),
		Gas:   tx.Gas,
		// These are copied below.
		AccessList: make(AccessList, len(tx.AccessList)),
		Value:      new(big.Int),
		ChainID:    new(big.Int),
		GasTipCap:  new(big.Int),
		GasFeeCap:  new(big.Int),
		V:          new(big.Int),
		R:          new(big.Int),
		S:          new(big.Int),
	}
	copy(cpy.AccessList, tx.AccessList)
	if tx.Value != nil {
		cpy.Value.Set(tx.Value)
	}
	if tx.ChainID != nil {
		cpy.ChainID.Set(tx.ChainID)
	}
	if tx.GasTipCap != nil {
		cpy.GasTipCap.Set(tx.GasTipCap)
	}
	if tx.GasFeeCap != nil {
		cpy.GasFeeCap.Set(tx.GasFeeCap)
	}
	if tx.V != nil {
		cpy.V.Set(tx.V)
	}
	if tx.R != nil {
		cpy.R.Set(tx.R)
	}
	if tx.S != nil {
		cpy.S.Set(tx.S)
	}
	return cpy
}

// accessors for innerTx.
func (tx *DynamicFeeTx) txType() byte           { return DynamicFeeTxType }
func (tx *DynamicFeeTx) chainID() *big.Int      { return tx.ChainID }
func (tx *DynamicFeeTx) accessList() AccessList { return tx.AccessList }
func (tx *DynamicFeeTx) data() []byte           { return tx.Data }
func (tx *DynamicFeeTx) gas() uint64            { return tx.Gas }
func (tx *DynamicFeeTx) gasFeeCap() *big.Int    { return tx.GasFeeCap }
func (tx *DynamicFeeTx) gasTipCap() *big.Int    { return tx.GasTipCap }
func (tx *DynamicFeeTx) gasPrice() *big.Int     { return tx.GasFeeCap }
func (tx *DynamicFeeTx) value() *big.Int        { return tx.Value }
func (tx *DynamicFeeTx) nonce() uint64          { return tx.Nonce }
func (tx *DynamicFeeTx) to() *common.Address    { return tx.To }

func (tx *DynamicFeeTx) rawSignatureValues() (v, r, s *big.Int) {
	return tx.V, tx.R, tx.S
}

func (tx *DynamicFeeTx) setSignatureValues(chainID, v, r, s *big.Int) {
	tx.ChainID, tx.V, tx.R, tx.S = chainID, v, r, s
}
