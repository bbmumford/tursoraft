package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/raft"

	"github.com/bbmumford/tursoraft/mesh"
)

// Published metadata keys. Every member publishes them under the cluster's
// mesh key.
const (
	metaRole  = "role"    // "voter", "host" or "voter,host": what the service may do
	metaJobs  = "jobs"    // comma-joined jobs this process registered
	metaState = "state"   // "1" once this member holds any Raft state
	metaID    = "cluster" // the cluster ID it has applied
	metaSpec  = "spec"    // Spec.Digest of its raft block
	metaFSMV  = "fsmv"    // the highest entry version it applies
	metaIdx   = "idx"     // its last applied log index
	metaBoot  = "boot"    // its bootstrap proposal while it holds no state
)

// pollInterval is how often the control loop wakes with nothing to wake it,
// which paces bootstrap agreement and a leader's reconcile clock.
const pollInterval = 100 * time.Millisecond

// probeRetry is how long the leader waits before probing again a member whose
// member proof failed.
const probeRetry = 30 * time.Second

// control publishes this member's state, forms the cluster when the bootstrap
// rule allows, and, on the leader, reconciles membership and assignments.
func (c *Cluster) control() {
	defer c.wg.Done()
	changes, unsubscribe := c.cfg.Mesh.Changes()
	defer unsubscribe()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		changed := false
		select {
		case <-c.stop:
			return
		case <-changes:
			changed = true
		case <-c.kickControl:
			changed = true
		case <-tick.C:
		}
		c.publish()
		if c.voter && c.raft.LastIndex() == 0 {
			c.tryBootstrap()
		}
		if c.raft.State() == raft.Leader {
			c.mu.Lock()
			due := time.Since(c.lastRecon) >= c.t.Reconcile || (changed && time.Since(c.lastRecon) >= pollInterval)
			c.mu.Unlock()
			if due {
				c.reconcile()
			}
		}
	}
}

// meta is what this member publishes.
func (c *Cluster) meta() map[string]string {
	role := []string{}
	if c.voter {
		role = append(role, "voter")
	}
	if c.cfg.Spec.IsHostService(c.service) {
		role = append(role, "host")
	}
	state := "0"
	if c.raft.LastIndex() > 0 {
		state = "1"
	}
	c.mu.Lock()
	jobs := make([]string, 0, len(c.jobs))
	for name := range c.jobs {
		jobs = append(jobs, name)
	}
	proposal := c.proposal
	c.mu.Unlock()
	sort.Strings(jobs)
	m := map[string]string{
		metaRole:  strings.Join(role, ","),
		metaJobs:  strings.Join(jobs, ","),
		metaState: state,
		metaID:    c.clusterID(),
		metaSpec:  c.spec,
		metaFSMV:  strconv.Itoa(FSMVersion),
		metaIdx:   strconv.FormatUint(c.raft.AppliedIndex(), 10),
	}
	if state == "0" && proposal != "" {
		m[metaBoot] = proposal
	}
	return m
}

// publish sends this member's metadata when it differs from what was last
// sent.
func (c *Cluster) publish() {
	m := c.meta()
	c.mu.Lock()
	same := mapsEqual(m, c.published)
	c.mu.Unlock()
	if same {
		return
	}
	if err := c.cfg.Mesh.Publish(c.key, m); err != nil {
		c.log.Warn("tursoraft: publish cluster state", "err", err)
		return
	}
	c.mu.Lock()
	c.published = m
	c.mu.Unlock()
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// sameCluster reports whether a published member reads the same raft block
// and applies this build's entries, alerting once for a member that does
// not. Such a member is never counted for bootstrap, never added and never
// assigned: the two processes would disagree on who may vote and where each
// job may run.
func (c *Cluster) sameCluster(m mesh.Member) bool {
	reason := ""
	switch v, _ := strconv.Atoi(m.Meta[metaFSMV]); {
	case m.Meta[metaSpec] != c.spec:
		reason = "its raft block differs from this member's"
	case v < FSMVersion:
		reason = "it applies entry version " + m.Meta[metaFSMV] + ", this build proposes " + strconv.Itoa(FSMVersion)
	}
	if id, mine := m.Meta[metaID], c.clusterID(); reason == "" && id != "" && mine != "" && id != mine {
		reason = "it belongs to cluster " + id + ", this member to " + mine
	}
	if reason == "" {
		return true
	}
	c.alert(m.NodeID, reason)
	return false
}

// alert logs, once per member and reason, why a member is ignored.
func (c *Cluster) alert(id, reason string) {
	c.mu.Lock()
	first := c.alerted[id] != reason
	c.alerted[id] = reason
	c.mu.Unlock()
	if first {
		c.log.Error("tursoraft: ignoring a cluster member", "peer", id, "reason", reason)
	}
}

// tryBootstrap forms the cluster when this voter holds no Raft state and the
// bootstrap rule holds:
//   - no voter on the mesh advertises state, so no cluster exists to join;
//   - at least BootstrapExpect voters with no state and the same raft block
//     are visible, and this member is among the BootstrapExpect lowest
//     NodeIDs, which is the proposed set;
//   - every member of the proposed set has published the same proposal, and
//     has for the settle time.
//
// The published agreement is what keeps two members that see different parts
// of the mesh from bootstrapping two different configurations with the same
// first log index and term, which Raft would then treat as one log.
func (c *Cluster) tryBootstrap() {
	var blanks []string
	proposals := map[string]string{}
	for _, m := range c.cfg.Mesh.Members(c.key) {
		if !c.cfg.Spec.IsVoterService(m.Service) || !c.sameCluster(m) {
			continue
		}
		if m.Meta[metaState] == "1" {
			c.setProposal("")
			return
		}
		blanks = append(blanks, m.NodeID)
		proposals[m.NodeID] = m.Meta[metaBoot]
	}
	sort.Strings(blanks)
	if !slices.Contains(blanks, c.local) || len(blanks) < c.cfg.Spec.BootstrapExpect {
		c.setProposal("")
		return
	}
	set := blanks[:c.cfg.Spec.BootstrapExpect]
	proposal := strings.Join(set, ",")
	if c.setProposal(proposal) {
		return
	}
	if !slices.Contains(set, c.local) {
		return
	}
	for _, id := range set {
		if proposals[id] != proposal {
			c.mu.Lock()
			c.agreedOn = ""
			c.mu.Unlock()
			return
		}
	}
	c.mu.Lock()
	if c.agreedOn != proposal {
		c.agreedOn, c.agreedAt = proposal, time.Now()
	}
	settled := time.Since(c.agreedAt) >= c.t.Settle
	c.mu.Unlock()
	if !settled {
		return
	}
	servers := make([]raft.Server, 0, len(set))
	for _, id := range set {
		servers = append(servers, raft.Server{Suffrage: raft.Voter, ID: raft.ServerID(id), Address: raft.ServerAddress(id)})
	}
	err := c.raft.BootstrapCluster(raft.Configuration{Servers: servers}).Error()
	if err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
		c.log.Error("tursoraft: bootstrap", "err", err)
		return
	}
	c.log.Info("tursoraft: bootstrapped the cluster", "voters", proposal)
	c.publish()
}

// setProposal records this member's bootstrap proposal and reports whether it
// changed, in which case it is published before anyone may act on it.
func (c *Cluster) setProposal(p string) bool {
	c.mu.Lock()
	changed := c.proposal != p
	c.proposal = p
	if changed {
		c.agreedOn = ""
	}
	c.mu.Unlock()
	if changed {
		c.publish()
	}
	return changed
}

// newClusterID is the ID the first leader of a new cluster applies.
func newClusterID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// apply proposes one entry and waits for it to be applied.
func (c *Cluster) apply(e entry) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f := c.raft.Apply(raw, c.t.Apply)
	if err := f.Error(); err != nil {
		return err
	}
	if err, ok := f.Response().(error); ok {
		return err
	}
	return nil
}
