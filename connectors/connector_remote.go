//go:build !libsql_embed
// +build !libsql_embed

package connectors

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/bbmumford/tursoraft/database"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// MakeConnector creates database connections using libSQL Go SDK for Turso.
//
// This is the STANDARD connection mode for MeshDB. It:
//   - Uses libSQL Go SDK (github.com/tursodatabase/libsql-client-go)
//   - Connects to Turso cloud databases for query execution
//   - Does NOT use Turso Platform API for queries
//   - Provides standard database/sql interface
//
// Connection Flow:
//  1. Constructs DSN with Turso database URL + auth token
//  2. Opens libSQL connection via database/sql driver
//  3. Pings database to verify connectivity
//  4. Returns handle for query execution
//
// Authentication:
//   - Uses database-level auth token (NOT Platform API token)
//   - Token grants read/write access to specific database
//   - Embedded in DSN connection string
//
// Build Tags:
//   - Default build (no tags): Uses this remote connector
//   - Build with -tags libsql_embed: Uses embedded SQLite instead
//
// Reference:
//   - libSQL SDK: https://docs.turso.tech/sdk/go/quickstart
//   - Connection strings: libsql://<database-url>?authToken=<token>
//
// Parameters:
//   - dbPath: Local file path (for future local replica support)
//   - primaryURL: Turso database URL (e.g., libsql://core-hstles.turso.io)
//   - authToken: Database authentication token
//   - encryptionKey: Optional encryption key (unused in remote mode, for signature compatibility)
//
// Returns:
//   - *database.DatabaseHandle: Wrapper around *sql.DB for MeshDB operations
//   - error: Connection or validation errors
func MakeConnector(dbPath, primaryURL, authToken, encryptionKey string) (*database.DatabaseHandle, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("database path cannot be empty")
	}

	dbPath = filepath.Clean(dbPath)

	if primaryURL == "" && authToken == "" {
		return nil, fmt.Errorf("embedded libSQL support not enabled; rebuild with -tags libsql_embed")
	}

	if primaryURL == "" {
		return nil, fmt.Errorf("primary URL cannot be empty (for remote mode)")
	}
	if authToken == "" {
		return nil, fmt.Errorf("auth token cannot be empty (for remote mode)")
	}

	// Note: encryptionKey is ignored in remote mode - encryption is handled server-side by Turso
	// Parameter included for function signature compatibility across build tags

	var dsn string
	if encryptionKey != "" {
		// If encryption key is provided, we MUST use a local replica to support encryption at rest.
		// This leverages the "Databases Anywhere" feature (Turso Sync) to create an encrypted local replica.
		dsn = fmt.Sprintf("file:%s?replica=%s&authToken=%s&encryption_key=%s",
			dbPath, primaryURL, authToken, encryptionKey)
	} else {
		// Standard remote connection
		dsn = fmt.Sprintf("%s?authToken=%s", primaryURL, authToken)
	}

	db, err := sql.Open("libsql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open libSQL connection for %s: %w", dbPath, err)
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
