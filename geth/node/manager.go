package node

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/les"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p/discover"
	whisper "github.com/ethereum/go-ethereum/whisper/whisperv5"
	"github.com/status-im/status-go/geth/log"
	"github.com/status-im/status-go/geth/mailservice"
	"github.com/status-im/status-go/geth/params"
	"github.com/status-im/status-go/geth/rpc"
)

// errors
var (
	ErrNodeExists                  = errors.New("node is already running")
	ErrNoRunningNode               = errors.New("there is no running node")
	ErrInvalidNodeManager          = errors.New("node manager is not properly initialized")
	ErrInvalidWhisperService       = errors.New("whisper service is unavailable")
	ErrInvalidLightEthereumService = errors.New("LES service is unavailable")
	ErrInvalidAccountManager       = errors.New("could not retrieve account manager")
	ErrAccountKeyStoreMissing      = errors.New("account key store is not set")
	ErrRPCClient                   = errors.New("failed to init RPC client")
)

// RPCClientError reported when rpc client is initialized.
type RPCClientError error

// EthNodeError is reported when node crashed on start up.
type EthNodeError error

// NodeManager manages Status node (which abstracts contained geth node)
// nolint: golint
// should be fixed at https://github.com/status-im/status-go/issues/200
type NodeManager struct {
	mu     sync.RWMutex
	config *params.NodeConfig // Status node configuration
	node   *node.Node         // reference to Geth P2P stack/node

	whisperService *whisper.Whisper   // reference to Whisper service
	lesService     *les.LightEthereum // reference to LES service
	rpcClient      *rpc.Client        // reference to RPC client
}

// NewNodeManager makes new instance of node manager
func NewNodeManager() *NodeManager {
	return &NodeManager{}
}

// StartNode start Status node, fails if node is already started
func (m *NodeManager) StartNode(config *params.NodeConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startNode(config)
}

// startNode start Status node, fails if node is already started
func (m *NodeManager) startNode(config *params.NodeConfig) error {
	if err := m.isNodeAvailable(); err == nil {
		return ErrNodeExists
	}
	m.initLog(config)

	ethNode, err := MakeNode(config)
	if err != nil {
		return err
	}
	m.node = ethNode
	m.config = config

	// activate MailService required for Offline Inboxing
	if err := ethNode.Register(func(_ *node.ServiceContext) (node.Service, error) {
		return mailservice.New(m), nil
	}); err != nil {
		return err
	}

	// start underlying node
	if err := ethNode.Start(); err != nil {
		return EthNodeError(err)
	}
	// init RPC client for this node
	localRPCClient, err := m.node.Attach()
	if err == nil {
		m.rpcClient, err = rpc.NewClient(localRPCClient, m.config.UpstreamConfig)
	}
	if err != nil {
		log.Error("Failed to create an RPC client", "error", err)
		return RPCClientError(err)
	}

	// populate static peers exits when node stopped
	go func() {
		if err := m.PopulateStaticPeers(); err != nil {
			log.Error("Static peers population", "error", err)
		}
	}()
	return nil
}

// StopNode stop Status node. Stopped node cannot be resumed.
func (m *NodeManager) StopNode() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopNode()
}

// stopNode stop Status node. Stopped node cannot be resumed.
func (m *NodeManager) stopNode() error {
	if err := m.isNodeAvailable(); err != nil {
		return err
	}
	// now attempt to stop
	if err := m.node.Stop(); err != nil {
		return err
	}
	m.node = nil
	m.config = nil
	m.lesService = nil
	m.whisperService = nil
	m.rpcClient = nil
	log.Info("Node manager notifed app, that node has stopped")
	return nil
}

// IsNodeRunning confirm that node is running
func (m *NodeManager) IsNodeRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := m.isNodeAvailable(); err != nil {
		return false
	}
	return true
}

// Node returns underlying Status node
func (m *NodeManager) Node() (*node.Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := m.isNodeAvailable(); err != nil {
		return nil, err
	}
	return m.node, nil
}

// PopulateStaticPeers connects current node with our publicly available LES/SHH/Swarm cluster
func (m *NodeManager) PopulateStaticPeers() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.populateStaticPeers()
}

// populateStaticPeers connects current node with our publicly available LES/SHH/Swarm cluster
func (m *NodeManager) populateStaticPeers() error {
	if err := m.isNodeAvailable(); err != nil {
		return err
	}
	if !m.config.BootClusterConfig.Enabled {
		log.Info("Boot cluster is disabled")
		return nil
	}

	for _, enode := range m.config.BootClusterConfig.BootNodes {
		err := m.addPeer(enode)
		if err != nil {
			log.Warn("Boot node addition failed", "error", err)
			continue
		}
		log.Info("Boot node added", "enode", enode)
	}

	return nil
}

// AddPeer adds new static peer node
func (m *NodeManager) AddPeer(url string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := m.isNodeAvailable(); err != nil {
		return err
	}
	return m.addPeer(url)
}

// addPeer adds new static peer node
func (m *NodeManager) addPeer(url string) error {
	// Try to add the url as a static peer and return
	parsedNode, err := discover.ParseNode(url)
	if err != nil {
		return err
	}
	m.node.Server().AddPeer(parsedNode)
	return nil
}

// PeerCount returns the number of connected peers.
func (m *NodeManager) PeerCount() int {
	if !m.IsNodeRunning() {
		return 0
	}
	return m.node.Server().PeerCount()
}

// NodeConfig exposes reference to running node's configuration
func (m *NodeManager) NodeConfig() (*params.NodeConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := m.isNodeAvailable(); err != nil {
		return nil, err
	}
	return m.config, nil
}

// LightEthereumService exposes reference to LES service running on top of the node
func (m *NodeManager) LightEthereumService() (*les.LightEthereum, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := m.isNodeAvailable(); err != nil {
		return nil, err
	}
	if m.lesService == nil {
		if err := m.node.Service(&m.lesService); err != nil {
			log.Warn("Cannot obtain LES service", "error", err)
			return nil, ErrInvalidLightEthereumService
		}
	}
	if m.lesService == nil {
		return nil, ErrInvalidLightEthereumService
	}
	return m.lesService, nil
}

// WhisperService exposes reference to Whisper service running on top of the node
func (m *NodeManager) WhisperService() (*whisper.Whisper, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := m.isNodeAvailable(); err != nil {
		return nil, err
	}
	if m.whisperService == nil {
		if err := m.node.Service(&m.whisperService); err != nil {
			log.Warn("Cannot obtain whisper service", "error", err)
			return nil, ErrInvalidWhisperService
		}
	}
	if m.whisperService == nil {
		return nil, ErrInvalidWhisperService
	}
	return m.whisperService, nil
}

// AccountManager exposes reference to node's accounts manager
func (m *NodeManager) AccountManager() (*accounts.Manager, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := m.isNodeAvailable(); err != nil {
		return nil, err
	}
	accountManager := m.node.AccountManager()
	if accountManager == nil {
		return nil, ErrInvalidAccountManager
	}
	return accountManager, nil
}

// AccountKeyStore exposes reference to accounts key store
func (m *NodeManager) AccountKeyStore() (*keystore.KeyStore, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := m.isNodeAvailable(); err != nil {
		return nil, err
	}
	accountManager := m.node.AccountManager()
	if accountManager == nil {
		return nil, ErrInvalidAccountManager
	}

	backends := accountManager.Backends(keystore.KeyStoreType)
	if len(backends) == 0 {
		return nil, ErrAccountKeyStoreMissing
	}

	keyStore, ok := backends[0].(*keystore.KeyStore)
	if !ok {
		return nil, ErrAccountKeyStoreMissing
	}

	return keyStore, nil
}

// RPCClient exposes reference to RPC client connected to the running node.
func (m *NodeManager) RPCClient() *rpc.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rpcClient
}

// initLog initializes global logger parameters based on
// provided node configurations.
func (m *NodeManager) initLog(config *params.NodeConfig) {
	log.SetLevel(config.LogLevel)

	if config.LogFile != "" {
		err := log.SetLogFile(config.LogFile)
		if err != nil {
			fmt.Println("Failed to open log file, using stdout")
		}
	}
}

// isNodeAvailable check if we have a node running and make sure is fully started
func (m *NodeManager) isNodeAvailable() error {
	if m.node == nil || m.node.Server() == nil {
		return ErrNoRunningNode
	}
	return nil
}
