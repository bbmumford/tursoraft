package fence_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bbmumford/tursoraft/fence"
	"github.com/bbmumford/tursoraft/localdb"
)

func localDB(t *testing.T) *sql.DB {
	t.Helper()
	key := make([]byte, 32)
	rand.Read(key)
	db, err := localdb.Open(context.Background(), filepath.Join(t.TempDir(), "fence.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE writes(seq INTEGER PRIMARY KEY AUTOINCREMENT, epoch INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	return db
}

// write runs one fenced write recording the fence's epoch.
func write(ctx context.Context, db *sql.DB, f fence.Fence) error {
	return f.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO writes(epoch) VALUES (?)", int64(f.Epoch))
		return err
	})
}

// sequence runs holder A at epoch 5, B at 3, B at 7, A at
// 5 again, B at 7 again — on dbA and dbB and returns the epochs written.
func sequence(t *testing.T, dbA, dbB, read *sql.DB, sync func()) []int64 {
	t.Helper()
	ctx := context.Background()
	steps := []struct {
		db    *sql.DB
		epoch uint64
		want  error
	}{
		{dbA, 5, nil},
		{dbB, 3, fence.ErrFenced},
		{dbB, 7, nil},
		{dbA, 5, fence.ErrFenced},
		{dbB, 7, nil},
	}
	for i, s := range steps {
		f := fence.Fence{Cluster: "c1", Job: "projector", Epoch: s.epoch, Node: "n"}
		err := write(ctx, s.db, f)
		if s.want == nil && err != nil || s.want != nil && !errors.Is(err, s.want) {
			t.Fatalf("step %d epoch %d: got %v, want %v", i, s.epoch, err, s.want)
		}
		sync()
	}
	rows, err := read.Query("SELECT epoch FROM writes ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var e int64
		rows.Scan(&e)
		got = append(got, e)
	}
	return got
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// On one local file a stale epoch is refused, an equal or newer one
// accepted.
func TestFenceOrdersEpochsOnALocalFile(t *testing.T) {
	db := localDB(t)
	if got := sequence(t, db, db, db, func() {}); !equal(got, []int64{5, 7, 7}) {
		t.Errorf("accepted epochs %v, want [5 7 7]", got)
	}
}

// A fence row written by another cluster refuses this cluster's holder with
// ErrForeignCluster, whether the new cluster's epoch is below or above the
// old one's, until an operator adopts the new cluster.
func TestForeignClusterIsRefusedUntilAdopted(t *testing.T) {
	for _, tc := range []struct {
		name           string
		oldEpoch, next uint64
	}{{"new epoch lower", 900, 4}, {"new epoch higher", 3, 900}} {
		t.Run(tc.name, func(t *testing.T) {
			db := localDB(t)
			ctx := context.Background()
			if err := write(ctx, db, fence.Fence{Cluster: "old", Job: "j", Epoch: tc.oldEpoch}); err != nil {
				t.Fatal(err)
			}
			next := fence.Fence{Cluster: "new", Job: "j", Epoch: tc.next}
			if err := write(ctx, db, next); !errors.Is(err, fence.ErrForeignCluster) {
				t.Fatalf("a new cluster's holder: got %v, want ErrForeignCluster", err)
			}
			if _, err := db.Exec(fence.AdoptSQL, "new"); err != nil {
				t.Fatal(err)
			}
			if err := write(ctx, db, next); err != nil {
				t.Fatalf("after adopt: %v", err)
			}
		})
	}
}

// The fence table is created inside the job's transaction, so a first
// transaction that rolls back leaves no trace and the next one still works.
func TestFirstTransactionRollingBackLeavesTheFenceUsable(t *testing.T) {
	db := localDB(t)
	ctx := context.Background()
	f := fence.Fence{Cluster: "c", Job: "j", Epoch: 1}
	boom := errors.New("boom")
	if err := f.Tx(ctx, db, func(*sql.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the body's error", err)
	}
	if err := write(ctx, db, f); err != nil {
		t.Fatalf("second transaction: %v", err)
	}
}

// A fence missing its cluster, job or epoch is refused before any SQL runs.
func TestIncompleteFenceIsRefused(t *testing.T) {
	db := localDB(t)
	for _, f := range []fence.Fence{{Job: "j", Epoch: 1}, {Cluster: "c", Epoch: 1}, {Cluster: "c", Job: "j"}} {
		if err := write(context.Background(), db, f); !errors.Is(err, fence.ErrInvalid) {
			t.Errorf("%+v: got %v, want ErrInvalid", f, err)
		}
	}
}
