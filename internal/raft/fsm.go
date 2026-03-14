package raft

import (
	"archive/tar"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	hashiraft "github.com/hashicorp/raft"

	"github.com/bbmumford/tursoraft/connectors"
)

// LogEntry represents a database operation to be replicated across the Raft cluster
type LogEntry struct {
	Group  string   `json:"group"`   // Turso group name
	DBName string   `json:"db_name"` // Database name within the group
	SQL    string   `json:"sql"`     // SQL statement to execute
	Args   [][]byte `json:"args"`    // Serialized arguments for the SQL statement
	WAL    []byte   `json:"wal"`     // WAL frames (for future use)
	Type   string   `json:"type"`    // Operation type: "exec" or "wal"
}

// MultiFSM implements the Raft FSM (Finite State Machine) interface
// It manages state changes across all database connectors in the mesh
type MultiFSM struct {
	connectorManager *connectors.ConnectorManager
	dbPaths          map[string]string // key: group.db
	onApplied        func()            // optional callback when a log entry is applied
}

// NewMultiFSM creates a new multi-database FSM
func NewMultiFSM(connectorManager *connectors.ConnectorManager, dbPaths map[string]string, onApplied func()) *MultiFSM {
	return &MultiFSM{
		connectorManager: connectorManager,
		dbPaths:          dbPaths,
		onApplied:        onApplied,
	}
}

// Apply is called by Raft when a log entry needs to be applied to the state machine.
// This method must be deterministic and should not fail (or the cluster may become inconsistent).
func (fsm *MultiFSM) Apply(log *hashiraft.Log) interface{} {
	// Parse the log entry
	var entry LogEntry
	if err := json.Unmarshal(log.Data, &entry); err != nil {
		return fmt.Errorf("failed to unmarshal log entry: %w", err)
	}

	// Construct database key
	dbKey := fmt.Sprintf("%s.%s", entry.Group, entry.DBName)

	// Get the database connection
	db, exists := fsm.connectorManager.GetDatabase(dbKey)
	if !exists {
		return fmt.Errorf("database %s not found", dbKey)
	}

	// Execute based on operation type
	switch entry.Type {
	case "exec":
		return fsm.applyExec(db, &entry)
	case "wal":
		return fsm.applyWAL(db, &entry)
	default:
		return fmt.Errorf("unknown operation type: %s", entry.Type)
	}
}

// applyExec executes a SQL statement on the specified database
func (fsm *MultiFSM) applyExec(db *sql.DB, entry *LogEntry) interface{} {
	// Deserialize arguments
	args := make([]interface{}, len(entry.Args))
	for i, argBytes := range entry.Args {
		var arg interface{}
		if err := json.Unmarshal(argBytes, &arg); err != nil {
			return fmt.Errorf("failed to unmarshal argument %d: %w", i, err)
		}
		args[i] = arg
	}

	// Execute the SQL statement
	ctx := context.Background()
	_, err := db.ExecContext(ctx, entry.SQL, args...)
	if err != nil {
		return fmt.Errorf("failed to execute SQL: %w", err)
	}

	if fsm.onApplied != nil {
		fsm.onApplied()
	}

	return nil
}

// applyWAL applies WAL frames to the database (placeholder for future implementation)
func (fsm *MultiFSM) applyWAL(_ *sql.DB, _ *LogEntry) interface{} {
	// WAL frame application would be implemented here
	// For now, return an error as this feature is not yet implemented
	return fmt.Errorf("WAL frame application not yet implemented")
}

// Snapshot creates a snapshot of the current state.
// This is called by Raft to create checkpoints for faster recovery.
func (fsm *MultiFSM) Snapshot() (hashiraft.FSMSnapshot, error) {
	// Create a tar archive snapshot of all managed .db files
	keys := fsm.connectorManager.GetAllKeys()
	paths := make([]string, 0, len(keys))
	for _, k := range keys {
		if p, ok := fsm.dbPaths[k]; ok {
			paths = append(paths, p)
		}
	}
	return &fsmSnapshot{paths: paths}, nil
}

// Restore restores the state machine from a snapshot.
// This is called during cluster recovery or when a new node joins.
func (fsm *MultiFSM) Restore(reader io.ReadCloser) error {
	defer reader.Close()
	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("snapshot restore: read next: %w", err)
		}
		// hdr.Name is a base name; find destination by matching
		base := filepath.Base(hdr.Name)
		var dest string
		for _, p := range fsm.dbPaths {
			if filepath.Base(p) == base {
				dest = p
				break
			}
		}
		if dest == "" {
			// Unknown file, skip
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return err
			}
			continue
		}
		// Ensure dir exists
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		f, err := os.Create(dest)
		if err != nil {
			return fmt.Errorf("restore create %s: %w", dest, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return fmt.Errorf("restore write %s: %w", dest, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

// fsmSnapshot represents a point-in-time snapshot of the FSM state
type fsmSnapshot struct {
	paths []string // absolute .db paths to include in tar stream
}

// Persist writes the snapshot to the provided sink.
// This is called by Raft to store the snapshot durably.
func (s *fsmSnapshot) Persist(sink hashiraft.SnapshotSink) error {
	tw := tar.NewWriter(sink)
	for _, p := range s.paths {
		fi, err := os.Stat(p)
		if err != nil {
			tw.Close()
			sink.Cancel()
			return fmt.Errorf("snapshot stat %s: %w", p, err)
		}
		hdr := &tar.Header{
			Name: filepath.Base(p),
			Mode: 0o644,
			Size: fi.Size(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			tw.Close()
			sink.Cancel()
			return fmt.Errorf("snapshot header %s: %w", p, err)
		}
		f, err := os.Open(p)
		if err != nil {
			tw.Close()
			sink.Cancel()
			return fmt.Errorf("snapshot open %s: %w", p, err)
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			tw.Close()
			sink.Cancel()
			return fmt.Errorf("snapshot copy %s: %w", p, err)
		}
		f.Close()
	}
	if err := tw.Close(); err != nil {
		sink.Cancel()
		return err
	}
	if err := sink.Close(); err != nil {
		return err
	}
	return nil
}

// Release is called when the snapshot is no longer needed.
// This allows for cleanup of any resources held by the snapshot.
func (s *fsmSnapshot) Release() {
	// No resources to clean up in our simple implementation
}

// serializeArgs converts interface{} arguments to [][]byte for JSON serialization
func serializeArgs(args []interface{}) ([][]byte, error) {
	serialized := make([][]byte, len(args))

	for i, arg := range args {
		data, err := json.Marshal(arg)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize argument %d: %w", i, err)
		}
		serialized[i] = data
	}

	return serialized, nil
}

// createLogEntry creates a new log entry for SQL execution
func CreateLogEntry(group, dbName, sql string, args []interface{}) (*LogEntry, error) {
	serializedArgs, err := serializeArgs(args)
	if err != nil {
		return nil, err
	}

	return &LogEntry{
		Group:  group,
		DBName: dbName,
		SQL:    sql,
		Args:   serializedArgs,
		Type:   "exec",
	}, nil
}
