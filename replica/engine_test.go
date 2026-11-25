//go:build cgo

package replica

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// errLocked is the refusal libSQL's HTTP sync path returns for a connection
// opened while a pull holds the database.
var errLocked = errors.New("failed to connect to database\nerror code = 1: Unable to connect: SQLite failure: `database is locked`")

// refusingEngine refuses its first refusals connects with err (every one when
// refusals is negative), then connects.
type refusingEngine struct {
	fakeEngine
	refusals int64
	err      error
	calls    atomic.Int64
}

func (e *refusingEngine) Connect(ctx context.Context) (driver.Conn, error) {
	if n := e.calls.Add(1); e.refusals < 0 || n <= e.refusals {
		return nil, e.err
	}
	return e.fakeEngine.Connect(ctx)
}

func shortBusyWait(t *testing.T, d time.Duration) {
	t.Helper()
	saved := connectBusyWait
	connectBusyWait = d
	t.Cleanup(func() { connectBusyWait = saved })
}

// connectWithin runs Connect and fails the test when it has not returned
// within limit.
func connectWithin(t *testing.T, ctx context.Context, e native, limit time.Duration) (driver.Conn, time.Duration, error) {
	t.Helper()
	type result struct {
		conn driver.Conn
		err  error
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		conn, err := currentConnector{&Replica{engine: e}}.Connect(ctx)
		ch <- result{conn, err}
	}()
	select {
	case res := <-ch:
		return res.conn, time.Since(start), res.err
	case <-time.After(limit):
		t.Fatalf("Connect had not returned after %v", limit)
		return nil, 0, nil
	}
}

// A connection the engine refuses because the database is locked is opened
// again until the engine connects.
func TestConnectWaitsOutALockedDatabase(t *testing.T) {
	e := &refusingEngine{refusals: 3, err: errLocked}
	conn, _, err := connectWithin(t, context.Background(), e, 5*time.Second)
	if err != nil {
		t.Fatalf("Connect after three locked refusals: %v", err)
	}
	conn.Close()
	if n := e.calls.Load(); n != 4 {
		t.Errorf("the engine was asked %d times, want 4", n)
	}
}

// Any other refusal is returned at once, unretried.
func TestConnectReturnsAnyOtherRefusalAtOnce(t *testing.T) {
	other := errors.New("failed to connect to database: unable to open database file")
	e := &refusingEngine{refusals: -1, err: other}
	_, _, err := connectWithin(t, context.Background(), e, 5*time.Second)
	if !errors.Is(err, other) {
		t.Errorf("err = %v, want the engine's refusal", err)
	}
	if n := e.calls.Load(); n != 1 {
		t.Errorf("the engine was asked %d times, want 1", n)
	}
}

// A database that stays locked is given up on after connectBusyWait, with the
// engine's refusal.
func TestConnectGivesUpOnADatabaseThatStaysLocked(t *testing.T) {
	shortBusyWait(t, 200*time.Millisecond)
	e := &refusingEngine{refusals: -1, err: errLocked}
	_, took, err := connectWithin(t, context.Background(), e, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("err = %v, want the locked refusal", err)
	}
	if took < 150*time.Millisecond || e.calls.Load() < 2 {
		t.Errorf("gave up after %v and %d connects, want the wait out first", took, e.calls.Load())
	}
}

// The wait ends with ctx.
func TestConnectStopsWaitingWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	e := &refusingEngine{refusals: -1, err: errLocked}
	_, took, err := connectWithin(t, ctx, e, 3*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's deadline", err)
	}
	if took > time.Second {
		t.Errorf("returned %v after the context's 100 ms deadline", took)
	}
}
