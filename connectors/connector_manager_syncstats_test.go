package connectors

import (
	"context"
	"errors"
	"testing"

	"github.com/bbmumford/tursoraft/database"
)

// TestSyncStatsReportsAKnownDatabase is the positive leg. Without it the
// unknown-key and unavailable-stats tests below would both pass against a
// method that always returned an error.
func TestSyncStatsReportsAKnownDatabase(t *testing.T) {
	cm := NewConnectorManager()
	h := database.NewDatabaseHandle(nil)
	h.WithSyncStats(func(context.Context) (database.SyncStats, error) {
		return database.SyncStats{PendingLocalWrites: 7, LastPushUnixTime: 0}, nil
	})
	// Populated directly: AddConnector verifies a live connection, which is not
	// what this exercises and cannot be satisfied by a handle over a nil DB.
	cm.connectors["g.db"] = h

	got, err := cm.SyncStats(context.Background(), "g.db")
	if err != nil {
		t.Fatalf("SyncStats on a known database: %v", err)
	}
	if got.PendingLocalWrites != 7 {
		t.Fatalf("PendingLocalWrites = %d, want the handle's 7", got.PendingLocalWrites)
	}
}

// TestSyncStatsRefusesAnUnknownDatabase pins the reason this method cannot
// answer with a zero value: zero pending writes is the HEALTHY reading, so a
// caller that mistyped a database name would read the typo as a synced replica.
func TestSyncStatsRefusesAnUnknownDatabase(t *testing.T) {
	cm := NewConnectorManager()

	got, err := cm.SyncStats(context.Background(), "g.absent")
	if err == nil {
		t.Fatalf("SyncStats on an absent database returned %+v and no error", got)
	}
	if got != (database.SyncStats{}) {
		t.Fatalf("SyncStats returned %+v beside its error; the reading must be empty", got)
	}
}

// TestSyncStatsPassesUnavailableThrough keeps "cannot measure" distinguishable
// from "measured zero". The handle returns ErrSyncStatsUnavailable rather than a
// zero reading when it carries no stats function, and wrapping or replacing that
// error here would cost a caller the ability to tell an unmeasurable replica
// from a healthy one.
func TestSyncStatsPassesUnavailableThrough(t *testing.T) {
	cm := NewConnectorManager()
	cm.connectors["g.nostats"] = database.NewDatabaseHandle(nil)

	_, err := cm.SyncStats(context.Background(), "g.nostats")
	if !errors.Is(err, database.ErrSyncStatsUnavailable) {
		t.Fatalf("SyncStats error = %v, want ErrSyncStatsUnavailable", err)
	}
}
