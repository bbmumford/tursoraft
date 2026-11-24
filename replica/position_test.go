package replica

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/pull"
)

// progressEngine stands in for an engine over a scope database: a pull
// brings the replica to the position the primary held when the pull began.
type progressEngine struct {
	fakeNative
	primary atomic.Uint64
	reached atomic.Uint64
	gate    chan struct{} // when set, each pull waits for a receive before it ends
}

func (p *progressEngine) Sync() (uint64, error) {
	at := p.primary.Load()
	frame, err := p.fakeNative.Sync()
	if p.gate != nil {
		<-p.gate
	}
	if err == nil {
		p.reached.Store(at)
	}
	return frame, err
}

// bounded is a context a test's wait gives up on after 5 s, so a position
// that never arrives fails the test instead of hanging it.
func bounded(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newPositionReplica(p *progressEngine, interval time.Duration) *Replica {
	r := &Replica{
		cfg: Config{Path: "/fake", Scope: Scope{Database: "orbtr-r-fake", Digest: make([]byte, 32)}},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), interval: interval, native: p,
		db:   sql.OpenDB(closingConnector{&p.fakeNative}),
		reqs: make(chan chan error), stop: make(chan struct{}), done: make(chan struct{}),
		lastPull: time.Now(),
		readPosition: func(context.Context) (uint64, error) {
			return p.reached.Load(), nil
		},
	}
	go r.loop()
	return r
}

func TestRequestPullForAHeldPositionReturnsAtOnce(t *testing.T) {
	p := &progressEngine{}
	p.primary.Store(4)
	r := newPositionReplica(p, time.Hour)
	defer r.Close()
	if err := r.RequestPull(bounded(t), 4); err != nil {
		t.Fatal(err)
	}
	base := p.pulls.Load()
	for _, at := range []uint64{0, 3, 4} {
		if err := r.RequestPull(bounded(t), at); err != nil {
			t.Fatalf("RequestPull(%d): %v", at, err)
		}
	}
	if n := p.pulls.Load() - base; n != 0 {
		t.Errorf("requests for held positions made %d pulls", n)
	}
}

// A request is answered by the position the pull reached, not by the pull
// ending: a pull that began before the position was on the primary does not
// answer it.
func TestRequestPullWaitsForThePositionNotForAPull(t *testing.T) {
	p := &progressEngine{gate: make(chan struct{})}
	p.primary.Store(1)
	r := newPositionReplica(p, time.Hour)
	defer r.Close()
	p.gate <- struct{}{} // the loop's first pull ends at 1

	done := make(chan error, 1)
	go func() { done <- r.RequestPull(bounded(t), 2) }()
	p.gate <- struct{}{} // a pull for the request ends, still at 1
	select {
	case err := <-done:
		t.Fatalf("RequestPull(2) returned %v with the replica at %d", err, r.Position())
	case <-time.After(100 * time.Millisecond):
	}
	p.primary.Store(2)
	for {
		select {
		case p.gate <- struct{}{}:
			continue
		case err := <-done:
			if err != nil || r.Position() < 2 {
				t.Fatalf("RequestPull(2) = %v at position %d", err, r.Position())
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("RequestPull(2) did not return once the primary held 2")
		}
	}
}

// 100 requests that arrive while a pull runs cost at most one more pull.
func TestConcurrentRequestsShareOnePull(t *testing.T) {
	p := &progressEngine{fakeNative: fakeNative{pullTime: 100 * time.Millisecond}}
	r := newPositionReplica(p, time.Hour)
	defer r.Close()
	time.Sleep(150 * time.Millisecond) // the loop's first pull
	p.primary.Store(9)
	go r.Sync(context.Background()) // a pull in progress
	time.Sleep(20 * time.Millisecond)
	base := p.pulls.Load()
	var wg sync.WaitGroup
	var failed atomic.Int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.RequestPull(bounded(t), 9); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.Fatalf("%d requests failed", failed.Load())
	}
	if n := p.pulls.Load() - base; n > 1 {
		t.Errorf("100 concurrent requests made %d pulls after the one in progress, want at most 1", n)
	}
}

// A position the primary never holds is not reported reached: the request
// ends with ctx's error.
func TestRequestPullForAPositionNeverWrittenEndsWithCtx(t *testing.T) {
	p := &progressEngine{}
	p.primary.Store(3)
	r := newPositionReplica(p, time.Hour)
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.RequestPull(ctx, 4) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("RequestPull(4) with the primary at 3 = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RequestPull(4) did not return 5 s after its ctx ended")
	}
	if n := p.pulls.Load(); n < 2 {
		t.Errorf("the request pulled %d times in 600 ms, want it to keep pulling", n)
	}
}

func TestWatchPositionSeesEveryPositionTheLoopReaches(t *testing.T) {
	p := &progressEngine{}
	p.primary.Store(1)
	r := newPositionReplica(p, time.Hour)
	if err := r.RequestPull(bounded(t), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := r.WatchPosition(ctx)
	next := func() uint64 {
		t.Helper()
		select {
		case p := <-w:
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("WatchPosition delivered nothing within 5 s")
			return 0
		}
	}
	var got []uint64
	got = append(got, next())
	for _, at := range []uint64{2, 3, 5} {
		p.primary.Store(at)
		if err := r.RequestPull(bounded(t), at); err != nil {
			t.Fatal(err)
		}
		got = append(got, next())
	}
	want := []uint64{1, 2, 3, 5}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("WatchPosition delivered %v, want %v", got, want)
		}
	}
	cancel()
	select {
	case _, open := <-w:
		if open {
			t.Error("the watch delivered a value after ctx ended")
		}
	case <-time.After(time.Second):
		t.Error("the watch's channel did not close when ctx ended")
	}
	r.Close()
}

func TestWholeDatabaseReplicaRecordsNoPosition(t *testing.T) {
	r := newLoopReplica(&fakeNative{}, time.Hour)
	defer r.Close()
	if err := r.RequestPull(bounded(t), 1); !errors.Is(err, ErrNoPosition) {
		t.Errorf("RequestPull = %v, want ErrNoPosition", err)
	}
	if _, open := <-r.WatchPosition(context.Background()); open {
		t.Error("WatchPosition of a whole-database replica delivered a position")
	}
	if err := NewSet().Add(r); !errors.Is(err, ErrNoPosition) {
		t.Errorf("Set.Add of a whole-database replica = %v, want ErrNoPosition", err)
	}
}

func TestSetRoutesRequestsBySegment(t *testing.T) {
	p := &progressEngine{}
	p.primary.Store(7)
	r := newPositionReplica(p, time.Hour)
	defer r.Close()
	s := NewSet()
	if err := s.Add(r); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(r); err == nil {
		t.Error("a second replica of one segment was added")
	}
	var trigger pull.Trigger = s
	if err := trigger.RequestPull(bounded(t), "orbtr-r-fake", 7); err != nil || r.Position() != 7 {
		t.Errorf("RequestPull of the held segment = %v at position %d", err, r.Position())
	}
	if err := trigger.RequestPull(bounded(t), "orbtr-r-other", 1); !errors.Is(err, pull.ErrUnknownSegment) {
		t.Errorf("RequestPull of a segment not held = %v, want pull.ErrUnknownSegment", err)
	}
	if got := s.Remove("orbtr-r-fake"); got != r {
		t.Error("Remove did not return the replica")
	}
	if err := trigger.RequestPull(bounded(t), "orbtr-r-fake", 1); !errors.Is(err, pull.ErrUnknownSegment) {
		t.Errorf("RequestPull after Remove = %v, want pull.ErrUnknownSegment", err)
	}
}

func TestPositionIsNeverLowered(t *testing.T) {
	r := &Replica{}
	r.notePosition(5)
	r.notePosition(3)
	if p := r.Position(); p != 5 {
		t.Errorf("Position after 5 then 3 = %d, want 5", p)
	}
}

func TestParsePosition(t *testing.T) {
	for s, want := range map[string]uint64{"": 0, "0": 0, "42": 42, "18446744073709551615": 1<<64 - 1} {
		if got, err := parsePosition(s); err != nil || got != want {
			t.Errorf("parsePosition(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
	for _, s := range []string{"-1", "x", "1.5", " 3", "18446744073709551616"} {
		if got, err := parsePosition(s); err == nil {
			t.Errorf("parsePosition(%q) = %d, want an error", s, got)
		}
	}
}
