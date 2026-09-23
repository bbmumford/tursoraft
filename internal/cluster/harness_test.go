package cluster

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/bbmumford/tursoraft/fence"
	"github.com/bbmumford/tursoraft/mesh/meshtest"
)

// Services the tests use, named as the production raft block names them.
const (
	svcAI  = "node.orbtr.ai"
	svcIO  = "node.orbtr.io"
	svcAPI = "api.orbtr.ai"
)

// fastTiming shortens every period so a test forms, fails over and removes
// members in seconds.
func fastTiming() Timing {
	return Timing{
		Heartbeat:   300 * time.Millisecond,
		Election:    300 * time.Millisecond,
		LeaderLease: 150 * time.Millisecond,
		Reconcile:   200 * time.Millisecond,
		HostAbsent:  1500 * time.Millisecond,
		VoterAbsent: 2 * time.Second,
		Settle:      200 * time.Millisecond,
		Hold:        900 * time.Millisecond,
		Apply:       5 * time.Second,
		CatchUp:     1024,
	}
}

// controlSpec is the orbtr-control raft block: node.orbtr.ai and
// node.orbtr.io vote, five voters, three to bootstrap, and the projector job
// runs on api.orbtr.ai or node.orbtr.io.
func controlSpec() Spec {
	return Spec{
		VoterServices:   []string{svcAI, svcIO},
		VoterCount:      5,
		BootstrapExpect: 3,
		Jobs:            map[string][]string{"projector": {svcAPI, svcIO}},
	}
}

func testKey() []byte {
	k := make([]byte, 32)
	rand.Read(k)
	return k
}

func quietLogger(t testing.TB) *slog.Logger {
	if testing.Verbose() && os.Getenv("TURSORAFT_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).With("test", t.Name())
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// env is a set of cluster members on one in-memory hub.
type env struct {
	t      *testing.T
	hub    *meshtest.Hub
	spec   Spec
	keys   [][]byte
	timing Timing
	dir    string

	mu      sync.Mutex
	members map[string]*node
}

// node is one member process.
type node struct {
	id, service string
	mesh        *meshtest.Node
	c           *Cluster
	runs        atomic.Int64 // job bodies started
	dir         string
}

func newEnv(t *testing.T, spec Spec, timing Timing) *env {
	t.Helper()
	e := &env{t: t, hub: meshtest.NewHub(), spec: spec, keys: [][]byte{testKey()}, timing: timing, dir: t.TempDir(), members: map[string]*node{}}
	t.Cleanup(e.shutdownAll)
	return e
}

// start starts a member; keys overrides the environment's group keys.
func (e *env) start(id, service string, keys ...[]byte) *node {
	e.t.Helper()
	if keys == nil {
		keys = e.keys
	}
	m := e.hub.Node(id, service)
	n := &node{id: id, service: service, mesh: m, dir: filepath.Join(e.dir, id)}
	c, err := Start(Config{
		Group: "control", DB: "orbtr-control", Spec: e.spec, Mesh: m, Keys: keys,
		Dir: n.dir, Logger: quietLogger(e.t).With("node", id), Timing: e.timing,
	})
	if err != nil {
		e.t.Fatalf("start %s: %v", id, err)
	}
	n.c = c
	e.mu.Lock()
	e.members[id] = n
	e.mu.Unlock()
	return n
}

// register registers the projector job on n with a body that counts its runs
// and holds until its context ends.
func (e *env) register(n *node, job string) *Job {
	e.t.Helper()
	j, err := n.c.Register(job, func(ctx context.Context, f fence.Fence) error {
		n.runs.Add(1)
		<-ctx.Done()
		return nil
	})
	if err != nil {
		e.t.Fatalf("register %s on %s: %v", job, n.id, err)
	}
	return j
}

// kill stops n as a crash would: its mesh presence and streams vanish first,
// then its Raft node is torn down.
func (e *env) kill(n *node) {
	e.t.Helper()
	n.mesh.Close()
	n.c.Shutdown()
	e.mu.Lock()
	delete(e.members, n.id)
	e.mu.Unlock()
}

// stop shuts n down gracefully and removes it from the hub.
func (e *env) stop(n *node) {
	e.t.Helper()
	if err := n.c.Shutdown(); err != nil {
		e.t.Errorf("shutdown %s: %v", n.id, err)
	}
	n.mesh.Close()
	e.mu.Lock()
	delete(e.members, n.id)
	e.mu.Unlock()
}

func (e *env) shutdownAll() {
	e.mu.Lock()
	var ns []*node
	for _, n := range e.members {
		ns = append(ns, n)
	}
	e.members = map[string]*node{}
	e.mu.Unlock()
	var wg sync.WaitGroup
	for _, n := range ns {
		wg.Add(1)
		go func(n *node) {
			defer wg.Done()
			n.c.Shutdown()
			n.mesh.Close()
		}(n)
	}
	wg.Wait()
}

func (e *env) live() []*node {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ns []*node
	for _, n := range e.members {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i].id < ns[j].id })
	return ns
}

// leader waits for a leader among the live members and returns it.
func (e *env) leader(within time.Duration) *node {
	e.t.Helper()
	var found *node
	eventually(e.t, within, "a leader", func() bool {
		found = nil
		for _, n := range e.live() {
			if n.c.IsLeader() {
				found = n
			}
		}
		return found != nil
	})
	return found
}

// config reads the configuration as the leader holds it.
func (e *env) config(within time.Duration) (voters, nonvoters []string) {
	e.t.Helper()
	l := e.leader(within)
	f := l.c.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		e.t.Fatal(err)
	}
	for _, s := range f.Configuration().Servers {
		if s.Suffrage == raft.Voter {
			voters = append(voters, string(s.ID))
		} else {
			nonvoters = append(nonvoters, string(s.ID))
		}
	}
	sort.Strings(voters)
	sort.Strings(nonvoters)
	return voters, nonvoters
}

// holders lists the live members holding job.
func (e *env) holders(job string) []string {
	var out []string
	for _, n := range e.live() {
		n.c.mu.Lock()
		j := n.c.jobs[job]
		n.c.mu.Unlock()
		if j != nil && j.Held() {
			out = append(out, n.id)
		}
	}
	return out
}

func eventually(t testing.TB, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serviceOf maps the ids the tests use to their service.
func count(ids []string, prefix string) int {
	n := 0
	for _, id := range ids {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func sameStrings(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	sort.Strings(a)
	sort.Strings(b)
	return slices.Equal(a, b)
}

func describe(e *env) string {
	s := ""
	for _, n := range e.live() {
		s += fmt.Sprintf("%s[%s id=%s idx=%d] ", n.id, n.c.raft.State(), n.c.ClusterID(), n.c.raft.AppliedIndex())
	}
	return s
}
