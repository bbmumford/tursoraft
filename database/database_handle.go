package database

import (
	"context"
	"database/sql"
	"errors"
)

// ErrSyncStatsUnavailable is what Stats returns when no stats function is set,
// which is every handle that is not an embedded replica: a pure-local database
// and a direct-to-primary connection have no local-versus-remote divergence to
// report.
//
// It is a distinct error rather than a zero SyncStats because the two mean
// opposite things to a caller deciding whether a replica is healthy. Zero
// PendingLocalWrites is the GOOD state — everything written here has been sent.
// No stats at all is the UNKNOWN state. Returning the zero value for both would
// let a health check read an unmeasurable handle as a fully-synced one.
var ErrSyncStatsUnavailable = errors.New("tursoraft: sync stats are not available on this handle")

// SyncStats reports how far an embedded replica's local database has diverged
// from its remote, in the terms the sync driver keeps.
//
// The fields carry the driver's own meanings. PendingLocalWrites counts local
// operations written since the last pull, so it grows as writes land and falls
// when they are sent onward; LastPushUnixTime stays 0 until a push has ever
// succeeded. A handle whose writes are never sent therefore reports a
// LastPushUnixTime of 0 and a PendingLocalWrites that only climbs — the pair a
// caller reads to tell a replica that is keeping up from one that is quietly
// accumulating.
//
// It is a neutral struct rather than the driver's own so this package keeps no
// driver dependency and the non-embedded build keeps its current import set;
// the connector that has the driver maps one into the other.
type SyncStats struct {
	// PendingLocalWrites is the count of local operations written since the
	// last pull.
	PendingLocalWrites int64
	// LastPullUnixTime is the time of the last successful pull, 0 if never.
	LastPullUnixTime int64
	// LastPushUnixTime is the time of the last successful push, 0 if never.
	LastPushUnixTime int64
	// Revision is the opaque server revision. Report it; never parse it.
	Revision string
}

// DatabaseHandle wraps a database connection with sync and cleanup capabilities.
// This is the public interface for tursoraft database handles.
type DatabaseHandle struct {
	db         *sql.DB
	syncFunc   func(context.Context) error
	statsFunc  func(context.Context) (SyncStats, error)
	closeFuncs []func() error
}

// NewDatabaseHandle creates a new database handle wrapper
func NewDatabaseHandle(db *sql.DB) *DatabaseHandle {
	return &DatabaseHandle{db: db}
}

// WithSync sets the sync function for this database handle
func (h *DatabaseHandle) WithSync(sync func(context.Context) error) {
	h.syncFunc = sync
}

// WithSyncStats sets the function that reports this handle's replica divergence.
// Only a connector holding an embedded sync database sets it; the rest leave it
// nil and Stats reports ErrSyncStatsUnavailable rather than a healthy-looking
// zero.
func (h *DatabaseHandle) WithSyncStats(stats func(context.Context) (SyncStats, error)) {
	h.statsFunc = stats
}

// Stats reports how far this handle's local database has diverged from its
// remote, or ErrSyncStatsUnavailable when the handle keeps no replica.
//
// Callers reading it to decide whether a replica is healthy must treat that
// error as unknown, never as synced — see ErrSyncStatsUnavailable.
func (h *DatabaseHandle) Stats(ctx context.Context) (SyncStats, error) {
	if h.statsFunc == nil {
		return SyncStats{}, ErrSyncStatsUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return SyncStats{}, ctx.Err()
	default:
	}
	return h.statsFunc(ctx)
}

// AddCloser adds a cleanup function to run on close
func (h *DatabaseHandle) AddCloser(fn func() error) {
	if fn == nil {
		return
	}
	h.closeFuncs = append(h.closeFuncs, fn)
}

// Database returns the underlying *sql.DB connection.
// This allows external consumers to access the database directly.
func (h *DatabaseHandle) Database() *sql.DB {
	return h.db
}

// Sync triggers the sync function if configured
func (h *DatabaseHandle) Sync(ctx context.Context) error {
	if h.syncFunc == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return h.syncFunc(ctx)
}

// Close closes the database and runs cleanup functions
func (h *DatabaseHandle) Close() error {
	var errs []error
	if h.db != nil {
		if err := h.db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, fn := range h.closeFuncs {
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
