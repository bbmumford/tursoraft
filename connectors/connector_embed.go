//go:build libsql_embed

package connectors

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/bbmumford/tursoraft/database"
	_ "github.com/tursodatabase/libsql-client-go/libsql" // registers "libsql" driver for legacy-server fallback
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
	return MakeConnectorForServer(dbPath, primaryURL, "", authToken, encryptionKey)
}

// MakeConnectorForServer is the version-aware variant. The serverVersion
// parameter is the value the Turso Platform API reports for the database
// (e.g. "0.24.29" for legacy sqld, "2026.6.0" for new tursodb). When set to
// a value beginning with "0.", the connector falls back to libsql-client-go
// (Hrana over HTTP) because tursogo's sync engine does not speak the legacy
// sync protocol — its endpoints return 404 on legacy servers.
//
// An empty serverVersion preserves the previous behaviour (assume new
// platform; use tursogo embedded sync). Pure-local mode is independent of
// version.
func MakeConnectorForServer(dbPath, primaryURL, serverVersion, authToken, encryptionKey string) (*database.DatabaseHandle, error) {
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

	// Legacy sqld server (version "0.x"): tursogo's sync engine returns
	// empty-body 404s on this platform because the new sync endpoints are
	// not implemented there. Fall back to libsql-client-go remote-only
	// (Hrana protocol via /v2/pipeline) — the same client the official
	// Turso Go remote-access example uses. No local replica on this path;
	// every query is an HTTP round-trip to the cloud.
	if strings.HasPrefix(serverVersion, "0.") {
		log.Printf("[tursoraft] %s on legacy sqld %s — using libsql-client-go remote (no local replica)", dbPath, serverVersion)
		dsn := fmt.Sprintf("%s?authToken=%s", primaryURL, authToken)
		db, err := sql.Open("libsql", dsn)
		if err != nil {
			return nil, fmt.Errorf("failed to open libsql remote for %s: %w", dbPath, err)
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
		// HSTLES schemas use SQL triggers (e.g. trg_cleanup_empty_session
		// in core.sql); the embedded Turso engine refuses to open such
		// databases unless this experimental feature is enabled.
		ExperimentalFeatures: "triggers",
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
	// closing the *sql.DB returned by Connect releases all resources we
	// allocated here.
	return handle, nil
}

// buildLocalDSN appends DSN-style options to a local file path. tursogo
// accepts a comma-separated experimental= list plus encryption options:
//
//	?experimental=triggers,encryption&encryption_cipher=aes256gcm&encryption_hexkey=<hex>
//
// "triggers" is always required because HSTLES schemas (e.g. core.sql's
// trg_cleanup_empty_session) define SQL triggers; the embedded engine
// refuses to open such databases without the flag.
//
// The caller may supply the encryption key as either hex or base64; the
// HSTLES encryption.yaml stores keys base64-encoded but tursogo requires
// hex on the encryption_hexkey parameter. We normalize transparently.
func buildLocalDSN(path, encryptionKey string) string {
	features := []string{"triggers"}
	q := url.Values{}
	if encryptionKey != "" {
		hexKey, err := normalizeKeyToHex(encryptionKey)
		if err != nil {
			log.Printf("[tursoraft] WARNING: encryption key not usable (%v) — opening %s unencrypted", err, path)
		} else {
			features = append(features, "encryption")
			q.Set("encryption_cipher", "aes256gcm")
			q.Set("encryption_hexkey", hexKey)
		}
	}
	q.Set("experimental", strings.Join(features, ","))
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}

// normalizeKeyToHex returns the input as a hex-encoded string. It accepts
// either hex (passed through verbatim) or standard/url-safe base64 (decoded
// then hex-encoded). The legacy bbmumford/go-libsql connector accepted base64
// directly via PRAGMA key — tursogo only supports hex on its DSN parameter.
func normalizeKeyToHex(key string) (string, error) {
	if isHex(key) {
		return key, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(key); err == nil {
		return hex.EncodeToString(decoded), nil
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(key); err == nil {
		return hex.EncodeToString(decoded), nil
	}
	if decoded, err := base64.URLEncoding.DecodeString(key); err == nil {
		return hex.EncodeToString(decoded), nil
	}
	return "", fmt.Errorf("not hex or base64")
}

func isHex(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
