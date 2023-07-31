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

//go:generate go run ../../rlp/rlpgen -type StateAccount -out gen_account_rlp.go

// StateAccount is the Ethereum consensus representation of accounts.
// These objects are stored in the main account trie.

// @see https://learnblockchain.cn/books/geth/part1/account.html
// ![](https://p.ipic.vip/975jpf.jpg)

// 外部账户无内部存储数据和合约代码，因此外部账户数据中 StateRootHash 和 CodeHash 是一个空默认值。
// 在程序逻辑上，存在code则为合约账户。 即 CodeHash 为空值时，账户是一个外部账户，否则是合约账户。
// 账户内部实际只存储关键数据，而合约代码以及合约自身数据则通过对应的哈希值关联
type StateAccount struct {
	Nonce    uint64      // 账户的交易计数,用于防止重放攻击
	Balance  *big.Int    // 账户的以太币余额
	Root     common.Hash // 账户存储的存储树(storage trie)的默克尔根 merkle root of the storage trie
	CodeHash []byte      // 账户的代码哈希
}
