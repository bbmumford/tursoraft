//go:build libsql_embed && windows
// +build libsql_embed,windows

package connectors

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/bbmumford/tursoraft/database"
	"github.com/maozhijie/go-libsql"
)

// Note: This is the Windows version using the maozhijie/go-libsql fork
// which adds Windows amd64 support to the libSQL driver.

// MakeConnector creates database handles supporting embedded libSQL when
// built with the libsql_embed tag on Windows.
//
// Purpose: LOCAL REPLICA mode with optional Turso synchronization
//
// This is an ALTERNATIVE to the remote connector. It provides:
//   - Local SQLite database files
//   - Embedded libSQL engine (no network for reads)
//   - Optional sync to Turso primary
//   - Ultra-low latency queries
//
// Build Configuration:
//   - Default build: Uses connector_remote.go (Turso queries)
//   - Build with -tags libsql_embed: Uses this file (local replicas)
//
// Two Modes Supported:
//
//  1. Pure Local Mode (no Turso):
//     - primaryURL = ""
//     - authToken = ""
//     - Uses local SQLite file only
//     - No cloud sync
//     - For testing/development/mesh-ephemeral state
//
//  2. Embedded Replica Mode (with Turso):
//     - primaryURL = Turso database URL
//     - authToken = database auth token
//     - Creates local SQLite replica
//     - Syncs from Turso primary every 30 seconds
//     - Reads from local replica (fast)
//     - Writes go to Turso (coordinated via Raft)
//
// Why Embedded Replicas?
//   - Ultra-low read latency (local disk)
//   - Reduced Turso API calls
//   - Works offline for reads
//   - Better performance for read-heavy workloads
//
// Sync Mechanism:
//   - Uses go-libsql embedded replica connector
//   - Downloads WAL frames from Turso
//   - Applies changes to local SQLite file
//   - Automatic sync every 30 seconds
//   - Manual sync via SyncDatabase()
//
// Trade-offs vs Remote Mode:
//   - Much faster reads (local disk vs network)
//   - Reduced bandwidth usage
//   - Offline read capability
//   - Larger disk footprint (full database copies)
//   - Eventual consistency for reads (sync lag)
//   - More complex dependency (go-libsql)
//
// Build Command:
//
//	go build -tags libsql_embed ./...
//
// Parameters:
//   - dbPath: Local SQLite file path
//   - primaryURL: Turso database URL (empty for pure local)
//   - authToken: Database auth token (empty for pure local)
//   - encryptionKey: Optional encryption key for data-at-rest (empty to disable)
//
// Returns:
//   - *database.DatabaseHandle: Handle with embedded replica
//   - error: Creation or connectivity failure
func MakeConnector(dbPath, primaryURL, authToken, encryptionKey string) (*database.DatabaseHandle, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("database path cannot be empty")
	}

	dbPath = filepath.Clean(dbPath)

	// Purely local embedded mode (no primary).
	if primaryURL == "" && authToken == "" {
		dsn := fmt.Sprintf("file:%s", dbPath)
		if encryptionKey != "" {
			dsn += fmt.Sprintf("?encryption_key=%s", encryptionKey)
		}
		db, err := sql.Open("libsql", dsn)
		if err != nil {
			return nil, fmt.Errorf("failed to open embedded libSQL at %s: %w", dbPath, err)
		}
		handle := database.NewDatabaseHandle(db)
		handle.WithSync(func(ctx context.Context) error {
			if ctx == nil {
				ctx = context.Background()
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return db.PingContext(ctx)
		})
		return handle, nil
	}

	if primaryURL == "" {
		return nil, fmt.Errorf("primary URL cannot be empty (for remote mode)")
	}
	if authToken == "" {
		return nil, fmt.Errorf("auth token cannot be empty (for remote mode)")
	}

	opts := []libsql.Option{libsql.WithAuthToken(authToken)}

	// Add encryption if key provided
	if encryptionKey != "" {
		opts = append(opts, libsql.WithEncryption(encryptionKey))
	}

	// Provide a modest sync interval so replicas stay warm but avoid chatter.
	opts = append(opts, libsql.WithSyncInterval(30*time.Second))
	connector, err := libsql.NewEmbeddedReplicaConnector(dbPath, primaryURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create embedded replica connector: %w", err)
	}
	// sql.OpenDB manages pooling around the libsql connector.
	db := sql.OpenDB(connector)
	handle := database.NewDatabaseHandle(db)
	handle.WithSync(func(ctx context.Context) error {
		// Respect cancellation before invoking Sync.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := connector.Sync()
		return err
	})
	handle.AddCloser(connector.Close)
	return handle, nil
}
