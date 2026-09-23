//go:build cgo

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bbmumford/tursoraft/fence"
	"github.com/bbmumford/tursoraft/internal/sqldtest"
	"github.com/bbmumford/tursoraft/replica"
)

// Against a real primary: rows fenced by a lost cluster refuse the new
// cluster's holder until fence-adopt moves them.
func TestFenceAdoptOnAPrimary(t *testing.T) {
	s := sqldtest.Start(t)
	key := make([]byte, 32)
	rand.Read(key)
	r, err := replica.Open(context.Background(), replica.Config{Path: filepath.Join(t.TempDir(), "r.db"), URL: s.URL(), AuthToken: "t", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	noop := func(*sql.Tx) error { return nil }
	if err := (fence.Fence{Cluster: "lost", Job: "projector", Epoch: 812}).Tx(ctx, r.DB(), noop); err != nil {
		t.Fatal(err)
	}
	next := fence.Fence{Cluster: "fresh", Job: "projector", Epoch: 3}
	if err := next.Tx(ctx, r.DB(), noop); !errors.Is(err, fence.ErrForeignCluster) {
		t.Fatalf("before adopt: got %v, want ErrForeignCluster", err)
	}
	var out, errOut bytes.Buffer
	env := map[string]string{"TURSO_DB_TOKEN": "t"}
	if code := run(ctx, []string{"fence-adopt", "-url", s.URL(), "-cluster", "fresh", "-confirm-old-cluster-gone"},
		func(k string) string { return env[k] }, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if err := next.Tx(ctx, r.DB(), noop); err != nil {
		t.Fatalf("after adopt: %v", err)
	}
}
