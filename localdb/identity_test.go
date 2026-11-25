package localdb_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bbmumford/filekind"
	"github.com/bbmumford/policypool"
	"github.com/bbmumford/tursoraft/localdb"
)

// TestOpenFileStampsANewFileOnceAndRefusesAnotherKind: a new file opened with
// an identity carries its kind and generation, opens again under it, refuses a
// header assignment, and is refused under another kind; Open still opens it.
func TestOpenFileStampsANewFileOnceAndRefusesAnotherKind(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local.db")
	k := key(t)
	local := policypool.Identity{Kind: filekind.TursoraftLocal, Generation: 1}
	db, err := localdb.OpenFile(ctx, path, k, local)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := filekind.Read(ctx, db); err != nil || h != (filekind.Header{ApplicationID: filekind.TursoraftLocal.ApplicationID(), UserVersion: 1}) {
		t.Fatalf("header of a new file = %+v, %v, want tursoraftLocal generation 1", h, err)
	}
	if _, err := db.Exec("PRAGMA application_id = 1"); !errors.Is(err, policypool.ErrPolicyRefused) {
		t.Errorf("a header assignment = %v, want policypool.ErrPolicyRefused", err)
	}
	if _, err := db.Exec("CREATE TABLE t (k INTEGER PRIMARY KEY); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = localdb.OpenFile(ctx, path, k, local)
	if err != nil {
		t.Fatalf("reopening under its own kind: %v", err)
	}
	db.Close()
	if db, err := localdb.OpenFile(ctx, path, k, policypool.Identity{Kind: filekind.Lad, Generation: 1}); db != nil || !errors.Is(err, policypool.ErrFileIdentity) || !errors.Is(err, filekind.ErrWrongKind) {
		t.Fatalf("OpenFile under another kind = %v, %v, want ErrFileIdentity and ErrWrongKind", db, err)
	}
	plain := open(t, path, k)
	var n int
	if err := plain.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 1 {
		t.Fatalf("the refused file's rows: %d, %v", n, err)
	}
}

// TestOpenFileRefusesAnUnstampedFileThatHoldsRows: a file Open wrote, with a
// schema and no stamp, is refused and left unstamped, since taking an existing
// file into a kind is a step of its own.
func TestOpenFileRefusesAnUnstampedFileThatHoldsRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	k := key(t)
	db := open(t, path, k)
	if _, err := db.Exec("CREATE TABLE t (k INTEGER PRIMARY KEY); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err := localdb.OpenFile(ctx, path, k, policypool.Identity{Kind: filekind.TursoraftLocal, Generation: 1}); db != nil || !errors.Is(err, policypool.ErrFileIdentity) || !errors.Is(err, filekind.ErrUnstamped) {
		t.Fatalf("OpenFile of an unstamped file with rows = %v, %v, want ErrFileIdentity and ErrUnstamped", db, err)
	}
	if h, err := filekind.Read(ctx, open(t, path, k)); err != nil || h != (filekind.Header{}) {
		t.Fatalf("the refused file's header = %+v, %v, want it unstamped", h, err)
	}
}

// TestOpenFileChecksTheKeyAndPathFirst: OpenFile refuses what Open refuses
// before it touches the file.
func TestOpenFileChecksTheKeyAndPathFirst(t *testing.T) {
	id := policypool.Identity{Kind: filekind.TursoraftLocal, Generation: 1}
	if _, err := localdb.OpenFile(context.Background(), filepath.Join(t.TempDir(), "k.db"), make([]byte, 16), id); !errors.Is(err, localdb.ErrKeyLength) {
		t.Errorf("a short key = %v, want ErrKeyLength", err)
	}
	if _, err := localdb.OpenFile(context.Background(), "relative.db", key(t), id); !errors.Is(err, localdb.ErrRelativePath) {
		t.Errorf("a relative path = %v, want ErrRelativePath", err)
	}
}
