// Package connectors opens one database outside a Manager, encrypted at rest:
// an embedded replica of a primary when given the primary's URL, else a local
// file.
//
// A Manager does not use this package: it opens replicas and local files
// through the replica and localdb packages directly.
package connectors

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/bbmumford/tursoraft/database"
	"github.com/bbmumford/tursoraft/localdb"
	"github.com/bbmumford/tursoraft/replica"
)

// openTimeout bounds MakeConnector, which has no context parameter.
const openTimeout = 60 * time.Second

// Errors MakeConnector returns before it touches the disk or the network.
var (
	// ErrNoKey means encryptionKey is empty. Every file MakeConnector opens
	// is encrypted, so there is no keyless open to fall back to.
	ErrNoKey = errors.New("tursoraft/connectors: encryptionKey is empty; every database file is encrypted")
	// ErrBadKey means encryptionKey is not base64 of exactly 32 bytes.
	ErrBadKey = errors.New("tursoraft/connectors: encryptionKey must be base64 of exactly 32 bytes")
)

// MakeConnector opens the database at dbPath encrypted under encryptionKey,
// base64 of exactly 32 bytes. An empty key is ErrNoKey and a malformed one
// ErrBadKey, both before anything is created on disk.
//
// With a primaryURL it opens an encrypted embedded replica of that primary
// (replica.Open), which needs authToken and a cgo build. Without one it opens
// a local file through tursogo (localdb.Open).
func MakeConnector(dbPath, primaryURL, authToken, encryptionKey string) (*database.DatabaseHandle, error) {
	if dbPath == "" {
		return nil, errors.New("tursoraft/connectors: database path is empty")
	}
	if encryptionKey == "" {
		return nil, ErrNoKey
	}
	key, err := base64.StdEncoding.DecodeString(encryptionKey)
	if err != nil || len(key) != 32 {
		return nil, ErrBadKey
	}
	path, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("tursoraft/connectors: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()

	if primaryURL != "" {
		r, err := replica.Open(ctx, replica.Config{Path: path, URL: primaryURL, AuthToken: authToken, Key: key, OpenTimeout: openTimeout})
		if err != nil {
			return nil, err
		}
		h := database.NewDatabaseHandle(r.DB())
		h.WithSync(r.Sync)
		h.WithSyncStats(func(context.Context) (database.SyncStats, error) { return r.SyncStats(), nil })
		h.WithPoolCloser(r.Close)
		return h, nil
	}

	db, err := localdb.Open(ctx, path, key)
	if err != nil {
		return nil, err
	}
	return database.NewDatabaseHandle(db), nil
}
