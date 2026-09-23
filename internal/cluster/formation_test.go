package cluster

import (
	"testing"
	"time"
)

// formControl starts three node.orbtr.ai (a1..a3), three node.orbtr.io
// (b1..b3) and two api.orbtr.ai (h1, h2) members of orbtr-control and waits
// for five voters.
func formControl(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, controlSpec(), fastTiming())
	for _, id := range []string{"a1", "a2", "a3"} {
		e.start(id, svcAI)
	}
	for _, id := range []string{"b1", "b2", "b3"} {
		n := e.start(id, svcIO)
		e.register(n, "projector")
	}
	for _, id := range []string{"h1", "h2"} {
		n := e.start(id, svcAPI)
		e.register(n, "projector")
	}
	eventually(t, 20*time.Second, "five voters and three non-voters", func() bool {
		v, nv := e.config(10 * time.Second)
		return len(v) == 5 && len(nv) == 3
	})
	return e
}

// 3 + 2 voters and 2 hosts form one cluster: one cluster ID everywhere, five
// voters with at least two from each service, the sixth voter-eligible node a
// standby, the hosts non-voting, and the job on exactly one host.
func TestControlClusterForms(t *testing.T) {
	e := formControl(t)
	voters, nonvoters := e.config(5 * time.Second)
	if count(voters, "a") < 2 || count(voters, "b") < 2 {
		t.Errorf("voters %v: each service needs at least two", voters)
	}
	if count(nonvoters, "h") != 2 {
		t.Errorf("non-voters %v: both api hosts must be members that do not vote", nonvoters)
	}
	standby := 0
	for _, id := range nonvoters {
		if id[0] == 'a' || id[0] == 'b' {
			standby++
		}
	}
	if standby != 1 {
		t.Errorf("non-voters %v: want exactly one voter-eligible standby", nonvoters)
	}
	var id string
	eventually(t, 10*time.Second, "one cluster id everywhere", func() bool {
		id = ""
		for _, n := range e.live() {
			got := n.c.ClusterID()
			if got == "" || id != "" && got != id {
				return false
			}
			id = got
		}
		return true
	})
	eventually(t, 10*time.Second, "exactly one projector holder", func() bool {
		return len(e.holders("projector")) == 1
	})
	if h := e.holders("projector"); h[0] != "b1" {
		t.Errorf("projector held by %v; the lowest registered NodeID is b1", h)
	}
	e.shutdownAll()
}

// When an io voter dies, the leader keeps it until the voter-absence limit,
// then removes it for the caught-up io standby, so each service keeps two
// voters.
func TestPerServiceFloorHoldsAfterAVoterLoss(t *testing.T) {
	e := formControl(t)
	voters, _ := e.config(5 * time.Second)
	var victim *node
	for _, n := range e.live() {
		if n.id[0] == 'b' && contains(voters, n.id) && !n.c.IsLeader() {
			victim = n
			break
		}
	}
	if victim == nil {
		t.Fatalf("no io voter to kill among %v", voters)
	}
	e.kill(victim)
	time.Sleep(e.timing.VoterAbsent / 2)
	if v, _ := e.config(5 * time.Second); !contains(v, victim.id) {
		t.Fatalf("the dead voter was removed before the voter-absence limit: %v", v)
	}
	eventually(t, 15*time.Second, "the dead voter replaced by the io standby", func() bool {
		v, _ := e.config(5 * time.Second)
		return len(v) == 5 && !contains(v, victim.id) && count(v, "b") >= 2 && count(v, "a") >= 2
	})
}

// A voter-eligible member that joins a formed cluster stays a standby while
// the cluster already has its voters.
func TestASixthNodeStaysStandby(t *testing.T) {
	e := formControl(t)
	late := e.start("b4", svcIO)
	eventually(t, 10*time.Second, "b4 added", func() bool {
		_, nv := e.config(5 * time.Second)
		return contains(nv, late.id)
	})
	time.Sleep(time.Second)
	if v, _ := e.config(5 * time.Second); len(v) != 5 || contains(v, late.id) {
		t.Errorf("voters %v after a seventh voter-eligible member joined", v)
	}
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// A service that arrives after the voter seats are full gets its floor of two
// voters by a swap: its caught-up standby is promoted and a voter of the
// service above its floor is demoted.
func TestALateServiceGetsItsFloorBySwap(t *testing.T) {
	e := newEnv(t, controlSpec(), fastTiming())
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		e.start(id, svcAI)
	}
	eventually(t, 20*time.Second, "four node.orbtr.ai voters", func() bool {
		v, _ := e.config(10 * time.Second)
		return len(v) == 4
	})
	e.start("b1", svcIO)
	eventually(t, 10*time.Second, "b1 promoted into the fifth seat", func() bool {
		v, _ := e.config(5 * time.Second)
		return len(v) == 5 && contains(v, "b1")
	})
	e.start("b2", svcIO)
	eventually(t, 15*time.Second, "three node.orbtr.ai and two node.orbtr.io voters", func() bool {
		v, _ := e.config(5 * time.Second)
		return len(v) == 5 && count(v, "a") == 3 && count(v, "b") == 2
	})
}
