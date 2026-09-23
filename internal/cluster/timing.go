package cluster

import "time"

// Timing is every duration the cluster runs by. DefaultTiming gives the
// production values; tests shorten them.
type Timing struct {
	// Heartbeat, Election and LeaderLease are Raft's HeartbeatTimeout,
	// ElectionTimeout and LeaderLeaseTimeout.
	Heartbeat, Election, LeaderLease time.Duration
	// Reconcile is how often the leader reconciles membership and
	// assignments when nothing on the mesh changes.
	Reconcile time.Duration
	// HostAbsent is how long a non-voting member may be absent before the
	// leader removes it.
	HostAbsent time.Duration
	// VoterAbsent is how long a voter may be absent before the leader
	// removes it, which it then does only if a caught-up replacement exists
	// and a quorum remains.
	VoterAbsent time.Duration
	// Settle is how long every member of a proposed bootstrap set must
	// publish the same proposal before its members bootstrap.
	Settle time.Duration
	// Hold is how long a job holder keeps its job without contact with a
	// leader, and how long the leader waits on a member failing heartbeats
	// before it stops counting it as live.
	Hold time.Duration
	// Apply bounds every Raft future and RPC.
	Apply time.Duration
	// CatchUp is how many entries behind the leader a member may be and
	// still count as caught up.
	CatchUp uint64
}

// DefaultTiming is the production timing.
//
// Heartbeat and Election are 1 s because a follower that still has a leader
// refuses every vote until its own heartbeat timer fires, so a failover takes
// as long as the slowest follower's timer, which Raft draws from one to two
// heartbeat timeouts: at 1 s a killed leader's job moves in about 2 s, at 2 s
// it took up to 5.1 s on the in-memory mesh. A spurious election is cheap,
// because a new leader leaves every job with its live holder.
func DefaultTiming() Timing {
	return Timing{
		Heartbeat:   time.Second,
		Election:    time.Second,
		LeaderLease: 500 * time.Millisecond,
		Reconcile:   10 * time.Second,
		HostAbsent:  2 * time.Minute,
		VoterAbsent: 10 * time.Minute,
		Settle:      2 * time.Second,
		Hold:        2 * time.Second,
		Apply:       10 * time.Second,
		CatchUp:     1024,
	}
}

func (t Timing) withDefaults() Timing {
	d := DefaultTiming()
	for _, f := range []struct{ v, def *time.Duration }{
		{&t.Heartbeat, &d.Heartbeat}, {&t.Election, &d.Election}, {&t.LeaderLease, &d.LeaderLease},
		{&t.Reconcile, &d.Reconcile}, {&t.HostAbsent, &d.HostAbsent}, {&t.VoterAbsent, &d.VoterAbsent},
		{&t.Settle, &d.Settle}, {&t.Hold, &d.Hold}, {&t.Apply, &d.Apply},
	} {
		if *f.v <= 0 {
			*f.v = *f.def
		}
	}
	if t.CatchUp == 0 {
		t.CatchUp = d.CatchUp
	}
	return t
}
