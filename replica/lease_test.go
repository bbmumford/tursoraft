//go:build cgo

package replica

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	libsql "github.com/bbmumford/go-libsql"
)

// leasedSource is a credential source answering one grant of GrantLifetime
// with an offline lease of lease, both from issued.
type leasedSource struct {
	t      *testing.T
	issued time.Time
	lease  time.Duration
}

func (s leasedSource) Credential(context.Context) (Credential, error) {
	return Credential{
		Token:         unsignedJWT(s.t, map[string]any{"a": "ro", "exp": s.issued.Add(GrantLifetime).Unix()}),
		NotAfter:      s.issued.Add(GrantLifetime),
		LeaseNotAfter: s.issued.Add(s.lease),
		GrantID:       "grant-1",
	}, nil
}

// fakeClock is the lease clock a test moves; err set makes it refuse, as
// keyprotect's monotonic clock does for a wall clock wound back.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
	err error
}

func (c *fakeClock) set(now time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now, c.err = now, err
}

func (c *fakeClock) read() (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now, c.err
}

// With no live grant a replica serves reads only while its offline lease
// runs: a read after the grant ended and a second before the lease ends
// reaches the engine, one a second after it is refused with ErrLeaseEnded
// before the engine is asked, and so is every read while the lease clock
// answers an error. A lease longer than MaxOfflineLease is held to it.
func TestOfflineReadEndsWithTheLease(t *testing.T) {
	var e engines
	e.install(t)
	issued := time.Now()
	clock := &fakeClock{now: issued}
	r, err := Open(context.Background(), Config{
		Path: filepath.Join(t.TempDir(), "r.db"), URL: "http://127.0.0.1:1", Key: make([]byte, 32),
		Mode: ReadOnly, Credentials: leasedSource{t: t, issued: issued, lease: 7 * 24 * time.Hour},
		SyncInterval: time.Hour, OpenTimeout: time.Second, Clock: clock.read,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	read := func() error {
		_, err := r.DB().ExecContext(context.Background(), "SELECT 1")
		return err
	}
	leaseEnd := issued.Add(7 * 24 * time.Hour)
	for _, tc := range []struct {
		name  string
		now   time.Time
		err   error
		ended bool
	}{
		{"grant live", issued.Add(time.Minute), nil, false},
		{"grant ended, lease running", leaseEnd.Add(-time.Second), nil, false},
		{"lease ended", leaseEnd.Add(time.Second), nil, true},
		{"clock wound back", issued.Add(time.Minute), errors.New("keyprotect: clock rolled back"), true},
	} {
		clock.set(tc.now, tc.err)
		err := read()
		if tc.ended && !errors.Is(err, ErrLeaseEnded) {
			t.Errorf("%s: read = %v, want ErrLeaseEnded", tc.name, err)
		}
		if !tc.ended && !errors.Is(err, libsql.ErrReadOnlyReplica) {
			t.Errorf("%s: read = %v, want it to reach the engine (its read-only refusal)", tc.name, err)
		}
	}
	if got := boundLease(Credential{LeaseNotAfter: issued.Add(45 * 24 * time.Hour)}, issued).LeaseNotAfter; !got.Equal(issued.Add(MaxOfflineLease)) {
		t.Errorf("a 45-day lease is held to %v, want %v", got.Sub(issued), MaxOfflineLease)
	}
}
