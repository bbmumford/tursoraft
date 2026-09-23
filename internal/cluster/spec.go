// Package cluster runs one Raft cluster per shared database that has
// singleton jobs. The cluster carries job assignments and a cluster ID, never
// rows: rows reach every process through its replica of the primary, and each
// job write is ordered by the fence row the assignment's epoch guards.
package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

// Spec is a cluster's raft block from the signed configuration.
type Spec struct {
	VoterServices   []string
	VoterCount      int
	BootstrapExpect int
	Jobs            map[string][]string
}

// Digest is the sha256 of the spec's canonical JSON. Members publish it, and
// a member whose digest differs is neither counted for bootstrap nor added:
// two processes reading different raft blocks would otherwise disagree on who
// may vote and where each job may run.
func (s Spec) Digest() string {
	voters := slices.Clone(s.VoterServices)
	sort.Strings(voters)
	jobs := make(map[string][]string, len(s.Jobs))
	for name, hosts := range s.Jobs {
		h := slices.Clone(hosts)
		sort.Strings(h)
		jobs[name] = h
	}
	raw, _ := json.Marshal(struct {
		VoterServices   []string            `json:"voterServices"`
		VoterCount      int                 `json:"voterCount"`
		BootstrapExpect int                 `json:"bootstrapExpect"`
		Jobs            map[string][]string `json:"jobs"`
	}{voters, s.VoterCount, s.BootstrapExpect, jobs})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// IsVoterService reports whether instances of service may vote.
func (s Spec) IsVoterService(service string) bool {
	return slices.Contains(s.VoterServices, service)
}

// HostsJob reports whether service may run job.
func (s Spec) HostsJob(service, job string) bool {
	return slices.Contains(s.Jobs[job], service)
}

// IsHostService reports whether service may run any job.
func (s Spec) IsHostService(service string) bool {
	for _, hosts := range s.Jobs {
		if slices.Contains(hosts, service) {
			return true
		}
	}
	return false
}

// Participates reports whether an instance of service joins the cluster at
// all: as a voter, as a job host, or both.
func (s Spec) Participates(service string) bool {
	return s.IsVoterService(service) || s.IsHostService(service)
}

// JobNames returns the declared job names in sorted order.
func (s Spec) JobNames() []string {
	names := make([]string, 0, len(s.Jobs))
	for name := range s.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Key is the mesh key a cluster publishes and listens under.
func Key(group, db string) string {
	return "tursoraft/" + group + "/" + db
}

// splitJobs parses the comma-joined job list a member publishes.
func splitJobs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
