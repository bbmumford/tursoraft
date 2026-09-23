//go:build cgo

package replica

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bbmumford/policypool"

	"github.com/bbmumford/tursoraft/sqldtest"
)

// The replica engine answers a SELECT from the local file and sends PRAGMA
// foreign_keys to the primary, so a foreign-key proof costs a round trip to
// the primary and fails while it is down. The pool proves the setting only
// when a connection opens, so a pooled SELECT on a connection already proved
// answers from the file with the primary down, as the engine's own SELECT
// does. This pins both measurements the replica's read path depends on.
func TestPolicyProofReachesThePrimary(t *testing.T) {
	s := sqldtest.Start(t)
	cfg := Config{Path: filepath.Join(t.TempDir(), "p.db"), URL: s.URL(), AuthToken: "test", Key: make([]byte, 32)}
	r, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.DB().Exec("CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	raw := sql.OpenDB(r.native)
	raw.SetMaxOpenConns(1)
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	timed := func(q string, fn func() error) time.Duration {
		start := time.Now()
		for i := 0; i < 50; i++ {
			if err := fn(); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		return time.Since(start) / 50
	}
	var n int64
	local := timed("engine SELECT", func() error { return conn.QueryRowContext(context.Background(), "SELECT count(*) FROM t").Scan(&n) })
	pragma := timed("engine PRAGMA", func() error { return conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&n) })
	pooled := timed("pooled SELECT", func() error { return r.DB().QueryRow("SELECT count(*) FROM t").Scan(&n) })
	t.Logf("per statement: engine SELECT %v, engine PRAGMA foreign_keys %v, pooled SELECT %v", local, pragma, pooled)

	s.Stop()
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Errorf("the engine did not serve a SELECT with the primary down: %v", err)
	}
	if err := conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&n); err == nil {
		t.Error("PRAGMA foreign_keys answered with the primary down: the engine no longer sends it to the primary, so the measurement behind the connect-only policy has changed")
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Errorf("a pooled SELECT failed with the primary down: the pool proved foreign keys again on reuse: %v", err)
	}
}

// A replica pool proves foreign keys only when a connection opens, so its
// guard is what keeps them on afterwards: a statement that turns them off is
// refused with policypool.ErrPolicyRefused before it reaches the primary, and
// the pool cannot write an orphan. The same statement on a raw engine
// connection is accepted, and that connection then writes the orphan.
func TestTheGuardHoldsForeignKeysOnAReplicaPool(t *testing.T) {
	s := sqldtest.Start(t)
	ctx := context.Background()
	r, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "g.db"), URL: s.URL(), AuthToken: "test", Key: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.DB().Exec("CREATE TABLE p(id INTEGER PRIMARY KEY); CREATE TABLE c(id INTEGER PRIMARY KEY, p INTEGER NOT NULL REFERENCES p(id))"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"PRAGMA foreign_keys=OFF", "PRAGMA foreign_keys = 0", "PRAGMA main.foreign_keys=no"} {
		if _, err := r.DB().ExecContext(ctx, q); !errors.Is(err, policypool.ErrPolicyRefused) {
			t.Errorf("pool %q: got %v, want ErrPolicyRefused", q, err)
		}
	}
	if _, err := r.DB().ExecContext(ctx, "INSERT INTO c VALUES (1, 99)"); err == nil {
		t.Error("the pool wrote an orphan row")
	}

	raw := sql.OpenDB(r.native)
	raw.SetMaxOpenConns(1)
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatalf("a raw engine connection refused PRAGMA foreign_keys=OFF: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO c VALUES (2, 99)"); err != nil {
		t.Errorf("with foreign keys off, a raw engine connection did not write the orphan: %v", err)
	}
}
