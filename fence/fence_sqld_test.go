//go:build cgo

package fence_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/fence"
	"github.com/bbmumford/tursoraft/replica"
	"github.com/bbmumford/tursoraft/sqldtest"
)

func replicas(t *testing.T) (a, b *replica.Replica) {
	t.Helper()
	s := sqldtest.Start(t)
	key := make([]byte, 32)
	rand.Read(key)
	dir := t.TempDir()
	open := func(name string) *replica.Replica {
		r, err := replica.Open(context.Background(), replica.Config{
			Path: filepath.Join(dir, name), URL: s.URL(), AuthToken: "test", Key: key,
			SyncInterval: 200 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		return r
	}
	a, b = open("a.db"), open("b.db")
	if _, err := a.DB().Exec("CREATE TABLE writes(seq INTEGER PRIMARY KEY AUTOINCREMENT, epoch INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	b.Sync(context.Background())
	if s.AtLeast(0, 24) {
		return a, b
	}
	t.Skipf("sqld %s cannot run two replicas' write transactions concurrently", s.Version)
	return nil, nil
}

// Through two replicas of one primary, holders on different processes are
// ordered by the primary, so the stale epochs are refused.
func TestFenceOrdersEpochsAcrossReplicas(t *testing.T) {
	a, b := replicas(t)
	sync := func() { a.Sync(context.Background()); b.Sync(context.Background()) }
	if got := sequence(t, a.DB(), b.DB(), a.DB(), sync); !equal(got, []int64{5, 7, 7}) {
		t.Errorf("accepted epochs %v, want [5 7 7]", got)
	}
}

// Two holders, on two replicas, write at once: the old epoch's writes all
// land before the new epoch's first write, every refusal is ErrFenced, and
// nothing interleaves.
func TestConcurrentHoldersNeverInterleave(t *testing.T) {
	a, b := replicas(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for _, h := range []struct {
		db    *sql.DB
		epoch uint64
	}{{a.DB(), 5}, {b.DB(), 7}} {
		wg.Add(1)
		go func(db *sql.DB, epoch uint64) {
			defer wg.Done()
			f := fence.Fence{Cluster: "c1", Job: "projector", Epoch: epoch}
			for i := 0; i < 60; i++ {
				if err := write(ctx, db, f); err != nil && !errors.Is(err, fence.ErrFenced) {
					errs <- err
				}
			}
		}(h.db, h.epoch)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a fenced write failed other than by the fence: %v", err)
	}
	a.Sync(ctx)
	rows, err := a.DB().Query("SELECT epoch FROM writes ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var last int64
	var n7 int
	for rows.Next() {
		var e int64
		rows.Scan(&e)
		if e < last {
			t.Fatalf("epoch %d written after epoch %d", e, last)
		}
		last = e
		if e == 7 {
			n7++
		}
	}
	if n7 != 60 {
		t.Errorf("the newer holder wrote %d times, want every one of its 60", n7)
	}
}
