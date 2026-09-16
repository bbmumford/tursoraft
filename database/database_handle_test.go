package database

import (
	"context"
	"errors"
	"testing"
)

// A handle that keeps no replica and a replica with nothing outstanding are the
// two states a caller must never confuse: one is unknown, the other is healthy.
// These pin that Stats keeps them apart, because the whole value of the counter
// is lost if an unmeasurable handle reads as a synced one.

// TestStatsOnAHandleWithNoReplicaIsUnavailableNotSynced is the negative leg.
//
// A nil stats function is every non-embedded build, so this is the common case
// rather than an edge one: if it returned a bare zero SyncStats with no error, a
// health check asking "are there writes still pending here?" would read 0 and
// report the handle as fully synced.
func TestStatsOnAHandleWithNoReplicaIsUnavailableNotSynced(t *testing.T) {
	h := NewDatabaseHandle(nil)

	got, err := h.Stats(context.Background())
	if !errors.Is(err, ErrSyncStatsUnavailable) {
		t.Fatalf("a handle with no stats function must report stats as unavailable, got err=%v", err)
	}
	if got != (SyncStats{}) {
		t.Errorf("the unavailable case must carry no figures a caller could act on, got %+v", got)
	}
}

// TestStatsReportsAReplicaThatHasNeverPushed is the positive leg, and it is the
// shape this counter exists to make visible: writes have landed locally and no
// push has ever succeeded.
//
// Without it the negative test above would pass against a Stats that always
// failed, so this is what proves the reader works at all.
func TestStatsReportsAReplicaThatHasNeverPushed(t *testing.T) {
	h := NewDatabaseHandle(nil)
	h.WithSyncStats(func(context.Context) (SyncStats, error) {
		return SyncStats{PendingLocalWrites: 42, LastPullUnixTime: 1700000000}, nil
	})

	got, err := h.Stats(context.Background())
	if err != nil {
		t.Fatalf("a handle with a stats function must report them: %v", err)
	}
	if got.PendingLocalWrites != 42 {
		t.Errorf("pending local writes must reach the caller, got %d", got.PendingLocalWrites)
	}
	if got.LastPushUnixTime != 0 {
		t.Errorf("this replica has never pushed, so its last push time must stay 0, got %d",
			got.LastPushUnixTime)
	}
}

// TestStatsSurfacesTheReadersError keeps a failing read distinguishable from an
// absent one: both leave the caller without figures, but only one says the
// handle cannot report at all, and a caller retrying on the wrong one waits
// forever for an answer that is never coming.
func TestStatsSurfacesTheReadersError(t *testing.T) {
	sentinel := errors.New("driver unreachable")
	h := NewDatabaseHandle(nil)
	h.WithSyncStats(func(context.Context) (SyncStats, error) {
		return SyncStats{}, sentinel
	})

	_, err := h.Stats(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("the reader's own error must reach the caller, got %v", err)
	}
	if errors.Is(err, ErrSyncStatsUnavailable) {
		t.Error("a reader that failed is not a handle that keeps no replica; the two must stay distinct")
	}
}

// TestStatsHonoursACancelledContext matches Sync's own behaviour, so a caller
// cancelling a health sweep does not start a driver round-trip it will discard.
func TestStatsHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	h := NewDatabaseHandle(nil)
	h.WithSyncStats(func(context.Context) (SyncStats, error) {
		called = true
		return SyncStats{}, nil
	})

	if _, err := h.Stats(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context must be reported, got %v", err)
	}
	if called {
		t.Error("the reader must not run once the context is already cancelled")
	}
}
