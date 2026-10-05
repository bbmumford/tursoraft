//go:build cgo

package manager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/fence"
	"github.com/bbmumford/tursoraft/mesh/meshtest"
	"github.com/bbmumford/tursoraft/sqldtest"
)

// outageWriter is a job body that writes its epoch and node through the
// fence every 50 ms and records each attempt's outcome, counting the commits
// under the epoch they committed under so that the rows the primary ends up
// holding can be compared key by key. It returns the first failed write,
// which leaves the retry to the cluster's backoff; once stop is set it writes
// nothing more and closes idle.
type outageWriter struct {
	db   *sql.DB
	node string
	stop atomic.Bool
	idle chan struct{}
	park sync.Once

	mu     sync.Mutex
	acked  map[uint64]int
	failed []error
}

func (w *outageWriter) run(ctx context.Context, f fence.Fence) error {
	for !w.stop.Load() {
		err := f.Tx(ctx, w.db, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO writes(epoch, node) VALUES (?, ?)", int64(f.Epoch), w.node)
			return err
		})
		w.mu.Lock()
		if err == nil {
			w.acked[f.Epoch]++
		} else {
			w.failed = append(w.failed, err)
		}
		w.mu.Unlock()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
	w.park.Do(func() { close(w.idle) })
	<-ctx.Done()
	return nil
}

// counts returns the writes that committed, under every epoch together, and
// the attempts that failed.
func (w *outageWriter) counts() (int, []error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.acked {
		n += c
	}
	return n, append([]error(nil), w.failed...)
}

// byEpoch returns the writes that committed under each epoch, which is what
// the rows on the primary are checked against: the cluster runs the body
// again under a new fence each time the job's assignment is written again, so
// one holder's rows carry every epoch it ran under, not only the last.
func (w *outageWriter) byEpoch() map[uint64]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[uint64]int, len(w.acked))
	for epoch, c := range w.acked {
		out[epoch] = c
	}
	return out
}

// While the primary is down a fenced job pauses where it is: its holder
// keeps the job under the same fence, every write it tries fails and none
// commits, and the cluster runs the body again after each failure; once the
// primary is back the same holder commits under the same epoch. The primary
// then holds every write the holder saw commit and none of another holder or
// epoch. Raft runs over the mesh, not the primary, so nothing in the outage
// moves the job.
func TestFencedJobPausesThroughAnOutage(t *testing.T) {
	s := sqldtest.Start(t)
	ctx := context.Background()
	spec := &RaftSpec{VoterServices: []string{"node.orbtr.ai"}, VoterCount: 3, BootstrapExpect: 3,
		Jobs: map[string][]string{"projector": {"node.orbtr.ai"}}}
	hub, key := meshtest.NewHub(), b64Key()
	type proc struct {
		id  string
		m   *Manager
		job *Job
		w   *outageWriter
	}
	var procs []*proc
	for _, id := range []string{"n1", "n2", "n3"} {
		m, n := startNode(t, hub, id, "control", "orbtr-control", s.URL(), key, spec)
		t.Cleanup(func() { m.Shutdown(); n.Close() })
		procs = append(procs, &proc{id: id, m: m})
	}
	db0, _ := procs[0].m.Database("control", "orbtr-control")
	if _, err := db0.Exec("CREATE TABLE writes(seq INTEGER PRIMARY KEY AUTOINCREMENT, epoch INTEGER NOT NULL, node TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		p.m.Sync(ctx)
		db, _ := p.m.Database("control", "orbtr-control")
		p.w = &outageWriter{db: db, node: p.id, idle: make(chan struct{}), acked: map[uint64]int{}}
		j, err := p.m.Job("control", "orbtr-control", "projector", p.w.run)
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
	wait := func(within time.Duration, what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(within); !cond(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out after %v waiting for %s", within, what)
			}
		}
	}
	wait(20*time.Second, "one holder", func() bool { return holder() != nil })
	h := holder()
	held, _ := h.job.Fence()
	acked := func() int { n, _ := h.w.counts(); return n }
	failures := func() int { _, f := h.w.counts(); return len(f) }
	wait(10*time.Second, "a committed write", func() bool { return acked() > 0 })
	s.Stop()
	wait(20*time.Second, "a write to fail", func() bool { return failures() > 0 })
	before := acked()
	outage := 30 * time.Second
	if testing.Short() {
		outage = 3 * time.Second
	}
	// The outage lasts at least until the body has run again after a
	// failure, which its backoff can put past a short outage.
	for end, limit := time.Now().Add(outage), time.Now().Add(outage+30*time.Second); time.Now().Before(end) || failures() < 2; time.Sleep(100 * time.Millisecond) {
		if holder() != h {
			t.Fatalf("with the primary down the job is no longer held by %s alone", h.id)
		}
		if f, _ := h.job.Fence(); f != held {
			t.Fatalf("with the primary down the holder's fence became %+v, was %+v", f, held)
		}
		if n := acked(); n != before {
			t.Fatalf("%d fenced writes committed with the primary down", n-before)
		}
		if time.Now().After(limit) {
			t.Fatalf("the body ran %d times with the primary down; it was not run again after a failure", failures())
		}
	}

	s.Restart()
	wait(90*time.Second, "a committed write after the primary returned", func() bool { return acked() > before })
	if f, ok := h.job.Fence(); !ok || f != held || holder() != h {
		t.Fatalf("after the outage the fence is %+v (held %v), was %+v on %s", f, ok, held, h.id)
	}
	h.w.stop.Store(true)
	select {
	case <-h.w.idle:
	case <-time.After(10 * time.Second):
		t.Fatal("the job body did not stop writing")
	}
	_, failed := h.w.counts()
	for _, err := range failed {
		if errors.Is(err, fence.ErrFenced) || errors.Is(err, fence.ErrForeignCluster) {
			t.Errorf("the fence refused the job's own holder: %v", err)
		}
	}
	if err := h.m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, _ := h.m.Database("control", "orbtr-control")
	rows, err := db.Query("SELECT epoch, node, count(*) FROM writes GROUP BY epoch, node")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	var top int64
	for rows.Next() {
		var epoch int64
		var node string
		var c int
		if err := rows.Scan(&epoch, &node, &c); err != nil {
			t.Fatal(err)
		}
		got[fmt.Sprintf("%s@%d", node, epoch)] = c
		if epoch > top {
			top = epoch
		}
	}
	want := map[string]int{}
	for epoch, c := range h.w.byEpoch() {
		want[fmt.Sprintf("%s@%d", h.id, epoch)] = c
	}
	// The write in flight when the primary stopped may have committed there
	// with its answer lost, so the primary may hold one write more than the
	// holder saw commit under the epoch it held across the outage, the one
	// the checks above hold to held; exactly as many under every other epoch
	// it ran under; and nothing under a key it did not write.
	inflight := fmt.Sprintf("%s@%d", h.id, held.Epoch)
	// The fence the job reports holding is the one its body was handed: the
	// writer stamps every row with the epoch it ran under, so the epoch read
	// back from the job has rows of its own, the writes made after the
	// primary returned.
	if _, ran := want[inflight]; !ran {
		t.Errorf("the holder reports holding %s and committed nothing under it; it committed %v", inflight, want)
	}
	// Every epoch a row carries was handed to the body as its fence, and a
	// fence carries the epoch of the assignment the body runs under, which
	// only rises, so the epoch the job reports bounds every row: a run that
	// raised the epoch itself, rather than taking the assignment's, would
	// leave a row above it. The count comparison alone cannot see that,
	// because the writer records the same epoch it stamps.
	if top > int64(held.Epoch) {
		t.Errorf("the primary holds a write under epoch %d, above the %d the holder reports: %v", top, held.Epoch, got)
	}
	agrees := len(got) == len(want)
	for key, c := range want {
		if g := got[key]; g != c && !(key == inflight && g == c+1) {
			agrees = false
		}
	}
	if !agrees {
		t.Errorf("writes on the primary by holder and epoch: %v; the holder saw %v commit, and only %s may hold one more",
			got, want, inflight)
	}
}
