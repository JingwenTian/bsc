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

// Package eth implements the Ethereum protocol.
package eth

import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/bloombits"
	"github.com/ethereum/go-ethereum/core/monitor"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state/pruner"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/core/vote"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/eth/protocols/bsc"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/eth/protocols/trust"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/internal/shutdowncheck"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/dnsdisc"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

// Config contains the configuration options of the ETH protocol.
// Deprecated: use ethconfig.Config instead.
type Config = ethconfig.Config

// Ethereum implements the Ethereum full node service.
type Ethereum struct {
	config *ethconfig.Config

	// Handlers
	txPool              *core.TxPool
	blockchain          *core.BlockChain
	handler             *handler
	ethDialCandidates   enode.Iterator
	snapDialCandidates  enode.Iterator
	trustDialCandidates enode.Iterator
	bscDialCandidates   enode.Iterator
	merger              *consensus.Merger

	// DB interfaces
	chainDb ethdb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	bloomRequests     chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer      *core.ChainIndexer             // Bloom indexer operating during block imports
	closeBloomHandler chan struct{}

	APIBackend *EthAPIBackend

	miner     *miner.Miner
	gasPrice  *big.Int
	etherbase common.Address

	networkID     uint64
	netRPCService *ethapi.PublicNetAPI

	p2pServer *p2p.Server

	lock sync.RWMutex // Protects the variadic fields (e.g. gas price and etherbase)

	shutdownTracker *shutdowncheck.ShutdownTracker // Tracks if and when the node has shutdown ungracefully

	votePool *vote.VotePool
}

// 以太坊后端（EthAPI Backend）服务是以太坊客户端的核心组件之一，负责处理和提供与以太坊区块链网络的交互。它扮演着连接以太坊区块链的桥梁，向外部应用程序（如区块浏览器、DApp、智能合约等）提供了一组API，使这些应用程序能够与以太坊网络进行交互和通信。
// 以太坊后端服务的主要作用如下：
// 1. 数据查询与检索: 以太坊后端提供了一系列的API，允许应用程序查询区块链的状态、账户余额、交易记录、智能合约信息等。应用程序可以通过这些API获取区块链上的实时数据。
// 2. 交易处理与广播: 应用程序可以使用以太坊后端的API创建和发送交易到区块链网络。这包括向其他账户转账、调用智能合约函数等。后端负责将交易广播到网络上，并在交易被打包进区块时返回交易的结果。
// 3. 智能合约交互: 以太坊后端允许应用程序与已部署的智能合约进行交互。通过提供合约地址和ABI（应用程序二进制接口），应用程序可以调用合约的函数和读取状态。
// 4. 事件监听与订阅: 后端提供了订阅机制，允许应用程序订阅区块、交易、日志等事件。当这些事件发生时，后端会主动通知应用程序，使应用程序能够实时响应。
// 5. Gas价格管理: 以太坊后端提供了有关当前网络上的燃气价格信息，应用程序可以根据这些信息设置交易的燃气价格，以确保交易能够被尽快打包进区块。
// 6. 账户管理与加密: 后端支持创建、管理和解锁以太坊账户。它还负责将私钥存储在安全的环境中，并对交易进行签名。
// 7. 网络信息获取: 以太坊后端可以提供与以太坊网络有关的信息，如节点数量、协议版本等。
// 总之，以太坊后端服务在以太坊生态系统中扮演着重要的角色，它使应用程序能够与区块链网络交互，执行交易、查询数据、与智能合约交互等，为构建去中心化应用和服务提供了强大的支持。

// New creates a new Ethereum object (including the
// initialisation of the common Ethereum object)
// 🆕 创建一个新的 Ethereum 对象（包括初始化共同的 Ethereum 对象）
// 负责创建以太坊客户端的核心对象，并初始化其所需的各个组件，以便节点能够加入以太坊网络并执行相应的任务。
func New(stack *node.Node, config *ethconfig.Config) (*Ethereum, error) {
	// 👇👇👇👇 校验配置值的合理性和兼容性
	// Ensure configuration values are compatible and sane
	if config.SyncMode == downloader.LightSync {
		return nil, errors.New("can't run eth.Ethereum in light sync mode, use les.LightEthereum")
	}
	if !config.SyncMode.IsValid() {
		return nil, fmt.Errorf("invalid sync mode %d", config.SyncMode)
	}
	if !config.TriesVerifyMode.IsValid() {
		return nil, fmt.Errorf("invalid tries verify mode %d", config.TriesVerifyMode)
	}
	if config.Miner.GasPrice == nil || config.Miner.GasPrice.Cmp(common.Big0) <= 0 {
		log.Warn("Sanitizing invalid miner gas price", "provided", config.Miner.GasPrice, "updated", ethconfig.Defaults.Miner.GasPrice)
		config.Miner.GasPrice = new(big.Int).Set(ethconfig.Defaults.Miner.GasPrice)
	}
	if config.NoPruning && config.TrieDirtyCache > 0 {
		if config.SnapshotCache > 0 {
			config.TrieCleanCache += config.TrieDirtyCache * 3 / 5
			config.SnapshotCache += config.TrieDirtyCache * 2 / 5
		} else {
			config.TrieCleanCache += config.TrieDirtyCache
		}
		config.TrieDirtyCache = 0
	}
	log.Info("Allocated trie memory caches", "clean", common.StorageSize(config.TrieCleanCache)*1024*1024, "dirty", common.StorageSize(config.TrieDirtyCache)*1024*1024)

	// Transfer mining-related config to the ethash config.
	ethashConfig := config.Ethash
	ethashConfig.NotifyFull = config.Miner.NotifyFull

	// Assemble the Ethereum object
	// 👇👇👇👇 打开并合并区块链数据库
	chainDb, err := stack.OpenAndMergeDatabase("chaindata", config.DatabaseCache, config.DatabaseHandles,
		config.DatabaseFreezer, config.DatabaseDiff, "eth/db/chaindata/", false, config.PersistDiff, config.PruneAncientData)
	if err != nil {
		return nil, err
	}
	chainConfig, genesisHash, genesisErr := core.SetupGenesisBlockWithOverride(chainDb, config.Genesis, config.OverrideBerlin, config.OverrideArrowGlacier, config.OverrideTerminalTotalDifficulty)
	if _, ok := genesisErr.(*params.ConfigCompatError); genesisErr != nil && !ok {
		return nil, genesisErr
	}
	log.Info("Initialised chain configuration", "config", chainConfig)

	if err := pruner.RecoverPruning(stack.ResolvePath(""), chainDb, stack.ResolvePath(config.TrieCleanCacheJournal), config.TriesInMemory); err != nil {
		log.Error("Failed to recover state", "error", err)
	}
	merger := consensus.NewMerger(chainDb)

	// 👇👇👇👇 组装 Ethereum 对象，设置各种配置和属性。
	eth := &Ethereum{
		config:            config,
		merger:            merger,
		chainDb:           chainDb,
		eventMux:          stack.EventMux(),
		accountManager:    stack.AccountManager(),
		closeBloomHandler: make(chan struct{}),
		networkID:         config.NetworkId,
		gasPrice:          config.Miner.GasPrice,
		etherbase:         config.Miner.Etherbase,
		bloomRequests:     make(chan chan *bloombits.Retrieval),
		bloomIndexer:      core.NewBloomIndexer(chainDb, params.BloomBitsBlocks, params.BloomConfirms),
		p2pServer:         stack.Server(),
		shutdownTracker:   shutdowncheck.NewShutdownTracker(chainDb),
	}

	// 👇👇👇👇 创建 API 后端，用于处理 RPC 请求。
	eth.APIBackend = &EthAPIBackend{stack.Config().ExtRPCEnabled(), stack.Config().AllowUnprotectedTxs, eth, nil}
	if eth.APIBackend.allowUnprotectedTxs {
		log.Info("Unprotected transactions allowed")
	}
	ethAPI := ethapi.NewPublicBlockChainAPI(eth.APIBackend)

	// 👇👇👇👇 创建以太坊引擎，该引擎实现共识算法和区块验证逻辑。
	eth.engine = ethconfig.CreateConsensusEngine(stack, chainConfig, &ethashConfig, config.Miner.Notify, config.Miner.Noverify, chainDb, ethAPI, genesisHash)

	bcVersion := rawdb.ReadDatabaseVersion(chainDb)
	var dbVer = "<nil>"
	if bcVersion != nil {
		dbVer = fmt.Sprintf("%d", *bcVersion)
	}
	log.Info("Initialising Ethereum protocol", "network", config.NetworkId, "dbversion", dbVer)

	if !config.SkipBcVersionCheck {
		if bcVersion != nil && *bcVersion > core.BlockChainVersion {
			return nil, fmt.Errorf("database version is v%d, Geth %s only supports v%d", *bcVersion, params.VersionWithMeta, core.BlockChainVersion)
		} else if bcVersion == nil || *bcVersion < core.BlockChainVersion {
			if bcVersion != nil { // only print warning on upgrade, not on init
				log.Warn("Upgrade blockchain database version", "from", dbVer, "to", core.BlockChainVersion)
			}
			rawdb.WriteDatabaseVersion(chainDb, core.BlockChainVersion)
		}
	}
	var (
		vmConfig = vm.Config{
			EnablePreimageRecording: config.EnablePreimageRecording,
		}
		cacheConfig = &core.CacheConfig{
			TrieCleanLimit:     config.TrieCleanCache,
			TrieCleanJournal:   stack.ResolvePath(config.TrieCleanCacheJournal),
			TrieCleanRejournal: config.TrieCleanCacheRejournal,
			TrieDirtyLimit:     config.TrieDirtyCache,
			TrieDirtyDisabled:  config.NoPruning,
			TrieTimeLimit:      config.TrieTimeout,
			NoTries:            config.TriesVerifyMode != core.LocalVerify,
			SnapshotLimit:      config.SnapshotCache,
			TriesInMemory:      config.TriesInMemory,
			Preimages:          config.Preimages,
		}
	)
	bcOps := make([]core.BlockChainOption, 0)
	if config.DiffSync && !config.PipeCommit && config.TriesVerifyMode == core.LocalVerify {
		bcOps = append(bcOps, core.EnableLightProcessor)
	}
	if config.PipeCommit {
		bcOps = append(bcOps, core.EnablePipelineCommit)
	}
	if config.PersistDiff {
		bcOps = append(bcOps, core.EnablePersistDiff(config.DiffBlock))
	}
	if stack.Config().EnableDoubleSignMonitor {
		bcOps = append(bcOps, core.EnableDoubleSignChecker)
	}

	peers := newPeerSet()
	bcOps = append(bcOps, core.EnableBlockValidator(chainConfig, eth.engine, config.TriesVerifyMode, peers))

	// 👇👇👇👇 创建区块链实例，用于管理区块和状态。
	eth.blockchain, err = core.NewBlockChain(chainDb, cacheConfig, chainConfig, eth.engine, vmConfig, eth.shouldPreserve, &config.TxLookupLimit, bcOps...)
	if err != nil {
		return nil, err
	}
	// Rewind the chain in case of an incompatible config upgrade.
	if compat, ok := genesisErr.(*params.ConfigCompatError); ok {
		log.Warn("Rewinding chain to upgrade configuration", "err", compat)
		eth.blockchain.SetHead(compat.RewindTo)
		rawdb.WriteChainConfig(chainDb, genesisHash, chainConfig)
	}
	eth.bloomIndexer.Start(eth.blockchain)

	if config.TxPool.Journal != "" {
		config.TxPool.Journal = stack.ResolvePath(config.TxPool.Journal)
	}
	eth.txPool = core.NewTxPool(config.TxPool, chainConfig, eth.blockchain)

	// Permit the downloader to use the trie cache allowance during fast sync
	cacheLimit := cacheConfig.TrieCleanLimit + cacheConfig.TrieDirtyLimit + cacheConfig.SnapshotLimit
	checkpoint := config.Checkpoint
	if checkpoint == nil {
		checkpoint = params.TrustedCheckpoints[genesisHash]
	}

	if eth.handler, err = newHandler(&handlerConfig{
		Database:               chainDb,
		Chain:                  eth.blockchain,
		TxPool:                 eth.txPool,
		Merger:                 merger,
		Network:                config.NetworkId,
		Sync:                   config.SyncMode,
		BloomCache:             uint64(cacheLimit),
		EventMux:               eth.eventMux,
		Checkpoint:             checkpoint,
		Whitelist:              config.Whitelist,
		DirectBroadcast:        config.DirectBroadcast,
		DiffSync:               config.DiffSync,
		DisablePeerTxBroadcast: config.DisablePeerTxBroadcast,
		PeerSet:                peers,
	}); err != nil {
		return nil, err
	}

	// 👇👇👇👇 🚀 创建 Miner 实例，用于挖矿操作, 只需等待命令开启挖矿
	eth.miner = miner.New(eth, &config.Miner, chainConfig, eth.EventMux(), eth.engine, eth.isLocalBlock)
	eth.miner.SetExtra(makeExtraData(config.Miner.ExtraData))

	// Create voteManager instance
	// 👇👇👇👇 创建 voteManager 实例，用于 PoSA 共识的投票管理
	if posa, ok := eth.engine.(consensus.PoSA); ok {
		// Create votePool instance
		votePool := vote.NewVotePool(chainConfig, eth.blockchain, posa)
		eth.votePool = votePool
		if parlia, ok := eth.engine.(*parlia.Parlia); ok {
			parlia.VotePool = votePool
		} else {
			return nil, fmt.Errorf("Engine is not Parlia type")
		}
		log.Info("Create votePool successfully")
		eth.handler.votepool = votePool
		if stack.Config().EnableMaliciousVoteMonitor {
			eth.handler.maliciousVoteMonitor = monitor.NewMaliciousVoteMonitor()
			log.Info("Create MaliciousVoteMonitor successfully")
		}

		if config.Miner.VoteEnable {
			conf := stack.Config()
			blsPasswordPath := stack.ResolvePath(conf.BLSPasswordFile)
			blsWalletPath := stack.ResolvePath(conf.BLSWalletDir)
			voteJournalPath := stack.ResolvePath(conf.VoteJournalDir)
			if _, err := vote.NewVoteManager(eth, chainConfig, eth.blockchain, votePool, voteJournalPath, blsPasswordPath, blsWalletPath, posa); err != nil {
				log.Error("Failed to Initialize voteManager", "err", err)
				return nil, err
			}
			log.Info("Create voteManager successfully")
		}
	}

	// 👇👇👇👇 创建 Gas Price Oracle（GPO）实例，用于计算合理的矿工费
	gpoParams := config.GPO
	if gpoParams.Default == nil {
		gpoParams.Default = config.Miner.GasPrice
	}
	eth.APIBackend.gpo = gasprice.NewOracle(eth.APIBackend, gpoParams)

	// Setup DNS discovery iterators.
	// 👇👇👇👇 设置 DNS 发现迭代器，用于获取节点的地址。
	dnsclient := dnsdisc.NewClient(dnsdisc.Config{})
	eth.ethDialCandidates, err = dnsclient.NewIterator(eth.config.EthDiscoveryURLs...)
	if err != nil {
		return nil, err
	}
	eth.snapDialCandidates, err = dnsclient.NewIterator(eth.config.SnapDiscoveryURLs...)
	if err != nil {
		return nil, err
	}
	eth.trustDialCandidates, err = dnsclient.NewIterator(eth.config.TrustDiscoveryURLs...)
	if err != nil {
		return nil, err
	}
	eth.bscDialCandidates, err = dnsclient.NewIterator(eth.config.BscDiscoveryURLs...)
	if err != nil {
		return nil, err
	}

	// Start the RPC service
	// 👇👇👇👇 启动 RPC 服务，允许客户端通过 RPC 接口与节点交互
	eth.netRPCService = ethapi.NewPublicNetAPI(eth.p2pServer, config.NetworkId)

	// Register the backend on the node
	// 👇👇👇👇 在节点上注册 API、协议和生命周期。
	stack.RegisterAPIs(eth.APIs())
	stack.RegisterProtocols(eth.Protocols())
	stack.RegisterLifecycle(eth)

	// Successful startup; push a marker and check previous unclean shutdowns.
	// 👇👇👇👇 标记成功启动，并检查之前是否有非正常关闭的情况
	eth.shutdownTracker.MarkStartup()

	return eth, nil
}

func makeExtraData(extra []byte) []byte {
	if len(extra) == 0 {
		// create default extradata
		extra, _ = rlp.EncodeToBytes([]interface{}{
			uint(params.VersionMajor<<16 | params.VersionMinor<<8 | params.VersionPatch),
			"geth",
			runtime.Version(),
			runtime.GOOS,
		})
	}
	if uint64(len(extra)) > params.MaximumExtraDataSize-params.ForkIDSize {
		log.Warn("Miner extra data exceed limit", "extra", hexutil.Bytes(extra), "limit", params.MaximumExtraDataSize-params.ForkIDSize)
		extra = nil
	}
	return extra
}

// APIs return the collection of RPC services the ethereum package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *Ethereum) APIs() []rpc.API {
	apis := ethapi.GetAPIs(s.APIBackend)

	// Append any APIs exposed explicitly by the consensus engine
	apis = append(apis, s.engine.APIs(s.BlockChain())...)

	// Append all the local APIs and return
	return append(apis, []rpc.API{
		{
			Namespace: "eth",
			Version:   "1.0",
			Service:   NewPublicEthereumAPI(s),
			Public:    true,
		}, {
			Namespace: "eth",
			Version:   "1.0",
			Service:   NewPublicMinerAPI(s),
			Public:    true,
		}, {
			Namespace: "eth",
			Version:   "1.0",
			Service:   downloader.NewPublicDownloaderAPI(s.handler.downloader, s.eventMux),
			Public:    true,
		}, {
			Namespace: "miner",
			Version:   "1.0",
			Service:   NewPrivateMinerAPI(s),
			Public:    false,
		}, {
			Namespace: "eth",
			Version:   "1.0",
			Service:   filters.NewPublicFilterAPI(s.APIBackend, false, 5*time.Minute, s.config.RangeLimit),
			Public:    true,
		}, {
			Namespace: "admin",
			Version:   "1.0",
			Service:   NewPrivateAdminAPI(s),
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPublicDebugAPI(s),
			Public:    true,
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPrivateDebugAPI(s),
		}, {
			Namespace: "net",
			Version:   "1.0",
			Service:   s.netRPCService,
			Public:    true,
		},
	}...)
}

func (s *Ethereum) ResetWithGenesisBlock(gb *types.Block) {
	s.blockchain.ResetWithGenesisBlock(gb)
}

func (s *Ethereum) Etherbase() (eb common.Address, err error) {
	s.lock.RLock()
	etherbase := s.etherbase
	s.lock.RUnlock()

	if etherbase != (common.Address{}) {
		return etherbase, nil
	}
	if wallets := s.AccountManager().Wallets(); len(wallets) > 0 {
		if accounts := wallets[0].Accounts(); len(accounts) > 0 {
			etherbase := accounts[0].Address

			s.lock.Lock()
			s.etherbase = etherbase
			s.lock.Unlock()

			log.Info("Etherbase automatically configured", "address", etherbase)
			return etherbase, nil
		}
	}
	return common.Address{}, fmt.Errorf("etherbase must be explicitly specified")
}

// isLocalBlock checks whether the specified block is mined
// by local miner accounts.
//
// We regard two types of accounts as local miner account: etherbase
// and accounts specified via `txpool.locals` flag.
func (s *Ethereum) isLocalBlock(header *types.Header) bool {
	author, err := s.engine.Author(header)
	if err != nil {
		log.Warn("Failed to retrieve block author", "number", header.Number.Uint64(), "hash", header.Hash(), "err", err)
		return false
	}
	// Check whether the given address is etherbase.
	s.lock.RLock()
	etherbase := s.etherbase
	s.lock.RUnlock()
	if author == etherbase {
		return true
	}
	// Check whether the given address is specified by `txpool.local`
	// CLI flag.
	for _, account := range s.config.TxPool.Locals {
		if account == author {
			return true
		}
	}
	return false
}

// shouldPreserve checks whether we should preserve the given block
// during the chain reorg depending on whether the author of block
// is a local account.
func (s *Ethereum) shouldPreserve(header *types.Header) bool {
	// The reason we need to disable the self-reorg preserving for clique
	// is it can be probable to introduce a deadlock.
	//
	// e.g. If there are 7 available signers
	//
	// r1   A
	// r2     B
	// r3       C
	// r4         D
	// r5   A      [X] F G
	// r6    [X]
	//
	// In the round5, the inturn signer E is offline, so the worst case
	// is A, F and G sign the block of round5 and reject the block of opponents
	// and in the round6, the last available signer B is offline, the whole
	// network is stuck.
	if _, ok := s.engine.(*clique.Clique); ok {
		return false
	}
	if _, ok := s.engine.(*parlia.Parlia); ok {
		return false
	}
	return s.isLocalBlock(header)
}

// SetEtherbase sets the mining reward address.
func (s *Ethereum) SetEtherbase(etherbase common.Address) {
	s.lock.Lock()
	s.etherbase = etherbase
	s.lock.Unlock()

	s.miner.SetEtherbase(etherbase)
}

// StartMining starts the miner with the given number of CPU threads. If mining
// is already running, this method adjust the number of threads allowed to use
// and updates the minimum price required by the transaction pool.
// 👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️👷‍♂️ 启动挖矿
func (s *Ethereum) StartMining(threads int) error {
	// Update the thread count within the consensus engine
	// 首先看挖矿的共识引擎是否支持设置线程数，如果支持，将更新此共识引擎参数
	type threaded interface {
		SetThreads(threads int)
	}
	if th, ok := s.engine.(threaded); ok {
		log.Info("Updated mining threads", "threads", threads)
		if threads == 0 {
			threads = -1 // Disable the miner from within
		}
		th.SetThreads(threads)
	}
	// If the miner was not running, initialize it
	if !s.IsMining() {
		// 在启动前，需要确定两项配置：交易GasPrice下限，和挖矿奖励接收账户（矿工账户地址）。
		// Propagate the initial price point to the transaction pool
		s.lock.RLock()
		price := s.gasPrice
		s.lock.RUnlock()
		s.txPool.SetGasPrice(price)

		// Configure the local mining address
		eb, err := s.Etherbase()
		if err != nil {
			log.Error("Cannot start mining without etherbase", "err", err)
			return fmt.Errorf("etherbase missing: %v", err)
		}
		var cli *clique.Clique
		if c, ok := s.engine.(*clique.Clique); ok {
			cli = c
		} else if cl, ok := s.engine.(*beacon.Beacon); ok {
			if c, ok := cl.InnerEngine().(*clique.Clique); ok {
				cli = c
			}
		}
		if cli != nil {
			// 对于 clique.Clique 共识引擎（PoA 权限共识），进行了特殊处理，需要从钱包中查找挖矿账户
			wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
			if wallet == nil || err != nil {
				log.Error("Etherbase account unavailable locally", "err", err)
				return fmt.Errorf("signer missing: %v", err)
			}
			// 在进行挖矿时不再是进行PoW计算，而是使用认可的账户进行区块签名
			cli.Authorize(eb, wallet.SignData)
		}
		// 对于Parlia 共识引擎，授权 Etherbase 帐户
		if parlia, ok := s.engine.(*parlia.Parlia); ok {
			wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
			if wallet == nil || err != nil {
				log.Error("Etherbase account unavailable locally", "err", err)
				return fmt.Errorf("signer missing: %v", err)
			}

			parlia.Authorize(eb, wallet.SignData, wallet.SignTx)
		}
		// If mining is started, we can disable the transaction rejection mechanism
		// introduced to speed sync times.
		// 在挖矿前将允许接收网络交易
		atomic.StoreUint32(&s.handler.acceptTxs, 1)
		// 开始在挖矿账户下开启挖矿
		go s.miner.Start(eb)
	}
	return nil
}

// StopMining terminates the miner, both at the consensus engine level as well as
// at the block creation level.
func (s *Ethereum) StopMining() {
	// Update the thread count within the consensus engine
	type threaded interface {
		SetThreads(threads int)
	}
	if th, ok := s.engine.(threaded); ok {
		th.SetThreads(-1)
	}
	// Stop the block creating itself
	s.miner.Stop()
}

func (s *Ethereum) IsMining() bool      { return s.miner.Mining() }
func (s *Ethereum) Miner() *miner.Miner { return s.miner }

func (s *Ethereum) AccountManager() *accounts.Manager  { return s.accountManager }
func (s *Ethereum) BlockChain() *core.BlockChain       { return s.blockchain }
func (s *Ethereum) TxPool() *core.TxPool               { return s.txPool }
func (s *Ethereum) VotePool() *vote.VotePool           { return s.votePool }
func (s *Ethereum) EventMux() *event.TypeMux           { return s.eventMux }
func (s *Ethereum) Engine() consensus.Engine           { return s.engine }
func (s *Ethereum) ChainDb() ethdb.Database            { return s.chainDb }
func (s *Ethereum) IsListening() bool                  { return true } // Always listening
func (s *Ethereum) Downloader() *downloader.Downloader { return s.handler.downloader }
func (s *Ethereum) Synced() bool                       { return atomic.LoadUint32(&s.handler.acceptTxs) == 1 }
func (s *Ethereum) SetSynced()                         { atomic.StoreUint32(&s.handler.acceptTxs, 1) }
func (s *Ethereum) ArchiveMode() bool                  { return s.config.NoPruning }
func (s *Ethereum) BloomIndexer() *core.ChainIndexer   { return s.bloomIndexer }
func (s *Ethereum) Merger() *consensus.Merger          { return s.merger }
func (s *Ethereum) SyncMode() downloader.SyncMode {
	mode, _ := s.handler.chainSync.modeAndLocalHead()
	return mode
}

// Protocols returns all the currently configured
// network protocols to start.
func (s *Ethereum) Protocols() []p2p.Protocol {
	protos := eth.MakeProtocols((*ethHandler)(s.handler), s.networkID, s.ethDialCandidates)
	if !s.config.DisableSnapProtocol && s.config.SnapshotCache > 0 {
		protos = append(protos, snap.MakeProtocols((*snapHandler)(s.handler), s.snapDialCandidates)...)
	}
	// diff protocol can still open without snap protocol
	// if !s.config.DisableDiffProtocol {
	// 	protos = append(protos, diff.MakeProtocols((*diffHandler)(s.handler), s.snapDialCandidates)...)
	// }
	if s.config.EnableTrustProtocol {
		protos = append(protos, trust.MakeProtocols((*trustHandler)(s.handler), s.snapDialCandidates)...)
	}
	if !s.config.DisableBscProtocol {
		protos = append(protos, bsc.MakeProtocols((*bscHandler)(s.handler), s.bscDialCandidates)...)
	}
	return protos
}

// Start implements node.Lifecycle, starting all internal goroutines needed by the
// Ethereum protocol implementation.
// Start 实现了 node.Lifecycle 接口，启动以太坊协议实现所需的所有内部 goroutine。
func (s *Ethereum) Start() error {
	// 启动 ENR 过滤器和更新器，用于管理节点记录（ENR）
	eth.StartENRFilter(s.blockchain, s.p2pServer)
	eth.StartENRUpdater(s.blockchain, s.p2pServer.LocalNode())

	// Start the bloom bits servicing goroutines
	// 启动布隆过滤器位服务的 goroutine，用于处理布隆过滤器的位
	s.startBloomHandlers(params.BloomBitsBlocks)

	// Regularly update shutdown marker
	// 定期更新关闭标记，以确保在关闭节点时的状态更新
	s.shutdownTracker.Start()

	// Figure out a max peers count based on the server limits
	// 计算可用的最大节点数，考虑是否启用了轻节点服务
	maxPeers := s.p2pServer.MaxPeers
	// 如果启用了轻节点服务，确保轻节点数不超过总节点数，并调整最大节点数。
	if s.config.LightServ > 0 {
		if s.config.LightPeers >= s.p2pServer.MaxPeers {
			return fmt.Errorf("invalid peer config: light peer count (%d) >= total peer count (%d)", s.config.LightPeers, s.p2pServer.MaxPeers)
		}
		maxPeers -= s.config.LightPeers
	}
	// Start the networking layer and the light server if requested
	// 🌻🌻🌻🌻🌻🌻 启动网络层和轻节点服务器的 goroutine，开始处理节点之间的通信和数据同步。
	s.handler.Start(maxPeers, s.p2pServer.MaxPeersPerIP)
	return nil
}

// Stop implements node.Lifecycle, terminating all internal goroutines used by the
// Ethereum protocol.
func (s *Ethereum) Stop() error {
	// Stop all the peer-related stuff first.
	s.ethDialCandidates.Close()
	s.snapDialCandidates.Close()
	s.trustDialCandidates.Close()
	s.bscDialCandidates.Close()
	s.handler.Stop()

	// Then stop everything else.
	s.bloomIndexer.Close()
	close(s.closeBloomHandler)
	s.txPool.Stop()
	s.miner.Close()
	s.blockchain.Stop()
	s.engine.Close()

	// Clean shutdown marker as the last thing before closing db
	s.shutdownTracker.Stop()

	s.chainDb.Close()
	s.eventMux.Stop()

	return nil
}
