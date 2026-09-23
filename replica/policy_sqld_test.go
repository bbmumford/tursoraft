//go:build cgo

package replica

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/internal/sqldtest"
)

// The replica engine answers a SELECT from the local file and sends PRAGMA
// foreign_keys to the primary. policypool runs that PRAGMA on every new
// connection and on every reuse of a pooled one, so each pooled statement
// costs a round trip to the primary and fails while it is down, while the
// engine alone keeps serving the SELECT. This pins the measurement the
// replica's read path depends on.
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
		t.Error("PRAGMA foreign_keys answered with the primary down; it is no longer sent to the primary, so pooled reads may now survive an outage: revisit TestOutageServesReads")
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM t").Scan(&n); err == nil {
		t.Error("a pooled SELECT answered with the primary down: revisit TestOutageServesReads")
	}
}
