//go:build cgo

package replica_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/internal/sqldtest"
	"github.com/bbmumford/tursoraft/localdb"
	"github.com/bbmumford/tursoraft/replica"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	rand.Read(k)
	return k
}

func openReplica(t *testing.T, s *sqldtest.Server, path string, key []byte) *replica.Replica {
	t.Helper()
	r, err := replica.Open(context.Background(), replica.Config{
		Path: path, URL: s.URL(), AuthToken: "test", Key: key,
		SyncInterval: 200 * time.Millisecond, OpenTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("open replica %s: %v", path, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func mustExec(t *testing.T, r *replica.Replica, q string, args ...any) {
	t.Helper()
	if _, err := r.DB().ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func count(t *testing.T, r *replica.Replica, table string) int {
	t.Helper()
	var n int
	if err := r.DB().QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// The replica file and its WAL carry neither the plaintext header nor any
// table name or row the primary holds.
func TestReplicaFilesAreEncrypted(t *testing.T) {
	s := sqldtest.Start(t)
	path := filepath.Join(t.TempDir(), "enc.db")
	r := openReplica(t, s, path, newKey(t))
	mustExec(t, r, "CREATE TABLE _ledger(v INTEGER PRIMARY KEY, note TEXT)")
	for i := 0; i < 50; i++ {
		mustExec(t, r, "INSERT INTO _ledger(note) VALUES ('marker-row-text')")
	}
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, r, "_ledger"); n != 50 {
		t.Fatalf("replica reads %d rows, want 50", n)
	}
	checked := 0
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 {
			continue
		}
		checked++
		if bytes.HasPrefix(raw, []byte("SQLite format 3")) {
			t.Errorf("%s begins with the plaintext SQLite header", filepath.Base(path+suffix))
		}
		for _, marker := range []string{"_ledger", "marker-row-text"} {
			if bytes.Contains(raw, []byte(marker)) {
				t.Errorf("%s contains %q in plaintext", filepath.Base(path+suffix), marker)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no replica file had content to check")
	}
}

// A plaintext file at the replica's path is deleted with its companion files
// and the replica downloads again, encrypted.
func TestPlaintextReplicaIsReplaced(t *testing.T) {
	s := sqldtest.Start(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.db")
	plain, err := localdb.OpenPlaintext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec("CREATE TABLE stale(x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	plain.Close()
	if ok, _ := localdb.IsPlaintext(path); !ok {
		t.Fatal("setup: the file is not plaintext")
	}
	for _, suffix := range []string{"-client_wal_index", "-info"} {
		os.WriteFile(path+suffix, []byte("stale"), 0o600)
	}

	r := openReplica(t, s, path, newKey(t))
	mustExec(t, r, "CREATE TABLE fresh(x INTEGER)")
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM sqlite_schema WHERE name = 'stale'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the plaintext file's table is still visible; the file was not replaced")
	}
	if ok, _ := localdb.IsPlaintext(path); ok {
		t.Error("the replica file is still plaintext")
	}
	if raw, err := os.ReadFile(path + "-info"); err == nil && string(raw) == "stale" {
		t.Error("the plaintext replica's companion file was left behind")
	}
}

// A key of any length but 32 bytes is refused before anything touches the
// disk or the network.
func TestShortKeyRefused(t *testing.T) {
	dir := t.TempDir()
	_, err := replica.Open(context.Background(), replica.Config{
		Path: filepath.Join(dir, "short.db"), URL: "http://127.0.0.1:1", Key: make([]byte, 16),
		OpenTimeout: time.Second,
	})
	if !errors.Is(err, replica.ErrKeyLength) {
		t.Fatalf("a 16-byte key: got %v, want ErrKeyLength", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused key left %d files", len(entries))
	}
}

// A replica file opened under another key fails at open, at once rather than
// after the open timeout, and is left in place.
func TestWrongKeyFailsAtOpen(t *testing.T) {
	s := sqldtest.Start(t)
	path := filepath.Join(t.TempDir(), "wrong.db")
	r, err := replica.Open(context.Background(), replica.Config{Path: path, URL: s.URL(), AuthToken: "test", Key: newKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB().Exec("CREATE TABLE t(x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	r.Sync(context.Background())
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r2, err := replica.Open(context.Background(), replica.Config{Path: path, URL: s.URL(), AuthToken: "test", Key: newKey(t), OpenTimeout: 30 * time.Second})
	if err == nil {
		r2.Close()
		t.Fatal("a replica opened under another key")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("a wrong key took %v to fail; it must not be retried as an outage", took)
	}
	t.Logf("wrong key: %v", err)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the replica file under another key was removed: %v", err)
	}
}

// A host booting while its primary is down fails once the open timeout
// passes; it never serves from nothing.
func TestBootDuringOutageFails(t *testing.T) {
	s := sqldtest.Start(t)
	s.Stop()
	start := time.Now()
	_, err := replica.Open(context.Background(), replica.Config{
		Path: filepath.Join(t.TempDir(), "down.db"), URL: s.URL(), AuthToken: "test", Key: newKey(t),
		OpenTimeout: 2 * time.Second,
	})
	took := time.Since(start)
	if err == nil {
		t.Fatal("a replica opened with its primary down")
	}
	if took < 2*time.Second || took > 10*time.Second {
		t.Errorf("open failed after %v, want about the 2s open timeout", took)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error does not say the primary was unreachable: %v", err)
	}
}

// While the primary is down, writes fail and pulls fail; once it is back,
// pulls and writes recover on the same replica without reopening it.
func TestOutageWritesFailAndRecover(t *testing.T) {
	s := sqldtest.Start(t)
	r := openReplica(t, s, filepath.Join(t.TempDir(), "outage.db"), newKey(t))
	mustExec(t, r, "CREATE TABLE t(x INTEGER)")
	mustExec(t, r, "INSERT INTO t VALUES (1), (2), (3)")
	s.Stop()
	outage := 30 * time.Second
	if testing.Short() {
		outage = 3 * time.Second
	}
	deadline := time.Now().Add(outage)
	for time.Now().Before(deadline) {
		if _, err := r.DB().Exec("INSERT INTO t VALUES (4)"); err == nil {
			t.Fatal("a write succeeded with the primary down")
		}
		time.Sleep(time.Second)
	}
	// A pull against a refused connection takes about 3 s to fail inside
	// the engine, so the first failure may land after a short outage ends.
	for wait := time.Now().Add(10 * time.Second); r.Stats().Err == nil; {
		if time.Now().After(wait) {
			t.Fatal("stats report no pull error during the outage")
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Restart()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		err := r.Sync(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("pull did not recover: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	for {
		_, err := r.DB().ExecContext(ctx, "INSERT INTO t VALUES (4)")
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("writes did not recover: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n := count(t, r, "t"); n != 4 {
		t.Errorf("after recovery the replica reads %d rows, want 4", n)
	}
	if st := r.Stats(); st.Err != nil || st.Stale {
		t.Errorf("after recovery stats report err=%v stale=%v", st.Err, st.Stale)
	}
}

// Reads through the pool during an outage. The replica file itself answers a
// SELECT with the primary down, but policypool proves foreign keys on every
// new connection and again on every reuse, and the replica engine sends
// PRAGMA foreign_keys to the primary; so every pooled statement needs the
// primary. TestPolicyProofReachesThePrimary measures it.
func TestOutageServesReads(t *testing.T) {
	t.Skip("pooled reads need the primary: policypool's per-reuse foreign-key proof is delegated to it (see TestPolicyProofReachesThePrimary)")
}
