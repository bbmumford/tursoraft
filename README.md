# tursoraft

A Go package for distributed Turso/libraryQL databases with Raft consensus for strong consistency across mesh nodes.

## Overview

`tursoraft` combines **Turso (libraryQL)** with **Raft Consensus** to create a hybrid cloud/edge distributed database. It provides strongly-consistent, replicated data storage with automatic synchronization between mesh nodes and optional cloud databases.

## When to Use TursoRaft vs TursoKV

| Use Case | TursoRaft | TursoKV |
|----------|-----------|---------|
| **Mesh node coordination** | ✅ Best fit | ❌ No consensus |
| **Distributed sessions/auth** | ✅ Strong consistency | ❌ Eventual only |
| **Multiple nodes same data** | ✅ Raft replication | ❌ Cloud sync lag |
| **Single-node/edge caching** | ❌ Overkill | ✅ Best fit |
| **Offline-first mobile** | ❌ Requires cluster | ✅ Embedded replica |

**Rule of thumb**:
- **TursoRaft** = Multiple mesh nodes that must agree on state (auth, sessions, coordination)
- **TursoKV** = Single node or edge devices syncing to cloud

## Architecture

```
[ Mesh Node A ] <───Raft───> [ Mesh Node B ] <───Raft───> [ Mesh Node C ]
   (leader)                    (follower)                   (follower)
      │                            │                            │
      └────────────────────────────┼────────────────────────────┘
                                   │
                            (optional sync)
                                   │
                                   v
                            [ Turso Cloud ]
```

1. **Raft Consensus**: Strong consistency via HashiCorp Raft
2. **Embedded Replicas**: Local SQLite on each node (fast reads)
3. **Concurrent Writes**: MVCC via `BEGIN CONCURRENT` (always enabled)
4. **Optional Cloud Sync**: Backup/disaster recovery to Turso Cloud

## Features

| Feature | Description |
|---------|-------------|
| **Raft Consensus** | Strong consistency via HashiCorp Raft |
| **Concurrent Writes** | MVCC `BEGIN CONCURRENT` enabled by default |
| **Encryption at Rest** | AES-256/AEGIS-256 via go-libsql |
| **Embedded Replicas** | Local SQLite for fast reads |
| **Multi-Database** | Multiple databases across groups |
| **Auto-Sync** | Leader syncs to Turso Cloud |
| **Snapshot/Restore** | Full snapshot support for recovery |

## Installation

```bash
go get github.com/bbmumford/tursoraft
```

**⚠️ CGO Required**: Uses `go-libsql` for embedded replicas and concurrent writes.

## Build Tags

TursoRaft supports two build modes via conditional compilation:

| Build Command | Mode | Use Case |
|--------------|------|----------|
| `go build ./...` | **Remote** (default) | Connect to Turso Cloud databases only |
| `go build -tags libsql_embed ./...` | **Embedded** | Local SQLite + optional Turso sync |

### When to use `-tags libsql_embed`

Use the `libsql_embed` build tag when you need:

- **Pure local databases** (no Turso Cloud connection)
- **Embedded replicas** with local SQLite files
- **Offline-capable** operation
- **LAD (mesh ephemeral state)** storage

```bash
# Build with embedded libraryQL support
go build -tags libsql_embed ./...

# Docker: Enable in Dockerfile
RUN go build -tags libsql_embed -v -o server .
```

**Note**: The embedded mode requires CGO and `go-libsql`. The runtime image needs glibc (use `distroless/base-debian12`, not `static-debian12`).

### Runtime behavior difference

```go
// With default build (remote mode):
handle, err := connectors.MakeConnector(path, "", "", "")
// Returns error: "embedded libraryQL support not enabled; rebuild with -tags libsql_embed"

// With -tags libsql_embed:
handle, err := connectors.MakeConnector(path, "", "", "")
// Creates a pure local SQLite database at `path`
```

## Package Structure

```
tursoraft/
├── connectors/       # Public: Database connectors
│   ├── connector_embed.go    # (build tag: libsql_embed) - Local/embedded mode
│   ├── connector_remote.go   # (build tag: !libsql_embed) - Remote Turso mode
│   └── connector_manager.go  # Connection pool manager
├── database/         # Public: DatabaseHandle type
├── manager/          # Public: Main API entry point
│   ├── manager.go    #   Config types + Manager implementation
│   └── manager_test.go
├── internal/         # Private: Implementation details
│   ├── raft/         #   Raft consensus (FSM, peers, snapshots)
│   └── turso/        #   Turso Platform API client
└── examples/         # Usage examples
```

## Usage

### Basic Setup

```go
package main

import (
    "context"
    "log"
    
    "github.com/bbmumford/tursoraft/manager"
)

func main() {
    cfg := manager.ManagerConfig{
        TursoConfig: manager.TursoConfig{
            APIToken: "your-platform-api-token",
            OrgName:  "your-org",
        },
        Groups: []manager.GroupConfig{
            {
                GroupName: "mygroup",
                AuthToken: "your-db-auth-token",
                DBNames:   []string{"core", "sessions"},
            },
        },
        RaftConfig: manager.RaftConfig{
            RaftBindAddr: ":7000",
            LocalDir:     "/var/lib/tursoraft",
            SnapshotDir:  "/var/lib/tursoraft/snapshots",
        },
    }
    
    mgr, err := manager.NewManager(context.Background(), cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer mgr.Shutdown()
    
    // Get database handle
    db, err := mgr.Database("mygroup", "core")
    if err != nil {
        log.Fatal(err)
    }
    
    // Use standard database/sql interface
    _, err = db.Exec("INSERT INTO users (name) VALUES (?)", "alice")
}
```

### With Encryption

```go
cfg := manager.ManagerConfig{
    Groups: []manager.GroupConfig{
        {
            GroupName:     "mygroup",
            AuthToken:     "db-auth-token",
            DBNames:       []string{"core"},
            EncryptionKey: "32-byte-key-for-encryption!!!!!",
        },
    },
    // ...
}
```

### Pure Local Mode (No Cloud Sync)

For mesh nodes that don't need cloud backup:

```go
cfg := manager.ManagerConfig{
    Groups: []manager.GroupConfig{
        {
            GroupName: "local",
            DBNames:   []string{"cache"},
            PureLocal: true,  // No Turso sync, pure local SQLite
        },
    },
    // ...
}
```

Pure-local groups do not require `AuthToken`, because they never connect to Turso Cloud.
Pure-local groups also skip Turso Platform API group validation and database provisioning entirely, so the corresponding Turso group does not need to exist.

## Configuration

### ManagerConfig
```go
type ManagerConfig struct {
    TursoConfig  TursoConfig   // Turso Platform API settings
    RaftConfig   RaftConfig    // Raft consensus settings
    Groups       []GroupConfig // Database groups to manage
    PeerEnvVar   string        // Environment variable for peer discovery
    SyncInterval time.Duration // How often to sync to Turso Cloud
    SnapMinGap   time.Duration // Minimum time between snapshots
}
```

When `RaftBindAddr` is configured as a wildcard listen address such as `:7331`,
tursoraft now derives an advertisable peer identity automatically. It prefers
`TURSORAFT_ADVERTISE_HOST`, then `RAFT_ADVERTISE_HOST`, then Fly.io's
`FLY_PRIVATE_IP`, and falls back to the machine hostname.

### TursoConfig
```go
type TursoConfig struct {
    APIToken string // Platform API token (for management, not queries)
    OrgName  string // Organization name
    BaseURL  string // Optional: override API base URL
}
```

### GroupConfig
```go
type GroupConfig struct {
    GroupName     string   // Turso group name
    AuthToken     string   // Auth token for query execution
    DBNames       []string // Database names in this group
    EncryptionKey string   // Optional: data-at-rest encryption (32 bytes)
    PureLocal     bool     // If true, no Turso sync
}
```

### RaftConfig
```go
type RaftConfig struct {
    RaftBindAddr string // Address for Raft transport (e.g., ":7000")
    LocalDir     string // Directory for Raft logs and replicas
    SnapshotDir  string // Directory for Raft snapshots
}
```

## Peer Discovery

Peers are discovered via environment variable:

```bash
export TURSORAFT_PEERS="node1:7000,node2:7000,node3:7000"
```

The node automatically removes itself from the peer list.

## Concurrent Writes

TursoRaft automatically enables `BEGIN CONCURRENT` (MVCC) for all embedded replica connections. This allows:

- Multiple Raft followers to process reads concurrently
- Row-level conflict detection instead of database-level locking
- Better throughput for write-heavy workloads

This is always-on and cannot be disabled (it's the right default for mesh nodes).

## Dependencies

- `github.com/hashicorp/raft` - Raft consensus
- `github.com/hashicorp/raft-boltdb/v2` - BoltDB storage for Raft
- `github.com/tursodatabase/go-libsql` - libraryQL Go bindings (CGO)
- `github.com/tursodatabase/libsql-client-go` - libraryQL client

## License

MIT
