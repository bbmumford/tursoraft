//go:build cgo

package replica_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/pull"
	"github.com/bbmumford/tursoraft/replica"
	"github.com/bbmumford/tursoraft/sqldtest"
)

// A scope database's writer advances replicaProgress.sourceCursor in the
// transaction that writes the segment's rows, as the projector does; a
// read-only replica whose timed pull is an hour away reaches the position
// only through RequestPull, and holds the rows when it returns.
func TestRequestPullReturnsAtThePosition(t *testing.T) {
	s, signer := sqldtest.StartWithAuth(t)
	dir := t.TempDir()
	ctx := context.Background()
	digest := make([]byte, 32)
	digest[0] = 0x5e
	w, err := replica.Open(ctx, replica.Config{
		Path: filepath.Join(dir, "projector.db"), URL: s.URL(), Mode: replica.ReadWrite, Key: make([]byte, 32),
		Credentials: replica.StaticCredential(signer.Token(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})),
		OpenTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, q := range []string{
		"CREATE TABLE replicaScope (scopeDigest BLOB PRIMARY KEY CHECK (length(scopeDigest) = 32), partitionId TEXT NOT NULL) STRICT",
		"CREATE TABLE replicaProgress (scopeDigest BLOB PRIMARY KEY CHECK (length(scopeDigest) = 32), sourceCursor INTEGER NOT NULL DEFAULT 0, syncedToMs INTEGER NOT NULL DEFAULT 0, projectedAtMs INTEGER NOT NULL DEFAULT 0) STRICT",
		"CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT NOT NULL)",
	} {
		if _, err := w.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.DB().Exec("INSERT INTO replicaScope (scopeDigest, partitionId) VALUES (?, 'p1')", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DB().Exec("INSERT INTO replicaProgress (scopeDigest, sourceCursor) VALUES (?, 0)", digest); err != nil {
		t.Fatal(err)
	}
	advance := func(position int64, body string) {
		t.Helper()
		tx, err := w.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tx.Exec("INSERT INTO messages (body) VALUES (?)", body); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec("UPDATE replicaProgress SET sourceCursor = ? WHERE scopeDigest = ?", position, digest); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	r, err := replica.Open(ctx, replica.Config{
		Path: filepath.Join(dir, "device.db"), URL: s.URL(), Mode: replica.ReadOnly, Key: make([]byte, 32),
		Scope:        replica.Scope{Database: "orbtr-r-test", Digest: digest},
		Credentials:  grant{Token: signer.Token(map[string]any{"a": "ro", "exp": time.Now().Add(10 * time.Minute).Unix()}), Database: "orbtr-r-test", NotAfter: time.Now().Add(15 * time.Minute)},
		SyncInterval: time.Hour, OpenTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	set := replica.NewSet()
	if err := set.Add(r); err != nil {
		t.Fatal(err)
	}
	var port pull.Trigger = set

	advance(5, "the message the push announced")
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := port.RequestPull(reqCtx, "orbtr-r-test", 5); err != nil {
		t.Fatalf("RequestPull(5): %v", err)
	}
	var body string
	if err := r.DB().QueryRow("SELECT body FROM messages").Scan(&body); err != nil || body != "the message the push announced" {
		t.Fatalf("after RequestPull(5) the replica reads %q, %v", body, err)
	}
	if p := r.Position(); p != 5 {
		t.Errorf("Position() = %d, want 5", p)
	}

	// A change the segment does not admit moves the position with no row.
	advance(6, "")
	if err := port.RequestPull(reqCtx, "orbtr-r-test", 6); err != nil || r.Position() != 6 {
		t.Errorf("RequestPull(6) for a change the segment does not admit = %v at %d", err, r.Position())
	}

	short, cancelShort := context.WithTimeout(ctx, 2*time.Second)
	defer cancelShort()
	if err := port.RequestPull(short, "orbtr-r-test", 99); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RequestPull(99), a position never written = %v, want context.DeadlineExceeded", err)
	}
}
