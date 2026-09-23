package cluster

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/mesh"
	"github.com/bbmumford/tursoraft/mesh/meshtest"
)

// scriptedView is a member's mesh with extra members whose published state
// the test writes directly. tryBootstrap decides from Members alone, so a
// scripted view pins what one voter does for a given view without a second
// process that could act first.
type scriptedView struct {
	*meshtest.Node
	mu    sync.Mutex
	extra []mesh.Member
}

func (v *scriptedView) Members(key string) []mesh.Member {
	out := v.Node.Members(key)
	v.mu.Lock()
	defer v.mu.Unlock()
	return append(out, v.extra...)
}

func (v *scriptedView) script(members ...mesh.Member) {
	v.mu.Lock()
	v.extra = members
	v.mu.Unlock()
}

// blankVoter is a voter with no Raft state publishing the given raft block
// digest, entry version and bootstrap proposal.
func blankVoter(id, spec, fsmv, proposal string) mesh.Member {
	meta := map[string]string{metaRole: "voter", metaState: "0", metaSpec: spec, metaFSMV: fsmv}
	if proposal != "" {
		meta[metaBoot] = proposal
	}
	return mesh.Member{NodeID: id, Service: svcAI, Meta: meta}
}

// startScripted starts a1 alone on a hub, seeing whatever the view scripts.
func startScripted(t *testing.T, spec Spec) (*Cluster, *scriptedView) {
	t.Helper()
	n := meshtest.NewHub().Node("a1", svcAI)
	view := &scriptedView{Node: n}
	c, err := Start(Config{
		Group: "control", DB: "orbtr-control", Spec: spec, Mesh: view, Keys: [][]byte{testKey()},
		Dir: t.TempDir(), Logger: quietLogger(t), Timing: fastTiming(),
	})
	if err != nil {
		n.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Shutdown()
		n.Close()
	})
	return c, view
}

// A voter in the proposed set does not bootstrap until every member of the
// set publishes the same proposal: until then another voter may be proposing
// a different set, and two sets bootstrapped apart share a first index and
// term. Once they all agree for the settle time, it bootstraps.
func TestBootstrapWaitsForTheProposedSetToAgree(t *testing.T) {
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 3, BootstrapExpect: 3, Jobs: map[string][]string{}}
	c, view := startScripted(t, spec)
	fsmv := strconv.Itoa(FSMVersion)
	view.script(blankVoter("a2", spec.Digest(), fsmv, ""), blankVoter("a3", spec.Digest(), fsmv, "a1,a2,a4"))
	time.Sleep(10 * fastTiming().Settle)
	if c.raft.LastIndex() != 0 {
		t.Fatal("a1 bootstrapped while a2 proposed nothing and a3 proposed another set")
	}
	c.mu.Lock()
	proposal := c.proposal
	c.mu.Unlock()
	if proposal != "a1,a2,a3" {
		t.Fatalf("a1 proposes %q, want a1,a2,a3", proposal)
	}
	view.script(blankVoter("a2", spec.Digest(), fsmv, "a1,a2,a3"), blankVoter("a3", spec.Digest(), fsmv, "a1,a2,a3"))
	eventually(t, 5*time.Second, "a1 to bootstrap once the set agrees", func() bool { return c.raft.LastIndex() > 0 })
}

// A voter publishing another raft block, or an older entry version, is not
// counted toward bootstrapExpect, even when it proposes the same set: the two
// processes would disagree on who may vote and where each job may run, or
// the older one could not apply the entries.
func TestAMemberThatDisagreesIsNotCountedForBootstrap(t *testing.T) {
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 3, BootstrapExpect: 3, Jobs: map[string][]string{}}
	other := spec
	other.VoterCount = 5
	fsmv := strconv.Itoa(FSMVersion)
	for name, peers := range map[string][]mesh.Member{
		"another raft block": {
			blankVoter("a2", other.Digest(), fsmv, "a1,a2,a3"),
			blankVoter("a3", other.Digest(), fsmv, "a1,a2,a3"),
		},
		"an older entry version": {
			blankVoter("a2", spec.Digest(), strconv.Itoa(FSMVersion-1), "a1,a2,a3"),
			blankVoter("a3", spec.Digest(), strconv.Itoa(FSMVersion-1), "a1,a2,a3"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, view := startScripted(t, spec)
			view.script(peers...)
			time.Sleep(10 * fastTiming().Settle)
			if c.raft.LastIndex() != 0 {
				t.Fatalf("a1 bootstrapped counting members with %s", name)
			}
		})
	}
}
