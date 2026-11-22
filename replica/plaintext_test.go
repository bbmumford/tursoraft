//go:build cgo

package replica

import (
	"bytes"
	"context"
	"crypto/rand"
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
	openNative = func(cfg Config) (native, error) {
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
		Path: path, URL: "http://127.0.0.1:1", AuthToken: "t", Key: make([]byte, 32),
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

// libSQL's HTTP sync protocol keeps no local key; its companion file beside a
// replica file with no header yet is refused too.
func TestOpenRefusesAnHTTPSyncReplicaWithNoHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.db")
	closed := withUnencryptedEngine(t, func(p string) {
		for _, f := range []string{p, p + infoSuffix} {
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	r, err := Open(context.Background(), plaintextSpec(path))
	if r != nil {
		r.Close()
		t.Fatalf("Open served a replica beside %s with no header, err = %v", infoSuffix, err)
	}
	if !errors.Is(err, ErrPlaintextReplica) || !strings.Contains(err.Error(), infoSuffix) {
		t.Errorf("err = %v, want ErrPlaintextReplica naming %s", err, infoSuffix)
	}
	if !closed.Load() {
		t.Error("the engine was left open")
	}
}

func TestUnencryptedJudgesTheHeaderAndTheInfoFile(t *testing.T) {
	random := make([]byte, 4096)
	rand.Read(random)
	fixture, err := os.ReadFile(filepath.Join("testdata", "plaintext-replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		main    []byte // nil: no file
		info    bool
		refused bool
	}{
		{"unencrypted", fixture, false, true},
		{"unencrypted, HTTP sync", fixture, true, true},
		{"empty, HTTP sync", []byte{}, true, true},
		{"missing, HTTP sync", nil, true, true},
		{"short, HTTP sync", fixture[:10], true, true},
		{"encrypted, HTTP sync", random, true, false},
		{"encrypted", random, false, false},
		{"empty", []byte{}, false, false},
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
			if err := os.WriteFile(path+infoSuffix, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		why, err := unencrypted(path)
		if err != nil || (why != "") != tc.refused {
			t.Errorf("%s: why=%q err=%v, want refused=%v", tc.name, why, err, tc.refused)
		}
	}

	// A path that cannot be read as a file is not passed as encrypted.
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "r.db")
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
