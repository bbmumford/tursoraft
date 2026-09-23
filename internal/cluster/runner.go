package cluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/bbmumford/tursoraft/fence"
)

// JobFunc is a singleton job's body. It runs while this process holds the
// job and must return soon after ctx ends; every write it makes goes through
// f.Tx. If it returns while the job is still held it runs again after a
// backoff.
type JobFunc func(ctx context.Context, f fence.Fence) error

// jobWait bounds how long Shutdown waits for job bodies to return after their
// contexts end.
const jobWait = 30 * time.Second

// Job is one registered singleton job in this process.
type Job struct {
	c    *Cluster
	name string
	fn   JobFunc

	mu  sync.Mutex
	run *jobRun
}

type jobRun struct {
	fence  fence.Fence
	cancel context.CancelFunc
	done   chan struct{}
}

// Name is the job's name.
func (j *Job) Name() string { return j.name }

// Held reports whether this process holds the job now.
func (j *Job) Held() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.run != nil
}

// Fence is the fence this process holds the job under, while it holds it.
func (j *Job) Fence() (fence.Fence, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.run == nil {
		return fence.Fence{}, false
	}
	return j.run.fence, true
}

// Register declares that this process runs job with fn. The job must be in
// the raft block, and this process's service among its hosts. The member
// publishes the job; the leader assigns it to one registered host.
func (c *Cluster) Register(name string, fn JobFunc) (*Job, error) {
	if fn == nil {
		return nil, fmt.Errorf("tursoraft/cluster: job %s has no function", name)
	}
	if _, ok := c.cfg.Spec.Jobs[name]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownJob, name)
	}
	if !c.cfg.Spec.HostsJob(c.service, name) {
		return nil, fmt.Errorf("%w: %s on %s", ErrNotHost, name, c.service)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping {
		return nil, ErrShutdown
	}
	if _, ok := c.jobs[name]; ok {
		return nil, fmt.Errorf("%w: %s", ErrJobRegistered, name)
	}
	j := &Job{c: c, name: name, fn: fn}
	c.jobs[name] = j
	c.wake()
	return j, nil
}

// HeldJobs lists the jobs this process holds now.
func (c *Cluster) HeldJobs() []string {
	c.mu.Lock()
	jobs := make([]*Job, 0, len(c.jobs))
	for _, j := range c.jobs {
		jobs = append(jobs, j)
	}
	c.mu.Unlock()
	var out []string
	for _, j := range jobs {
		if j.Held() {
			out = append(out, j.name)
		}
	}
	return out
}

// runner starts and stops job bodies as assignments change.
func (c *Cluster) runner() {
	defer c.wg.Done()
	tick := time.NewTicker(c.t.Hold / 4)
	defer tick.Stop()
	for {
		c.evaluate()
		select {
		case <-c.stop:
			return
		case <-c.kickRunner:
		case <-tick.C:
		}
	}
}

// contact reports whether this member has heard from a leader recently
// enough to trust its view of the assignments: it is the leader, or a leader
// reached it within Timing.Hold. A member cut off from the leader stops its
// jobs, because the leader may already have given them to someone else.
func (c *Cluster) contact() bool {
	if c.raft.State() == raft.Leader {
		return true
	}
	last := c.raft.LastContact()
	return !last.IsZero() && time.Since(last) < c.t.Hold
}

// evaluate runs each registered job this member holds and stops each it does
// not.
func (c *Cluster) evaluate() {
	id := c.fsm.clusterID()
	ok := id != "" && c.fsm.haltReason() == "" && c.contact()
	c.mu.Lock()
	if c.stopping {
		ok = false
	}
	jobs := make([]*Job, 0, len(c.jobs))
	for _, j := range c.jobs {
		jobs = append(jobs, j)
	}
	c.mu.Unlock()
	for _, j := range jobs {
		a, assigned := c.fsm.assignment(j.name)
		want := ok && assigned && a.Node == c.local
		j.mu.Lock()
		cur := j.run
		switch {
		case want && (cur == nil || cur.fence.Epoch != a.Epoch || cur.fence.Cluster != id):
			if cur != nil {
				cur.cancel()
			}
			j.run = c.start(j, fence.Fence{Cluster: id, Job: j.name, Epoch: a.Epoch, Node: c.local}, cur)
		case !want && cur != nil:
			cur.cancel()
			j.run = nil
		}
		j.mu.Unlock()
	}
}

// start runs j's body under f once the previous run, if any, has returned,
// and again after a backoff each time the body returns while ctx is live. It
// starts nothing once Shutdown has begun, which is what lets stopJobs wait
// for every run.
func (c *Cluster) start(j *Job, f fence.Fence, prev *jobRun) *jobRun {
	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		return nil
	}
	c.jobWG.Add(1)
	c.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	r := &jobRun{fence: f, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer c.jobWG.Done()
		defer close(r.done)
		defer cancel()
		if prev != nil {
			<-prev.done
		}
		backoff := time.Second
		for ctx.Err() == nil {
			err := j.fn(ctx, f)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				c.log.Warn("tursoraft: job returned an error; running it again", "job", j.name, "epoch", f.Epoch, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}()
	return r
}

// stopJobs cancels every job run and waits, up to jobWait, for the bodies to
// return.
func (c *Cluster) stopJobs() {
	c.mu.Lock()
	jobs := make([]*Job, 0, len(c.jobs))
	for _, j := range c.jobs {
		jobs = append(jobs, j)
	}
	c.mu.Unlock()
	for _, j := range jobs {
		j.mu.Lock()
		if j.run != nil {
			j.run.cancel()
			j.run = nil
		}
		j.mu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		c.jobWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(jobWait):
		c.log.Error("tursoraft: a job body did not return within the shutdown wait", "wait", jobWait)
	}
}
