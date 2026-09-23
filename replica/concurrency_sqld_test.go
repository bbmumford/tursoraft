//go:build cgo

package replica_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bbmumford/tursoraft/internal/sqldtest"
	"github.com/bbmumford/tursoraft/replica"
)

// twoReplicas opens two replicas of one primary, as two processes holding
// the same shared database would.
func twoReplicas(t *testing.T, s *sqldtest.Server) (*replica.Replica, *replica.Replica) {
	t.Helper()
	dir := t.TempDir()
	key := newKey(t)
	return openReplica(t, s, filepath.Join(dir, "a.db"), key), openReplica(t, s, filepath.Join(dir, "b.db"), key)
}

// migrate applies version v in one write transaction unless the ledger
// already records it, and reports whether this call applied it.
func migrate(ctx context.Context, db *sql.DB, v int) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM _ledger WHERE v = ?", v).Scan(&n); err != nil {
		return false, fmt.Errorf("ledger read: %w", err)
	}
	if n > 0 {
		return false, tx.Commit()
	}
	for _, q := range []string{
		fmt.Sprintf("CREATE TABLE t%d(id INTEGER PRIMARY KEY, a TEXT)", v),
		fmt.Sprintf("ALTER TABLE t%d ADD COLUMN b TEXT", v),
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return false, fmt.Errorf("v%d: %w", v, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO _ledger(v) VALUES (?)", v); err != nil {
		return false, fmt.Errorf("ledger insert: %w", err)
	}
	return true, tx.Commit()
}

// Two replicas running the same 20 migrations at once apply each exactly
// once, with no error: every replica transaction begins with BEGIN
// IMMEDIATE on the primary, so the second migrator waits and then sees the
// first one's ledger row.
func TestConcurrentMigrationsApplyOnce(t *testing.T) {
	s := sqldtest.Start(t)
	s.RequireConcurrentTransactions(t)
	a, b := twoReplicas(t, s)
	mustExec(t, a, "CREATE TABLE _ledger(v INTEGER PRIMARY KEY)")
	b.Sync(context.Background())

	var mu sync.Mutex
	applied, errs := 0, []error{}
	var wg sync.WaitGroup
	for _, r := range []*replica.Replica{a, b} {
		wg.Add(1)
		go func(db *sql.DB) {
			defer wg.Done()
			for v := 1; v <= 20; v++ {
				did, err := migrate(context.Background(), db, v)
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				}
				if did {
					applied++
				}
				mu.Unlock()
			}
		}(r.DB())
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Errorf("%d migrations failed; first: %v", len(errs), errs[0])
	}
	if applied != 20 {
		t.Errorf("%d migrations applied, want 20", applied)
	}
	a.Sync(context.Background())
	if n := count(t, a, "_ledger"); n != 20 {
		t.Errorf("ledger holds %d rows, want 20", n)
	}
}

func counter(t *testing.T, a, b *replica.Replica) {
	t.Helper()
	mustExec(t, a, "CREATE TABLE c(id INTEGER PRIMARY KEY, v INTEGER NOT NULL)")
	mustExec(t, a, "INSERT INTO c VALUES (1, 0)")
	b.Sync(context.Background())
}

func value(t *testing.T, r *replica.Replica) int {
	t.Helper()
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := r.DB().QueryRow("SELECT v FROM c WHERE id = 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func both(t *testing.T, a, b *replica.Replica, n int, step func(*sql.DB) error) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for _, r := range []*replica.Replica{a, b} {
		wg.Add(1)
		go func(db *sql.DB) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				if err := step(db); err != nil {
					errs <- err
				}
			}
		}(r.DB())
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("step failed: %v", err)
		return
	}
}

// One atomic statement per increment loses nothing.
func TestAtomicIncrementsFromTwoReplicas(t *testing.T) {
	s := sqldtest.Start(t)
	a, b := twoReplicas(t, s)
	counter(t, a, b)
	both(t, a, b, 200, func(db *sql.DB) error {
		_, err := db.Exec("UPDATE c SET v = v + 1 WHERE id = 1")
		return err
	})
	if v := value(t, a); v != 400 {
		t.Errorf("atomic increments: counter is %d, want 400", v)
	}
}

// A read then a write inside one transaction loses nothing, because the
// transaction takes the primary's write lock before the read.
func TestReadThenWriteInATransactionFromTwoReplicas(t *testing.T) {
	s := sqldtest.Start(t)
	s.RequireConcurrentTransactions(t)
	a, b := twoReplicas(t, s)
	counter(t, a, b)
	both(t, a, b, 200, func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var v int
		if err := tx.QueryRow("SELECT v FROM c WHERE id = 1").Scan(&v); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE c SET v = ? WHERE id = 1", v+1); err != nil {
			return err
		}
		return tx.Commit()
	})
	if v := value(t, a); v != 400 {
		t.Errorf("read-then-write transactions: counter is %d, want 400", v)
	}
}

// A read from the replica and then a write outside a transaction loses
// the update another process made in between. This is the pattern every store
// on a database with more than one writing process must avoid.
func TestReadThenWriteOutsideATransactionLosesAnUpdate(t *testing.T) {
	s := sqldtest.Start(t)
	a, b := twoReplicas(t, s)
	counter(t, a, b)
	var seen int
	if err := a.DB().QueryRow("SELECT v FROM c WHERE id = 1").Scan(&seen); err != nil {
		t.Fatal(err)
	}
	mustExec(t, b, "UPDATE c SET v = v + 1 WHERE id = 1")
	mustExec(t, a, "UPDATE c SET v = ? WHERE id = 1", seen+1)
	if v := value(t, a); v != seen+1 {
		t.Fatalf("counter is %d; the demonstration expects %d, one of two increments lost", v, seen+1)
	}
}
