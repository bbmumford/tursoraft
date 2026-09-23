package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

// A handle that keeps no replica and a replica that has pulled are the two
// states a caller must never confuse: one is unknown, the other is a reading.
// These pin that Stats keeps them apart.

// TestStatsOnAHandleWithNoReplicaIsUnavailableNotSynced is the negative leg:
// a local file has no stats function, and a bare zero SyncStats with no error
// would read as a replica with a pull at time zero.
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

// TestStatsReportsTheReplicasReading is the positive leg: the figures the
// replica's stats function returns reach the caller unchanged.
func TestStatsReportsTheReplicasReading(t *testing.T) {
	h := NewDatabaseHandle(nil)
	want := SyncStats{LastPullUnixTime: 1700000000, Revision: "42"}
	h.WithSyncStats(func(context.Context) (SyncStats, error) { return want, nil })

	got, err := h.Stats(context.Background())
	if err != nil {
		t.Fatalf("a handle with a stats function must report them: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
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

// Close runs the cleanup functions before it closes the pool: a replica's
// sync loop drives the connector the pool owns and must stop first.
func TestCloseRunsCleanupBeforeThePool(t *testing.T) {
	var order []string
	h := NewDatabaseHandle(sql.OpenDB(recordingConnector{&order}))
	h.AddCloser(func() error { order = append(order, "cleanup"); return nil })
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "cleanup" || order[1] != "pool" {
		t.Errorf("close order %v, want [cleanup pool]", order)
	}
}

// recordingConnector notes when the pool closes it.
type recordingConnector struct{ order *[]string }

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("no connections")
}
func (c recordingConnector) Driver() driver.Driver { return nil }
func (c recordingConnector) Close() error {
	*c.order = append(*c.order, "pool")
	return nil
}
