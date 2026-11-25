//go:build cgo

package replica

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bbmumford/filekind"
	libsql "github.com/bbmumford/go-libsql"
	"github.com/bbmumford/policypool"
)

// identityScope is the scope the stamped primaries below hold.
var identityScope = func() Scope {
	d := sha256.Sum256([]byte("identity-scope"))
	return Scope{Database: "orbtr-r-identity", Digest: d[:]}
}()

// stampedFile writes a plaintext scope database at path whose header holds
// app and version, with the replicaScope and replicaProgress rows of
// identityScope.
func stampedFile(t *testing.T, path string, app, version int32) {
	t.Helper()
	c, err := libsql.NewLocalConnector(path)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1)
	h := fmt.Sprintf("X'%x'", identityScope.Digest)
	for _, q := range []string{
		"CREATE TABLE replicaScope (scopeDigest BLOB PRIMARY KEY, partitionId TEXT NOT NULL)",
		"CREATE TABLE replicaProgress (scopeDigest BLOB PRIMARY KEY, sourceCursor INTEGER NOT NULL DEFAULT 0)",
		"INSERT INTO replicaScope VALUES (" + h + ", 'p')",
		"INSERT INTO replicaProgress (scopeDigest, sourceCursor) VALUES (" + h + ", 1)",
		fmt.Sprintf("PRAGMA application_id = %d", app),
		fmt.Sprintf("PRAGMA user_version = %d", version),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("primary: %s: %v", q, err)
		}
	}
	db.Close()
	c.Close()
}

// identityPrimary serves, over libSQL's HTTP sync protocol, a scope database
// whose header holds app and version: the export the replica starts from and
// no frames after it, as a primary seeded from a stamped file does.
func identityPrimary(t *testing.T, app, version int32) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "primary.db")
	stampedFile(t, path, app, version)
	export, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := strings.Trim(r.URL.Path, "/"); {
		case p == "info":
			w.Write([]byte(`{"current_generation":1}`))
		case p == "export/1":
			w.Write(export)
		case strings.HasPrefix(p, "sync/1/"):
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"generation":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestScopeReplicaChecksItsPrimarysFileIdentity: a read-only scope replica
// opens only when its primary's page 1 carries the scope database identity;
// any other header is refused with policypool.ErrFileIdentity, and the copy is
// never stamped, so a second open is refused the same way. A whole-database
// replica checks no identity.
func TestScopeReplicaChecksItsPrimarysFileIdentity(t *testing.T) {
	scope := filekind.ScopeReplica.ApplicationID()
	cases := []struct {
		name         string
		app, version int32
		whole        bool
		reason       error // nil: opens
	}{
		{"the scope database identity", scope, scopeGeneration, false, nil},
		{"an unstamped primary", 0, 0, false, filekind.ErrUnstamped},
		{"another kind", filekind.Lad.ApplicationID(), 1, false, filekind.ErrWrongKind},
		{"a newer format", scope, scopeGeneration + 1, false, filekind.ErrNewerGeneration},
		{"an unstamped whole database", 0, 0, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := identityPrimary(t, c.app, c.version)
			cfg := httpSyncSpec(filepath.Join(t.TempDir(), "replica.db"), url, httpSyncKey(t))
			if !c.whole {
				cfg.Scope = identityScope
				cfg.Credentials = fixedSource{c: Credential{Token: mustToken(cfg), Database: identityScope.Database}}
			}
			for attempt := 1; attempt <= 2; attempt++ {
				r, err := Open(context.Background(), cfg)
				if c.reason == nil {
					if err != nil {
						t.Fatalf("open %d: %v", attempt, err)
					}
					h, herr := filekind.Read(context.Background(), r.DB())
					r.Close()
					if herr != nil || h != (filekind.Header{ApplicationID: c.app, UserVersion: c.version}) {
						t.Fatalf("open %d: the replica's header = %+v, %v, want the primary's", attempt, h, herr)
					}
					continue
				}
				if r != nil {
					r.Close()
				}
				if !errors.Is(err, policypool.ErrFileIdentity) || !errors.Is(err, c.reason) {
					t.Fatalf("open %d: err = %v, want policypool.ErrFileIdentity and %v", attempt, err, c.reason)
				}
			}
		})
	}
}

// mustToken is the token cfg's static credential carries.
func mustToken(cfg Config) string {
	cred, err := cfg.Credentials.Credential(context.Background())
	if err != nil {
		panic(err)
	}
	return cred.Token
}

// TestReadWriteScopePoolChecksTheFileIdentity: a read-write scope replica's
// pool opens through policypool with the check-only identity, so a file with
// the scope database identity opens and refuses header assignments, an
// unstamped one, with a schema or empty, is refused and left unstamped, and a
// whole database is not checked.
func TestReadWriteScopePoolChecksTheFileIdentity(t *testing.T) {
	ctx := context.Background()
	open := func(path string, scope Scope) (*sql.DB, error) {
		c, err := libsql.NewLocalConnector(path)
		if err != nil {
			t.Fatal(err)
		}
		return openPool(ctx, Config{Mode: ReadWrite, Scope: scope}, c)
	}
	dir := t.TempDir()
	stamped := filepath.Join(dir, "stamped.db")
	stampedFile(t, stamped, filekind.ScopeReplica.ApplicationID(), scopeGeneration)
	db, err := open(stamped, identityScope)
	if err != nil {
		t.Fatalf("a stamped scope database: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 7"); !errors.Is(err, policypool.ErrPolicyRefused) {
		t.Errorf("a header assignment on the scope pool = %v, want policypool.ErrPolicyRefused", err)
	}
	db.Close()
	unstamped := filepath.Join(dir, "unstamped.db")
	stampedFile(t, unstamped, 0, 0)
	if db, err := open(unstamped, identityScope); db != nil || !errors.Is(err, policypool.ErrFileIdentity) || !errors.Is(err, filekind.ErrUnstamped) {
		t.Fatalf("an unstamped scope database = %v, %v, want policypool.ErrFileIdentity and filekind.ErrUnstamped", db, err)
	}
	if h, err := filekind.ReadFileHeader(unstamped); err != nil || h != (filekind.Header{}) {
		t.Fatalf("the refused file's header is %+v, %v, want it unstamped", h, err)
	}
	empty := filepath.Join(dir, "empty.db")
	if db, err := open(empty, identityScope); db != nil || !errors.Is(err, policypool.ErrFileIdentity) || !errors.Is(err, filekind.ErrUnstamped) {
		t.Fatalf("an empty scope database = %v, %v, want policypool.ErrFileIdentity and filekind.ErrUnstamped", db, err)
	}
	if h, err := filekind.ReadFileHeader(empty); err == nil && h != (filekind.Header{}) {
		t.Fatalf("the check stamped the empty file: %+v", h)
	}
	db, err = open(unstamped, Scope{})
	if err != nil {
		t.Fatalf("a whole database is not checked: %v", err)
	}
	db.Close()
}

// TestScopeIdentityIsTheRegistrysRow holds the identity every scope replica is
// checked against to filekind's registry row for scope databases.
func TestScopeIdentityIsTheRegistrysRow(t *testing.T) {
	row, ok := filekind.Lookup(filekind.ScopeReplica)
	if !ok || row.Generation != scopeGeneration {
		t.Fatalf("filekind's scopeReplica row = %+v (%v), the replica reads generation %d", row, ok, scopeGeneration)
	}
	if scopeIdentity.Kind != filekind.ScopeReplica || scopeIdentity.Generation != scopeGeneration || !scopeIdentity.CheckOnly {
		t.Fatalf("scopeIdentity = %+v, want scopeReplica at generation %d, check-only", scopeIdentity, scopeGeneration)
	}
}
