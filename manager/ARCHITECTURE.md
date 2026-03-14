# MeshDB Architecture Overview

## Purpose

MeshDB is a distributed database layer that provides:
- **Cloud-Native Storage**: Turso databases (libraryQL) in the cloud
- **Local Replicas**: Fast local read access via SQLite
- **Distributed Consensus**: Raft protocol for coordination across nodes
- **Strong Consistency**: Write-ahead logging with majority quorum

## Key Concepts

### Two Separate APIs

MeshDB interacts with Turso through TWO distinct APIs:

#### 1. libraryQL Go SDK (Query Execution)
- **Purpose**: ALL SQL query execution
- **Package**: `github.com/tursodatabase/libsql-client-go/libsql`
- **Reference**: https://docs.turso.tech/sdk/go/quickstart
- **Used For**:
  - SELECT, INSERT, UPDATE, DELETE
  - DDL (CREATE TABLE, ALTER, DROP)
  - Transactions
  - All data operations

**Connection Pattern**:
```go
import _ "github.com/tursodatabase/libsql-client-go/libsql"

dsn := fmt.Sprintf("%s?authToken=%s", databaseURL, authToken)
db, err := sql.Open("libsql", dsn)
// db is a standard *sql.DB - use it like any Go database
```

#### 2. Turso Platform API (Database Management)
- **Purpose**: Database provisioning and configuration ONLY
- **Location**: `internal/turso/client.go`
- **Used For**:
  - Creating new databases
  - Managing database groups
  - Configuring replication
  - Retrieving database URLs

**Important**: The Platform API **cannot** and **should not** be used for SQL queries.

### Authentication Tokens

MeshDB requires TWO different tokens:

#### Platform API Token
- **Field**: `TursoConfig.APIToken`
- **Scope**: Organization-level administrative operations
- **Used For**: Database provisioning, group management
- **Permissions**: Create/delete databases, manage infrastructure

#### Database Auth Token  
- **Field**: `GroupConfig.AuthToken`
- **Scope**: Database-level read/write access
- **Used For**: SQL query execution via libraryQL SDK
- **Permissions**: Execute queries on specific databases
- **Exception**: Not required when `GroupConfig.PureLocal=true`, because that group runs as local SQLite + Raft only with no Turso sync

#### Pure-Local Group Provisioning
- **Field**: `GroupConfig.PureLocal`
- **Behavior**: Skips Turso Platform API group validation and database provisioning
- **Implication**: The named group only needs to exist in local Tursoraft configuration; it does not need a matching Turso Cloud group

## Architecture Components

### Manager
The main entry point for MeshDB operations.

**Location**: `manager/manager.go`

**Key Methods**:
```go
// Initialization
NewManager(ctx, cfg) (*Manager, error)          // Standard setup
NewManagerWithTransport(...)                     // Custom transport (e.g., HTTPS port 443)

// Database Access
Database(group, dbName) (*sql.DB, error)        // Get database handle
Exec(ctx, group, db, sql, args)                 // Coordinated writes
Query(ctx, group, db, sql, args)                // Fast local reads

// Cluster Management
AddPeer(addr) error                              // Expand cluster
RemovePeer(addr) error                           // Shrink cluster
GetPeers() ([]string, error)                     // List peers
GetLeader() (string, error)                      // Find current leader
IsLeader() bool                                  // Check local leadership

// Lifecycle
Shutdown() error                                 // Graceful shutdown
```

### Configuration
Complete configuration for MeshDB initialization.

**Location**: `manager/manager.go` (config types are in the same file)

**Structure**:
```go
type ManagerConfig struct {
    TursoConfig  TursoConfig      // Platform API settings
    RaftConfig   RaftConfig       // Raft consensus settings
    Groups       []GroupConfig    // Database groups
    PeerEnvVar   string           // Peer discovery env var
    SyncInterval time.Duration    // Turso sync frequency
    SnapMinGap   time.Duration    // Snapshot throttling
    UseEmbedded  bool             // Embedded mode flag
}
```

### Connector System
Manages libraryQL connections to Turso databases.

**Location**: `connectors/`

**Files**:
- `connector_remote.go` - Standard Turso connection (build tag: `!libsql_embed`)
- `connector_embed.go` - Embedded SQLite mode (build tag: `libsql_embed`)
- `connector_manager.go` - Connection pool management

**Database Handle**: `database/database_handle.go`

**Connection Process**:
1. Parse database group and name → construct database key
2. Retrieve Turso database URL from Platform API
3. Build DSN with URL + auth token
4. Open libraryQL connection via `sql.Open("libsql", dsn)`
5. Store in connection pool for reuse

### Raft Consensus
Distributed coordination across MeshDB nodes.

**Location**: `internal/raft/raft.go`

**Features**:
- Leader election (automatic, <1s failover)
- Write serialization (all writes through leader)
- Log replication (majority quorum required)
- Snapshot/restore (periodic compaction)

**Cluster Formation**:
```bash
# Set peers via environment variable
export MESHDB_PEERS="node1:443,node2:443,node3:443"

# Or add dynamically
mgr.AddPeer("node4:443")
```

If `RaftBindAddr` is set to a wildcard listener like `:7331`, MeshDB binds on
that port but advertises a routable node identity instead of the wildcard
address. The advertise host resolution order is:

1. `TURSORAFT_ADVERTISE_HOST`
2. `RAFT_ADVERTISE_HOST`
3. `FLY_PRIVATE_IP`
4. Hostname / first non-loopback interface

### Finite State Machine (FSM)
Applies Raft log entries to local databases.

**Location**: `internal/raft/fsm.go`

**Process**:
1. Leader proposes write operation
2. Raft replicates to majority of nodes
3. FSM applies write to local database
4. All nodes execute same operations in same order
5. Strong consistency guaranteed

## Data Flow

### Write Operations

```
Client
  ↓
Manager.Exec(ctx, "hstles", "core", "INSERT INTO users ...")
  ↓
Create Raft log entry
  ↓
Propose to Raft cluster
  ↓
Wait for majority consensus
  ↓
FSM applies to local database (all nodes)
  ↓
Return success
```

**Characteristics**:
- Latency: Network RTT + consensus time (~10-50ms typical)
- Consistency: Strong (linearizable)
- Availability: Requires majority quorum

### Read Operations

```
Client
  ↓
Manager.Query(ctx, "hstles", "core", "SELECT * FROM users")
  ↓
Query local replica directly (no Raft)
  ↓
Return results immediately
```

**Characteristics**:
- Latency: Local database query (~1-5ms typical)
- Consistency: Eventual (may lag leader by milliseconds)
- Availability: Always available on any node

### Alternative: Direct Database Access

```
Client
  ↓
db, _ := Manager.Database("hstles", "core")
  ↓
rows, _ := db.Query("SELECT * FROM users")  // Fast local reads
  ↓
_, _ = db.Exec("INSERT INTO ...")  // ⚠️ Bypasses Raft (DANGEROUS)
```

**Recommended Pattern**:
- Use `Database()` for reads (fast, safe)
- Use `Manager.Exec()` for writes (coordinated, safe)

## Turso Integration

### Database Setup

Verify your Turso configuration:

```bash
# List groups
turso group list
# Output: hstles, calndr, orbtr

# List databases
turso db list
# Output: core (hstles group), files (hstles group), etc.

# Show database details
turso db show core
# Verify: Group=hstles, URL=libsql://core-hstles.turso.io
```

### MeshDB Configuration

```go
import "github.com/bbmumford/tursoraft/manager"

cfg := manager.ManagerConfig{
    TursoConfig: manager.TursoConfig{
        APIToken: os.Getenv("TURSO_PLATFORM_TOKEN"),  // For provisioning
        OrgName:  "hstles",
        BaseURL:  "https://api.turso.tech",
    },
    Groups: []manager.GroupConfig{
        {
            GroupName: "hstles",                      // From: turso group list
            DBNames:   []string{"core", "files"},     // From: turso db list
            AuthToken: os.Getenv("TURSO_AUTH_TOKEN"), // For queries
        },
    },
    RaftConfig: manager.RaftConfig{
        RaftBindAddr: ":443",  // Firewall-friendly port
        LocalDir:     "/var/lib/tursoraft",
        SnapshotDir:  "/var/lib/tursoraft/snapshots",
    },
    SyncInterval: 5 * time.Minute,
}

mgr, err := manager.NewManager(context.Background(), cfg)
if err != nil {
    log.Fatal(err)
}
defer mgr.Shutdown()

// Get database handle
platformDB, err := mgr.Database("hstles", "core")
if err != nil {
    log.Fatal(err)
}

// Use standard database/sql operations
rows, err := platformDB.Query("SELECT * FROM users WHERE active = ?", true)
```

### Synchronization

MeshDB periodically syncs from Turso:

1. **Leader-Only Operation**: Only the Raft leader performs syncs
2. **Scheduled Interval**: Every `SyncInterval` duration
3. **Change Detection**: Only syncs if local database changed
4. **Snapshot Creation**: Creates Raft snapshot after successful sync
5. **Replication**: New snapshot distributed to all nodes

## Cluster Deployment

### Single Node (Development)

```go
cfg := manager.ManagerConfig{
    // ... configuration ...
    RaftConfig: manager.RaftConfig{
        RaftBindAddr: ":443",
        LocalDir:     "./meshdb_data",
        SnapshotDir:  "./meshdb_snapshots",
    },
    PeerEnvVar: "MESHDB_PEERS",  // Empty env var = single node
}

mgr, _ := manager.NewManager(ctx, cfg)
// Starts immediately, no peer discovery
```

### Multi-Node Cluster

**Node 1**:
```bash
export MESHDB_PEERS="node2.internal:443,node3.internal:443"
./service --raft-addr=:443
```

**Node 2**:
```bash
export MESHDB_PEERS="node1.internal:443,node3.internal:443"
./service --raft-addr=:443
```

**Node 3**:
```bash
export MESHDB_PEERS="node1.internal:443,node2.internal:443"
./service --raft-addr=:443
```

**Features**:
- Automatic peer discovery via environment variable
- Self-removal from peer list (avoids self-connection)
- Leader election within 30 seconds
- Fault tolerance: Survives (N-1)/2 node failures

### Port 443 Deployment

Running Raft on HTTPS port 443 provides:
- **Firewall compatibility**: Port 443 always open
- **No special rules**: Works through corporate firewalls
- **Multiplexing**: Can share port with HTTP/2 traffic
- **TLS termination**: Standard HTTPS infrastructure

```go
cfg.RaftConfig.RaftBindAddr = ":443"  // Use standard HTTPS port
```

## Best Practices

### Reads
- ✅ Use `Query()` or `Database()` for read operations
- ✅ Accept eventual consistency for most reads
- ✅ High throughput, low latency

### Writes
- ✅ Always use `Exec()` for write operations
- ✅ Never use `Database().Exec()` directly
- ⚠️ Direct writes bypass Raft and corrupt cluster state

### Cluster Sizing
- **3 nodes**: Tolerates 1 failure (recommended minimum)
- **5 nodes**: Tolerates 2 failures (production standard)
- **7+ nodes**: Diminishing returns (slower consensus)

### Monitoring
```go
// Check leadership
if mgr.IsLeader() {
    metrics.RecordLeadership(true)
}

// Check cluster health
peers, _ := mgr.GetPeers()
leader, _ := mgr.GetLeader()
log.Printf("Cluster: %d peers, leader=%s", len(peers), leader)
```

### Error Handling
```go
// Write operations may fail due to:
// - Not leader (redirect to leader)
// - No quorum (majority nodes unavailable)
// - Network partition
err := mgr.Exec(ctx, "hstles", "core", sql, args...)
if err != nil {
    if strings.Contains(err.Error(), "not leader") {
        leader, _ := mgr.GetLeader()
        // Redirect client to leader
    }
    return err
}
```

## Troubleshooting

### Connection Issues
```bash
# Verify database exists and is accessible
turso db show core

# Check auth token validity
curl -H "Authorization: Bearer $TURSO_AUTH_TOKEN" \
  https://core-hstles.turso.io/v2/pipeline
```

### Cluster Issues
```go
// Check peer connectivity
peers, _ := mgr.GetPeers()
log.Printf("Peers: %v", peers)

// Verify leader election
leader, err := mgr.GetLeader()
if err != nil {
    log.Printf("No leader elected: %v", err)
}
```

### Performance Issues
- **Slow writes**: Check network latency between nodes
- **Slow reads**: Use `Database()` for direct access
- **High latency**: Reduce `SyncInterval`, optimize Raft transport

## References

- **libraryQL SDK**: https://docs.turso.tech/sdk/go/quickstart
- **Turso Platform API**: https://docs.turso.tech/api-reference/platform
- **Raft Consensus**: https://raft.github.io/
- **Database Groups**: `turso group --help`
- **Database Management**: `turso db --help`

---

**Last Updated**: December 18, 2025  
**Maintainer**: Update when architecture changes or new features added
