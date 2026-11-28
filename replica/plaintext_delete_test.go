//go:build cgo

package replica

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A replica the engine left unencrypted is deleted at once with its WAL, its
// shared-memory index and the engine's other companion files: by the time
// Open returns ErrPlaintextReplica none of them is on disk, so the
// unencrypted rows are not left readable until a later open.
func TestARefusedUnencryptedReplicaIsDeletedAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	closed := withUnencryptedEngine(t, func(p string) {
		copyFixture(t, p)
		for _, suffix := range []string{"-wal", "-info"} {
			if err := os.WriteFile(p+suffix, []byte("companion"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	r, err := Open(context.Background(), plaintextSpec(path))
	if r != nil {
		r.Close()
		t.Fatal("Open served the unencrypted replica")
	}
	if !errors.Is(err, ErrPlaintextReplica) {
		t.Fatalf("Open = %v, want ErrPlaintextReplica", err)
	}
	if !closed.Load() {
		t.Error("the engine was left open")
	}
	for _, suffix := range replicaFiles {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still on disk after the refusal (stat: %v)", "r.db"+suffix, err)
		}
	}
}
