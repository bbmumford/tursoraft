package manager

import (
	"sort"
	"time"
)

// Health is one database's state as this process sees it.
type Health struct {
	// Leader is the NodeID of the Raft leader of the database's cluster, ""
	// with no cluster or no leader.
	Leader string
	// ClusterID is the cluster's ID, "" before one exists.
	ClusterID string
	// HeldJobs are the jobs of the database this process holds now.
	HeldJobs []string
	// Frame is the replica's frame number after its last pull.
	Frame uint64
	// LastPull is the time of the replica's last successful pull.
	LastPull time.Time
	// Stale means the replica has not pulled for three sync intervals.
	Stale bool
	// SyncErr is the last pull's error.
	SyncErr error
}

// Health reports one database's state. A local file reports no replica
// fields.
func (m *Manager) Health(group, db string) (Health, error) {
	h, err := m.holding(group, db)
	if err != nil {
		return Health{}, err
	}
	var out Health
	if h.replica != nil {
		s := h.replica.Stats()
		out.Frame, out.LastPull, out.Stale, out.SyncErr = s.Frame, s.LastPull, s.Stale, s.Err
	}
	if c, err := m.cluster(group, db); err == nil {
		out.Leader = c.Leader()
		out.ClusterID = c.ClusterID()
		out.HeldJobs = c.HeldJobs()
		sort.Strings(out.HeldJobs)
	}
	return out, nil
}
