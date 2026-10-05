package sqldtest

import (
	"fmt"
	"testing"
)

// TestTheServedVersionGatesConcurrentTransactions holds the two readings of
// one sqld's release together: the "--version" line binaryVersion ranks
// candidates by, and the /version body start reads off the running server
// into Server.Version.
//
// CONSUMES candidates, binaryVersion, Start and
// RequireConcurrentTransactions. CONSUMED BY nothing; it is the only check on
// the path from Server.Version through AtLeast to the skip, which
// manager_sqld_test.go's TestJobsAcrossManagers and concurrency_sqld_test.go's
// TestConcurrentMigrationsApplyOnce and
// TestReadThenWriteInATransactionFromTwoReplicas reach through
// RequireConcurrentTransactions, and fence_sqld_test.go branches on through
// AtLeast.
//
// CONDITIONS: every release among the candidates is started once, each under
// $TURSORAFT_SQLD so Start takes that candidate rather than the newest, so a
// host holding one sqld before 0.24 and one after covers both sides of the
// bound. With no candidate whose version reads, nothing is started.
//
// WHY THIS SHAPE: AtLeast parses Server.Version, which start takes from the
// server's own /version body, while Binary ranks the same binary by its
// "--version" line; the two strings are shaped differently, and a
// Server.Version that no version comparison can read, or an AtLeast that
// ranks the release Binary chose as earlier than 0.24, makes
// RequireConcurrentTransactions skip. A skip is not a failure, so without
// this check every one of those tests can stop running while the suite still
// reports ok.
func TestTheServedVersionGatesConcurrentTransactions(t *testing.T) {
	started := 0
	seen := map[[3]int]bool{}
	for _, bin := range candidates() {
		ranked, known := binaryVersion(bin)
		if !known || seen[ranked] {
			continue
		}
		seen[ranked] = true
		started++
		t.Run(fmt.Sprintf("%d.%d.%d", ranked[0], ranked[1], ranked[2]), func(t *testing.T) {
			t.Setenv("TURSORAFT_SQLD", bin)
			s := Start(t)
			var served [3]int
			if _, err := fmt.Sscanf(s.Version, "%d.%d.%d", &served[0], &served[1], &served[2]); err != nil {
				t.Fatalf("%s served Version %q, which no version comparison reads; its --version ranks it %d.%d.%d",
					bin, s.Version, ranked[0], ranked[1], ranked[2])
			}
			if served != ranked {
				t.Errorf("%s serves version %v and its --version ranks it %v; the tests are chosen by one and gated by the other",
					bin, served, ranked)
			}
			concurrent := ranked[0] > 0 || ranked[1] >= 24
			if got := s.AtLeast(0, 24); got != concurrent {
				t.Errorf("AtLeast(0, 24) = %v for %v, want %v", got, served, concurrent)
			}
			var skipped bool
			t.Run("the two-replica write gate", func(t *testing.T) {
				defer func() { skipped = t.Skipped() }()
				s.RequireConcurrentTransactions(t)
			})
			if skipped == concurrent {
				t.Errorf("RequireConcurrentTransactions skipped = %v on sqld %v, want %v", skipped, served, !concurrent)
			}
		})
	}
	if started == 0 {
		t.Skip("no sqld whose version reads: set TURSORAFT_SQLD")
	}
}
