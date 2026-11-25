//go:build cgo

package replica

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	libsql "github.com/bbmumford/go-libsql"
)

// fileEngine stands in for an engine that left its replica unencrypted, as
// libSQL does when the primary answers its HTTP sync protocol: it serves the
// file at the replica's path through go-libsql's local connector, with no
// key.
type fileEngine struct {
	*libsql.Connector
	closed *atomic.Bool
}

func (e fileEngine) Sync() (uint64, error) { return 1, nil }

func (e fileEngine) Close() error {
	e.closed.Store(true)
	return e.Connector.Close()
}

// withUnencryptedEngine makes openNative lay out the replica's files with
// layout and serve them unencrypted; the returned flag reports whether the
// engine was closed.
func withUnencryptedEngine(t *testing.T, layout func(path string)) *atomic.Bool {
	t.Helper()
	closed := &atomic.Bool{}
	saved := openNative
	openNative = func(cfg Config, _ string) (native, error) {
		layout(cfg.Path)
		c, err := libsql.NewLocalConnector(cfg.Path)
		if err != nil {
			return nil, err
		}
		return fileEngine{Connector: c, closed: closed}, nil
	}
	t.Cleanup(func() { openNative = saved })
	return closed
}

func copyFixture(t *testing.T, to string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "plaintext-replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func plaintextSpec(path string) Config {
	return Config{
		Path: path, URL: "http://127.0.0.1:1", Mode: ReadWrite, Credentials: StaticCredential("t"), Key: make([]byte, 32),
		SyncInterval: time.Hour, OpenTimeout: time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// A replica file the engine wrote unencrypted under a configured key is
// refused after the open, the engine closed, and nothing served from it.
func TestOpenRefusesAReplicaLeftUnencrypted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.db")
	closed := withUnencryptedEngine(t, func(p string) { copyFixture(t, p) })

	r, err := Open(context.Background(), plaintextSpec(path))
	if r != nil {
		var body string
		qerr := r.DB().QueryRow("SELECT body FROM notes").Scan(&body)
		r.Close()
		t.Fatalf("Open served the unencrypted replica (read %q, %v), err = %v", body, qerr, err)
	}
	if !errors.Is(err, ErrPlaintextReplica) || !strings.Contains(err.Error(), "unencrypted SQLite header") {
		t.Errorf("err = %v, want ErrPlaintextReplica naming the header", err)
	}
	if !closed.Load() {
		t.Error("the engine was left open")
	}
}

// walWithPageOne writes a WAL of page size 4096 whose frames hold the pages
// named, page 1's bytes beginning with first.
func walWithPageOne(t *testing.T, path string, pages []uint32, first []byte) {
	t.Helper()
	const pageSize = 4096
	b := make([]byte, 32)
	binary.BigEndian.PutUint32(b[0:4], 0x377f0682)
	binary.BigEndian.PutUint32(b[4:8], 3007000)
	binary.BigEndian.PutUint32(b[8:12], pageSize)
	for _, pg := range pages {
		frame := make([]byte, 24+pageSize)
		binary.BigEndian.PutUint32(frame[0:4], pg)
		rand.Read(frame[24:])
		if pg == 1 {
			copy(frame[24:], first)
		}
		b = append(b, frame...)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Before the engine has written page 1 into the database file it may hold it
// only in the WAL; a plaintext page 1 there is refused too.
func TestOpenRefusesAReplicaWhosePageOneIsUnencryptedInTheWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.db")
	closed := withUnencryptedEngine(t, func(p string) {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		walWithPageOne(t, p+"-wal", []uint32{1}, sqliteHeader)
	})
	r, err := Open(context.Background(), plaintextSpec(path))
	if r != nil {
		r.Close()
		t.Fatalf("Open served a replica whose page 1 in the WAL is unencrypted, err = %v", err)
	}
	if !errors.Is(err, ErrPlaintextReplica) || !strings.Contains(err.Error(), "WAL") {
		t.Errorf("err = %v, want ErrPlaintextReplica naming the WAL", err)
	}
	if !closed.Load() {
		t.Error("the engine was left open")
	}
}

// The next Open deletes a replica refused for an unencrypted page 1 in the
// WAL, with its companions, before the engine opens it, as it does one whose
// database file begins with the header; a WAL whose page 1 is encrypted is
// kept.
func TestOpenDeletesAReplicaWhosePageOneIsUnencryptedInTheWAL(t *testing.T) {
	random := make([]byte, 16)
	rand.Read(random)
	for _, tc := range []struct {
		name    string
		walHead []byte
		deleted bool
	}{
		{name: "unencrypted page 1 in the WAL", walHead: sqliteHeader, deleted: true},
		{name: "encrypted page 1 in the WAL", walHead: random},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "r.db")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			walWithPageOne(t, path+"-wal", []uint32{1}, tc.walHead)
			if err := os.WriteFile(path+"-info", []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			var left []string
			withUnencryptedEngine(t, func(p string) {
				for _, suffix := range []string{"-wal", "-info"} {
					if _, err := os.Stat(p + suffix); err == nil {
						left = append(left, suffix)
					}
				}
			})
			r, err := Open(context.Background(), plaintextSpec(path))
			if r != nil {
				r.Close()
			}
			if tc.deleted {
				if len(left) != 0 {
					t.Fatalf("the engine opened beside the refused replica's %v, want it deleted first", left)
				}
				if err != nil {
					t.Fatalf("Open after the deletion: %v", err)
				}
			} else if len(left) != 2 {
				t.Fatalf("the engine opened beside %v, want the replica with an encrypted page 1 kept", left)
			}
		})
	}
}

func TestUnencryptedJudgesPageOne(t *testing.T) {
	random := make([]byte, 4096)
	rand.Read(random)
	fixture, err := os.ReadFile(filepath.Join("testdata", "plaintext-replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		main    []byte   // nil: no file
		info    bool     // the HTTP sync protocol's -info file beside it
		wal     []uint32 // the pages of a WAL beside it, none: no WAL
		walHead []byte   // the bytes page 1 begins with in that WAL
		refused bool
	}{
		{name: "unencrypted file", main: fixture, refused: true},
		{name: "unencrypted file, -info", main: fixture, info: true, refused: true},
		{name: "encrypted file", main: random},
		{name: "encrypted file, -info (a keyed HTTP sync replica)", main: random, info: true},
		{name: "empty file, -info, no WAL", main: []byte{}, info: true},
		{name: "no file, -info, no WAL", info: true},
		{name: "empty file, unencrypted page 1 in the WAL", main: []byte{}, wal: []uint32{1}, walHead: sqliteHeader, refused: true},
		{name: "no file, unencrypted page 1 after page 2 in the WAL", wal: []uint32{2, 1}, walHead: sqliteHeader, refused: true},
		{name: "empty file, encrypted page 1 in the WAL", main: []byte{}, wal: []uint32{1}, walHead: random[:16]},
		{name: "empty file, a WAL without page 1", main: []byte{}, wal: []uint32{2, 3}},
		{name: "short file, unencrypted page 1 in the WAL", main: fixture[:10], wal: []uint32{1}, walHead: sqliteHeader, refused: true},
		{name: "encrypted file, unencrypted page 1 in the WAL", main: random, wal: []uint32{1}, walHead: sqliteHeader},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, "r.db")
		if tc.main != nil {
			if err := os.WriteFile(path, tc.main, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if tc.info {
			if err := os.WriteFile(path+"-info", []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if tc.wal != nil {
			walWithPageOne(t, path+"-wal", tc.wal, tc.walHead)
		}
		why, err := unencrypted(path)
		if err != nil || (why != "") != tc.refused {
			t.Errorf("%s: why=%q err=%v, want refused=%v", tc.name, why, err, tc.refused)
		}
	}

	// A file that is not a WAL is not read as one.
	dir := t.TempDir()
	path := filepath.Join(dir, "r.db")
	b := make([]byte, 32+24+4096)
	binary.BigEndian.PutUint32(b[8:12], 4096)
	binary.BigEndian.PutUint32(b[32:36], 1)
	copy(b[32+24:], sqliteHeader)
	if err := os.WriteFile(path+"-wal", b, 0o600); err != nil {
		t.Fatal(err)
	}
	if why, err := unencrypted(path); err != nil || why != "" {
		t.Errorf("a -wal without the WAL magic: why=%q err=%v, want kept", why, err)
	}

	// A path that cannot be read as a file is not passed as encrypted.
	unreadable := filepath.Join(t.TempDir(), "r.db")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if why, err := unencrypted(unreadable); err == nil && why == "" {
		t.Error("a directory at the replica path was judged encrypted")
	}
	if !bytes.HasPrefix(fixture, sqliteHeader) {
		t.Fatal("the fixture does not begin with the SQLite header")
	}
}
