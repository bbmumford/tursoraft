//go:build cgo

package replica

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	libsql "github.com/bbmumford/go-libsql"
)

// mintingSource hands out a new grant of life on every call, a read-only
// token with each, and fails the calls listed in failing (1-based).
type mintingSource struct {
	life    time.Duration
	calls   atomic.Int64
	failing func(call int64) bool
}

func (m *mintingSource) Credential(context.Context) (Credential, error) {
	call := m.calls.Add(1)
	if m.failing != nil && m.failing(call) {
		return Credential{}, errors.New("the grant host is unreachable")
	}
	now := time.Now()
	return Credential{
		Token:         unsignedToken(map[string]any{"a": "ro", "exp": now.Add(time.Minute).Unix(), "call": call}),
		NotAfter:      now.Add(m.life),
		LeaseNotAfter: now.Add(7 * 24 * time.Hour),
		GrantID:       fmt.Sprint("rgr_", call),
	}, nil
}

// engines records every engine openNative made and the token it was given;
// from the plaintextFrom-th open on (0: never) the engine leaves the replica
// file unencrypted.
type engines struct {
	mu            sync.Mutex
	opened        []*fakeEngine
	tokens        []string
	plaintextFrom int
}

func (e *engines) install(t *testing.T) {
	t.Helper()
	saved := openNative
	openNative = func(cfg Config, token string) (native, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		f := &fakeEngine{execErr: libsql.ErrReadOnlyReplica}
		e.opened = append(e.opened, f)
		e.tokens = append(e.tokens, token)
		if e.plaintextFrom > 0 && len(e.opened) >= e.plaintextFrom {
			b, err := os.ReadFile(filepath.Join("testdata", "plaintext-replica.db"))
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(cfg.Path, b, 0o600); err != nil {
				return nil, err
			}
		}
		return f, nil
	}
	t.Cleanup(func() { openNative = saved })
}

func (e *engines) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.opened)
}

func renewingReplica(t *testing.T, src CredentialSource) *Replica {
	t.Helper()
	r, err := Open(context.Background(), Config{
		Path: filepath.Join(t.TempDir(), "r.db"), URL: "http://127.0.0.1:1", Key: make([]byte, 32),
		Mode: ReadOnly, Credentials: src, SyncInterval: 50 * time.Millisecond, OpenTimeout: time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A grant is renewed two thirds into its life by closing the engine and
// opening it again under the new token, inside the loop: callers waiting for
// pulls all the while see no error, every replaced engine is closed, and the
// replica never stops holding a live grant.
func TestRenewalSwapsTheEngineInsideTheLoop(t *testing.T) {
	var e engines
	e.install(t)
	src := &mintingSource{life: 300 * time.Millisecond}
	r := renewingReplica(t, src)

	stop := make(chan struct{})
	var syncErrs atomic.Int64
	var syncs atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := r.Sync(context.Background()); err != nil {
					syncErrs.Add(1)
				}
				syncs.Add(1)
			}
		}()
	}
	time.Sleep(1300 * time.Millisecond)
	close(stop)
	wg.Wait()

	n := e.count()
	if n < 4 {
		t.Fatalf("%d engines opened in 1.3 s of 300 ms grants, want at least 4 (the open and a renewal at 200 ms each)", n)
	}
	e.mu.Lock()
	for i, f := range e.opened[:n-1] {
		if !f.closed.Load() {
			t.Errorf("engine %d was replaced but not closed", i)
		}
	}
	if e.opened[n-1].closed.Load() {
		t.Error("the engine in use was closed")
	}
	seen := map[string]bool{}
	for _, tok := range e.tokens {
		if seen[tok] {
			t.Error("an engine was reopened under a token it already used")
		}
		seen[tok] = true
	}
	e.mu.Unlock()
	if syncErrs.Load() != 0 {
		t.Errorf("%d of %d Sync calls failed across the renewals", syncErrs.Load(), syncs.Load())
	}
	if st := r.Stats(); st.Err != nil || st.LeaseNotAfter.IsZero() {
		t.Errorf("Stats after the renewals: err=%v lease=%v", st.Err, st.LeaseNotAfter)
	}
}

// A connection made on an engine the loop has replaced reports itself
// invalid and refuses a session reset, so database/sql closes it instead of
// handing it out, and the next connection is on the new engine.
func TestRenewalRetiresConnectionsOfTheReplacedEngine(t *testing.T) {
	var e engines
	e.install(t)
	r := renewingReplica(t, &mintingSource{life: 300 * time.Millisecond})
	ctx := context.Background()
	held, err := r.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var first *engineConn
	held.Raw(func(dc any) error { first = dc.(*engineConn); return nil })
	if !first.IsValid() {
		t.Fatal("a connection of the engine in use is invalid")
	}
	waitFor(t, "a renewal", func() bool { return e.count() >= 2 })
	if first.IsValid() {
		t.Error("a connection of the replaced engine still reports itself valid")
	}
	if err := first.ResetSession(ctx); !errors.Is(err, driver.ErrBadConn) {
		t.Errorf("ResetSession of a replaced engine's connection = %v, want driver.ErrBadConn", err)
	}
	held.Close()
	fresh, err := r.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	var next *engineConn
	fresh.Raw(func(dc any) error { next = dc.(*engineConn); return nil })
	if next.gen == first.gen || !next.IsValid() {
		t.Errorf("the next connection is of engine %d (valid %v), the replaced one was %d", next.gen, next.IsValid(), first.gen)
	}
}

// A grant that is not renewed stops the pulls when it ends: every pull fails
// with ErrGrantExpired, the replica turns stale, and the engine is not asked
// to pull again.
func TestGrantThatLapsesStopsPulls(t *testing.T) {
	var e engines
	e.install(t)
	r := renewingReplica(t, &mintingSource{life: 300 * time.Millisecond, failing: func(call int64) bool { return call > 1 }})
	time.Sleep(400 * time.Millisecond)
	if err := r.Sync(context.Background()); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("Sync after the grant ended = %v, want ErrGrantExpired", err)
	}
	e.mu.Lock()
	f := e.opened[0]
	e.mu.Unlock()
	before := f.syncs.Load()
	time.Sleep(250 * time.Millisecond)
	st := r.Stats()
	if !errors.Is(st.Err, ErrGrantExpired) || !st.Stale {
		t.Errorf("Stats after the grant ended: err=%v stale=%v, want ErrGrantExpired and stale", st.Err, st.Stale)
	}
	if after := f.syncs.Load(); after != before {
		t.Errorf("the engine pulled %d times after the grant ended", after-before)
	}
}

// A renewal that fails is asked for again one interval later, so a grant
// host that misses one call does not end the replica's pulls.
func TestRenewalFailureIsRetried(t *testing.T) {
	var e engines
	e.install(t)
	r := renewingReplica(t, &mintingSource{life: 600 * time.Millisecond, failing: func(call int64) bool { return call == 2 }})
	waitFor(t, "a renewal after the failed one", func() bool { return e.count() >= 2 })
	time.Sleep(300 * time.Millisecond)
	if err := r.Sync(context.Background()); err != nil {
		t.Errorf("Sync after a missed renewal and a retried one = %v", err)
	}
}

// An engine reopened under a renewed grant that leaves the file unencrypted
// ends the replica: pulls and new connections fail with ErrPlaintextReplica,
// and that engine is closed.
func TestReopenRefusesAnEngineThatLeftTheFileUnencrypted(t *testing.T) {
	e := engines{plaintextFrom: 2}
	e.install(t)
	r := renewingReplica(t, &mintingSource{life: 300 * time.Millisecond})
	waitFor(t, "the reopen", func() bool { return e.count() >= 2 })
	waitFor(t, "the refusal", func() bool { return errors.Is(r.Sync(context.Background()), ErrPlaintextReplica) })
	if c, err := r.DB().Conn(context.Background()); !errors.Is(err, ErrPlaintextReplica) {
		if c != nil {
			c.Close()
		}
		t.Errorf("a new connection after the refusal: err = %v, want ErrPlaintextReplica", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.opened[1].closed.Load() {
		t.Error("the engine that left the file unencrypted was not closed")
	}
}
