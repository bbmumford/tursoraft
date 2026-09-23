package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	bolt "go.etcd.io/bbolt"

	"github.com/bbmumford/tursoraft/mesh"
)

// Errors Start and Register return.
var (
	// ErrNotParticipant means this process's service neither votes nor hosts
	// a job in the cluster, so it runs no Raft node for it.
	ErrNotParticipant = errors.New("tursoraft/cluster: this service neither votes nor hosts a job")
	// ErrNoDir means a voter was started without a directory for its Raft
	// state.
	ErrNoDir = errors.New("tursoraft/cluster: a voter needs a directory for its raft state")
	// ErrNoKey means the cluster was started without a group key.
	ErrNoKey = errors.New("tursoraft/cluster: no group key")
	// ErrUnknownJob means the job is not declared in the raft block.
	ErrUnknownJob = errors.New("tursoraft/cluster: job is not declared")
	// ErrNotHost means this process's service is not among the job's hosts.
	ErrNotHost = errors.New("tursoraft/cluster: this service does not host the job")
	// ErrJobRegistered means the job was registered twice in this process.
	ErrJobRegistered = errors.New("tursoraft/cluster: job already registered")
	// ErrShutdown means the cluster has been shut down.
	ErrShutdown = errors.New("tursoraft/cluster: shut down")
)

// Config describes one cluster member.
type Config struct {
	// Group and DB name the shared database the cluster coordinates.
	Group, DB string
	// Spec is the group's raft block.
	Spec Spec
	// Mesh is the network; its LocalID is this member's NodeID and its
	// LocalService decides whether it votes, hosts jobs, or both.
	Mesh mesh.Mesh
	// Keys are the group's keys, the current one first and, during a
	// rotation, the prior one second.
	Keys [][]byte
	// Dir holds a voter's Raft state. A process that only hosts jobs keeps
	// its state in memory and ignores Dir.
	Dir string
	// Logger receives the cluster's alerts; nil means slog.Default.
	Logger *slog.Logger
	// Timing overrides DefaultTiming field by field.
	Timing Timing
}

// Cluster is this process's member of one cluster.
type Cluster struct {
	cfg     Config
	t       Timing
	name    string // "<group>/<db>"
	key     string // mesh key
	local   string
	service string
	voter   bool // this service may vote
	spec    string
	log     *slog.Logger

	fsm    *fsm
	raft   *raft.Raft
	trans  *raft.NetworkTransport
	layer  *streamLayer
	closer []func() error // stores to close after raft shuts down
	logs   raft.LogStore
	stable raft.StableStore
	obs    *raft.Observer
	obsCh  chan raft.Observation

	kickControl chan struct{}
	kickRunner  chan struct{}
	stop        chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup // control, runner and observer loops
	jobWG       sync.WaitGroup // job runs

	mu         sync.Mutex
	jobs       map[string]*Job
	persisted  string               // cluster ID read from the stable store at start
	hbFailed   map[string]time.Time // peer → last contact, while its heartbeats fail
	published  map[string]string
	proposal   string
	agreedAt   time.Time
	agreedOn   string
	stopping   bool
	leaderTerm uint64 // term whose barrier this leader has passed
	absent     map[string]time.Time
	services   map[string]string    // NodeID → service, as last seen live
	probeFail  map[string]time.Time // NodeID → last failed probe
	alerted    map[string]string    // NodeID → reason last alerted
	lastRecon  time.Time
}

// stableClusterKey is where a voter records its cluster ID in its stable
// store.
var stableClusterKey = []byte("tursoraft/cluster_id")

// Start joins this process to the cluster: it opens the Raft stores (on disk
// for a voter, in memory for a job host), starts Raft over the mesh and
// begins publishing this member's state under the cluster's mesh key. It
// never waits for a leader.
func Start(cfg Config) (*Cluster, error) {
	if cfg.Mesh == nil {
		return nil, errors.New("tursoraft/cluster: no mesh")
	}
	if len(cfg.Keys) == 0 || len(cfg.Keys) > maxProofs {
		return nil, ErrNoKey
	}
	t := cfg.Timing.withDefaults()
	c := &Cluster{
		cfg: cfg, t: t,
		name:    cfg.Group + "/" + cfg.DB,
		key:     Key(cfg.Group, cfg.DB),
		local:   cfg.Mesh.LocalID(),
		service: cfg.Mesh.LocalService(),
		spec:    cfg.Spec.Digest(),
		log:     cfg.Logger,

		kickControl: make(chan struct{}, 1),
		kickRunner:  make(chan struct{}, 1),
		stop:        make(chan struct{}),
		jobs:        map[string]*Job{},
		hbFailed:    map[string]time.Time{},
		absent:      map[string]time.Time{},
		services:    map[string]string{},
		probeFail:   map[string]time.Time{},
		alerted:     map[string]string{},
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	c.log = c.log.With("cluster", c.name)
	c.voter = cfg.Spec.IsVoterService(c.service)
	if !c.voter && !cfg.Spec.IsHostService(c.service) {
		return nil, ErrNotParticipant
	}
	if c.voter && cfg.Dir == "" {
		return nil, ErrNoDir
	}
	memberKeys := make([][]byte, 0, len(cfg.Keys))
	for _, k := range cfg.Keys {
		mk, err := MemberKey(k, c.name)
		if err != nil {
			return nil, err
		}
		memberKeys = append(memberKeys, mk)
	}
	if err := c.open(memberKeys); err != nil {
		c.closeStores()
		return nil, err
	}
	c.wg.Add(3)
	go c.observe()
	go c.control()
	go c.runner()
	return c, nil
}

// open builds the stores, the transport and the Raft node.
func (c *Cluster) open(memberKeys [][]byte) error {
	hlog := newHCLogger(c.log)
	var (
		logs   raft.LogStore
		stable raft.StableStore
		snaps  raft.SnapshotStore
	)
	if c.voter {
		if err := os.MkdirAll(c.cfg.Dir, 0o700); err != nil {
			return err
		}
		// A bolt file locked by another open handle blocks its opener
		// forever; the timeout turns a second open of the same state into
		// an error.
		boltOpts := &bolt.Options{Timeout: 5 * time.Second}
		ls, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(c.cfg.Dir, "logs.db"), BoltOptions: boltOpts})
		if err != nil {
			return fmt.Errorf("tursoraft/cluster: open log store: %w", err)
		}
		c.closer = append(c.closer, ls.Close)
		ss, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(c.cfg.Dir, "stable.db"), BoltOptions: boltOpts})
		if err != nil {
			return fmt.Errorf("tursoraft/cluster: open stable store: %w", err)
		}
		c.closer = append(c.closer, ss.Close)
		fs, err := raft.NewFileSnapshotStoreWithLogger(c.cfg.Dir, 2, hlog)
		if err != nil {
			return fmt.Errorf("tursoraft/cluster: open snapshot store: %w", err)
		}
		logs, stable, snaps = ls, ss, fs
		if id, err := ss.Get(stableClusterKey); err == nil {
			c.persisted = string(id)
		}
	} else {
		mem := raft.NewInmemStore()
		logs, stable, snaps = mem, mem, raft.NewInmemSnapshotStore()
	}
	c.logs, c.stable = logs, stable

	c.fsm = newFSM(fsmHooks{
		notify: c.wake,
		halted: func(reason string) {
			c.log.Error("tursoraft: this member met a raft entry it cannot apply and stopped applying; upgrade it", "reason", reason)
		},
		clusterID: func(id string) {
			if c.voter {
				if err := c.stable.Set(stableClusterKey, []byte(id)); err != nil {
					c.log.Error("tursoraft: record cluster id", "err", err)
				}
			}
		},
	})

	layer, err := newStreamLayer(c.cfg.Mesh, c.key, handshake{
		cluster: c.name, local: c.local, keys: memberKeys, standing: c.standing,
	}, c.log, nil)
	if err != nil {
		return err
	}
	c.layer = layer
	c.trans = raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:  layer,
		MaxPool: 3,
		Timeout: c.t.Apply,
		Logger:  hlog,
	})

	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(c.local)
	rc.HeartbeatTimeout = c.t.Heartbeat
	rc.ElectionTimeout = c.t.Election
	rc.LeaderLeaseTimeout = c.t.LeaderLease
	rc.TrailingLogs = c.t.CatchUp
	rc.SnapshotThreshold = c.t.CatchUp
	rc.Logger = hlog
	rc.NoLegacyTelemetry = true
	// A removed member stays up so the leader can add it again when it
	// reappears; a member shut down by its own removal would need a restart.
	rc.ShutdownOnRemove = false
	r, err := raft.NewRaft(rc, c.fsm, logs, stable, snaps, c.trans)
	if err != nil {
		c.trans.Close()
		return fmt.Errorf("tursoraft/cluster: start raft: %w", err)
	}
	c.raft = r
	obsCh := make(chan raft.Observation, 64)
	c.obs = raft.NewObserver(obsCh, false, func(o *raft.Observation) bool {
		switch o.Data.(type) {
		case raft.FailedHeartbeatObservation, raft.ResumedHeartbeatObservation,
			raft.LeaderObservation, raft.RaftState, raft.PeerObservation:
			return true
		}
		return false
	})
	r.RegisterObserver(c.obs)
	c.obsCh = obsCh
	return nil
}

// closeStores closes whatever stores open made, for a Start that failed.
func (c *Cluster) closeStores() {
	for _, fn := range c.closer {
		fn()
	}
	c.closer = nil
}

// clusterID is the cluster ID this member has applied, or the one it recorded
// before a restart while its log is not yet replayed.
func (c *Cluster) clusterID() string {
	if c.fsm != nil {
		if id := c.fsm.clusterID(); id != "" {
			return id
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persisted
}

// standing is what this member reports in the handshake. It reads the log
// store rather than Raft, because a stream can arrive before Raft starts.
func (c *Cluster) standing() standing {
	last, _ := c.logs.LastIndex()
	return standing{cluster: c.clusterID(), state: last > 0}
}

// wake signals the control loop and the job runner; it never blocks.
func (c *Cluster) wake() {
	for _, ch := range []chan struct{}{c.kickControl, c.kickRunner} {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// observe turns Raft observations into the leader's view of failing peers and
// wakes the loops.
func (c *Cluster) observe() {
	defer c.wg.Done()
	for {
		select {
		case <-c.stop:
			return
		case o := <-c.obsCh:
			c.mu.Lock()
			switch d := o.Data.(type) {
			case raft.FailedHeartbeatObservation:
				if _, ok := c.hbFailed[string(d.PeerID)]; !ok {
					at := d.LastContact
					if at.IsZero() {
						at = time.Now()
					}
					c.hbFailed[string(d.PeerID)] = at
				}
			case raft.ResumedHeartbeatObservation:
				delete(c.hbFailed, string(d.PeerID))
			case raft.RaftState:
				if d != raft.Leader {
					c.hbFailed = map[string]time.Time{}
				}
			}
			c.mu.Unlock()
			c.wake()
		}
	}
}

// Name is "<group>/<db>".
func (c *Cluster) Name() string { return c.name }

// ClusterID is the applied cluster ID, "" before one exists.
func (c *Cluster) ClusterID() string { return c.clusterID() }

// Leader is the NodeID of the leader this member knows, "" when it knows none.
func (c *Cluster) Leader() string {
	_, id := c.raft.LeaderWithID()
	return string(id)
}

// IsLeader reports whether this member is the Raft leader.
func (c *Cluster) IsLeader() bool { return c.raft.State() == raft.Leader }

// Halted is the reason this member stopped applying entries, "" if it has not.
func (c *Cluster) Halted() string { return c.fsm.haltReason() }

// Assignment reports who holds job and under which epoch.
func (c *Cluster) Assignment(job string) (Assignment, bool) { return c.fsm.assignment(job) }

// Voters lists the NodeIDs the configuration holds as voters.
func (c *Cluster) Voters() ([]string, error) {
	f := c.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range f.Configuration().Servers {
		if s.Suffrage == raft.Voter {
			out = append(out, string(s.ID))
		}
	}
	return out, nil
}

// Members lists every NodeID in the configuration, voting or not.
func (c *Cluster) Members() ([]string, error) {
	f := c.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range f.Configuration().Servers {
		out = append(out, string(s.ID))
	}
	return out, nil
}

// Shutdown leaves the cluster: it stops the control, runner and observer
// loops, cancels this member's jobs, withdraws its published state, hands off
// leadership, shuts Raft down, closes the Raft stores and then the transport.
// It waits for every goroutine it started.
//
// The loops stop before Raft does because the control loop may be waiting on
// a Raft future, and a future whose entry Raft committed but had not applied
// when it shut down is never answered.
func (c *Cluster) Shutdown() error {
	var err error
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.stopping = true
		c.mu.Unlock()
		close(c.stop)
		c.wg.Wait()
		c.stopJobs()
		c.cfg.Mesh.Publish(c.key, nil)
		if c.raft.State() == raft.Leader {
			c.raft.LeadershipTransfer().Error()
		}
		c.raft.DeregisterObserver(c.obs)
		err = c.raft.Shutdown().Error()
		for _, fn := range c.closer {
			err = errors.Join(err, fn())
		}
		err = errors.Join(err, c.trans.Close())
	})
	return err
}

// ctxStop is a context Shutdown ends, for the dials the control loop makes.
func (c *Cluster) ctxStop() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-c.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
