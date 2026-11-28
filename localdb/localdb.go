// Package localdb opens a database file that one process owns, through the
// tursogo engine, encrypted at rest and pooled by policypool.
//
// Consumed by the Manager for a pureLocal group's databases (OpenFile, kind
// tursoraftLocal), by connectors.MakeConnector (Open), by loom's node runtime
// for its LAD file (OpenFile, kind lad); Open and OpenFile call IsPlaintext
// themselves before the engine opens the file. One opener serves several
// kinds of file, so the kind comes from the caller. A local file is
// authoritative: nothing else holds its rows, so nothing here ever deletes
// one.
package localdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bbmumford/policypool"
	_ "turso.tech/database/tursogo" // registers driver "turso"
)

// BusyTimeoutMillis is how long a connection waits for another connection's
// write lock before failing with "database is locked".
const BusyTimeoutMillis = 5000

// KeyLen is the only key length accepted.
const KeyLen = 32

// Errors Open returns before touching the file.
var (
	// ErrKeyLength means the key is not exactly KeyLen bytes.
	ErrKeyLength = fmt.Errorf("tursoraft/localdb: key must be exactly %d bytes", KeyLen)
	// ErrPlaintext means a key is configured but the file on disk is a
	// plaintext SQLite database. The file is left in place: it is the only
	// copy of its rows, and an operator decides whether to export it.
	ErrPlaintext = errors.New("tursoraft/localdb: file is a plaintext SQLite database but a key is configured")
	// ErrRelativePath means path is not absolute. The pool's DSN carries the
	// path, and a relative one resolves against whatever directory the process
	// was started in.
	ErrRelativePath = errors.New("tursoraft/localdb: path must be absolute")
)

// sqliteHeader is the first 16 bytes of every plaintext SQLite database file.
var sqliteHeader = []byte("SQLite format 3\x00")

// Open opens the file at path encrypted under key (aes256gcm), creating it and
// its directory when absent, and returns a pool on which every connection has
// foreign keys on.
//
// A wrong key fails here: the schema is read once before the pool is returned.
func Open(ctx context.Context, path string, key []byte) (*sql.DB, error) {
	if len(key) != KeyLen {
		return nil, ErrKeyLength
	}
	if err := prepare(path); err != nil {
		return nil, err
	}
	if plain, err := IsPlaintext(path); err != nil {
		return nil, err
	} else if plain {
		return nil, fmt.Errorf("%w: %s", ErrPlaintext, path)
	}
	return open(ctx, path, DSN(path, key), nil)
}

// OpenFile is Open for a file that carries identity in its header
// (policypool.OpenFile): a new file is stamped with identity's kind and
// generation once, however many openers race; a file of that kind at a
// generation identity reads opens; any other file, one stamped with another
// kind or generation or an unstamped one that already holds a schema, is
// refused with policypool.ErrFileIdentity and left as it is. Every connection
// of the pool then refuses a statement assigning application_id or
// user_version.
//
// A refusal is the caller's to act on by what the file is: a cache's opener
// rebuilds it, an authoritative file's opener stops and reports it.
func OpenFile(ctx context.Context, path string, key []byte, identity policypool.Identity) (*sql.DB, error) {
	if len(key) != KeyLen {
		return nil, ErrKeyLength
	}
	if err := prepare(path); err != nil {
		return nil, err
	}
	if plain, err := IsPlaintext(path); err != nil {
		return nil, err
	} else if plain {
		return nil, fmt.Errorf("%w: %s", ErrPlaintext, path)
	}
	return open(ctx, path, DSN(path, key), &identity)
}

func prepare(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: %q", ErrRelativePath, path)
	}
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

// open opens the pool, through policypool.OpenFile when identity is set.
func open(ctx context.Context, path, dsn string, identity *policypool.Identity) (*sql.DB, error) {
	var db *sql.DB
	var err error
	if identity == nil {
		db, err = policypool.Open(ctx, "turso", dsn, policypool.ForeignKeysRequired)
	} else {
		db, err = policypool.OpenFile(ctx, "turso", dsn, policypool.ForeignKeysRequired, *identity)
	}
	if err != nil {
		return nil, fmt.Errorf("tursoraft/localdb: open %s: %w", path, err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
		db.Close()
		return nil, fmt.Errorf("tursoraft/localdb: read the schema of %s: %w", path, err)
	}
	return db, nil
}

// DSN is the tursogo data source name for path under key; a nil key means
// plaintext.
//
// tursogo refuses a keyed open unless the encryption feature is named, and
// refuses to open a file whose schema defines a trigger unless the triggers
// feature is named, so both are on for every keyed file.
func DSN(path string, key []byte) string {
	q := url.Values{}
	q.Set("_busy_timeout", strconv.Itoa(BusyTimeoutMillis))
	if key == nil {
		q.Set("experimental", "triggers")
	} else {
		q.Set("experimental", "encryption,triggers")
		q.Set("encryption_cipher", "aes256gcm")
		q.Set("encryption_hexkey", hex.EncodeToString(key))
	}
	return path + "?" + q.Encode()
}

// IsPlaintext reports whether the file at path begins with the plaintext
// SQLite header. A missing file, or one shorter than the header, is a database
// not yet written and reports false.
func IsPlaintext(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(f, head); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(head, sqliteHeader), nil
}
