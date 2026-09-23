package cluster

import (
	"testing"
	"time"
)

// Killing the leader while it holds a job moves the job to another host in
// under five seconds, with the production election timeouts.
func TestLeaderKillReassignsTheJobWithinFiveSeconds(t *testing.T) {
	timing := fastTiming()
	prod := DefaultTiming()
	timing.Heartbeat, timing.Election, timing.LeaderLease, timing.Hold = prod.Heartbeat, prod.Election, prod.LeaderLease, prod.Hold
	spec := Spec{
		VoterServices: []string{svcAI}, VoterCount: 3, BootstrapExpect: 3,
		Jobs: map[string][]string{"sweep": {svcAI}},
	}
	e := newEnv(t, spec, timing)
	for _, id := range []string{"a1", "a2", "a3"} {
		e.start(id, svcAI)
	}
	l := e.leader(20 * time.Second)
	e.register(l, "sweep")
	eventually(t, 10*time.Second, "the leader holding the job", func() bool {
		h := e.holders("sweep")
		return len(h) == 1 && h[0] == l.id
	})
	var others []*node
	for _, n := range e.live() {
		if n != l {
			others = append(others, n)
			e.register(n, "sweep")
		}
	}
	time.Sleep(500 * time.Millisecond)
	if h := e.holders("sweep"); len(h) != 1 || h[0] != l.id {
		t.Fatalf("the job moved off its live holder: %v", h)
	}
	killed := time.Now()
	e.kill(l)
	eventually(t, 10*time.Second, "the job held by a survivor", func() bool {
		return len(e.holders("sweep")) == 1
	})
	took := time.Since(killed)
	t.Logf("reassigned %v after the leader was killed", took)
	if took >= 5*time.Second {
		t.Errorf("the job took %v to move; want under 5s", took)
	}
	f, _ := others[0].c.fsm.assignment("sweep")
	if f.Node == l.id {
		t.Error("the assignment still names the killed leader")
	}
}

// A job holder cut off from the majority stops its job once it has not heard
// from a leader for the hold time; the majority gives the job to another host;
// after the partition heals exactly one member holds it, under a newer epoch.
func TestPartitionedHolderStopsAndTheMajorityReassigns(t *testing.T) {
	spec := Spec{
		VoterServices: []string{svcAI}, VoterCount: 5, BootstrapExpect: 5,
		Jobs: map[string][]string{"sweep": {svcAI}},
	}
	e := newEnv(t, spec, fastTiming())
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		e.register(e.start(id, svcAI), "sweep")
	}
	eventually(t, 20*time.Second, "one holder", func() bool { return len(e.holders("sweep")) == 1 })
	holderID := e.holders("sweep")[0]
	var holder *node
	for _, n := range e.live() {
		if n.id == holderID {
			holder = n
		}
	}
	before, _ := holder.c.fsm.assignment("sweep")
	e.hub.Isolate(holderID)
	eventually(t, 10*time.Second, "the isolated holder to stop its job", func() bool {
		holder.c.mu.Lock()
		j := holder.c.jobs["sweep"]
		holder.c.mu.Unlock()
		return !j.Held()
	})
	eventually(t, 10*time.Second, "the majority to reassign", func() bool {
		h := e.holders("sweep")
		return len(h) == 1 && h[0] != holderID
	})
	e.hub.Heal()
	time.Sleep(2 * time.Second)
	h := e.holders("sweep")
	if len(h) != 1 {
		t.Fatalf("after healing, holders %v", h)
	}
	for _, n := range e.live() {
		if n.id == h[0] {
			a, _ := n.c.fsm.assignment("sweep")
			if a.Epoch <= before.Epoch {
				t.Errorf("new epoch %d is not newer than %d", a.Epoch, before.Epoch)
			}
		}
	}
}

// A dead voter stays in the configuration while no caught-up replacement
// exists, however long it is gone; once a standby has caught up, the dead
// voter is removed and the standby promoted.
func TestDeadVoterRemovedOnlyForACaughtUpReplacement(t *testing.T) {
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 3, BootstrapExpect: 3, Jobs: map[string][]string{}}
	e := newEnv(t, spec, fastTiming())
	for _, id := range []string{"a1", "a2", "a3"} {
		e.start(id, svcAI)
	}
	eventually(t, 20*time.Second, "three voters", func() bool {
		v, _ := e.config(10 * time.Second)
		return len(v) == 3
	})
	var victim *node
	for _, n := range e.live() {
		if !n.c.IsLeader() {
			victim = n
			break
		}
	}
	e.kill(victim)
	time.Sleep(2*e.timing.VoterAbsent + time.Second)
	if v, _ := e.config(5 * time.Second); !contains(v, victim.id) {
		t.Fatalf("the dead voter was removed with no replacement: voters %v", v)
	}
	e.start("a4", svcAI)
	eventually(t, 15*time.Second, "a4 to replace the dead voter", func() bool {
		v, _ := e.config(5 * time.Second)
		return len(v) == 3 && contains(v, "a4") && !contains(v, victim.id)
	})
}

// Fewer than bootstrapExpect voter-eligible members never form a cluster.
func TestNoBootstrapBelowBootstrapExpect(t *testing.T) {
	e := newEnv(t, controlSpec(), fastTiming())
	a := e.start("a1", svcAI)
	b := e.start("b1", svcIO)
	e.start("h1", svcAPI)
	time.Sleep(3 * time.Second)
	for _, n := range []*node{a, b} {
		if n.c.raft.LastIndex() != 0 || n.c.IsLeader() {
			t.Errorf("%s formed a cluster with two of three expected voters", n.id)
		}
	}
	e.start("a2", svcAI)
	e.leader(20 * time.Second)
}
