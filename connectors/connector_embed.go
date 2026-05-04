//go:build libsql_embed

package connectors

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/bbmumford/tursoraft/database"
	turso "turso.tech/database/tursogo"
)

// MakeConnector creates database handles supporting embedded Turso when
// built with the libsql_embed tag.
//
// Backed by turso.tech/database/tursogo — the official, pure-Go Turso SDK
// (no CGO, cross-platform). Replaces the legacy bbmumford/go-libsql and
// maozhijie/go-libsql forks; one Windows-specific file is no longer
// required because tursogo builds identically on all supported targets.
//
// Purpose: LOCAL REPLICA mode with optional Turso synchronization
//
// This is an ALTERNATIVE to the remote connector. It provides:
//   - Local SQLite database files
//   - Embedded Turso engine (no network for reads)
//   - Optional sync to Turso primary via explicit Pull()
//   - Ultra-low latency queries
//
// Build Configuration:
//   - Default build: Uses connector_remote.go (libsql-client-go remote)
//   - Build with -tags libsql_embed: Uses this file (local replicas)
//
// Two Modes Supported:
//
//  1. Pure Local Mode (no Turso):
//     - primaryURL = ""
//     - authToken = ""
//     - Uses local Turso file only via sql.Open("turso", path)
//     - No cloud sync
//     - For testing/development/mesh-ephemeral state
//
//  2. Embedded Replica Mode (with Turso):
//     - primaryURL = Turso database URL
//     - authToken = database auth token
//     - Creates local SQLite replica
//     - WithSync callback invokes syncDb.Pull(ctx)
//     - Reads from local replica (fast)
//     - Writes go to Turso (coordinated via Raft)
//
// Sync Mechanism:
//   - Uses tursogo's NewTursoSyncDb with explicit Push/Pull
//   - Replaces go-libsql's continuous WithSyncInterval with caller-driven
//     Pull (handle.WithSync). Raft already drives sync points; the 30s
//     ticker that the legacy connector added on top is redundant when
//     consensus events trigger sync explicitly.
//
// Encryption-at-rest:
//
//	Pure local mode: supported via DSN options
//	  ?experimental=encryption&encryption_cipher=aes256gcm&encryption_hexkey=…
//
//	Embedded replica mode: NOT yet exposed on tursogo's high-level
//	NewTursoSyncDb API. The encryptionKey parameter is preserved as a
//	skeleton and a warning is logged when set. This matches the user
//	policy of "drop with skeleton" until upstream surfaces it.
//
// Build Command:
//
//	go build -tags libsql_embed ./...
//
// Parameters:
//   - dbPath: Local Turso file path
//   - primaryURL: Turso database URL (empty for pure local)
//   - authToken: Database auth token (empty for pure local)
//   - encryptionKey: Optional hex-encoded encryption key. Honored in
//     pure-local mode; logged-but-skeletal in embedded-replica mode
//     (until upstream tursogo surfaces it).
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
		dsn := buildLocalDSN(dbPath, encryptionKey)
		db, err := sql.Open("turso", dsn)
		if err != nil {
			return nil, fmt.Errorf("failed to open embedded Turso at %s: %w", dbPath, err)
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

	// Embedded replica with sync. Encryption-on-sync is a known gap in
	// tursogo's high-level API — log a warning so operators know the
	// data is unencrypted at rest on this code path.
	if encryptionKey != "" {
		log.Printf("[tursoraft] WARNING: encryptionKey set with embedded replica — sync-mode encryption is not yet supported by tursogo; storing unencrypted")
	}

	syncCtx := context.Background()
	syncDb, err := turso.NewTursoSyncDb(syncCtx, turso.TursoSyncDbConfig{
		Path:      dbPath,
		RemoteUrl: primaryURL,
		AuthToken: authToken,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create embedded sync db: %w", err)
	}

	db, err := syncDb.Connect(syncCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect embedded sync db: %w", err)
	}

	handle := database.NewDatabaseHandle(db)

	// handle.WithSync replaces go-libsql's WithSyncInterval — Raft drives
	// the sync cadence here, so caller-driven Pull is the correct shape.
	// Push isn't needed: writes flow through the regular *sql.DB and are
	// committed remotely by tursogo's normal write path.
	handle.WithSync(func(ctx context.Context) error {
		if ctx == nil {
			ctx = context.Background()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := syncDb.Pull(ctx)
		return err
	})
	// tursogo's TursoSyncDb does not (yet) expose a Close method;
	// closing the *sql.DB releases all resources we allocated.
	return handle, nil
}

// buildLocalDSN appends DSN-style encryption options to a local file path
// when an encryption key is provided. tursogo accepts these query
// parameters on sql.Open("turso", …):
//
//	?experimental=encryption&encryption_cipher=aes256gcm&encryption_hexkey=<hex>
func buildLocalDSN(path, encryptionHexKey string) string {
	if encryptionHexKey == "" {
		return path
	}
	q := url.Values{}
	q.Set("experimental", "encryption")
	q.Set("encryption_cipher", "aes256gcm")
	q.Set("encryption_hexkey", encryptionHexKey)
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}
