//go:build cgo

package replica_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/bbmumford/filekind"
	libsql "github.com/bbmumford/go-libsql"
	"github.com/bbmumford/tursoraft/sqldtest"
)

// startScopePrimary starts sqld with auth on a default database seeded from a
// file stamped with the scope database file identity, as a scope database is
// created from a stamped file, so a scope replica of it opens. sqld refuses
// the header pragmas as statements, so the stamp is in the seed.
func startScopePrimary(t *testing.T) (*sqldtest.Server, *sqldtest.Signer) {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed.db")
	db, err := sql.Open(libsql.DriverName, "file:"+seed)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		fmt.Sprintf("PRAGMA application_id = %d", filekind.ScopeReplica.ApplicationID()),
		"PRAGMA user_version = 1",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("stamp the seed: %s: %v", q, err)
		}
	}
	return sqldtest.StartWithAuthFrom(t, seed)
}
