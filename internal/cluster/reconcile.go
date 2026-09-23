package cluster

import (
	"sort"
	"strconv"
	"time"

	"github.com/hashicorp/raft"
)

// member is a live member as the leader sees it.
type member struct {
	id      string
	service string
	jobs    map[string]bool
	idx     uint64
	state   bool   // it holds Raft state
	cluster string // the cluster ID it has applied
}

// view is the live membership: every member publishing under the cluster key
// with the same raft block, entry version and cluster ID, whose service takes
// part, and which the leader is not failing to heartbeat for longer than
// Timing.Hold. This member is always in it.
func (c *Cluster) view() map[string]member {
	out := map[string]member{}
	for _, m := range c.cfg.Mesh.Members(c.key) {
		if !c.cfg.Spec.Participates(m.Service) || !c.sameCluster(m) {
			continue
		}
		if m.NodeID != c.local && c.heartbeatsFailing(m.NodeID) {
			continue
		}
		idx, _ := strconv.ParseUint(m.Meta[metaIdx], 10, 64)
		jobs := map[string]bool{}
		for _, j := range splitJobs(m.Meta[metaJobs]) {
			jobs[j] = true
		}
		out[m.NodeID] = member{id: m.NodeID, service: m.Service, jobs: jobs, idx: idx,
			state: m.Meta[metaState] == "1", cluster: m.Meta[metaID]}
	}
	if _, ok := out[c.local]; !ok {
		c.mu.Lock()
		jobs := map[string]bool{}
		for name := range c.jobs {
			jobs[name] = true
		}
		c.mu.Unlock()
		out[c.local] = member{id: c.local, service: c.service, jobs: jobs, idx: c.raft.AppliedIndex(),
			state: true, cluster: c.clusterID()}
	}
	c.mu.Lock()
	for id, m := range out {
		c.services[id] = m.service
	}
	c.mu.Unlock()
	return out
}

func (c *Cluster) heartbeatsFailing(id string) bool {
	c.mu.Lock()
	since, ok := c.hbFailed[id]
	c.mu.Unlock()
	return ok && time.Since(since) > c.t.Hold
}

// reconcile is the leader's pass over membership and assignments. Each pass
// makes at most one membership change, then re-reads the configuration on the
// next pass, so no decision is made from a configuration that change made
// stale.
func (c *Cluster) reconcile() {
	c.mu.Lock()
	c.lastRecon = time.Now()
	term := c.leaderTerm
	c.mu.Unlock()
	if current := c.raft.CurrentTerm(); term != current {
		// The barrier makes this leader apply every entry of earlier terms
		// before it reads assignments.
		if err := c.raft.Barrier(c.t.Apply).Error(); err != nil {
			return
		}
		c.mu.Lock()
		c.leaderTerm = current
		c.mu.Unlock()
	}
	if c.fsm.haltReason() != "" {
		return
	}
	if c.fsm.clusterID() == "" {
		if err := c.apply(entry{V: FSMVersion, Op: opCluster, ID: newClusterID()}); err != nil {
			c.log.Warn("tursoraft: apply the cluster id", "err", err)
		}
		c.wake()
		return
	}
	f := c.raft.GetConfiguration()
	if f.Error() != nil {
		return
	}
	conf := f.Configuration()
	live := c.view()
	changed := c.addMembers(conf, live) || c.removeAbsent(conf, live) || c.balanceVoters(conf, live)
	c.assign(conf, live)
	if changed {
		c.wake()
	}
}

// addMembers adds, as a non-voter, one live member missing from the
// configuration once it has passed the member proof and shown it belongs to no
// other cluster: it holds no Raft state, or has applied this cluster's ID. A
// member with state and no applied ID may be a voter of a cluster formed
// apart from this one whose ID it has not applied yet; adding it would have
// two leaders write one log.
func (c *Cluster) addMembers(conf raft.Configuration, live map[string]member) bool {
	in := servers(conf)
	mine := c.clusterID()
	for _, id := range sortedIDs(live) {
		if _, ok := in[id]; ok {
			continue
		}
		if live[id].state && live[id].cluster != mine {
			continue
		}
		c.mu.Lock()
		failedAt, failed := c.probeFail[id]
		c.mu.Unlock()
		if failed && time.Since(failedAt) < probeRetry {
			continue
		}
		ctx, cancel := c.ctxStop()
		peer, err := c.layer.probe(ctx, id)
		cancel()
		if err == nil && peer.state && peer.cluster != mine {
			c.alert(id, "it holds raft state but has not applied this cluster's id")
			err = ErrForeignCluster
		}
		if err != nil {
			c.mu.Lock()
			c.probeFail[id] = time.Now()
			c.mu.Unlock()
			continue
		}
		c.mu.Lock()
		delete(c.probeFail, id)
		c.mu.Unlock()
		if err := c.raft.AddNonvoter(raft.ServerID(id), raft.ServerAddress(id), 0, c.t.Apply).Error(); err != nil {
			c.log.Warn("tursoraft: add member", "peer", id, "err", err)
			return false
		}
		c.log.Info("tursoraft: added a member", "peer", id, "service", live[id].service)
		return true
	}
	return false
}

// removeAbsent removes one member absent for too long: a non-voter after
// HostAbsent, a voter after VoterAbsent and only while a caught-up
// replacement is live and the voters left still hold a quorum. A dead voter
// left in the configuration counts against every election's quorum.
func (c *Cluster) removeAbsent(conf raft.Configuration, live map[string]member) bool {
	now := time.Now()
	in := servers(conf)
	c.mu.Lock()
	for id := range c.absent {
		if _, ok := in[id]; !ok {
			delete(c.absent, id)
		}
	}
	var gone []raft.Server
	for _, s := range conf.Servers {
		id := string(s.ID)
		if _, ok := live[id]; ok || id == c.local {
			delete(c.absent, id)
			continue
		}
		since, ok := c.absent[id]
		if !ok {
			c.absent[id] = now
			continue
		}
		if s.Suffrage == raft.Voter && now.Sub(since) > c.t.VoterAbsent || s.Suffrage != raft.Voter && now.Sub(since) > c.t.HostAbsent {
			gone = append(gone, s)
		}
	}
	c.mu.Unlock()
	for _, s := range gone {
		if s.Suffrage == raft.Voter && !(c.replacementLive(conf, live) && c.quorumWithout(conf, live, string(s.ID))) {
			continue
		}
		if err := c.raft.RemoveServer(s.ID, 0, c.t.Apply).Error(); err != nil {
			c.log.Warn("tursoraft: remove absent member", "peer", s.ID, "err", err)
			return false
		}
		c.log.Info("tursoraft: removed an absent member", "peer", s.ID, "voter", s.Suffrage == raft.Voter)
		c.mu.Lock()
		delete(c.absent, string(s.ID))
		c.mu.Unlock()
		return true
	}
	return false
}

// caughtUp reports whether m is within CatchUp entries of this leader.
func (c *Cluster) caughtUp(m member) bool {
	last := c.raft.LastIndex()
	return m.idx > 0 && (m.idx >= last || last-m.idx <= c.t.CatchUp)
}

// candidates are the live, caught-up non-voters whose service may vote.
func (c *Cluster) candidates(conf raft.Configuration, live map[string]member) []member {
	in := servers(conf)
	var out []member
	for _, id := range sortedIDs(live) {
		m := live[id]
		if s, ok := in[id]; ok && s.Suffrage != raft.Voter && c.cfg.Spec.IsVoterService(m.service) && c.caughtUp(m) {
			out = append(out, m)
		}
	}
	return out
}

func (c *Cluster) replacementLive(conf raft.Configuration, live map[string]member) bool {
	return len(c.candidates(conf, live)) > 0
}

// quorumWithout reports whether the live voters would still be a quorum of
// the configuration once id is removed.
func (c *Cluster) quorumWithout(conf raft.Configuration, live map[string]member, id string) bool {
	voters, alive := 0, 0
	for _, s := range conf.Servers {
		if s.Suffrage != raft.Voter || string(s.ID) == id {
			continue
		}
		voters++
		if _, ok := live[string(s.ID)]; ok {
			alive++
		}
	}
	return voters > 0 && alive >= voters/2+1
}

// balanceVoters makes one change toward VoterCount voters with each voter
// service holding at least its floor. The floor of a service is 2, or the
// number of its live eligible members when fewer, and applies only when more
// than one service votes: a split between two apps must not leave either
// without a voter to hand the cluster's state to. A promoted member is always
// caught up; a demoted one is never this leader.
func (c *Cluster) balanceVoters(conf raft.Configuration, live map[string]member) bool {
	spec := c.cfg.Spec
	in := servers(conf)
	votersOf := map[string][]string{}
	nVoters := 0
	eligible := map[string]int{}
	c.mu.Lock()
	for _, s := range conf.Servers {
		if s.Suffrage == raft.Voter {
			nVoters++
			votersOf[c.services[string(s.ID)]] = append(votersOf[c.services[string(s.ID)]], string(s.ID))
		}
	}
	c.mu.Unlock()
	for id, m := range live {
		if _, ok := in[id]; ok && spec.IsVoterService(m.service) {
			eligible[m.service]++
		}
	}
	floor := func(service string) int {
		if len(spec.VoterServices) < 2 {
			return 0
		}
		return min(2, eligible[service])
	}
	cands := c.candidates(conf, live)
	if nVoters < spec.VoterCount && len(cands) > 0 {
		best := cands[0]
		for _, m := range cands[1:] {
			if floor(m.service)-len(votersOf[m.service]) > floor(best.service)-len(votersOf[best.service]) {
				best = m
			}
		}
		return c.promote(best.id)
	}
	for _, service := range spec.VoterServices {
		if len(votersOf[service]) >= floor(service) {
			continue
		}
		var cand *member
		for i := range cands {
			if cands[i].service == service {
				cand = &cands[i]
				break
			}
		}
		if cand == nil {
			continue
		}
		victim := ""
		for _, other := range spec.VoterServices {
			if other == service || len(votersOf[other]) <= floor(other) {
				continue
			}
			for _, id := range votersOf[other] {
				if _, ok := live[id]; ok && id != c.local && id > victim {
					victim = id
				}
			}
		}
		if victim == "" {
			continue
		}
		if !c.promote(cand.id) {
			return false
		}
		return c.demote(victim)
	}
	if nVoters > spec.VoterCount {
		victim := ""
		for service, ids := range votersOf {
			if len(ids) <= floor(service) {
				continue
			}
			for _, id := range ids {
				if _, ok := live[id]; ok && id != c.local && id > victim {
					victim = id
				}
			}
		}
		if victim != "" {
			return c.demote(victim)
		}
	}
	return false
}

func (c *Cluster) promote(id string) bool {
	if err := c.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(id), 0, c.t.Apply).Error(); err != nil {
		c.log.Warn("tursoraft: promote to voter", "peer", id, "err", err)
		return false
	}
	c.log.Info("tursoraft: promoted a member to voter", "peer", id)
	return true
}

func (c *Cluster) demote(id string) bool {
	if err := c.raft.DemoteVoter(raft.ServerID(id), 0, c.t.Apply).Error(); err != nil {
		c.log.Warn("tursoraft: demote voter", "peer", id, "err", err)
		return false
	}
	c.log.Info("tursoraft: demoted a voter to standby", "peer", id)
	return true
}

// assign keeps each job with its live holder and otherwise gives it to the
// live member with the lowest NodeID that is in the configuration, whose
// service hosts the job, and which has registered it.
func (c *Cluster) assign(conf raft.Configuration, live map[string]member) {
	in := servers(conf)
	eligible := func(id, job string) bool {
		m, ok := live[id]
		if !ok || !m.jobs[job] || !c.cfg.Spec.HostsJob(m.service, job) {
			return false
		}
		_, ok = in[id]
		return ok
	}
	for _, job := range c.cfg.Spec.JobNames() {
		if a, ok := c.fsm.assignment(job); ok && eligible(a.Node, job) {
			continue
		}
		for _, id := range sortedIDs(live) {
			if !eligible(id, job) {
				continue
			}
			if err := c.apply(entry{V: FSMVersion, Op: opAssign, Job: job, Node: id}); err != nil {
				c.log.Warn("tursoraft: assign job", "job", job, "node", id, "err", err)
				return
			}
			c.log.Info("tursoraft: assigned a job", "job", job, "node", id)
			break
		}
	}
}

func servers(conf raft.Configuration) map[string]raft.Server {
	out := make(map[string]raft.Server, len(conf.Servers))
	for _, s := range conf.Servers {
		out[string(s.ID)] = s
	}
	return out
}

func sortedIDs(live map[string]member) []string {
	ids := make([]string, 0, len(live))
	for id := range live {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
