//go:build cgo

package replica_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	libsql "github.com/bbmumford/go-libsql"

	"github.com/bbmumford/tursoraft/replica"
	"github.com/bbmumford/tursoraft/sqldtest"
)

// grant is a CredentialSource handing out one credential, as the device's
// grant client would.
type grant replica.Credential

func (g grant) Credential(context.Context) (replica.Credential, error) {
	return replica.Credential(g), nil
}

func openSpec(t *testing.T, cfg replica.Config) (*replica.Replica, error) {
	t.Helper()
	if cfg.Key == nil {
		cfg.Key = make([]byte, 32)
	}
	if cfg.OpenTimeout == 0 {
		cfg.OpenTimeout = 10 * time.Second
	}
	if cfg.SyncInterval == 0 {
		cfg.SyncInterval = time.Hour
	}
	r, err := replica.Open(context.Background(), cfg)
	if err == nil {
		t.Cleanup(func() { r.Close() })
	}
	return r, err
}

func inMinutes(n int) int64 { return time.Now().Add(time.Duration(n) * time.Minute).Unix() }

// A ReadOnly spec handed a token without "a": "ro" refuses to open, so a
// device replica never pulls with authority that could write.
func TestReadOnlySpecRefusesWriteCapableToken(t *testing.T) {
	s, signer := sqldtest.StartWithAuth(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		name   string
		claims map[string]any
	}{
		{"no a claim (full access)", map[string]any{"exp": inMinutes(10)}},
		{"a rw", map[string]any{"a": "rw", "exp": inMinutes(10)}},
	} {
		r, err := openSpec(t, replica.Config{
			Path: filepath.Join(dir, tc.name+".db"), URL: s.URL(), Mode: replica.ReadOnly,
			Credentials: replica.StaticCredential(signer.Token(tc.claims)),
		})
		if !errors.Is(err, replica.ErrWriteCapableToken) {
			t.Errorf("%s: err = %v, want ErrWriteCapableToken", tc.name, err)
		}
		if err == nil {
			_, werr := r.DB().Exec("CREATE TABLE written_through_a_read_only_spec (x)")
			t.Errorf("%s: the read-only spec opened; a forwarded write returned %v", tc.name, werr)
		}
	}
}

// A ReadOnly replica under a read-only token reads what the primary holds,
// refuses every write itself, and pulls what a writer adds.
func TestReadOnlyReplicaReadsAndRefusesWrites(t *testing.T) {
	s, signer := sqldtest.StartWithAuth(t)
	dir := t.TempDir()
	ctx := context.Background()
	w, err := openSpec(t, replica.Config{
		Path: filepath.Join(dir, "writer.db"), URL: s.URL(), Mode: replica.ReadWrite,
		Credentials: replica.StaticCredential(signer.Token(map[string]any{"exp": inMinutes(60)})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.DB().Exec("CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DB().Exec("INSERT INTO notes (body) VALUES ('first')"); err != nil {
		t.Fatal(err)
	}

	r, err := openSpec(t, replica.Config{
		Path: filepath.Join(dir, "reader.db"), URL: s.URL(), Mode: replica.ReadOnly,
		Credentials: replica.StaticCredential(signer.Token(map[string]any{"a": "ro", "exp": inMinutes(10)})),
	})
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	count := func(db *sql.DB) int {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM notes").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(r.DB()); n != 1 {
		t.Fatalf("the read-only replica counts %d notes, want 1", n)
	}
	if _, err := r.DB().Exec("INSERT INTO notes (body) VALUES ('from the reader')"); !errors.Is(err, libsql.ErrReadOnlyReplica) {
		t.Errorf("INSERT on the read-only replica: err = %v, want ErrReadOnlyReplica", err)
	}
	if tx, err := r.DB().BeginTx(ctx, nil); !errors.Is(err, libsql.ErrReadOnlyReplica) {
		if tx != nil {
			tx.Rollback()
		}
		t.Errorf("BeginTx that can write: err = %v, want ErrReadOnlyReplica", err)
	}
	tx, err := r.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only BeginTx: %v", err)
	}
	tx.Rollback()

	if _, err := w.DB().Exec("INSERT INTO notes (body) VALUES ('second')"); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(w.DB()); n != 2 {
		t.Errorf("the primary holds %d notes, want 2: the reader's INSERT reached it or the writer's did not", n)
	}
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("read-only pull: %v", err)
	}
	if n := count(r.DB()); n != 2 {
		t.Errorf("after a pull the read-only replica counts %d notes, want 2", n)
	}
}

// A scope database opens only when its replicaScope row carries the spec's
// digest and the grant names the spec's database.
func TestReadOnlyScopeIsChecked(t *testing.T) {
	s, signer := sqldtest.StartWithAuth(t)
	dir := t.TempDir()
	ro := signer.Token(map[string]any{"a": "ro", "exp": inMinutes(10)})
	digest := make([]byte, 32)
	digest[0] = 0xd1
	other := make([]byte, 32)
	other[0] = 0xd2
	spec := func(name string, d []byte, grantDB string) replica.Config {
		return replica.Config{
			Path: filepath.Join(dir, name+".db"), URL: s.URL(), Mode: replica.ReadOnly,
			Scope:       replica.Scope{Database: "orbtr-r-test", Digest: d},
			Credentials: grant{Token: ro, Database: grantDB, NotAfter: time.Now().Add(15 * time.Minute)},
		}
	}
	if _, err := openSpec(t, spec("unscoped", digest, "orbtr-r-test")); !errors.Is(err, replica.ErrScopeMismatch) {
		t.Errorf("a database with no replicaScope: err = %v, want ErrScopeMismatch", err)
	}

	w, err := openSpec(t, replica.Config{
		Path: filepath.Join(dir, "writer.db"), URL: s.URL(), Mode: replica.ReadWrite,
		Credentials: replica.StaticCredential(signer.Token(map[string]any{"exp": inMinutes(60)})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.DB().Exec("CREATE TABLE replicaScope (scopeDigest BLOB PRIMARY KEY CHECK (length(scopeDigest) = 32), partitionId TEXT NOT NULL) STRICT"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DB().Exec("INSERT INTO replicaScope (scopeDigest, partitionId) VALUES (?, 'p1')", digest); err != nil {
		t.Fatal(err)
	}

	if _, err := openSpec(t, spec("matching", digest, "orbtr-r-test")); err != nil {
		t.Errorf("the matching scope: err = %v", err)
	}
	if _, err := openSpec(t, spec("other-digest", other, "orbtr-r-test")); !errors.Is(err, replica.ErrScopeMismatch) {
		t.Errorf("another scope's digest: err = %v, want ErrScopeMismatch", err)
	}
	if _, err := w.DB().Exec("INSERT INTO replicaScope (scopeDigest, partitionId) VALUES (?, 'p2')", other); err != nil {
		t.Fatal(err)
	}
	if _, err := openSpec(t, spec("two-rows", digest, "orbtr-r-test")); !errors.Is(err, replica.ErrScopeMismatch) {
		t.Errorf("a database holding two scopes: err = %v, want ErrScopeMismatch", err)
	}

	if _, err := openSpec(t, spec("other-grant", digest, "orbtr-r-other")); !errors.Is(err, replica.ErrGrantMismatch) {
		t.Errorf("a grant for another database: err = %v, want ErrGrantMismatch", err)
	}
}

// The engine opens a replica under no token, or an expired one, and fails
// only at the first pull, as a handshake timeout Open would retry until its
// timeout. Open refuses both before the engine is reached, at once and with
// the reason.
func TestOpenRefusesAnExpiredToken(t *testing.T) {
	s, signer := sqldtest.StartWithAuth(t)
	dir := t.TempDir()
	expired := time.Now().Add(-10 * time.Minute).Unix()
	for _, tc := range []struct {
		name  string
		mode  replica.Mode
		token string
		want  error
	}{
		{"read-write, expired ten minutes ago", replica.ReadWrite, signer.Token(map[string]any{"exp": expired}), replica.ErrGrantExpired},
		{"read-only, expired ten minutes ago", replica.ReadOnly, signer.Token(map[string]any{"a": "ro", "exp": expired}), replica.ErrGrantExpired},
		{"read-write, no token", replica.ReadWrite, "", replica.ErrNoCredential},
		{"read-only, no token", replica.ReadOnly, "", replica.ErrNoCredential},
	} {
		start := time.Now()
		_, err := openSpec(t, replica.Config{
			Path: filepath.Join(dir, tc.name+".db"), URL: s.URL(), Mode: tc.mode,
			Credentials: replica.StaticCredential(tc.token), OpenTimeout: 20 * time.Second,
		})
		took := time.Since(start)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if took > 2*time.Second {
			t.Errorf("%s: Open took %v: it reached the engine", tc.name, took.Round(time.Millisecond))
		}
	}
}
