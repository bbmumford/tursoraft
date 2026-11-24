package replica

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNative counts pulls and records whether Close ever ran while a pull
// was inside the engine.
type fakeNative struct {
	pullTime  time.Duration
	pulls     atomic.Int64
	inPull    atomic.Bool
	closed    atomic.Bool
	unsafe    atomic.Bool
	mu        sync.Mutex
	starts    []time.Time
	failPulls atomic.Bool
}

func (f *fakeNative) Connect(context.Context) (driver.Conn, error) { return nil, errors.New("fake") }
func (f *fakeNative) Driver() driver.Driver                        { return nil }
func (f *fakeNative) Close() error {
	if f.inPull.Load() {
		f.unsafe.Store(true)
	}
	f.closed.Store(true)
	return nil
}
func (f *fakeNative) Sync() (uint64, error) {
	f.inPull.Store(true)
	defer f.inPull.Store(false)
	f.mu.Lock()
	f.starts = append(f.starts, time.Now())
	f.mu.Unlock()
	n := f.pulls.Add(1)
	time.Sleep(f.pullTime)
	if f.failPulls.Load() {
		return 0, errors.New("primary down")
	}
	return uint64(n), nil
}

// closingConnector closes the fake when the pool closes, as go-libsql's
// connector is closed by the policy pool that wraps it.
type closingConnector struct{ f *fakeNative }

func (c closingConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("fake")
}
func (c closingConnector) Driver() driver.Driver { return nil }
func (c closingConnector) Close() error          { return c.f.Close() }

func newLoopReplica(f *fakeNative, interval time.Duration) *Replica {
	r := &Replica{
		cfg: Config{Path: "/fake"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), interval: interval, engine: f,
		db:   sql.OpenDB(closingConnector{f}),
		reqs: make(chan chan error), stop: make(chan struct{}), done: make(chan struct{}),
		lastPull: time.Now(),
	}
	go r.loop()
	return r
}

// Concurrent Sync callers share pulls, and pulls start at least SyncGap
// apart, so a flood of callers costs a bounded number of pulls; each caller
// returns only after a pull that began after its call.
func TestSyncIsSingleFlightWithAGap(t *testing.T) {
	f := &fakeNative{pullTime: 20 * time.Millisecond}
	r := newLoopReplica(f, time.Hour)
	defer r.Close()
	time.Sleep(50 * time.Millisecond) // the loop's first pull
	base := f.pulls.Load()

	start := time.Now()
	var wg sync.WaitGroup
	var bad atomic.Int64
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			called := time.Now()
			if err := r.Sync(context.Background()); err != nil {
				bad.Add(1)
				return
			}
			f.mu.Lock()
			last := f.starts[len(f.starts)-1]
			f.mu.Unlock()
			if last.Before(called) {
				bad.Add(1)
			}
		}()
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
	elapsed := time.Since(start)
	pulls := f.pulls.Load() - base
	if bad.Load() != 0 {
		t.Errorf("%d callers returned without a pull that started after their call", bad.Load())
	}
	limit := int64(elapsed/SyncGap) + 2
	if pulls > limit {
		t.Errorf("200 callers over %v made %d pulls, want at most %d", elapsed, pulls, limit)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 1; i < len(f.starts); i++ {
		if gap := f.starts[i].Sub(f.starts[i-1]); gap < SyncGap-5*time.Millisecond {
			t.Errorf("pulls %d and %d started %v apart, want at least %v", i-1, i, gap, SyncGap)
		}
	}
}

// Close waits for a pull in progress before the pool closes the connector.
func TestCloseStopsTheLoopBeforeTheConnector(t *testing.T) {
	f := &fakeNative{pullTime: 300 * time.Millisecond}
	r := newLoopReplica(f, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // inside the first pull
	if !f.inPull.Load() {
		t.Fatal("setup: no pull in progress")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !f.closed.Load() {
		t.Error("Close did not close the connector")
	}
	if f.unsafe.Load() {
		t.Error("the connector was closed while a pull was inside it")
	}
	if err := r.Sync(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Sync after Close: got %v, want ErrClosed", err)
	}
}

// A replica whose pulls fail turns stale after three intervals and reports
// the error; a later success clears both.
func TestStatsReportStaleness(t *testing.T) {
	f := &fakeNative{}
	f.failPulls.Store(true)
	r := newLoopReplica(f, 20*time.Millisecond)
	defer r.Close()
	time.Sleep(100 * time.Millisecond)
	if st := r.Stats(); !st.Stale || st.Err == nil {
		t.Fatalf("after failing pulls: stale=%v err=%v, want stale with an error", st.Stale, st.Err)
	}
	f.failPulls.Store(false)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := r.Stats()
	if st.Stale || st.Err != nil || st.Frame == 0 {
		t.Errorf("after a pull: %+v", st)
	}
	if ss := st.SyncStats(); ss.PendingLocalWrites != 0 || ss.LastPushUnixTime != 0 || ss.LastPullUnixTime == 0 || ss.Revision == "" {
		t.Errorf("sync stats %+v", ss)
	}
}

// Callers that arrive while a pull is running share the next pull, however
// long that pull took. Each is blocked sending its request while the loop is
// inside the engine; once the pull returns, the gap since it started has
// already passed, so a loop that took one request at a time would pull once
// per caller, one after another. A session store asks for a pull on every
// proved request, so against a slow primary that queue grows with the request
// rate.
func TestCallersQueuedBehindASlowPullShareTheNextOne(t *testing.T) {
	f := &fakeNative{pullTime: 300 * time.Millisecond}
	r := newLoopReplica(f, time.Hour)
	defer r.Close()
	time.Sleep(400 * time.Millisecond) // the loop's first pull
	base := f.pulls.Load()

	first := make(chan error, 1)
	go func() { first <- r.Sync(context.Background()) }()
	for !f.inPull.Load() {
		time.Sleep(time.Millisecond)
	}

	const callers = 16
	var wg sync.WaitGroup
	var late atomic.Int64
	start := time.Now()
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			called := time.Now()
			if err := r.Sync(context.Background()); err != nil {
				t.Errorf("Sync: %v", err)
				return
			}
			f.mu.Lock()
			last := f.starts[len(f.starts)-1]
			f.mu.Unlock()
			if last.Before(called) {
				late.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	if err := <-first; err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	pulls := f.pulls.Load() - base
	if late.Load() != 0 {
		t.Errorf("%d callers returned without a pull that started after their call", late.Load())
	}
	// One pull for the first caller and one shared by the sixteen queued behind it.
	if pulls > 2 {
		t.Errorf("one caller and %d queued behind its pull made %d pulls, want 2: the queued callers did not share one", callers, pulls)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the queued callers took %v, want them answered by the one pull after the first", elapsed)
	}
}

// One pull answers at most maxPullBatch of the callers waiting for it, and
// the rest share the pulls after it. Taking every caller that is ready, with
// no bound, lets callers arriving faster than they are taken hold the next
// pull off; the bound is what keeps the pull coming. Every caller is answered
// once, by a pull that started after its call.
func TestOnePullAnswersAtMostABatchOfWaitingCallers(t *testing.T) {
	f := &fakeNative{pullTime: 300 * time.Millisecond}
	r := newLoopReplica(f, time.Hour)
	time.Sleep(400 * time.Millisecond) // the loop's first pull
	base := f.pulls.Load()

	first := make(chan error, 1)
	go func() { first <- r.Sync(context.Background()) }()
	for !f.inPull.Load() {
		time.Sleep(time.Millisecond)
	}

	const callers = 2*maxPullBatch + 100
	var wg sync.WaitGroup
	var late, failed atomic.Int64
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			called := time.Now()
			if err := r.Sync(context.Background()); err != nil {
				failed.Add(1)
				return
			}
			f.mu.Lock()
			last := f.starts[len(f.starts)-1]
			f.mu.Unlock()
			if last.Before(called) {
				late.Add(1)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("callers still waiting 10s after they called")
	}
	if err := <-first; err != nil {
		t.Fatalf("the first Sync: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if failed.Load() != 0 || late.Load() != 0 {
		t.Errorf("%d callers failed and %d returned without a pull that started after their call", failed.Load(), late.Load())
	}
	// The first caller's pull, then three batches for the callers queued
	// behind it.
	want := int64(1 + (callers+maxPullBatch-1)/maxPullBatch)
	if pulls := f.pulls.Load() - base; pulls < want {
		t.Errorf("%d callers queued behind one pull were answered by %d pulls, want %d: one pull took more than maxPullBatch", callers, pulls, want)
	}
}
