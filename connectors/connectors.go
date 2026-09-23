// Package connectors opens one database outside a Manager.
//
// Consumed by loom's node runtime, which opens its LAD file through
// MakeConnector. A Manager does not use this package: it opens replicas and
// local files through the replica and localdb packages directly.
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

// MakeConnector opens the database at dbPath.
//
// With a primaryURL it opens an encrypted embedded replica of that primary
// (replica.Open), which needs authToken, a key and a cgo build. Without one it
// opens a local file through tursogo, encrypted under encryptionKey.
//
// encryptionKey is base64 of exactly 32 bytes. An empty key opens a local file
// in plaintext.
// TODO: refuse an empty key once loom's node runtime passes one for its LAD
// file; it calls MakeConnector(path, "", "", "") today and would fail to boot.
func MakeConnector(dbPath, primaryURL, authToken, encryptionKey string) (*database.DatabaseHandle, error) {
	if dbPath == "" {
		return nil, errors.New("tursoraft/connectors: database path is empty")
	}
	path, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("tursoraft/connectors: %w", err)
	}
	var key []byte
	if encryptionKey != "" {
		key, err = base64.StdEncoding.DecodeString(encryptionKey)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("tursoraft/connectors: encryption key must be base64 of exactly 32 bytes")
		}
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
		h.AddCloser(r.Close)
		return h, nil
	}

	if key == nil {
		db, err := localdb.OpenPlaintext(ctx, path)
		if err != nil {
			return nil, err
		}
		return database.NewDatabaseHandle(db), nil
	}
	db, err := localdb.Open(ctx, path, key)
	if err != nil {
		return nil, err
	}
	return database.NewDatabaseHandle(db), nil
}
