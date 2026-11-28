package replica

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// A RequestPull whose context carries no deadline is bounded: for a position
// the replica never reaches it pulls until requestPullTimeout and answers
// ErrPositionNotReached, rather than pulling every SyncGap for as long as the
// replica is open. A caller's own deadline still governs its own request.
func TestRequestPullWithNoDeadlineIsBounded(t *testing.T) {
	saved := requestPullTimeout
	requestPullTimeout = 300 * time.Millisecond
	t.Cleanup(func() { requestPullTimeout = saved })
	f := &fakeNative{}
	r := &Replica{
		cfg: Config{Path: "/fake"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), interval: time.Hour, engine: f,
		db:           sql.OpenDB(closingConnector{f}),
		readPosition: func(context.Context) (uint64, error) { return 7, nil },
		reqs:         make(chan chan error), stop: make(chan struct{}), done: make(chan struct{}),
		lastPull: time.Now(),
	}
	go r.loop()
	t.Cleanup(func() { r.Close() })
	answer := make(chan error, 1)
	go func() { answer <- r.RequestPull(context.Background(), 100) }()
	select {
	case err := <-answer:
		if !errors.Is(err, ErrPositionNotReached) {
			t.Fatalf("RequestPull for a position never reached = %v, want ErrPositionNotReached", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RequestPull with no deadline for a position never reached was still pulling after 10s")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.RequestPull(ctx, 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RequestPull under the caller's deadline = %v, want its DeadlineExceeded", err)
	}
	if err := r.RequestPull(context.Background(), 7); err != nil {
		t.Fatalf("RequestPull for the position held = %v", err)
	}
}
