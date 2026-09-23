package cluster

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/hashicorp/raft"
)

// memSink is a raft.SnapshotSink over a buffer.
type memSink struct{ bytes.Buffer }

func (s *memSink) ID() string    { return "mem" }
func (s *memSink) Cancel() error { return nil }
func (s *memSink) Close() error  { return nil }

func applyEntry(t *testing.T, f *fsm, index uint64, e entry) {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if err, ok := f.Apply(&raft.Log{Index: index, Data: raw}).(error); ok {
		t.Fatalf("apply %+v: %v", e, err)
	}
}

// A member restored from a snapshot has the cluster ID and every assignment
// with its epoch, and records the ID as a voter does when it applies the ID
// from the log: a voter restarted from a snapshot must refuse a foreign
// cluster's streams before it has replayed anything.
func TestSnapshotRestoresTheClusterIDAndAssignments(t *testing.T) {
	src := newFSM(fsmHooks{})
	applyEntry(t, src, 1, entry{V: FSMVersion, Op: opCluster, ID: "c-1"})
	applyEntry(t, src, 7, entry{V: FSMVersion, Op: opAssign, Job: "sweep", Node: "a2"})
	snap, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var sink memSink
	if err := snap.Persist(&sink); err != nil {
		t.Fatal(err)
	}

	var recorded []string
	dst := newFSM(fsmHooks{clusterID: func(id string) { recorded = append(recorded, id) }})
	if err := dst.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	if got := dst.clusterID(); got != "c-1" {
		t.Errorf("restored cluster id %q, want c-1", got)
	}
	if a, ok := dst.assignment("sweep"); !ok || a != (Assignment{Node: "a2", Epoch: 7}) {
		t.Errorf("restored assignment %+v %v, want a2 at epoch 7", a, ok)
	}
	if len(recorded) != 1 || recorded[0] != "c-1" {
		t.Errorf("recorded cluster ids %v, want [c-1]", recorded)
	}

	var s snapshotV1
	if err := json.Unmarshal(sink.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	s.V = FSMVersion + 1
	raw, _ := json.Marshal(s)
	if err := newFSM(fsmHooks{}).Restore(io.NopCloser(bytes.NewReader(raw))); err == nil {
		t.Error("a snapshot of another version was restored")
	}
}
