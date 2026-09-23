package manager

// IsLeader reports whether this process holds any job of the first declared
// cluster, and true when no group declares a raft block. A process with no
// seat in the first declared cluster holds none of its jobs and reports
// false.
//
// Deprecated: register the work as a Job and gate it on Job.Held, whose
// writes are fenced. IsLeader exists because fleet.orbtr.ai gates background
// writers on it and passes it as a func() bool value.
func (m *Manager) IsLeader() bool {
	m.mu.Lock()
	declared, first := m.declared, m.first
	m.mu.Unlock()
	if !declared {
		return true
	}
	return first != nil && len(first.HeldJobs()) > 0
}

// Callers pass IsLeader as a func() bool value (fleet.orbtr.ai's scheduler
// takes it as its leadership check); this keeps the signature from drifting.
var _ func() bool = (*Manager)(nil).IsLeader
