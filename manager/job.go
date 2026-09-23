package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/bbmumford/tursoraft/fence"
	"github.com/bbmumford/tursoraft/internal/cluster"
)

// Errors Job returns.
var (
	// ErrNoCluster means the group declares no raft block, or this process's
	// service neither votes nor hosts a job in it.
	ErrNoCluster = errors.New("tursoraft: no raft cluster for this database in this process")
	// ErrUnknownJob means the raft block does not declare the job.
	ErrUnknownJob = cluster.ErrUnknownJob
	// ErrNotHost means this process's service is not among the job's hosts.
	ErrNotHost = cluster.ErrNotHost
)

// JobFunc is a singleton job's body. It runs while this process holds the
// job, gets a context that ends when the job moves elsewhere or the manager
// shuts down, and must send every write through f.Tx. If it returns while the
// job is still held it runs again after a backoff.
type JobFunc func(ctx context.Context, f fence.Fence) error

// Job is one singleton job registered in this process.
type Job struct{ j *cluster.Job }

// Held reports whether this process holds the job now.
func (j *Job) Held() bool { return j.j.Held() }

// Fence is the fence this process holds the job under, while it holds it.
func (j *Job) Fence() (fence.Fence, bool) { return j.j.Fence() }

// Job registers fn as this process's body for the named singleton job of the
// group's shared database. The cluster runs it on exactly one live host at a
// time; for the failover window in which two may believe they hold it, the
// fence orders their writes.
func (m *Manager) Job(group, db, name string, fn JobFunc) (*Job, error) {
	c, err := m.cluster(group, db)
	if err != nil {
		return nil, err
	}
	j, err := c.Register(name, cluster.JobFunc(fn))
	if err != nil {
		return nil, err
	}
	return &Job{j}, nil
}

func (m *Manager) cluster(group, db string) (*cluster.Cluster, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[dbKey(group, db)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoCluster, dbKey(group, db))
	}
	return c, nil
}

// Leader returns the NodeID of the Raft leader of the group's cluster as this
// process knows it, "" while there is none.
func (m *Manager) Leader(group, db string) (string, error) {
	c, err := m.cluster(group, db)
	if err != nil {
		return "", err
	}
	return c.Leader(), nil
}

// MeshKey is the key the Raft cluster of a group's shared database runs under
// on the mesh: its members publish their metadata under it, and every Raft
// stream is dialled and listened for on it.
//
// Consumed by a mesh runtime that admits a cluster's streams by key, such as
// loom's node runtime, which accepts an inbound stream only when its key
// belongs to a raft block giving the dialling service a seat. A key that
// differs from the one the cluster uses matches no stream and admits nobody,
// so the runtime takes it from here rather than restating its form.
func MeshKey(group, db string) string { return cluster.Key(group, db) }
