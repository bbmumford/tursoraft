package database

import (
	"context"
	"database/sql"
	"errors"
)

// DatabaseHandle wraps a database connection with sync and cleanup capabilities.
// This is the public interface for tursoraft database handles.
type DatabaseHandle struct {
	db         *sql.DB
	syncFunc   func(context.Context) error
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
