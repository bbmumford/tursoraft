package connectors

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bbmumford/tursoraft/database"
)

// ConnectorManager manages a collection of database handles keyed by group/database name.
//
// Purpose: Connection pool management for multiple Turso databases
//
// The ConnectorManager acts as a registry and lifecycle manager for database connections.
// Each database is identified by a key in the format "group.database" (e.g., "hstles.core").
//
// Key Responsibilities:
//   - Store and retrieve database handles by key
//   - Validate connectivity when registering new databases
//   - Coordinate sync operations across databases
//   - Gracefully close all connections on shutdown
//
// Why a separate manager?
//   - MeshDB supports multiple databases in multiple groups
//   - Each database needs its own libraryQL connection
//   - Connection pooling and reuse across Manager operations
//   - Centralized lifecycle management (open/close/sync)
//
// Example structure:
//
//	connectors map:
//	"hstles.core"  → *DatabaseHandle (libraryQL connection to core-hstles.turso.io)
//	"hstles.files" → *DatabaseHandle (libraryQL connection to files-hstles.turso.io)
//	"calndr.main"  → *DatabaseHandle (libraryQL connection to calndr database)
type ConnectorManager struct {
	connectors map[string]*database.DatabaseHandle // Key format: "group.database"
}

// NewConnectorManager creates a new connector manager instance.
//
// Initializes an empty connection pool. Database handles are added later
// during Manager initialization via AddConnector().
func NewConnectorManager() *ConnectorManager {
	return &ConnectorManager{
		connectors: make(map[string]*database.DatabaseHandle),
	}
}

// AddConnector registers a database handle after basic connectivity validation.
//
// This method is called during MeshDB initialization for each database in the
// configuration. It performs validation before accepting the connection.
//
// Validation Steps:
//  1. Checks handle is not nil
//  2. Ensures dbKey is not already registered (no duplicates)
//  3. Tests connectivity via Ping()
//  4. Executes test query (SELECT 1) to verify database is operational
//
// Database Key Format:
//
//	"group.database" - e.g., "hstles.core", "calndr.main"
//
// Why validate?
//   - Fail fast if Turso credentials are invalid
//   - Detect network/DNS issues during startup
//   - Ensure database is accessible before accepting traffic
//   - Prevent registration of broken connections
//
// Called From:
//   - NewManager() during initialization
//   - After makeConnector() creates libraryQL connection
//
// Parameters:
//   - dbKey: Database identifier (group.database format)
//   - handle: Wrapper around *sql.DB with sync capabilities
//
// Returns:
//   - error: Nil handle, duplicate key, or connectivity failure
//
// Example:
//
//	handle, _ := makeConnector(dbPath, url, token)
//	err := cm.AddConnector("hstles.core", handle)
func (cm *ConnectorManager) AddConnector(dbKey string, handle *database.DatabaseHandle) error {
	if handle == nil {
		return fmt.Errorf("handle cannot be nil")
	}
	if _, exists := cm.connectors[dbKey]; exists {
		return fmt.Errorf("connector for %s already exists", dbKey)
	}

	if err := testConnector(handle.Database()); err != nil {
		return fmt.Errorf("connector test failed for %s: %w", dbKey, err)
	}

	cm.connectors[dbKey] = handle
	return nil
}

// GetDatabase retrieves a *sql.DB for the specified key.
//
// This is the primary access method for retrieving database connections.
// Returns the standard Go database/sql handle for query execution.
//
// Connection Details:
//   - The returned *sql.DB uses libraryQL driver
//   - Connected to Turso cloud database
//   - Pooled connection (safe for concurrent use)
//   - Standard database/sql interface
//
// Usage Pattern:
//
//	db, exists := cm.GetDatabase("hstles.core")
//	if !exists {
//	    return fmt.Errorf("database not found")
//	}
//	rows, err := db.Query("SELECT * FROM users")
//
// Why return (db, bool)?
//   - Distinguishes between "not found" vs "connection error"
//   - Allows caller to decide error handling
//   - Go idiom for optional return values
//
// Called From:
//   - Manager.Database() - public API for getting database handles
//   - Manager.Query() - for executing read operations
//   - FSM operations - when applying Raft log entries
//
// Parameters:
//   - dbKey: Database identifier (e.g., "hstles.core")
//
// Returns:
//   - *sql.DB: Database handle (nil if not found)
//   - bool: true if database exists, false otherwise
func (cm *ConnectorManager) GetDatabase(dbKey string) (*sql.DB, bool) {
	handle, exists := cm.connectors[dbKey]
	if !exists {
		return nil, false
	}
	return handle.Database(), true
}

// GetAllKeys returns all database keys managed by the connector.
//
// Purpose: Enumerate all registered databases for iteration or monitoring.
//
// Use Cases:
//   - Periodic sync operations (iterate all databases)
//   - Health checks and monitoring
//   - Administrative dashboards
//   - Debugging connection issues
//
// Key Format:
//
//	Returns slice of "group.database" strings
//	Example: ["hstles.core", "hstles.files", "calndr.main"]
//
// Called From:
//   - Manager.maybeSyncOnSchedule() - syncs all databases from Turso
//   - Health check endpoints
//   - Administrative tooling
//
// Returns:
//   - []string: List of all database keys (order not guaranteed)
//
// Example:
//
//	for _, dbKey := range cm.GetAllKeys() {
//	    if err := cm.SyncDatabase(ctx, dbKey); err != nil {
//	        log.Printf("Sync failed for %s: %v", dbKey, err)
//	    }
//	}
func (cm *ConnectorManager) GetAllKeys() []string {
	keys := make([]string, 0, len(cm.connectors))
	for key := range cm.connectors {
		keys = append(keys, key)
	}
	return keys
}

// SyncDatabase invokes the connector-specific sync procedure.
//
// Purpose: Synchronize local database replica with Turso cloud database.
//
// What is "sync"?
//   - Pulls latest data from Turso primary database
//   - Updates local SQLite replica with new changes
//   - Ensures local copy is current with cloud state
//   - Different behavior based on connector type:
//   - Embedded mode: Uses libraryQL embedded replica sync
//   - Remote mode: Typically just pings to verify connectivity
//
// Sync Behavior by Mode:
//
//	Embedded Mode (connector_embed.go):
//	  - Calls connector.Sync() from go-libsql
//	  - Downloads WAL frames from Turso
//	  - Applies changes to local replica
//	  - Returns sync generation number
//
//	Remote Mode (connector_remote.go):
//	  - Executes Ping() to verify connection
//	  - No actual sync (queries go directly to Turso)
//	  - Used for connection health check
//
// When is this called?
//   - Periodically by Raft leader (every SyncInterval)
//   - Only on leader node (followers replicate via Raft)
//   - After detecting database changes
//   - Before creating Raft snapshots
//
// Leadership Pattern:
//
//	if mgr.IsLeader() {
//	    for _, key := range cm.GetAllKeys() {
//	        cm.SyncDatabase(ctx, key)  // Pull latest from Turso
//	    }
//	    mgr.Snapshot()  // Distribute to followers via Raft
//	}
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - dbKey: Database to sync (e.g., "hstles.core")
//
// Returns:
//   - error: Database not found or sync operation failure
func (cm *ConnectorManager) SyncDatabase(ctx context.Context, dbKey string) error {
	handle, exists := cm.connectors[dbKey]
	if !exists {
		return fmt.Errorf("database %s not found", dbKey)
	}
	return handle.Sync(ctx)
}

// Close closes all managed database handles.
//
// Purpose: Graceful shutdown of all database connections.
//
// Shutdown Process:
//  1. Iterates through all registered database handles
//  2. Calls close() on each handle
//  3. Closes underlying libraryQL connections
//  4. Cleans up connection pool
//  5. Returns last error (if any)
//
// When is this called?
//   - During Manager.Shutdown()
//   - Before process termination
//   - During cleanup after errors
//
// Error Handling:
//   - Continues closing remaining connections even if one fails
//   - Returns last encountered error
//   - Logs errors but doesn't stop process
//
// Connection Cleanup:
//   - Closes *sql.DB connections
//   - In embedded mode: also closes libraryQL connector
//   - Releases file handles
//   - Clears connection map
//
// Safety:
//   - Idempotent (safe to call multiple times)
//   - Doesn't error on empty connection pool
//   - Prevents resource leaks
//
// Called From:
//   - Manager.Shutdown() as part of graceful shutdown
//   - defer statements for cleanup
//
// Returns:
//   - error: Last error encountered (nil if all closed successfully)
//
// Example:
//
//	defer cm.Close()  // Ensure cleanup on exit
func (cm *ConnectorManager) Close() error {
	var lastErr error
	for key, handle := range cm.connectors {
		if err := handle.Close(); err != nil {
			lastErr = fmt.Errorf("failed to close database %s: %w", key, err)
		}
	}
	cm.connectors = make(map[string]*database.DatabaseHandle)
	return lastErr
}

// testConnector verifies basic connectivity and query capability for a DB handle.
//
// Purpose: Pre-flight validation of database connections before registration.
//
// Validation Steps:
//
//  1. Ping Test:
//     - Verifies network connectivity to Turso
//     - Checks authentication token validity
//     - Ensures database server is responding
//     - Fails fast if credentials are wrong
//
//  2. Query Test:
//     - Executes simple "SELECT 1" query
//     - Verifies query execution capability
//     - Ensures database is not in read-only/corrupt state
//     - Validates result scanning works
//
// Why both tests?
//   - Ping: Tests connection at network/auth level
//   - Query: Tests actual database functionality
//   - Together: Comprehensive validation
//
// When is this called?
//   - During AddConnector() before accepting new connection
//   - After makeConnector() creates libraryQL connection
//   - Before adding to connection pool
//
// Common Failures:
//   - Invalid auth token → Ping fails
//   - Wrong database URL → Ping fails
//   - Network issues → Ping timeout
//   - Database corruption → Query fails
//   - Permission issues → Query fails
//
// Parameters:
//   - db: Standard *sql.DB handle to test
//
// Returns:
//   - error: Connectivity or query execution failure
//
// Example output on success:
//
//	SELECT 1 → result=1 → nil error
//
// Example output on failure:
//
//	Ping error: "authentication failed"
//	Query error: "database is locked"
func testConnector(db *sql.DB) error {
	if err := db.Ping(); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	row := db.QueryRow("SELECT 1")
	var result int
	if err := row.Scan(&result); err != nil {
		return fmt.Errorf("failed to execute test query: %w", err)
	}

	if result != 1 {
		return fmt.Errorf("unexpected test query result: got %d, expected 1", result)
	}

	return nil
}
