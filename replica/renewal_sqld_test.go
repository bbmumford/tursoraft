//go:build cgo

package replica_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/replica"
	"github.com/bbmumford/tursoraft/sqldtest"
)

// signedGrants mints a read-only token of life on every call, as the grant
// host does, until stopAfter calls (0: never stops).
type signedGrants struct {
	signer    *sqldtest.Signer
	life      time.Duration
	stopAfter int64
	database  string // the scope database each grant names; empty for a whole database
	calls     atomic.Int64
}

func (g *signedGrants) Credential(context.Context) (replica.Credential, error) {
	n := g.calls.Add(1)
	if g.stopAfter > 0 && n > g.stopAfter {
		return replica.Credential{}, errors.New("the grant host refuses")
	}
	notAfter := time.Now().Add(g.life)
	claims := map[string]any{"a": "ro", "exp": notAfter.Unix()}
	id := ""
	if g.database != "" {
		// sqld serves the scope primary as its default namespace; the token names it by that id.
		claims["id"], id = "default", "default"
	}
	return replica.Credential{Token: g.signer.Token(claims), NotAfter: notAfter, Database: g.database, DatabaseID: id}, nil
}

// pullFailures counts the replica's warnings that a pull or a renewal failed.
type pullFailures struct{ n atomic.Int64 }

func (p *pullFailures) Enabled(context.Context, slog.Level) bool { return true }
func (p *pullFailures) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn && (strings.Contains(r.Message, "pull failed") || strings.Contains(r.Message, "renewal failed") || strings.Contains(r.Message, "reopening")) {
		p.n.Add(1)
	}
	return nil
}
func (p *pullFailures) WithAttrs([]slog.Attr) slog.Handler { return p }
func (p *pullFailures) WithGroup(string) slog.Handler      { return p }

// With grants of 10 s renewed at two thirds, a read-only replica keeps pulling
// for 90 s, past sqld's 60 s leeway on an expired token, with no failed pull
// and every row a writer adds every 2 s.
func TestGrantRenewalKeepsPulling(t *testing.T) {
	s, signer := startScopePrimary(t)
	dir := t.TempDir()
	ctx := context.Background()
	w, err := replica.Open(ctx, replica.Config{
		Path: filepath.Join(dir, "writer.db"), URL: s.URL(), Mode: replica.ReadWrite, Key: make([]byte, 32),
		Credentials: replica.StaticCredential(signer.Token(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})),
		OpenTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	digest := make([]byte, 32)
	digest[0] = 0x20
	for _, q := range []string{
		"CREATE TABLE ticks (n INTEGER PRIMARY KEY)",
		"CREATE TABLE replicaScope (scopeDigest BLOB PRIMARY KEY CHECK (length(scopeDigest) = 32), partitionId TEXT NOT NULL) STRICT",
		"CREATE TABLE replicaProgress (scopeDigest BLOB PRIMARY KEY CHECK (length(scopeDigest) = 32), sourceCursor INTEGER NOT NULL DEFAULT 0) STRICT",
	} {
		if _, err := w.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"INSERT INTO replicaScope (scopeDigest, partitionId) VALUES (?, 'p1')", "INSERT INTO replicaProgress (scopeDigest) VALUES (?)"} {
		if _, err := w.DB().Exec(q, digest); err != nil {
			t.Fatal(err)
		}
	}
	failures := &pullFailures{}
	grants := &signedGrants{signer: signer, life: 10 * time.Second, database: "orbtr-r-renewal"}
	r, err := replica.Open(ctx, replica.Config{
		Path: filepath.Join(dir, "reader.db"), URL: s.URL(), Mode: replica.ReadOnly, Key: make([]byte, 32),
		Scope:       replica.Scope{Database: "orbtr-r-renewal", Digest: digest},
		Credentials: grants, SyncInterval: time.Second, OpenTimeout: 10 * time.Second,
		Logger: slog.New(failures),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// A waiter armed before the first renewal waits across every swap for the
	// last tick's position.
	const ticks = 45
	waited := make(chan error, 1)
	go func() {
		wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		waited <- r.RequestPull(wctx, ticks)
	}()

	// A reader keeps querying through the pool across every renewal.
	stopReads := make(chan struct{})
	readsDone := make(chan struct{})
	var reads, readErrs atomic.Int64
	var firstReadErr atomic.Value
	go func() {
		defer close(readsDone)
		for {
			select {
			case <-stopReads:
				return
			case <-time.After(100 * time.Millisecond):
			}
			var n int
			if err := r.DB().QueryRow("SELECT count(*) FROM ticks").Scan(&n); err != nil {
				readErrs.Add(1)
				firstReadErr.CompareAndSwap(nil, err.Error())
			}
			reads.Add(1)
		}
	}()

	for i := 1; i <= ticks; i++ {
		tx, err := w.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO ticks (n) VALUES (?)", i); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if _, err := tx.Exec("UPDATE replicaProgress SET sourceCursor = ? WHERE scopeDigest = ?", i, digest); err != nil {
			t.Fatalf("tick %d position: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("tick %d commit: %v", i, err)
		}
		time.Sleep(2 * time.Second)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Errorf("the waiter armed before the first renewal returned %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("the waiter armed before the first renewal did not return")
	}
	close(stopReads)
	<-readsDone
	if e := readErrs.Load(); e != 0 {
		t.Errorf("%d of %d reads through the pool failed across the renewals; first: %v", e, reads.Load(), firstReadErr.Load())
	}
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("the last pull: %v", err)
	}
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM ticks").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != ticks {
		t.Errorf("the read-only replica holds %d of %d ticks", n, ticks)
	}
	if f := failures.n.Load(); f != 0 {
		t.Errorf("%d pulls or renewals failed over %d grants", f, grants.calls.Load())
	}
	if c := grants.calls.Load(); c < 12 {
		t.Errorf("%d grants in 90 s of 10 s grants, want at least 12", c)
	}
}

// A grant source that stops renewing leaves the replica stale with
// ErrGrantExpired once the grant ends, without waiting for the primary's own
// refusal of the expired token.
func TestGrantLapseLeavesTheReplicaStale(t *testing.T) {
	s, signer := sqldtest.StartWithAuth(t)
	dir := t.TempDir()
	r, err := replica.Open(context.Background(), replica.Config{
		Path: filepath.Join(dir, "reader.db"), URL: s.URL(), Mode: replica.ReadOnly, Key: make([]byte, 32),
		Credentials:  &signedGrants{signer: signer, life: 10 * time.Second, stopAfter: 1},
		SyncInterval: time.Second, OpenTimeout: 10 * time.Second,
		Logger: slog.New(&pullFailures{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	deadline := time.Now().Add(10*time.Second + 3*time.Second + 2*time.Second)
	for {
		st := r.Stats()
		if errors.Is(st.Err, replica.ErrGrantExpired) && st.Stale {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("15 s after a 10 s grant that was not renewed: %s", fmt.Sprintf("err=%v stale=%v", st.Err, st.Stale))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := r.Sync(context.Background()); !errors.Is(err, replica.ErrGrantExpired) {
		t.Errorf("Sync after the grant ended = %v, want ErrGrantExpired", err)
	}
}
