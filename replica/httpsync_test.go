//go:build cgo

package replica

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	libsql "github.com/bbmumford/go-libsql"
)

// libSQL's embedded replica takes the HTTP sync protocol whenever the primary
// answers GET /info: GET /export/{generation} is the database file the replica
// starts from, GET /sync/{generation}/{start}/{end} the WAL frames after it (a
// 24-byte header and one 4096-byte page each), and a 400 carrying the
// generation answers a start past the last frame. On that path the engine
// keeps a plaintext -info file beside the replica whatever its key, so these
// tests judge the check after the open on the files a real engine writes
// there, served by the in-process primary below with no network and no sqld.

const (
	httpSyncExportMarker = "HTTP-SYNC-EXPORT-ROW-3f1a"
	httpSyncFrameMarker  = "HTTP-SYNC-FRAME-ROW-9c07"
	httpSyncRows         = 200
	httpSyncPageSize     = 4096
)

// httpSyncPrimary serves a plaintext database over the HTTP sync protocol and
// returns its URL: httpSyncRows rows holding httpSyncExportMarker in the
// exported file and httpSyncRows holding httpSyncFrameMarker in the WAL frames
// of generation 1 after it.
func httpSyncPrimary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "primary.db")
	c, err := libsql.NewLocalConnector(path)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1)
	run := func(q string) {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("primary: %s: %v", q, err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("primary: %s: %v", q, err)
		}
		rows.Close()
	}
	fill := func(marker string) {
		q := fmt.Sprintf("WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < %d) "+
			"INSERT INTO t(v) SELECT '%s-' || i FROM c", httpSyncRows, marker)
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("primary: %v", err)
		}
	}
	run("PRAGMA journal_mode = WAL")
	if _, err := db.Exec("CREATE TABLE t(v TEXT)"); err != nil {
		t.Fatalf("primary: %v", err)
	}
	fill(httpSyncExportMarker)
	run("PRAGMA wal_checkpoint(TRUNCATE)")
	export, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(export, sqliteHeader) || len(export)%httpSyncPageSize != 0 || len(export) <= httpSyncPageSize {
		t.Fatalf("primary export: %d bytes, %v", len(export), err)
	}
	run("PRAGMA wal_autocheckpoint = 0")
	fill(httpSyncFrameMarker)
	wal, err := os.ReadFile(path + "-wal")
	const frameSize = walFrameHeaderSize + httpSyncPageSize
	if err != nil || len(wal) <= walHeaderSize || binary.BigEndian.Uint32(wal[8:12]) != httpSyncPageSize || (len(wal)-walHeaderSize)%frameSize != 0 {
		t.Fatalf("primary WAL: %d bytes, %v", len(wal), err)
	}
	db.Close()
	c.Close()
	frames := wal[walHeaderSize:]
	count := len(frames) / frameSize

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case r.Method == http.MethodGet && len(parts) == 1 && parts[0] == "info":
			w.Write([]byte(`{"current_generation":1}`))
		case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "export" && parts[1] == "1":
			w.Write(export)
		case r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "sync" && parts[1] == "1":
			start, err1 := strconv.Atoi(parts[2])
			end, err2 := strconv.Atoi(parts[3])
			if err1 != nil || err2 != nil || start < 1 || start > count {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"generation":1}`))
				return
			}
			end = min(max(end, start+1), count+1)
			w.Write(frames[(start-1)*frameSize : (end-1)*frameSize])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// httpSyncSpec is a read-only replica of the primary at url, as a device holds
// one, under a read-only token that ends within its grant.
func httpSyncSpec(path, url string, key []byte) Config {
	token := unsignedToken(map[string]any{"a": "ro", "exp": time.Now().Add(10 * time.Minute).Unix()})
	return Config{
		Path: path, URL: url, Mode: ReadOnly, Credentials: StaticCredential(token), Key: key,
		SyncInterval: time.Hour, OpenTimeout: 20 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func httpSyncKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func httpSyncCount(t *testing.T, db *sql.DB, marker string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t WHERE v LIKE ?", marker+"%").Scan(&n); err != nil {
		t.Fatalf("count %s: %v", marker, err)
	}
	return n
}

// httpSyncLeaks names every file in dir that holds a row in plaintext.
func httpSyncLeaks(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range []string{httpSyncExportMarker, httpSyncFrameMarker} {
			if bytes.Contains(b, []byte(m)) {
				found = append(found, e.Name()+" holds "+m)
			}
		}
	}
	return found
}

// requireHTTPSyncLayout fails unless the engine took the HTTP sync path (its
// -info file is there) and the database file holds page 1 for the check to
// judge, beginning with the SQLite header exactly when plain.
func requireHTTPSyncLayout(t *testing.T, path string, plain bool) {
	t.Helper()
	if _, err := os.Stat(path + "-info"); err != nil {
		t.Fatalf("the engine did not take the HTTP sync path (no -info file): %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) < httpSyncPageSize {
		t.Fatalf("the replica file holds no page 1: %d bytes, %v", len(b), err)
	}
	if bytes.HasPrefix(b, sqliteHeader) != plain {
		t.Fatalf("the replica file begins with the SQLite header: %v, want %v", bytes.HasPrefix(b, sqliteHeader), plain)
	}
}

// keylessEngine is the real engine opened without the replica's key, standing
// in for an engine that drops the key on the HTTP sync path: it writes the
// database file and its WAL unencrypted.
type keylessEngine struct {
	engine
	closed *atomic.Bool
}

func (e keylessEngine) Close() error {
	e.closed.Store(true)
	return e.engine.Close()
}

// withKeylessEngine makes openNative open keylessEngine; the returned flag
// reports whether one was closed.
func withKeylessEngine(t *testing.T) *atomic.Bool {
	t.Helper()
	closed := &atomic.Bool{}
	saved := openNative
	openNative = func(cfg Config, token string) (native, error) {
		c, err := libsql.NewEmbeddedReplicaConnector(cfg.Path, cfg.URL,
			libsql.WithAuthToken(token), libsql.WithRawText(true), libsql.WithReadOnly())
		if err != nil {
			return nil, err
		}
		return keylessEngine{engine: engine{c}, closed: closed}, nil
	}
	t.Cleanup(func() { openNative = saved })
	return closed
}

// A keyed replica on the HTTP sync path is an encrypted database file beside
// the protocol's plaintext -info file. Open keeps and serves it, and keeps it
// again when it opens the same files a second time.
func TestOpenServesAKeyedReplicaOnHTTPSync(t *testing.T) {
	url := httpSyncPrimary(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "r.db")
	key := httpSyncKey(t)
	for open := 1; open <= 2; open++ {
		r, err := Open(context.Background(), httpSyncSpec(path, url, key))
		if err != nil {
			t.Fatalf("open %d of a keyed replica on the HTTP sync path: %v", open, err)
		}
		e, f := httpSyncCount(t, r.DB(), httpSyncExportMarker), httpSyncCount(t, r.DB(), httpSyncFrameMarker)
		r.Close()
		if e != httpSyncRows || f != httpSyncRows {
			t.Fatalf("open %d: the replica reads %d exported and %d pulled rows, want %d each", open, e, f, httpSyncRows)
		}
		requireHTTPSyncLayout(t, path, false)
		if leaks := httpSyncLeaks(t, dir); len(leaks) != 0 {
			t.Fatalf("open %d: plaintext rows on disk: %v", open, leaks)
		}
	}
}

// A replica the engine left unencrypted on the HTTP sync path is refused after
// the open: the engine is closed, nothing is served, and the database file and
// every companion the HTTP sync path wrote are deleted before Open returns.
func TestReplicaRefusesPlaintextFileAfterOpen(t *testing.T) {
	url := httpSyncPrimary(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "r.db")
	closed := withKeylessEngine(t)

	r, err := Open(context.Background(), httpSyncSpec(path, url, httpSyncKey(t)))
	if r != nil {
		n := httpSyncCount(t, r.DB(), httpSyncExportMarker)
		r.Close()
		t.Fatalf("Open served the unencrypted replica (%d rows read), err = %v", n, err)
	}
	if !errors.Is(err, ErrPlaintextReplica) || !strings.Contains(err.Error(), "unencrypted SQLite header") {
		t.Errorf("err = %v, want ErrPlaintextReplica naming the header", err)
	}
	if !closed.Load() {
		t.Error("the engine was left open")
	}
	for _, suffix := range replicaFiles {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("r.db%s is still on disk after the refusal (stat: %v)", suffix, err)
		}
	}
	if leaks := httpSyncLeaks(t, dir); len(leaks) != 0 {
		t.Fatalf("plaintext rows left on disk after the refusal: %v", leaks)
	}
}

// Such a refusal leaves no file holding a row in plaintext, and the next Open's
// keyed engine downloads the replica again, encrypted.
func TestTheNextOpenReplacesAPlaintextReplicaOnHTTPSync(t *testing.T) {
	url := httpSyncPrimary(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "r.db")
	spec := httpSyncSpec(path, url, httpSyncKey(t))

	keyed := openNative
	withKeylessEngine(t)
	if r, err := Open(context.Background(), spec); !errors.Is(err, ErrPlaintextReplica) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("setup: the keyless engine's replica: err = %v, want ErrPlaintextReplica", err)
	}
	if leaks := httpSyncLeaks(t, dir); len(leaks) != 0 {
		t.Fatalf("the refusal left plaintext rows on disk: %v", leaks)
	}

	openNative = keyed
	r, err := Open(context.Background(), spec)
	if err != nil {
		t.Fatalf("the next Open: %v", err)
	}
	n := httpSyncCount(t, r.DB(), httpSyncFrameMarker)
	r.Close()
	if n != httpSyncRows {
		t.Fatalf("the replaced replica reads %d rows, want %d", n, httpSyncRows)
	}
	requireHTTPSyncLayout(t, path, false)
	if leaks := httpSyncLeaks(t, dir); len(leaks) != 0 {
		t.Fatalf("plaintext rows left on disk: %v", leaks)
	}
}

// A renewal closes the real engine and opens it again on the same keyed files
// under the new token; the file checked after each swap is kept, and the
// replica goes on pulling and serving its rows.
func TestRenewalKeepsAKeyedReplicaOnHTTPSync(t *testing.T) {
	url := httpSyncPrimary(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "r.db")
	src := &mintingSource{life: 3 * time.Second}
	spec := httpSyncSpec(path, url, httpSyncKey(t))
	spec.Credentials, spec.SyncInterval = src, 100*time.Millisecond
	r, err := Open(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	deadline := time.Now().Add(30 * time.Second)
	for r.gen.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d engine swaps after 30 s, want 2 (credential calls %d)", r.gen.Load(), src.calls.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.emu.RLock()
	failed := r.failed
	r.emu.RUnlock()
	if failed != nil {
		t.Fatalf("a reopened engine was refused: %v", failed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("a pull after the swaps: %v", err)
	}
	if n := httpSyncCount(t, r.DB(), httpSyncFrameMarker); n != httpSyncRows {
		t.Fatalf("after the swaps the replica reads %d rows, want %d", n, httpSyncRows)
	}
	requireHTTPSyncLayout(t, path, false)
	if leaks := httpSyncLeaks(t, dir); len(leaks) != 0 {
		t.Fatalf("plaintext rows on disk: %v", leaks)
	}
}

// Every read that opens a new pool connection while the loop pulls is
// answered: the engine's refusals of a connection while a pull holds the
// database are waited out.
func TestNewConnectionsOpenWhileTheLoopPullsOnHTTPSync(t *testing.T) {
	url := httpSyncPrimary(t)
	spec := httpSyncSpec(filepath.Join(t.TempDir(), "r.db"), url, httpSyncKey(t))
	spec.SyncInterval = 5 * time.Millisecond
	r, err := Open(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.DB().SetMaxIdleConns(0)
	failed := 0
	var first error
	for i := 0; i < 300; i++ {
		var n int
		if err := r.DB().QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
			failed++
			if first == nil {
				first = err
			}
		}
	}
	if failed != 0 {
		t.Fatalf("%d of 300 reads on a new connection failed while the loop pulled; the first: %v", failed, first)
	}
}
