package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Two clusters formed apart for the same database never merge: members of
// one ignore the other's members, and the member proof refuses streams
// between them.
func TestClusterIDMismatchIsRefused(t *testing.T) {
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 3, BootstrapExpect: 3, Jobs: map[string][]string{}}
	e := newEnv(t, spec, fastTiming())
	e.hub.Isolate("x1", "x2", "x3")
	for _, id := range []string{"a1", "a2", "a3", "x1", "x2", "x3"} {
		e.start(id, svcAI)
	}
	byID := map[string]*node{}
	for _, n := range e.live() {
		byID[n.id] = n
	}
	eventually(t, 20*time.Second, "two clusters with ids", func() bool {
		return byID["a1"].c.ClusterID() != "" && byID["x1"].c.ClusterID() != ""
	})
	a, x := byID["a1"].c.ClusterID(), byID["x1"].c.ClusterID()
	if a == x {
		t.Fatal("two separately formed clusters share an id")
	}
	e.hub.Heal()
	time.Sleep(2 * time.Second)
	for _, n := range e.live() {
		members, err := n.c.Members()
		if err != nil {
			continue
		}
		for _, m := range members {
			if m[0] != n.id[0] {
				t.Errorf("%s's configuration holds %s from the other cluster", n.id, m)
			}
		}
	}
	_, err := byID["a1"].c.layer.probe(context.Background(), "x1")
	if !errors.Is(err, ErrForeignCluster) {
		t.Errorf("a stream between the clusters: got %v, want ErrForeignCluster", err)
	}
}

// An entry of a version this build does not apply stops every member from
// applying and ends every job; nothing after it is applied.
func TestUnknownEntryVersionHaltsTheMember(t *testing.T) {
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 3, BootstrapExpect: 3, Jobs: map[string][]string{"sweep": {svcAI}}}
	e := newEnv(t, spec, fastTiming())
	for _, id := range []string{"a1", "a2", "a3"} {
		e.register(e.start(id, svcAI), "sweep")
	}
	eventually(t, 20*time.Second, "one holder", func() bool { return len(e.holders("sweep")) == 1 })
	l := e.leader(5 * time.Second)
	raw, _ := json.Marshal(entry{V: FSMVersion + 1, Op: opAssign, Job: "sweep", Node: "a3"})
	f := l.c.raft.Apply(raw, 5*time.Second)
	if err := f.Error(); err != nil {
		t.Fatal(err)
	}
	if err, _ := f.Response().(error); !errors.Is(err, ErrHalted) {
		t.Fatalf("the leader applied an unknown entry version: %v", f.Response())
	}
	eventually(t, 10*time.Second, "every member halted with no job held", func() bool {
		for _, n := range e.live() {
			if n.c.Halted() == "" {
				return false
			}
		}
		return len(e.holders("sweep")) == 0
	})
	raw, _ = json.Marshal(entry{V: FSMVersion, Op: opAssign, Job: "sweep", Node: "a2"})
	f = l.c.raft.Apply(raw, 5*time.Second)
	f.Error()
	if err, _ := f.Response().(error); !errors.Is(err, ErrHalted) {
		t.Errorf("a halted member applied a later entry: %v", f.Response())
	}
}

// A member holding a different group key never passes the member proof, so
// the leader never adds it; a member holding the new key and the prior one
// joins a cluster still on the prior key.
func TestMemberProofDecidesMembership(t *testing.T) {
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 5, BootstrapExpect: 3, Jobs: map[string][]string{}}
	e := newEnv(t, spec, fastTiming())
	for _, id := range []string{"a1", "a2", "a3"} {
		e.start(id, svcAI)
	}
	e.leader(20 * time.Second)
	stranger := e.start("a4", svcAI, testKey())
	rotated := e.start("a5", svcAI, testKey(), e.keys[0])
	eventually(t, 15*time.Second, "the rotated member added", func() bool {
		m, err := e.leader(5 * time.Second).c.Members()
		return err == nil && contains(m, rotated.id)
	})
	time.Sleep(time.Second)
	m, _ := e.leader(5 * time.Second).c.Members()
	if contains(m, stranger.id) {
		t.Errorf("a member without the group key joined: %v", m)
	}
	if _, err := stranger.c.layer.probe(context.Background(), "a1"); !errors.Is(err, ErrProofFailed) {
		t.Errorf("a stream from a member without the key: got %v, want ErrProofFailed", err)
	}
}

// A leader never adds a member that holds Raft state but has not applied this
// cluster's ID, whether it learns that from the member's published state or
// only from the probe: such a member may be a voter of a cluster formed apart
// from this one, and adding it would have two leaders write one log.
func TestAMemberWithStateButNoClusterIDIsNotAdded(t *testing.T) {
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 5, BootstrapExpect: 1, Jobs: map[string][]string{}}
	e := newEnv(t, spec, fastTiming())
	e.hub.Isolate("z9")
	z := e.start("z9", svcAI)
	for _, id := range []string{"a1", "a2"} {
		e.start(id, svcAI)
	}
	eventually(t, 20*time.Second, "both clusters formed", func() bool {
		return z.c.fsm.clusterID() != "" && len(e.live()) == 3 && e.live()[0].c.fsm.clusterID() != ""
	})
	// z9 now holds state for its own cluster; make it one whose ID is not
	// applied: halted before the ID, so its leader cannot apply one again.
	z.c.fsm.mu.Lock()
	z.c.fsm.id, z.c.fsm.halted = "", "test: halted before its cluster id"
	z.c.fsm.mu.Unlock()
	z.c.mu.Lock()
	z.c.persisted = ""
	z.c.mu.Unlock()
	e.hub.Heal()

	var l *node
	eventually(t, 10*time.Second, "the leader of a1 and a2", func() bool {
		for _, n := range e.live() {
			if n.id != "z9" && n.c.IsLeader() {
				l = n
			}
		}
		return l != nil
	})
	time.Sleep(2 * time.Second)
	if m, _ := l.c.Members(); contains(m, "z9") {
		t.Fatalf("the leader added a member with state and no cluster id: %v", m)
	}
	// The same member presented as if it had published no state: the probe
	// reports its state and the leader still refuses it.
	f := l.c.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		t.Fatal(err)
	}
	live := map[string]member{"z9": {id: "z9", service: svcAI, jobs: map[string]bool{}}}
	if l.c.addMembers(f.Configuration(), live) {
		t.Fatal("the leader added a member whose probe reported state and no cluster id")
	}
}
