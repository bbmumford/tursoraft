package manager

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/bbmumford/tursoraft/connectors"
	raftpkg "github.com/bbmumford/tursoraft/internal/raft"
	"github.com/bbmumford/tursoraft/internal/turso"
)

// =============================================================================
// Configuration Types
// =============================================================================

// TursoConfig represents Turso Platform API configuration.
//
// Platform API Usage:
//   - Database provisioning (create/delete databases)
//   - Group management (list/show groups)
//   - NOT used for SQL queries (use libraryQL SDK instead)
//
// Authentication:
//   - Requires Platform API token (not database auth token)
//   - Get token from: https://turso.tech/app or `turso auth token`
//   - Scoped to organization level
//
// Reference:
//   - API docs: https://docs.turso.tech/api-reference/
//   - Authentication: https://docs.turso.tech/api-reference/authentication
type TursoConfig struct {
	APIToken string `json:"api_token"` // Platform API token (for management, not queries)
	OrgName  string `json:"org_name"`  // Organization name (e.g., "hstles")
	BaseURL  string `json:"base_url"`  // Optional: override API base URL (default: https://api.turso.tech)
}

// GroupConfig represents configuration for a single Turso database group.
//
// Database Groups:
//   - Logical containers for related databases
//   - Share replication configuration
//   - Located in specific geographic regions
//   - Example: "hstles" group in Sydney, "calndr" group in Mumbai
//
// Verify with CLI:
//
//	turso group list              # List all groups
//	turso group show <group>      # Show group details
//	turso db list                 # See which databases are in which groups
type GroupConfig struct {
	GroupName     string   `json:"group_name"`     // Group name from Turso (e.g., "hstles")
	AuthToken     string   `json:"auth_token"`     // Auth token for query execution (db-level access)
	DBNames       []string `json:"db_names"`       // Database names within this group (e.g., ["core", "files"])
	EncryptionKey string   `json:"encryption_key"` // Optional: encryption key for local replica (data-at-rest)
	PureLocal     bool     `json:"pure_local"`     // If true, force pure local mode (no Turso sync) regardless of UseEmbedded flag
}

// RaftConfig represents Raft consensus configuration.
//
// Raft Setup:
//   - Distributed consensus for coordinating database operations
//   - Leader election for write coordination
//   - Log replication across nodes
//
// Peer Discovery:
//   - Peers discovered via environment variable
//   - Format: comma-separated addresses (e.g., "node1:7000,node2:7000,node3:7000")
//   - Node automatically removes itself from peer list
//
// Network Requirements:
//   - All nodes must be able to reach each other on RaftBindAddr
//   - Typically use internal/private network addresses
//   - For production, consider ":443" to avoid firewall issues
type RaftConfig struct {
	RaftBindAddr string `json:"raft_bind_addr"` // Address to bind Raft transport (e.g., ":7000", ":443")
	LocalDir     string `json:"local_dir"`      // Directory for Raft logs and local SQLite replicas
	SnapshotDir  string `json:"snapshot_dir"`   // Directory for Raft snapshots
}

// ManagerConfig holds the complete configuration for the MeshDB manager.
//
// This is the primary configuration struct for initializing MeshDB. It combines:
//   - Turso connection settings (cloud databases)
//   - Local replica settings (fast read access)
//   - Raft consensus settings (distributed coordination)
//
// Typical Configuration:
//
//	cfg := manager.ManagerConfig{
//	    TursoConfig: manager.TursoConfig{
//	        APIToken: os.Getenv("TURSO_API_TOKEN"),
//	        OrgName:  "hstles",
//	    },
//	    Groups: []manager.GroupConfig{{
//	        GroupName: "hstles",
//	        DBNames:   []string{"core"},
//	        AuthToken: os.Getenv("TURSO_AUTH_TOKEN"),
//	    }},
//	    RaftConfig: manager.RaftConfig{
//	        RaftBindAddr: ":443",
//	        LocalDir:     "/var/lib/meshdb",
//	        SnapshotDir:  "/var/lib/meshdb/snapshots",
//	    },
//	    SyncInterval: 5 * time.Minute,
//	}
type ManagerConfig struct {
	TursoConfig  TursoConfig   `json:"turso_config"`  // Turso Platform API settings
	RaftConfig   RaftConfig    `json:"raft_config"`   // Raft consensus settings
	Groups       []GroupConfig `json:"groups"`        // Database groups to manage
	PeerEnvVar   string        `json:"peer_env_var"`  // Environment variable containing peer addresses
	SyncInterval time.Duration `json:"sync_interval"` // How often to sync from Turso (leader only)
	SnapMinGap   time.Duration `json:"snap_min_gap"`  // Minimum gap between snapshots after successful pull
	UseEmbedded  bool          `json:"use_embedded"`  // If true, use go-libsql embedded for local DBs (Turso optional)
}

// DefaultManagerConfig returns a configuration with sensible defaults.
//
// Default Values:
//   - LocalDir: "./meshdb_data" (local database replicas)
//   - SnapshotDir: "./meshdb_snapshots" (Raft snapshots)
//   - RaftBindAddr: ":8300" (default Raft port)
//   - PeerEnvVar: "MESHDB_PEERS" (environment variable for peer discovery)
//   - SyncInterval: 6 hours (how often leader syncs from Turso)
//   - SnapMinGap: 2 minutes (minimum time between snapshots)
//   - UseEmbedded: false (use Turso cloud databases)
//
// You must still provide:
//   - TursoConfig (Turso credentials)
//   - Groups (database group configurations)
//
// Recommended Customizations:
//   - RaftBindAddr: ":443" for firewall-friendly deployments
//   - SyncInterval: 5-15 minutes for active applications
//   - LocalDir/SnapshotDir: Production paths (e.g., "/var/lib/meshdb")
//
// Example:
//
//	cfg := manager.DefaultManagerConfig()
//	cfg.RaftConfig.RaftBindAddr = ":443"
//	cfg.TursoConfig = manager.TursoConfig{...}
//	cfg.Groups = []manager.GroupConfig{{...}}
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		RaftConfig: RaftConfig{
			LocalDir:     "./meshdb_data",
			SnapshotDir:  "./meshdb_snapshots",
			RaftBindAddr: ":8300",
		},
		PeerEnvVar:   "MESHDB_PEERS",
		SyncInterval: 6 * time.Hour,
		SnapMinGap:   2 * time.Minute,
		UseEmbedded:  false,
	}
}

// Validate checks if the configuration is valid
func (cfg *ManagerConfig) Validate() error {
	// In non-embedded mode, Turso API is required. In embedded mode, it's optional.
	if !cfg.UseEmbedded {
		if cfg.TursoConfig.APIToken == "" {
			return fmt.Errorf("API token is required")
		}
		if cfg.TursoConfig.OrgName == "" {
			return fmt.Errorf("organization name is required")
		}
	}

	if len(cfg.Groups) == 0 {
		return fmt.Errorf("at least one group must be configured")
	}

	for i, group := range cfg.Groups {
		if group.GroupName == "" {
			return fmt.Errorf("group %d: group name is required", i)
		}
		if !cfg.UseEmbedded && !group.PureLocal {
			if group.AuthToken == "" {
				return fmt.Errorf("group %d: auth token is required", i)
			}
		}
		if len(group.DBNames) == 0 {
			return fmt.Errorf("group %d: at least one database name is required", i)
		}
	}

	if cfg.RaftConfig.LocalDir == "" {
		return fmt.Errorf("local directory is required")
	}

	if cfg.RaftConfig.SnapshotDir == "" {
		return fmt.Errorf("snapshot directory is required")
	}

	if cfg.RaftConfig.RaftBindAddr == "" {
		return fmt.Errorf("raft bind address is required")
	}

	// Optional defaults if user left them zero (defensive)
	if cfg.SyncInterval == 0 {
		cfg.SyncInterval = 6 * time.Hour
	}
	if cfg.SnapMinGap == 0 {
		cfg.SnapMinGap = 2 * time.Minute
	}

	return nil
}

// toInternalTursoConfig converts manager.TursoConfig to internal turso.TursoConfig
func toInternalTursoConfig(cfg TursoConfig) turso.TursoConfig {
	return turso.TursoConfig{
		APIToken: cfg.APIToken,
		OrgName:  cfg.OrgName,
		BaseURL:  cfg.BaseURL,
	}
}

// toInternalGroups converts []GroupConfig to []turso.GroupConfig
func toInternalGroups(groups []GroupConfig) []turso.GroupConfig {
	result := make([]turso.GroupConfig, len(groups))
	for i, g := range groups {
		result[i] = turso.GroupConfig{
			GroupName:     g.GroupName,
			AuthToken:     g.AuthToken,
			DBNames:       g.DBNames,
			EncryptionKey: g.EncryptionKey,
			PureLocal:     g.PureLocal,
		}
	}
	return result
}

// toInternalRaftConfig converts manager.RaftConfig to internal raft.RaftConfig
func toInternalRaftConfig(cfg RaftConfig) raftpkg.RaftConfig {
	return raftpkg.RaftConfig{
		RaftBindAddr: cfg.RaftBindAddr,
		LocalDir:     cfg.LocalDir,
		SnapshotDir:  cfg.SnapshotDir,
	}
}

// =============================================================================
// Manager Implementation
// =============================================================================

// Manager provides the main interface for the MeshDB distributed database system.
//
// MeshDB is a distributed database layer that combines:
//   - Turso databases (libraryQL) for cloud-native storage
//   - Local replicas for fast read access
//   - Raft consensus for distributed coordination
//
// Architecture:
//   - Uses libraryQL Go SDK for ALL query execution (not Turso Platform API)
//   - Turso Platform API used ONLY for database provisioning/management
//   - Raft ensures strong consistency across multiple nodes
//   - Local replicas provide low-latency reads
//
// Typical usage:
//  1. Create Manager with NewManager() or NewManagerWithTransport()
//  2. Get database handles with Database(group, dbName)
//  3. Execute queries through returned *sql.DB handles
//  4. Shutdown gracefully with Shutdown()
type Manager struct {
	config           ManagerConfig                // User-provided configuration
	tursoClient      *turso.TursoClient           // For database management (NOT query execution)
	connectorManager *connectors.ConnectorManager // Manages libraryQL connections to databases
	raftNode         *raft.Raft                   // Raft consensus node for distributed coordination
	fsm              *raftpkg.MultiFSM            // Finite state machine for applying Raft commands
	stopCh           chan struct{}                // Signal channel for graceful shutdown
	mu               sync.RWMutex                 // Protects Manager state
	isRunning        bool                         // Whether Manager is currently active

	// Snapshot and sync gating fields prevent unnecessary work
	paths      map[string]string   // Maps database keys to local file paths
	fileHashes map[string][32]byte // SHA-256 hashes for change detection
	lastSnap   time.Time           // Last snapshot creation time
	lastSync   time.Time           // Last Turso sync time

	// lastWrite marks the most recent commit applied via the multiFSM callback. The
	// leader-only sync loop inspects this to avoid snapshot/sync work when no new
	// raft logs have landed since the previous cycle. Followers still track the
	// timestamp so they can take over immediately after promotion.
	lastWrite time.Time
}

// NewManager creates and initializes a new MeshDB manager instance.
//
// This is the standard initialization path for MeshDB. It:
//  1. Validates the provided configuration
//  2. Creates necessary directories for local replicas and snapshots
//  3. Provisions Turso databases via Platform API (if not using embedded mode)
//  4. Establishes libraryQL connections to each database for query execution
//  5. Initializes Raft consensus cluster with discovered peers
//  6. Starts background sync loops for database replication
//
// Connection Architecture:
//   - Query execution: Uses libraryQL Go SDK (github.com/tursodatabase/libsql-client-go)
//   - Database management: Uses Turso Platform API (only for provisioning)
//   - The Platform API cannot and should not be used for SQL queries
//
// Raft Cluster Formation:
//   - Discovers peers via environment variable (cfg.PeerEnvVar)
//   - Removes self from peer list automatically
//   - Waits for leader election before returning
//   - Single-node clusters work without peers
//
// Parameters:
//   - ctx: Context for initialization (typically context.Background())
//   - cfg: Complete ManagerConfig with Turso credentials and Raft settings
//
// Returns:
//   - *Manager: Initialized and running manager instance
//   - error: Configuration validation, connection, or Raft startup errors
//
// Example:
//
//	cfg := meshdb.ManagerConfig{
//	    APIConfig: meshdb.TursoConfig{...},
//	    Groups: []meshdb.GroupConfig{{GroupName: "hstles", DBNames: []string{"core"}}},
//	    RaftBindAddr: ":443",
//	}
//	mgr, err := meshdb.NewManager(context.Background(), cfg)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer mgr.Shutdown()
func NewManager(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	// Create directories
	if err := os.MkdirAll(cfg.RaftConfig.LocalDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create local directory: %w", err)
	}

	if err := os.MkdirAll(cfg.RaftConfig.SnapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot directory: %w", err)
	}

	// Initialize Turso client and database URLs unless using embedded-only mode
	var (
		tursoClient *turso.TursoClient
		dbInfo      map[string]turso.DBInfo
		err         error
	)
	if !cfg.UseEmbedded {
		tursoClient = turso.NewTursoClient(toInternalTursoConfig(cfg.TursoConfig))
		dbInfo, err = turso.EnsureAllDBs(ctx, tursoClient, toInternalGroups(cfg.Groups))
		if err != nil {
			return nil, fmt.Errorf("failed to ensure databases: %w", err)
		}
	} else {
		dbInfo = make(map[string]turso.DBInfo) // empty; embedded will open local files
	}

	// Create connector manager and paths map
	connectorManager := connectors.NewConnectorManager()
	paths := make(map[string]string)
	// Create connectors for each database
	for _, group := range cfg.Groups {
		for _, dbName := range group.DBNames {
			dbKey := fmt.Sprintf("%s.%s", group.GroupName, dbName)
			groupDir := filepath.Join(cfg.RaftConfig.LocalDir, group.GroupName)
			if err := os.MkdirAll(groupDir, 0o755); err != nil {
				return nil, fmt.Errorf("mkdir group dir: %w", err)
			}
			dbPath := filepath.Join(groupDir, dbName+".db")
			info := dbInfo[dbKey]
			primaryURL := info.URL
			serverVersion := info.Version
			authToken := group.AuthToken
			encryptionKey := group.EncryptionKey

			// Determine connector mode based on PureLocal flag and UseEmbedded setting
			if group.PureLocal {
				// Force pure local mode (no Turso sync) regardless of UseEmbedded
				// Used for ephemeral mesh-only data like auth sessions
				primaryURL, authToken, serverVersion = "", "", ""
			} else if cfg.UseEmbedded {
				// Embedded replica mode: local file + Turso sync
				// Keep primaryURL and authToken for sync
				// (Both libsql_embed and !libsql_embed can use embedded mode)
			} else {
				// Remote-only mode: direct Turso queries
				// Keep primaryURL and authToken
			}

			handle, err := connectors.MakeConnectorForServer(dbPath, primaryURL, serverVersion, authToken, encryptionKey)
			if err != nil {
				return nil, fmt.Errorf("failed to create connector for %s: %w", dbKey, err)
			}
			if err := connectorManager.AddConnector(dbKey, handle); err != nil {
				return nil, fmt.Errorf("failed to add connector for %s: %w", dbKey, err)
			}
			paths[dbKey] = dbPath
		}
	}

	// Pre-create manager to capture in onApplied callback
	manager := &Manager{
		config:           cfg,
		tursoClient:      tursoClient,
		connectorManager: connectorManager,
		stopCh:           make(chan struct{}),
		isRunning:        true,
		paths:            paths,
		fileHashes:       make(map[string][32]byte),
	}

	// Create FSM with onApplied callback to capture writes originating anywhere
	fsm := raftpkg.NewMultiFSM(connectorManager, paths, func() {
		manager.mu.Lock()
		manager.lastWrite = time.Now()
		manager.mu.Unlock()
	})

	// Discover peers (keep current behavior)
	selfAddr, err := raftpkg.ResolveAdvertiseAddress(cfg.RaftConfig.RaftBindAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve raft advertise address: %w", err)
	}
	peerAddrs := raftpkg.DiscoverAndValidatePeers(cfg.PeerEnvVar)
	peerAddrs = raftpkg.NormalizePeerAddresses(peerAddrs)
	peerAddrs = raftpkg.RemoveSelfFromPeers(peerAddrs, selfAddr)
	peerAddrs = raftpkg.RemoveSelfFromPeers(peerAddrs, cfg.RaftConfig.RaftBindAddr)

	// Start Raft cluster
	raftNode, err := raftpkg.StartCluster(toInternalRaftConfig(cfg.RaftConfig), fsm, peerAddrs)
	if err != nil {
		return nil, fmt.Errorf("failed to start Raft cluster: %w", err)
	}

	// Wait for leader election
	if err := raftpkg.WaitForLeader(raftNode, 30*time.Second); err != nil {
		return nil, fmt.Errorf("failed to elect leader: %w", err)
	}

	// Fill Raft and FSM on manager
	manager.raftNode = raftNode
	manager.fsm = fsm

	// Initialize file hashes
	for _, p := range manager.paths {
		if h, err := hashFile(p); err == nil {
			manager.fileHashes[p] = h
		}
	}

	// Start leadership watcher (use smaller cadence, gating will handle schedule)
	cadence := time.Minute
	if cfg.SyncInterval > 0 && cfg.SyncInterval/6 < cadence {
		cadence = cfg.SyncInterval / 6
	}
	if cadence < 10*time.Second {
		cadence = 10 * time.Second
	}
	go manager.watchLeadership(ctx, cadence)

	return manager, nil
}

// NewManagerWithTransport is like NewManager but allows providing a custom raft.Transport.
//
// This advanced initialization path is used when you need to:
//   - Multiplex Raft traffic with other protocols (e.g., gRPC on port 443)
//   - Use custom networking layers (TLS, QUIC, etc.)
//   - Integrate with existing transport infrastructure
//
// Use Cases:
//   - Running Raft over HTTPS port 443 alongside HTTP/2 traffic
//   - Custom authentication or encryption for Raft messages
//   - Integration with service mesh or custom network topology
//
// The provided transport must implement raft.Transport interface and handle:
//   - AppendEntries RPC
//   - RequestVote RPC
//   - InstallSnapshot RPC
//   - Peer connectivity management
//
// Parameters:
//   - ctx: Context for initialization
//   - cfg: ManagerConfig (RaftBindAddr ignored, transport handles binding)
//   - transport: Custom raft.Transport implementation
//   - localAddr: This node's address as advertised to peers
//
// Returns:
//   - *Manager: Initialized manager using custom transport
//   - error: Configuration or initialization errors
//
// Example (multiplexing on port 443):
//
//	transport := NewHTTP2RaftTransport(":443")
//	mgr, err := meshdb.NewManagerWithTransport(
//	    ctx, cfg, transport, raft.ServerAddress("node1:443"),
//	)
func NewManagerWithTransport(ctx context.Context, cfg ManagerConfig, transport raft.Transport, localAddr raft.ServerAddress) (*Manager, error) {
	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	// Create directories
	if err := os.MkdirAll(cfg.RaftConfig.LocalDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create local directory: %w", err)
	}
	if err := os.MkdirAll(cfg.RaftConfig.SnapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot directory: %w", err)
	}

	// Initialize Turso client and ensure DBs
	tursoClient := turso.NewTursoClient(toInternalTursoConfig(cfg.TursoConfig))
	dbInfo, err := turso.EnsureAllDBs(ctx, tursoClient, toInternalGroups(cfg.Groups))
	if err != nil {
		return nil, fmt.Errorf("failed to ensure databases: %w", err)
	}

	// Create connectors and paths
	connectorManager := connectors.NewConnectorManager()
	paths := make(map[string]string)
	for _, group := range cfg.Groups {
		for _, dbName := range group.DBNames {
			dbKey := fmt.Sprintf("%s.%s", group.GroupName, dbName)
			groupDir := filepath.Join(cfg.RaftConfig.LocalDir, group.GroupName)
			if err := os.MkdirAll(groupDir, 0o755); err != nil {
				return nil, fmt.Errorf("mkdir group dir: %w", err)
			}
			dbPath := filepath.Join(groupDir, dbName+".db")
			info := dbInfo[dbKey]
			encryptionKey := group.EncryptionKey

			handle, err := connectors.MakeConnectorForServer(dbPath, info.URL, info.Version, group.AuthToken, encryptionKey)
			if err != nil {
				return nil, fmt.Errorf("failed to create connector for %s: %w", dbKey, err)
			}
			if err := connectorManager.AddConnector(dbKey, handle); err != nil {
				return nil, fmt.Errorf("failed to add connector for %s: %w", dbKey, err)
			}
			paths[dbKey] = dbPath
		}
	}

	manager := &Manager{
		config:           cfg,
		tursoClient:      tursoClient,
		connectorManager: connectorManager,
		stopCh:           make(chan struct{}),
		isRunning:        true,
		paths:            paths,
		fileHashes:       make(map[string][32]byte),
	}

	fsm := raftpkg.NewMultiFSM(connectorManager, paths, func() {
		manager.mu.Lock()
		manager.lastWrite = time.Now()
		manager.mu.Unlock()
	})

	// Discover peers and remove self using provided localAddr
	peerAddrs := raftpkg.DiscoverAndValidatePeers(cfg.PeerEnvVar)
	peerAddrs = raftpkg.RemoveSelfFromPeers(peerAddrs, string(localAddr))
	peerAddrs = raftpkg.NormalizePeerAddresses(peerAddrs)

	// Start cluster with provided transport
	raftNode, err := raftpkg.StartClusterWithTransport(toInternalRaftConfig(cfg.RaftConfig), fsm, transport, localAddr, peerAddrs)
	if err != nil {
		return nil, fmt.Errorf("failed to start Raft cluster (custom transport): %w", err)
	}

	if err := raftpkg.WaitForLeader(raftNode, 30*time.Second); err != nil {
		return nil, fmt.Errorf("failed to elect leader: %w", err)
	}

	manager.raftNode = raftNode
	manager.fsm = fsm

	// Initialize file hashes
	for _, p := range manager.paths {
		if h, err := hashFile(p); err == nil {
			manager.fileHashes[p] = h
		}
	}

	cadence := time.Minute
	if cfg.SyncInterval > 0 && cfg.SyncInterval/6 < cadence {
		cadence = cfg.SyncInterval / 6
	}
	if cadence < 10*time.Second {
		cadence = 10 * time.Second
	}
	go manager.watchLeadership(ctx, cadence)

	return manager, nil
}

// watchLeadership monitors Raft leadership changes and manages sync operations.
// Only the leader performs periodic sync operations with Turso.
func (m *Manager) watchLeadership(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var syncTicker *time.Ticker
	var syncStop chan struct{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			if syncTicker != nil {
				syncTicker.Stop()
				close(syncStop)
			}
			return
		case isLeader := <-m.raftNode.LeaderCh():
			if isLeader {
				// Became leader - start sync ticker
				if syncTicker == nil {
					syncTicker = time.NewTicker(interval)
					syncStop = make(chan struct{})

					go func() {
						for {
							select {
							case <-syncStop:
								return
							case <-syncTicker.C:
								if err := m.maybeSyncOnSchedule(ctx); err != nil {
									fmt.Printf("Sync error: %v\n", err)
								}
							}
						}
					}()
				}
			} else {
				// Lost leadership - stop sync ticker
				if syncTicker != nil {
					syncTicker.Stop()
					close(syncStop)
					syncTicker = nil
					syncStop = nil
				}
			}
		}
	}
}

// maybeSyncOnSchedule enforces leader-only sync with interval + lastWrite gating
func (m *Manager) maybeSyncOnSchedule(ctx context.Context) error {
	if m.raftNode.State() != raft.Leader {
		return nil
	}
	if time.Since(m.lastSync) < m.config.SyncInterval {
		return nil
	}
	if !m.lastWrite.After(m.lastSync) {
		return nil
	}
	return m.SyncAll(ctx)
}

// SyncAll synchronizes all local replicas with their Turso primaries.
// This operation should only be performed by the Raft leader.
func (m *Manager) SyncAll(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !raftpkg.IsLeader(m.raftNode) {
		return fmt.Errorf("only the leader can perform sync operations")
	}

	changed := false
	dbKeys := m.connectorManager.GetAllKeys()
	for _, dbKey := range dbKeys {
		p := m.paths[dbKey]
		before, _ := hashFile(p)
		if err := m.connectorManager.SyncDatabase(ctx, dbKey); err != nil {
			fmt.Printf("Failed to sync %s: %v\n", dbKey, err)
			continue
		}
		after, _ := hashFile(p)
		if before != after {
			m.fileHashes[p] = after
			changed = true
		}
	}

	if changed && time.Since(m.lastSnap) >= m.config.SnapMinGap {
		if err := m.raftNode.Snapshot().Error(); err != nil {
			return fmt.Errorf("raft snapshot: %w", err)
		}
		m.lastSnap = time.Now()
	}
	m.lastSync = time.Now()
	return nil
}

// Exec executes a SQL statement across the Raft cluster.
//
// This method ensures strong consistency by:
//  1. Creating a Raft log entry with the SQL statement
//  2. Proposing the entry to the Raft leader
//  3. Waiting for cluster consensus (majority of nodes)
//  4. Applying the statement to all replicas in the same order
//
// Use for:
//   - INSERT, UPDATE, DELETE operations
//   - DDL statements (CREATE, ALTER, DROP)
//   - Any statement that modifies database state
//
// Performance Characteristics:
//   - Requires network round-trip for consensus
//   - Blocks until majority acknowledgment
//   - Serializes through Raft log (strong consistency)
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - group: Database group name (e.g., "hstles")
//   - dbName: Database name within group (e.g., "core")
//   - sql: SQL statement to execute
//   - args: Parameters for prepared statement (optional)
//
// Returns:
//   - error: Raft consensus failure or SQL execution error
//
// Example:
//
//	err := mgr.Exec(ctx, "hstles", "core",
//	    "INSERT INTO users (email, name) VALUES (?, ?)",
//	    "user@example.com", "John Doe")
func (m *Manager) Exec(ctx context.Context, group, dbName, sql string, args ...interface{}) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.isRunning {
		return fmt.Errorf("manager is not running")
	}

	// Create log entry
	logEntry, err := raftpkg.CreateLogEntry(group, dbName, sql, args)
	if err != nil {
		return fmt.Errorf("failed to create log entry: %w", err)
	}

	// Serialize log entry
	data, err := json.Marshal(logEntry)
	if err != nil {
		return fmt.Errorf("failed to serialize log entry: %w", err)
	}

	// Apply through Raft
	if err := raftpkg.ApplyLog(m.raftNode, data, 10*time.Second); err != nil {
		return fmt.Errorf("failed to apply log through Raft: %w", err)
	}

	return nil
}

// Query executes a read-only SQL query on the local replica.
//
// Queries are served directly from the local database without Raft coordination.
// This provides:
//   - Low latency (no network round-trip)
//   - High throughput (no serialization bottleneck)
//   - Stale reads possible (eventual consistency)
//
// Use for:
//   - SELECT queries
//   - Read-heavy workloads
//   - Analytics and reporting
//   - When eventual consistency is acceptable
//
// Consistency Guarantees:
//   - Reads from local replica only
//   - May lag behind leader (replication delay)
//   - No read-after-write consistency guarantee
//   - Suitable for most application reads
//
// For Strong Consistency:
//   - Use Exec() for write operations that need confirmation
//   - Consider read-through-leader for critical reads
//   - Local reads are typically within milliseconds of leader
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - group: Database group name
//   - dbName: Database name within group
//   - sql: SELECT query
//   - args: Query parameters
//
// Returns:
//   - *sql.Rows: Result set (caller must close)
//   - error: Database not found or query execution error
//
// Example:
//
//	rows, err := mgr.Query(ctx, "hstles", "core",
//	    "SELECT id, email FROM users WHERE created_at > ?",
//	    time.Now().Add(-24*time.Hour))
//	if err != nil {
//	    return err
//	}
//	defer rows.Close()
func (m *Manager) Query(ctx context.Context, group, dbName, sql string, args ...interface{}) (*sql.Rows, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.isRunning {
		return nil, fmt.Errorf("manager is not running")
	}

	dbKey := fmt.Sprintf("%s.%s", group, dbName)

	db, exists := m.connectorManager.GetDatabase(dbKey)
	if !exists {
		return nil, fmt.Errorf("database %s not found", dbKey)
	}

	rows, err := db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to execute query: %w", err)
	}

	return rows, nil
}

// Database returns the underlying *sql.DB handle for the requested database.
//
// This is the PRIMARY method for accessing databases in MeshDB. It returns
// a standard Go database/sql handle that can be used with any SQL library.
//
// Important Usage Notes:
//   - The returned *sql.DB connects via libraryQL SDK to Turso
//   - Treats reads as eventually consistent (local replica)
//   - Writes should go through Exec() for Raft coordination
//   - Direct writes bypass Raft and break consistency guarantees
//
// Recommended Pattern:
//
//	db, err := mgr.Database("hstles", "core")
//	if err != nil {
//	    return err
//	}
//	// Reads: Use db.Query() directly (fast, local)
//	rows, _ := db.Query("SELECT * FROM users")
//
//	// Writes: Use Manager.Exec() (coordinated, replicated)
//	err = mgr.Exec(ctx, "hstles", "core", "INSERT INTO users ...")
//
// Migration Pattern (Compatibility):
//
//	For gradual migration from platform/datastore, this handle can be
//	wrapped in compatibility structs to maintain existing handler signatures.
//
// Parameters:
//   - group: Database group name (verified via `turso group list`)
//   - dbName: Database name within group (verified via `turso db list`)
//
// Returns:
//   - *sql.DB: Standard database handle (uses libraryQL driver internally)
//   - error: Manager not running or database not found
//
// Example:
//
//	coreDB, err := meshMgr.Database("hstles", "core")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	// Enable foreign keys (SQLite/libraryQL specific)
//	_, err = coreDB.Exec("PRAGMA foreign_keys = ON")
func (m *Manager) Database(group, dbName string) (*sql.DB, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.isRunning {
		return nil, fmt.Errorf("manager is not running")
	}

	dbKey := fmt.Sprintf("%s.%s", group, dbName)
	db, exists := m.connectorManager.GetDatabase(dbKey)
	if !exists {
		return nil, fmt.Errorf("database %s not found", dbKey)
	}
	return db, nil
}

// AddPeer adds a new peer to the Raft cluster.
//
// This operation dynamically expands the cluster by:
//  1. Validating the peer address format
//  2. Proposing a configuration change to Raft
//  3. Waiting for cluster consensus
//  4. Establishing connectivity with the new peer
//
// Cluster Expansion Use Cases:
//   - Scaling up for higher availability
//   - Adding nodes in new geographic regions
//   - Replacing failed nodes
//   - Temporary capacity increases
//
// Requirements:
//   - Must be called on the Raft leader
//   - Peer must be reachable via network
//   - Peer must be running compatible MeshDB version
//
// Address Format:
//   - host:port (e.g., "node2.example.com:443")
//   - IP:port (e.g., "10.0.1.5:443")
//   - Automatically normalized and validated
//
// Parameters:
//   - addr: Network address of peer to add
//
// Returns:
//   - error: Not leader, invalid address, or Raft operation failure
//
// Example:
//
//	if err := mgr.AddPeer("node2.internal:443"); err != nil {
//	    log.Printf("Failed to add peer: %v", err)
//	}
func (m *Manager) AddPeer(addr string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.isRunning {
		return fmt.Errorf("manager is not running")
	}

	addr = raftpkg.NormalizePeerAddress(addr)

	if !raftpkg.ValidatePeerAddress(addr) {
		return fmt.Errorf("invalid peer address: %s", addr)
	}

	return raftpkg.AddPeer(m.raftNode, addr)
}

// RemovePeer removes a peer from the Raft cluster.
//
// This operation safely shrinks the cluster by:
//  1. Validating the peer address
//  2. Proposing configuration change to Raft
//  3. Removing peer from quorum calculations
//  4. Allowing cluster to continue with remaining nodes
//
// Cluster Reduction Use Cases:
//   - Decommissioning nodes
//   - Scaling down during low traffic
//   - Removing failed nodes that won't recover
//   - Maintenance operations
//
// Safety Considerations:
//   - Maintains quorum requirements (majority still operational)
//   - Do not remove too many nodes at once
//   - Ensure remaining nodes can achieve quorum
//   - Leader can remove itself (triggers re-election)
//
// Requirements:
//   - Must be called on the Raft leader
//   - Cluster must maintain quorum after removal
//
// Parameters:
//   - addr: Network address of peer to remove
//
// Returns:
//   - error: Not leader, invalid address, or Raft operation failure
//
// Example:
//
//	// Remove failed node
//	if err := mgr.RemovePeer("node3.internal:443"); err != nil {
//	    log.Printf("Failed to remove peer: %v", err)
//	}
func (m *Manager) RemovePeer(addr string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.isRunning {
		return fmt.Errorf("manager is not running")
	}

	addr = raftpkg.NormalizePeerAddress(addr)

	return raftpkg.RemovePeer(m.raftNode, addr)
}

// GetPeers returns a list of all peers in the Raft cluster.
//
// This method provides cluster membership information including:
//   - All nodes participating in consensus
//   - Both voters and non-voters (if configured)
//   - Peer addresses as configured
//
// Use Cases:
//   - Cluster health monitoring
//   - Administrative dashboards
//   - Debugging connectivity issues
//   - Load balancing decisions
//
// Returns:
//   - []string: List of peer addresses (e.g., ["node1:443", "node2:443"])
//   - error: Manager not running or Raft state unavailable
//
// Example:
//
//	peers, err := mgr.GetPeers()
//	if err != nil {
//	    log.Printf("Failed to get peers: %v", err)
//	    return
//	}
//	log.Printf("Cluster has %d peers: %v", len(peers), peers)
func (m *Manager) GetPeers() ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.isRunning {
		return nil, fmt.Errorf("manager is not running")
	}

	return raftpkg.GetPeers(m.raftNode)
}

// GetLeader returns the address of the current Raft leader.
//
// The leader is the node responsible for:
//   - Accepting write operations
//   - Coordinating cluster consensus
//   - Managing cluster configuration changes
//   - Initiating snapshots and log compaction
//
// Leader Election:
//   - Automatic when cluster starts
//   - Re-election on leader failure (typically < 1 second)
//   - Single leader per term (strong consistency guarantee)
//
// Use Cases:
//   - Directing write traffic to leader
//   - Health monitoring
//   - Client retry logic (redirect to leader)
//   - Debugging consensus issues
//
// Returns:
//   - string: Leader address (e.g., "node1.internal:443")
//   - error: No leader elected or manager not running
//
// Example:
//
//	leader, err := mgr.GetLeader()
//	if err != nil {
//	    log.Printf("No leader available: %v", err)
//	    return
//	}
//	log.Printf("Current leader: %s", leader)
func (m *Manager) GetLeader() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.isRunning {
		return "", fmt.Errorf("manager is not running")
	}

	leader := raftpkg.GetLeaderAddress(m.raftNode)
	if leader == "" {
		return "", fmt.Errorf("no leader elected")
	}

	return leader, nil
}

// IsLeader returns true if this node is the current Raft leader.
//
// Leadership Responsibilities:
//   - Only the leader accepts write operations
//   - Leader coordinates all cluster consensus
//   - Leader manages configuration changes
//   - Leader initiates Turso sync operations
//
// Use Cases:
//   - Conditional behavior based on leadership
//   - Background job coordination (only run on leader)
//   - Metrics and monitoring
//   - Write request validation
//
// Leadership Changes:
//   - Can change due to network partitions
//   - Automatic re-election on failure
//   - Split-brain prevented by majority quorum
//
// Returns:
//   - bool: true if this node is leader, false otherwise
//
// Example:
//
//	if mgr.IsLeader() {
//	    // Only run sync operations on leader
//	    go startPeriodicSync()
//	}
func (m *Manager) IsLeader() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.isRunning {
		return false
	}

	return raftpkg.IsLeader(m.raftNode)
}

// Shutdown gracefully shuts down the manager and all its components.
//
// Shutdown Process:
//  1. Stops accepting new operations
//  2. Waits for in-flight operations to complete
//  3. Stops leadership monitoring goroutines
//  4. Closes Raft node (transfers leadership if leader)
//  5. Closes all database connections
//  6. Cleans up resources
//
// Safety Guarantees:
//   - Idempotent (safe to call multiple times)
//   - Waits for graceful Raft leadership transfer
//   - Ensures database connections are properly closed
//   - Prevents resource leaks
//
// Cluster Impact:
//   - If leader: Triggers leader election among remaining nodes
//   - If follower: Cluster continues without interruption
//   - No data loss (Raft log persisted)
//
// Recommended Usage:
//
//	defer mgr.Shutdown() // Ensure cleanup on exit
//
// Returns:
//   - error: Shutdown errors (logged but typically ignored)
//
// Example:
//
//	mgr, err := meshdb.NewManager(ctx, cfg)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer mgr.Shutdown() // Always cleanup
//
//	// Use manager...
func (m *Manager) Shutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.isRunning {
		return nil
	}

	m.isRunning = false

	// Stop the leadership watcher
	close(m.stopCh)

	// Shutdown Raft
	if m.raftNode != nil {
		future := m.raftNode.Shutdown()
		if err := future.Error(); err != nil {
			fmt.Printf("Error shutting down Raft: %v\n", err)
		}
	}

	// Close all connectors
	if err := m.connectorManager.Close(); err != nil {
		fmt.Printf("Error closing connectors: %v\n", err)
	}

	return nil
}

// helpers
func hashFile(path string) ([32]byte, error) {
	var zero [32]byte
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return zero, err
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}
