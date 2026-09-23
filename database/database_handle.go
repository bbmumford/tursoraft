package database

import (
	"context"
	"database/sql"
	"errors"
)

// ErrSyncStatsUnavailable is what Stats returns when no stats function is set,
// which is every handle that is not an embedded replica: a local file has no
// primary to fall behind.
//
// It is a distinct error rather than a zero SyncStats because the two mean
// opposite things to a caller deciding whether a replica is healthy: zero
// figures from a replica are a reading, no stats at all is the unknown state.
var ErrSyncStatsUnavailable = errors.New("tursoraft: sync stats are not available on this handle")

// SyncStats reports how far an embedded replica is behind its primary.
//
// A replica sends every write to the primary before the write returns, so
// nothing waits locally to be sent: PendingLocalWrites and LastPushUnixTime
// are always 0. LastPullUnixTime is the last time the replica pulled from the
// primary successfully, and Revision is the replica's frame number after that
// pull, in decimal.
//
// The four fields keep the shape node.orbtr.io's control-database log line
// reads.
type SyncStats struct {
	// PendingLocalWrites is always 0 for a replica: writes are forwarded to the
	// primary before they return.
	PendingLocalWrites int64
	// LastPullUnixTime is the time of the last successful pull, 0 if never.
	LastPullUnixTime int64
	// LastPushUnixTime is always 0 for a replica: nothing is pushed.
	LastPushUnixTime int64
	// Revision is the replica's frame number after the last pull. Report it;
	// never parse it.
	Revision string
}

// DatabaseHandle wraps a database connection with sync and cleanup capabilities.
// This is the public interface for tursoraft database handles.
type DatabaseHandle struct {
	db         *sql.DB
	syncFunc   func(context.Context) error
	statsFunc  func(context.Context) (SyncStats, error)
	closeFuncs []func() error
	poolCloser func() error
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
// Only a handle holding an embedded replica sets it; the rest leave it
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

// WithPoolCloser makes fn the only way Close closes the pool: Close runs the
// cleanup functions, then fn, and never closes the pool itself.
//
// connectors.MakeConnector sets it to the replica's Close, which waits for a
// pull still inside the replica engine before it closes the pool. When that
// wait runs out, the replica's Close returns an error and closes the pool
// once the pull returns; a handle that closed the pool itself at that point
// would close the connector under the pull.
func (h *DatabaseHandle) WithPoolCloser(fn func() error) {
	h.poolCloser = fn
}

// Close runs the cleanup functions, in the order they were added, and then
// closes the pool, through the pool closer when one is set.
//
// The cleanup functions run first because a replica's sync loop drives the
// connector the pool owns, and must stop before the pool closes it.
func (h *DatabaseHandle) Close() error {
	var errs []error
	for _, fn := range h.closeFuncs {
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	switch {
	case h.poolCloser != nil:
		if err := h.poolCloser(); err != nil {
			errs = append(errs, err)
		}
	case h.db != nil:
		if err := h.db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
