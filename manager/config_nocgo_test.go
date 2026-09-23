//go:build !cgo

package manager

import (
	"context"
	"errors"
	"testing"
)

// A build without cgo that declares a shared group is refused at validation,
// before anything is opened: its replicas cannot exist.
func TestSharedGroupNeedsCGO(t *testing.T) {
	cfg := validConfig(t)
	if err := cfg.Validate(); !errors.Is(err, ErrReplicaNeedsCGO) {
		t.Fatalf("Validate: got %v, want ErrReplicaNeedsCGO", err)
	}
	if _, err := NewManager(context.Background(), cfg); !errors.Is(err, ErrReplicaNeedsCGO) {
		t.Fatalf("NewManager: got %v, want ErrReplicaNeedsCGO", err)
	}
}
