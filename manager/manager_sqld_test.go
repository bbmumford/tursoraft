//go:build cgo

package manager

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/bbmumford/tursoraft/fence"
	"github.com/bbmumford/tursoraft/internal/cluster"
	"github.com/bbmumford/tursoraft/mesh/meshtest"
	"github.com/bbmumford/tursoraft/sqldtest"
)

func testTiming() cluster.Timing {
	return cluster.Timing{
		Heartbeat: 300 * time.Millisecond, Election: 300 * time.Millisecond, LeaderLease: 150 * time.Millisecond,
		Reconcile: 200 * time.Millisecond, HostAbsent: 2 * time.Second, VoterAbsent: 3 * time.Second,
		Settle: 200 * time.Millisecond, Hold: 900 * time.Millisecond, Apply: 5 * time.Second,
	}
}

// One shared group through the manager: the replica reads and writes, its
// stats have the replica's shape, Sync and SyncOnMiss pull, and Shutdown
// leaves no goroutine behind.
func TestSharedGroupThroughTheManager(t *testing.T) {
	s := sqldtest.Start(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	ctx := context.Background()
	m, err := NewManager(ctx, ManagerConfig{
		Groups: []GroupConfig{{
			GroupName: "sessions", DBNames: []string{"orbtr-sessions"}, EncryptionKey: b64Key(),
			Databases: []DatabaseConfig{{Name: "orbtr-sessions", URL: s.URL(), AuthToken: "db-token"}},
		}},
		RaftConfig:   RaftConfig{LocalDir: t.TempDir()},
		SyncInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := m.Database("sessions", "orbtr-sessions")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE s(id TEXT PRIMARY KEY); INSERT INTO s VALUES ('x')"); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncOnMiss(ctx, "sessions", "orbtr-sessions"); err != nil {
		t.Fatal(err)
	}
	st, err := m.SyncStats(ctx, "sessions", "orbtr-sessions")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingLocalWrites != 0 || st.LastPushUnixTime != 0 || st.LastPullUnixTime == 0 || st.Revision == "" || st.Revision == "0" {
		t.Errorf("sync stats %+v", st)
	}
	h, err := m.Health("sessions", "orbtr-sessions")
	if err != nil {
		t.Fatal(err)
	}
	if h.Frame == 0 || h.Stale || h.SyncErr != nil || h.Leader != "" {
		t.Errorf("health %+v", h)
	}
	if err := m.Shutdown(); err != nil {
		t.Fatal(err)
	}
}

// startNode starts a manager holding the control database with a seat in
// its cluster, as one node.orbtr.ai instance would.
func startNode(t *testing.T, hub *meshtest.Hub, id, group, db, url, key string, spec *RaftSpec) (*Manager, *meshtest.Node) {
	t.Helper()
	vol := t.TempDir()
	n := hub.Node(id, "node.orbtr.ai")
	m, err := NewManager(context.Background(), ManagerConfig{
		Groups: []GroupConfig{{
			GroupName: group, DBNames: []string{db}, EncryptionKey: key,
			Databases: []DatabaseConfig{{Name: db, URL: url, AuthToken: "db-token"}},
			Raft:      spec,
		}},
		RaftConfig:   RaftConfig{LocalDir: filepath.Join(vol, "tursoraft")},
		VolumeDir:    vol,
		Mesh:         n,
		SyncInterval: 200 * time.Millisecond,
		timing:       testTiming(),
	})
	if err != nil {
		n.Close()
		t.Fatalf("start %s: %v", id, err)
	}
	return m, n
}

// fencedWriter is a job body that writes its epoch and node through the
// fence every 50 ms until its context ends.
func fencedWriter(db *sql.DB, node string) JobFunc {
	return func(ctx context.Context, f fence.Fence) error {
		for {
			err := f.Tx(ctx, db, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "INSERT INTO writes(epoch, node) VALUES (?, ?)", int64(f.Epoch), node)
				return err
			})
			if err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// Three node managers share one control database and its cluster: exactly
// one runs the projector, IsLeader is true on that one only, and when its
// process dies another takes the job over under a newer epoch with no write
// of the old epoch after the new one's first.
func TestJobsAcrossManagers(t *testing.T) {
	s := sqldtest.Start(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	s.RequireConcurrentTransactions(t)
	key := b64Key()
	spec := &RaftSpec{VoterServices: []string{"node.orbtr.ai"}, VoterCount: 3, BootstrapExpect: 3,
		Jobs: map[string][]string{"projector": {"node.orbtr.ai"}}}
	hub := meshtest.NewHub()
	type proc struct {
		id   string
		m    *Manager
		mesh *meshtest.Node
		job  *Job
	}
	var procs []*proc
	for _, id := range []string{"n1", "n2", "n3"} {
		m, n := startNode(t, hub, id, "control", "orbtr-control", s.URL(), key, spec)
		procs = append(procs, &proc{id: id, m: m, mesh: n})
	}
	var once sync.Once
	defer once.Do(func() {
		for _, p := range procs {
			p.m.Shutdown()
			p.mesh.Close()
		}
	})
	db0, _ := procs[0].m.Database("control", "orbtr-control")
	if _, err := db0.Exec("CREATE TABLE writes(seq INTEGER PRIMARY KEY AUTOINCREMENT, epoch INTEGER NOT NULL, node TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		p.m.Sync(context.Background())
		db, _ := p.m.Database("control", "orbtr-control")
		j, err := p.m.Job("control", "orbtr-control", "projector", fencedWriter(db, p.id))
		if err != nil {
			t.Fatal(err)
		}
		p.job = j
	}
	holder := func() *proc {
		var h *proc
		for _, p := range procs {
			if p.job.Held() {
				if h != nil {
					return nil
				}
				h = p
			}
		}
		return h
	}
	wait := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wait("one holder", func() bool { return holder() != nil })
	first := holder()
	for _, p := range procs {
		if p.m.IsLeader() != (p == first) {
			t.Errorf("%s: IsLeader %v, holds the job %v", p.id, p.m.IsLeader(), p == first)
		}
		if l, err := p.m.Leader("control", "orbtr-control"); err != nil || l == "" {
			t.Errorf("%s: Leader %q %v", p.id, l, err)
		}
	}
	oldFence, _ := first.job.Fence()
	time.Sleep(500 * time.Millisecond)

	first.mesh.Close()
	first.m.Shutdown()
	var rest []*proc
	for _, p := range procs {
		if p != first {
			rest = append(rest, p)
		}
	}
	procs = rest
	wait("a new holder", func() bool { return holder() != nil })
	next := holder()
	newFence, _ := next.job.Fence()
	if newFence.Epoch <= oldFence.Epoch {
		t.Errorf("new epoch %d is not newer than %d", newFence.Epoch, oldFence.Epoch)
	}
	time.Sleep(500 * time.Millisecond)
	db, _ := next.m.Database("control", "orbtr-control")
	next.m.Sync(context.Background())
	rows, err := db.Query("SELECT epoch, node FROM writes ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var last int64
	seen := map[string]int{}
	for rows.Next() {
		var e int64
		var n string
		rows.Scan(&e, &n)
		if e < last {
			t.Fatalf("epoch %d written after epoch %d", e, last)
		}
		last = e
		seen[fmt.Sprintf("%s@%d", n, e)]++
	}
	if seen[fmt.Sprintf("%s@%d", first.id, oldFence.Epoch)] == 0 || seen[fmt.Sprintf("%s@%d", next.id, newFence.Epoch)] == 0 {
		t.Errorf("writes by holder and epoch: %v", seen)
	}
}

// A group named raft holding a database named logs forms its cluster and
// runs its job: its files and Raft's own logs.db never share a path.
func TestGroupRaftWithDatabaseLogs(t *testing.T) {
	s := sqldtest.Start(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	hub := meshtest.NewHub()
	spec := &RaftSpec{VoterServices: []string{"node.orbtr.ai"}, VoterCount: 1, BootstrapExpect: 1,
		Jobs: map[string][]string{"sweep": {"node.orbtr.ai"}}}
	m, n := startNode(t, hub, "n1", "raft", "logs", s.URL(), b64Key(), spec)
	defer n.Close()
	defer m.Shutdown()
	ran := make(chan struct{}, 1)
	if _, err := m.Job("raft", "logs", "sweep", func(ctx context.Context, f fence.Fence) error {
		select {
		case ran <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
	case <-time.After(20 * time.Second):
		t.Fatal("the job never ran")
	}
	h, err := m.Health("raft", "logs")
	if err != nil || h.ClusterID == "" || len(h.HeldJobs) != 1 {
		t.Errorf("health %+v, %v", h, err)
	}
}

// MeshKey is the key the cluster runs under: once the manager is up, its
// member is listed under MeshKey of its database and the cluster holds that
// key's listener, while the key of another database lists nobody and is free
// to listen on.
func TestMeshKeyIsTheKeyTheClusterRunsUnder(t *testing.T) {
	s := sqldtest.Start(t)
	hub := meshtest.NewHub()
	spec := &RaftSpec{VoterServices: []string{"node.orbtr.ai"}, VoterCount: 1, BootstrapExpect: 1,
		Jobs: map[string][]string{"sweep": {"node.orbtr.ai"}}}
	m, n := startNode(t, hub, "n1", "control", "orbtr-control", s.URL(), b64Key(), spec)
	defer n.Close()
	defer m.Shutdown()

	key := MeshKey("control", "orbtr-control")
	for deadline := time.Now().Add(10 * time.Second); len(n.Members(key)) == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("no member is published under MeshKey %q", key)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := n.Members(key); len(got) != 1 || got[0].NodeID != "n1" {
		t.Errorf("members under %q: %+v, want n1 alone", key, got)
	}
	if ln, err := n.Listen(key); err == nil {
		ln.Close()
		t.Errorf("MeshKey %q was free to listen on: the cluster does not listen under it", key)
	}

	other := MeshKey("control", "other")
	if got := n.Members(other); len(got) != 0 {
		t.Errorf("members under another database's key %q: %+v", other, got)
	}
	ln, err := n.Listen(other)
	if err != nil {
		t.Fatalf("another database's key %q is already listened on: %v", other, err)
	}
	ln.Close()
}

// A process whose service neither votes nor hosts a job holds the database
// but has no seat: it can register no job, and IsLeader, which reports held
// jobs of the first declared cluster, is false.
func TestIsLeaderWithoutASeat(t *testing.T) {
	s := sqldtest.Start(t)
	hub := meshtest.NewHub()
	n := hub.Node("app1", "app.orbtr.ai")
	defer n.Close()
	m, err := NewManager(context.Background(), ManagerConfig{
		Groups: []GroupConfig{{
			GroupName: "control", DBNames: []string{"orbtr-control"}, EncryptionKey: b64Key(),
			Databases: []DatabaseConfig{{Name: "orbtr-control", URL: s.URL(), AuthToken: "db-token"}},
			Raft: &RaftSpec{VoterServices: []string{"node.orbtr.ai"}, VoterCount: 3, BootstrapExpect: 3,
				Jobs: map[string][]string{"projector": {"api.orbtr.ai"}}},
		}},
		RaftConfig: RaftConfig{LocalDir: t.TempDir()},
		Mesh:       n,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	if m.IsLeader() {
		t.Error("IsLeader is true on a process with no seat in the declared cluster")
	}
	if _, err := m.Job("control", "orbtr-control", "projector", func(context.Context, fence.Fence) error { return nil }); err == nil {
		t.Error("a process with no seat registered a job")
	}
}
