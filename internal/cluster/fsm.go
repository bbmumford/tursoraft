package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"

	"github.com/hashicorp/raft"
)

// FSMVersion is the entry version this build proposes and applies. Members
// publish it as "fsmv"; an entry of any other version stops the member.
const FSMVersion = 1

// Entry ops.
const (
	opAssign  = "assign"
	opCluster = "cluster"
)

// ErrHalted is what Apply returns once the FSM has met an entry it cannot
// apply. Every later entry is refused too: applying past an entry this build
// does not understand would leave this member's state different from the
// state of members that did apply it.
var ErrHalted = errors.New("tursoraft/cluster: FSM halted on an entry it cannot apply")

// entry is one Raft log command. It carries assignments and the cluster ID
// only.
type entry struct {
	V    int    `json:"v"`
	Op   string `json:"op"`
	Job  string `json:"job,omitempty"`
	Node string `json:"node,omitempty"`
	ID   string `json:"id,omitempty"`
}

// Assignment is who runs a job and under which epoch. The epoch is the log
// index of the entry that made the assignment, so a later assignment always
// has a larger epoch.
type Assignment struct {
	Node  string `json:"node"`
	Epoch uint64 `json:"epoch"`
}

// snapshotV1 is the snapshot encoding.
type snapshotV1 struct {
	V             int                   `json:"v"`
	ClusterID     string                `json:"cluster_id"`
	Assignments   map[string]Assignment `json:"assignments"`
	EntryVersions map[string]int        `json:"entry_versions"`
}

// fsm applies entries in memory. It takes no lock but its own and calls out
// only through hooks that must not block: the job runner and the control loop
// learn of a change through notify, which only signals a channel, so a slow
// runner cannot stall Raft's apply loop and Raft's apply loop cannot wait on a
// caller's lock.
type fsm struct {
	mu     sync.Mutex
	id     string
	assign map[string]Assignment
	halted string
	hooks  fsmHooks
}

// fsmHooks are the FSM's calls out. Each runs under the FSM lock.
type fsmHooks struct {
	// notify wakes whoever acts on the FSM's state; it must not block.
	notify func()
	// halted raises the alert when an entry cannot be applied; it must only
	// log.
	halted func(reason string)
	// clusterID records the cluster ID the first time it is applied, so a
	// restarted voter can refuse a foreign cluster's streams before its log
	// is replayed.
	clusterID func(id string)
}

func newFSM(h fsmHooks) *fsm {
	return &fsm{assign: map[string]Assignment{}, hooks: h}
}

func (f *fsm) signal() {
	if f.hooks.notify != nil {
		f.hooks.notify()
	}
}

func (f *fsm) haltLocked(reason string) error {
	if f.halted == "" {
		f.halted = reason
		if f.hooks.halted != nil {
			f.hooks.halted(reason)
		}
	}
	f.signal()
	return fmt.Errorf("%w: %s", ErrHalted, reason)
}

// Apply implements raft.FSM.
func (f *fsm) Apply(l *raft.Log) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.halted != "" {
		return fmt.Errorf("%w: %s", ErrHalted, f.halted)
	}
	var e entry
	if err := json.Unmarshal(l.Data, &e); err != nil {
		return f.haltLocked(fmt.Sprintf("entry %d does not decode: %v", l.Index, err))
	}
	if e.V != FSMVersion {
		return f.haltLocked(fmt.Sprintf("entry %d has version %d, this build applies %d", l.Index, e.V, FSMVersion))
	}
	switch e.Op {
	case opCluster:
		if e.ID == "" {
			return f.haltLocked(fmt.Sprintf("entry %d sets an empty cluster id", l.Index))
		}
		if f.id == "" {
			f.id = e.ID
			if f.hooks.clusterID != nil {
				f.hooks.clusterID(e.ID)
			}
		}
	case opAssign:
		if e.Job == "" || e.Node == "" {
			return f.haltLocked(fmt.Sprintf("entry %d assigns without a job or node", l.Index))
		}
		f.assign[e.Job] = Assignment{Node: e.Node, Epoch: l.Index}
	default:
		return f.haltLocked(fmt.Sprintf("entry %d has unknown op %q", l.Index, e.Op))
	}
	f.signal()
	return nil
}

// Snapshot implements raft.FSM.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := json.Marshal(snapshotV1{
		V:             FSMVersion,
		ClusterID:     f.id,
		Assignments:   maps.Clone(f.assign),
		EntryVersions: map[string]int{opAssign: FSMVersion, opCluster: FSMVersion},
	})
	if err != nil {
		return nil, err
	}
	return fsmSnapshot(raw), nil
}

// Restore implements raft.FSM.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var s snapshotV1
	if err := json.NewDecoder(rc).Decode(&s); err != nil {
		return err
	}
	if s.V != FSMVersion {
		return fmt.Errorf("tursoraft/cluster: snapshot version %d, this build reads %d", s.V, FSMVersion)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.id == "" && s.ClusterID != "" && f.hooks.clusterID != nil {
		f.hooks.clusterID(s.ClusterID)
	}
	f.id = s.ClusterID
	f.assign = map[string]Assignment{}
	for job, a := range s.Assignments {
		f.assign[job] = a
	}
	f.signal()
	return nil
}

func (f *fsm) clusterID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.id
}

func (f *fsm) assignment(job string) (Assignment, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.assign[job]
	return a, ok
}

func (f *fsm) haltReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.halted
}

type fsmSnapshot []byte

func (s fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s fsmSnapshot) Release() {}
