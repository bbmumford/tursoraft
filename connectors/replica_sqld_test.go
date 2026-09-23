//go:build cgo

package connectors

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/bbmumford/tursoraft/internal/sqldtest"
)

// With a primary URL MakeConnector opens an encrypted replica whose handle
// syncs and reports replica stats; closing the handle stops the replica
// before the pool.
func TestPrimaryURLOpensAReplica(t *testing.T) {
	s := sqldtest.Start(t)
	k := make([]byte, 32)
	rand.Read(k)
	h, err := MakeConnector(filepath.Join(t.TempDir(), "r.db"), s.URL(), "db-token", base64.StdEncoding.EncodeToString(k))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Database().Exec("CREATE TABLE t(x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := h.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := h.Stats(context.Background())
	if err != nil || st.LastPullUnixTime == 0 {
		t.Errorf("stats %+v, %v", st, err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := MakeConnector(filepath.Join(t.TempDir(), "n.db"), s.URL(), "db-token", ""); err == nil {
		t.Error("a replica opened with no key")
	}
}
