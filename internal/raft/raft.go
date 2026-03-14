package raft

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	hashiraft "github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

// RaftConfig represents Raft consensus configuration (local copy to avoid import cycle).
type RaftConfig struct {
	RaftBindAddr string `json:"raft_bind_addr"`
	LocalDir     string `json:"local_dir"`
	SnapshotDir  string `json:"snapshot_dir"`
}

// ResolveAdvertiseAddress converts a bind address into a routable Raft advertise
// address. Wildcard binds like ":7331" or "[::]:7331" are valid listen
// addresses, but they are not valid advertised identities for HashiCorp Raft.
func ResolveAdvertiseAddress(bindAddr string) (string, error) {
	_, advertiseAddr, err := resolveTransportAddresses(bindAddr)
	if err != nil {
		return "", err
	}
	return advertiseAddr.String(), nil
}

// startCluster initializes and starts a Raft cluster with the given configuration.
// It handles both cluster bootstrapping (when no peers exist) and joining an existing cluster.
func StartCluster(cfg RaftConfig, fsm hashiraft.FSM, peerAddrs []string) (*hashiraft.Raft, error) {
	listenAddr, advertiseAddr, err := resolveTransportAddresses(cfg.RaftBindAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve transport addresses: %w", err)
	}

	// Create Raft configuration
	raftConfig := hashiraft.DefaultConfig()
	raftConfig.LocalID = hashiraft.ServerID(advertiseAddr.String())

	// Set up Raft directories
	raftDir := filepath.Join(cfg.LocalDir, "raft")
	if err := os.MkdirAll(raftDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create raft directory: %w", err)
	}

	// Create log store using BoltDB
	logStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDir, "logs.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create log store: %w", err)
	}

	// Create stable store using BoltDB
	stableStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDir, "stable.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create stable store: %w", err)
	}

	// Create snapshot store
	snapshotStore, err := hashiraft.NewFileSnapshotStore(cfg.SnapshotDir, 3, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot store: %w", err)
	}

	// Create transport
	transport, err := hashiraft.NewTCPTransport(listenAddr, advertiseAddr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("failed to create transport: %w", err)
	}

	// Create the Raft instance
	raftNode, err := hashiraft.NewRaft(raftConfig, fsm, logStore, stableStore, snapshotStore, transport)
	if err != nil {
		return nil, fmt.Errorf("failed to create raft instance: %w", err)
	}

	// Handle cluster initialization
	if len(peerAddrs) == 0 {
		// Bootstrap a new cluster
		configuration := hashiraft.Configuration{
			Servers: []hashiraft.Server{
				{
					ID:      hashiraft.ServerID(advertiseAddr.String()),
					Address: hashiraft.ServerAddress(advertiseAddr.String()),
				},
			},
		}

		future := raftNode.BootstrapCluster(configuration)
		if err := future.Error(); err != nil {
			return nil, fmt.Errorf("failed to bootstrap cluster: %w", err)
		}
	} else {
		// Join existing cluster
		for _, peerAddr := range peerAddrs {
			future := raftNode.AddVoter(hashiraft.ServerID(peerAddr), hashiraft.ServerAddress(peerAddr), 0, 0)
			if err := future.Error(); err != nil {
				// Log the error but continue - the peer might already be in the cluster
				fmt.Printf("Warning: failed to add peer %s: %v\n", peerAddr, err)
			}
		}
	}

	return raftNode, nil
}

func resolveTransportAddresses(bindAddr string) (string, *net.TCPAddr, error) {
	listenAddr, err := normalizeListenAddress(bindAddr)
	if err != nil {
		return "", nil, err
	}

	listenTCP, err := net.ResolveTCPAddr("tcp", listenAddr)
	if err != nil {
		return "", nil, fmt.Errorf("resolve listen address: %w", err)
	}

	host := listenTCP.IP.String()
	if listenTCP.IP == nil || listenTCP.IP.IsUnspecified() {
		host, err = resolveAdvertiseHost()
		if err != nil {
			return "", nil, err
		}
	}

	advertiseAddr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", listenTCP.Port)))
	if err != nil {
		return "", nil, fmt.Errorf("resolve advertise address: %w", err)
	}

	return listenAddr, advertiseAddr, nil
}

func normalizeListenAddress(bindAddr string) (string, error) {
	if bindAddr == "" {
		return "", fmt.Errorf("bind address is required")
	}
	if _, _, err := net.SplitHostPort(bindAddr); err == nil {
		return bindAddr, nil
	} else if addrErr, ok := err.(*net.AddrError); ok && addrErr.Err == "missing port in address" {
		return net.JoinHostPort("", bindAddr), nil
	}
	return "", fmt.Errorf("invalid bind address %q", bindAddr)
}

func resolveAdvertiseHost() (string, error) {
	for _, key := range []string{"TURSORAFT_ADVERTISE_HOST", "RAFT_ADVERTISE_HOST", "FLY_PRIVATE_IP", "POD_IP", "HOST_IP"} {
		if value := os.Getenv(key); value != "" {
			return value, nil
		}
	}

	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		return hostname, nil
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("detect advertise host: %w", err)
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() || ipNet.IP.IsUnspecified() {
			continue
		}
		return ipNet.IP.String(), nil
	}

	return "127.0.0.1", nil
}

// startClusterWithTransport starts a Raft node using a provided transport (e.g., gRPC over :443).
// localAddr should be the address Raft will advertise for this node (e.g., host:port or logical ID).
func StartClusterWithTransport(cfg RaftConfig, fsm hashiraft.FSM, transport hashiraft.Transport, localAddr hashiraft.ServerAddress, peerAddrs []string) (*hashiraft.Raft, error) {
	// Raft configuration
	raftConfig := hashiraft.DefaultConfig()
	raftConfig.LocalID = hashiraft.ServerID(localAddr)

	// Raft directories
	raftDir := filepath.Join(cfg.LocalDir, "raft")
	if err := os.MkdirAll(raftDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create raft directory: %w", err)
	}

	// Stores
	logStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDir, "logs.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create log store: %w", err)
	}

	stableStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDir, "stable.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create stable store: %w", err)
	}

	snapshotStore, err := hashiraft.NewFileSnapshotStore(cfg.SnapshotDir, 3, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot store: %w", err)
	}

	// New raft node
	raftNode, err := hashiraft.NewRaft(raftConfig, fsm, logStore, stableStore, snapshotStore, transport)
	if err != nil {
		return nil, fmt.Errorf("failed to create raft instance: %w", err)
	}

	if len(peerAddrs) == 0 {
		// Bootstrap single-node cluster
		configuration := hashiraft.Configuration{Servers: []hashiraft.Server{{ID: hashiraft.ServerID(localAddr), Address: localAddr}}}
		if err := raftNode.BootstrapCluster(configuration).Error(); err != nil {
			return nil, fmt.Errorf("failed to bootstrap cluster: %w", err)
		}
	} else {
		// Join flow: attempt to add peers
		for _, peerAddr := range peerAddrs {
			future := raftNode.AddVoter(hashiraft.ServerID(peerAddr), hashiraft.ServerAddress(peerAddr), 0, 0)
			if err := future.Error(); err != nil {
				fmt.Printf("Warning: failed to add peer %s: %v\n", peerAddr, err)
			}
		}
	}

	return raftNode, nil
}

// waitForLeader waits for the Raft cluster to elect a leader
func WaitForLeader(raftNode *hashiraft.Raft, timeout time.Duration) error {
	start := time.Now()

	for time.Since(start) < timeout {
		if raftNode.State() == hashiraft.Leader {
			return nil
		}

		leader := raftNode.Leader()
		if leader != "" {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for leader election")
}

// isLeader checks if the current node is the Raft leader
func IsLeader(raftNode *hashiraft.Raft) bool {
	return raftNode.State() == hashiraft.Leader
}

// getLeaderAddress returns the address of the current Raft leader
func GetLeaderAddress(raftNode *hashiraft.Raft) string {
	return string(raftNode.Leader())
}

// getPeers returns a list of all peers in the Raft cluster
func GetPeers(raftNode *hashiraft.Raft) ([]string, error) {
	future := raftNode.GetConfiguration()
	if err := future.Error(); err != nil {
		return nil, fmt.Errorf("failed to get configuration: %w", err)
	}

	config := future.Configuration()
	peers := make([]string, 0, len(config.Servers))

	for _, server := range config.Servers {
		peers = append(peers, string(server.Address))
	}

	return peers, nil
}

// addPeer adds a new peer to the Raft cluster
func AddPeer(raftNode *hashiraft.Raft, peerAddr string) error {
	if !IsLeader(raftNode) {
		return fmt.Errorf("only the leader can add peers")
	}

	future := raftNode.AddVoter(hashiraft.ServerID(peerAddr), hashiraft.ServerAddress(peerAddr), 0, 0)
	if err := future.Error(); err != nil {
		return fmt.Errorf("failed to add peer %s: %w", peerAddr, err)
	}

	return nil
}

// removePeer removes a peer from the Raft cluster
func RemovePeer(raftNode *hashiraft.Raft, peerAddr string) error {
	if !IsLeader(raftNode) {
		return fmt.Errorf("only the leader can remove peers")
	}

	future := raftNode.RemoveServer(hashiraft.ServerID(peerAddr), 0, 0)
	if err := future.Error(); err != nil {
		return fmt.Errorf("failed to remove peer %s: %w", peerAddr, err)
	}

	return nil
}

// applyLog applies a log entry to the Raft cluster
func ApplyLog(raftNode *hashiraft.Raft, data []byte, timeout time.Duration) error {
	if !IsLeader(raftNode) {
		return fmt.Errorf("only the leader can apply log entries")
	}

	future := raftNode.Apply(data, timeout)
	if err := future.Error(); err != nil {
		return fmt.Errorf("failed to apply log: %w", err)
	}

	// Check if the apply returned an error
	if result := future.Response(); result != nil {
		if err, ok := result.(error); ok {
			return fmt.Errorf("log application failed: %w", err)
		}
	}

	return nil
}
