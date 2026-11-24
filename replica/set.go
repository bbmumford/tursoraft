package replica

import (
	"context"
	"fmt"
	"sync"

	"github.com/bbmumford/tursoraft/pull"
)

// Set is a device's open replicas of its segments, keyed by the scope
// database each holds (Config.Scope.Database), and the pull-trigger port over
// them: the system part's base and worker segments, an account part's person,
// and every subscription that learns of a change call its RequestPull. Each
// replica keeps its own timed pull. The set does not open or close replicas;
// its holder adds one after Open and removes it before Close.
type Set struct {
	mu     sync.RWMutex
	byName map[string]*Replica
}

var _ pull.Trigger = (*Set)(nil)

// NewSet returns an empty set.
func NewSet() *Set { return &Set{byName: map[string]*Replica{}} }

// Add puts r in the set under its scope database's name. A whole-database
// replica records no position (ErrNoPosition), and a second replica of a
// segment the set holds is refused.
func (s *Set) Add(r *Replica) error {
	name := r.cfg.Scope.Database
	if r.readPosition == nil || name == "" {
		return ErrNoPosition
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byName[name]; ok {
		return fmt.Errorf("tursoraft/replica: the set already holds a replica of %s", name)
	}
	s.byName[name] = r
	return nil
}

// Remove takes the replica of segment out of the set and returns it, nil when
// the set holds none.
func (s *Set) Remove(segment string) *Replica {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byName[segment]
	delete(s.byName, segment)
	return r
}

// Replica returns the replica of segment.
func (s *Set) Replica(segment string) (*Replica, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byName[segment]
	return r, ok
}

// RequestPull is the segment-keyed port (pull.Trigger): it finds the replica
// of segment and returns its RequestPull, or pull.ErrUnknownSegment.
func (s *Set) RequestPull(ctx context.Context, segment string, atLeastPosition uint64) error {
	r, ok := s.Replica(segment)
	if !ok {
		return fmt.Errorf("%w: %s", pull.ErrUnknownSegment, segment)
	}
	return r.RequestPull(ctx, atLeastPosition)
}
