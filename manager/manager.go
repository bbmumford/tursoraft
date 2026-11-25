// Package manager holds a process's shared and local databases and its seats
// in the Raft clusters that decide who runs each singleton job.
//
// Every process that holds a shared database keeps an encrypted embedded
// replica of its primary: reads are answered from the replica file, and each
// write reaches the primary before it returns, so every holder writes and
// none waits for Raft. A replica's pooled connection proves foreign keys once,
// when it opens, which needs the primary, so reads on the connections already
// open keep answering while the primary is down (see the replica package).
//
// A group with a raft block also gets one Raft cluster, run over the mesh,
// that assigns each declared job to one live host and stamps the assignment
// with an epoch; the job's writes go through fence.Fence.Tx, which the
// primary orders by that epoch. Raft carries assignments only, never rows.
//
// The package is neutral: the mesh is an interface (package mesh) that a mesh
// runtime implements, and nothing here reads an environment variable or calls
// the Turso platform API.
package manager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bbmumford/filekind"
	"github.com/bbmumford/policypool"
	"github.com/bbmumford/tursoraft/database"
	"github.com/bbmumford/tursoraft/internal/cluster"
	"github.com/bbmumford/tursoraft/localdb"
	"github.com/bbmumford/tursoraft/replica"
)

// ErrUnknownDatabase means the manager holds no database by that group and
// name.
var ErrUnknownDatabase = errors.New("tursoraft: unknown database")

// holding is one database the manager holds: a replica of a shared database,
// or a local file.
type holding struct {
	group, name string
	db          *sql.DB
	replica     *replica.Replica // nil for a local file
}

// Manager holds a process's databases and cluster seats.
type Manager struct {
	cfg ManagerConfig
	log *slog.Logger

	dbs      map[string]*holding         // "<group>/<db>"
	order    []*holding                  // in declaration order
	clusters map[string]*cluster.Cluster // "<group>/<db>"
	declared bool                        // some group declares a raft block
	first    *cluster.Cluster            // this process's seat in the first declared cluster, for IsLeader

	mu       sync.Mutex
	closed   bool
	shutdown error
	once     sync.Once
}

func dbKey(group, db string) string { return group + "/" + db }

// NewManager validates cfg with no network, opens every database, then
// starts the Raft cluster of every group with a raft block. It returns once
// every database is open; it never waits for a Raft leader.
//
// A shared database whose primary cannot be reached within the open timeout
// fails NewManager, and nothing opened before it is left open.
func NewManager(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m := &Manager{
		cfg:      cfg,
		log:      cfg.Logger,
		dbs:      map[string]*holding{},
		clusters: map[string]*cluster.Cluster{},
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	for _, g := range cfg.Groups {
		if err := m.openGroup(ctx, g); err != nil {
			m.Shutdown()
			return nil, err
		}
	}
	for _, g := range cfg.Groups {
		if g.Raft == nil {
			continue
		}
		if err := m.startCluster(g); err != nil {
			m.Shutdown()
			return nil, err
		}
	}
	return m, nil
}

// localIdentity is the file identity of a pure-local group's databases:
// filekind's tursoraftLocal kind, generation 1. A new file is stamped with it
// and any other file is refused with policypool.ErrFileIdentity: such a file is
// authoritative, so the group fails to open rather than lose its rows.
var localIdentity = policypool.Identity{Kind: filekind.TursoraftLocal, Generation: 1}

func (m *Manager) openGroup(ctx context.Context, g GroupConfig) error {
	keys, err := g.keys()
	if err != nil {
		return err
	}
	interval := g.SyncInterval
	if interval <= 0 {
		interval = m.cfg.SyncInterval
	}
	dir := m.cfg.RaftConfig.LocalDir
	for _, d := range g.databases() {
		h := &holding{group: g.GroupName, name: d.name}
		if g.PureLocal {
			db, err := localdb.OpenFile(ctx, localPath(dir, g.GroupName, d.name), keys[0], localIdentity)
			if err != nil {
				return fmt.Errorf("tursoraft: group %q database %q: %w", g.GroupName, d.name, err)
			}
			h.db = db
		} else {
			r, err := replica.Open(ctx, replica.Config{
				Path:         replicaPath(dir, g.GroupName, d.name),
				URL:          d.url,
				Mode:         replica.ReadWrite,
				Credentials:  replica.StaticCredential(d.token),
				Key:          keys[0],
				SyncInterval: interval,
				OpenTimeout:  m.cfg.OpenTimeout,
				Logger:       m.log,
			})
			if err != nil {
				return fmt.Errorf("tursoraft: group %q database %q: %w", g.GroupName, d.name, err)
			}
			h.db, h.replica = r.DB(), r
		}
		m.mu.Lock()
		m.dbs[dbKey(g.GroupName, d.name)] = h
		m.order = append(m.order, h)
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) startCluster(g GroupConfig) error {
	first := !m.declared
	m.declared = true
	keys, err := g.keys()
	if err != nil {
		return err
	}
	db := g.databases()[0].name
	c, err := cluster.Start(cluster.Config{
		Group:  g.GroupName,
		DB:     db,
		Spec:   g.Raft.spec(),
		Mesh:   m.cfg.Mesh,
		Keys:   keys,
		Dir:    raftDir(m.cfg.RaftConfig.LocalDir, g.GroupName, db),
		Logger: m.log,
		Timing: m.cfg.timing,
	})
	if errors.Is(err, cluster.ErrNotParticipant) {
		// This service holds the database but neither votes nor hosts a
		// job; it has no seat in the cluster.
		return nil
	}
	if err != nil {
		return fmt.Errorf("tursoraft: raft for %s: %w", dbKey(g.GroupName, db), err)
	}
	m.mu.Lock()
	m.clusters[dbKey(g.GroupName, db)] = c
	if first {
		m.first = c
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) holding(group, db string) (*holding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("tursoraft: manager is shut down")
	}
	h, ok := m.dbs[dbKey(group, db)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownDatabase, dbKey(group, db))
	}
	return h, nil
}

// Database returns the pool of one database. Every connection on it has
// foreign keys on. On a shared database every transaction that can write
// begins with BEGIN IMMEDIATE on the primary, every write needs the primary,
// and reads are answered from the replica file, also while the primary is
// down, on the connections the pool proved before.
//
// A read and then a write outside one transaction can lose an update another
// process makes between them; put both in one BeginTx, or in one statement.
func (m *Manager) Database(group, db string) (*sql.DB, error) {
	h, err := m.holding(group, db)
	if err != nil {
		return nil, err
	}
	return h.db, nil
}

// Sync pulls every shared database from its primary and returns every error,
// joined. An endpoint calls it after applying its schema and before serving,
// so it serves no older state than the primary held when it booted.
func (m *Manager) Sync(ctx context.Context) error {
	m.mu.Lock()
	order := append([]*holding(nil), m.order...)
	m.mu.Unlock()
	var errs []error
	for _, h := range order {
		if h.replica == nil {
			continue
		}
		if err := h.replica.Sync(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dbKey(h.group, h.name), err))
		}
	}
	return errors.Join(errs...)
}

// SyncOnMiss pulls one shared database, for a reader that did not find a row
// another process may have just written: a session minted by one api instance
// and proved through another before that one has pulled.
//
// Callers share pulls: one in progress serves everyone who asked before it
// started, and pulls start at least 250 ms apart, so a flood of unknown
// cookies costs at most four pulls a second. A local file returns nil.
func (m *Manager) SyncOnMiss(ctx context.Context, group, db string) error {
	h, err := m.holding(group, db)
	if err != nil {
		return err
	}
	if h.replica == nil {
		return nil
	}
	return h.replica.Sync(ctx)
}

// SyncStats reports how far a shared database's replica is behind its
// primary, or database.ErrSyncStatsUnavailable for a local file.
func (m *Manager) SyncStats(ctx context.Context, group, db string) (database.SyncStats, error) {
	h, err := m.holding(group, db)
	if err != nil {
		return database.SyncStats{}, err
	}
	if err := ctx.Err(); err != nil {
		return database.SyncStats{}, err
	}
	if h.replica == nil {
		return database.SyncStats{}, database.ErrSyncStatsUnavailable
	}
	return h.replica.SyncStats(), nil
}

// Shutdown leaves every cluster (cancelling this process's jobs first), then
// closes every database. It is safe to call more than once and returns the
// first call's result.
func (m *Manager) Shutdown() error {
	m.once.Do(func() {
		m.mu.Lock()
		m.closed = true
		clusters := make([]*cluster.Cluster, 0, len(m.clusters))
		for _, c := range m.clusters {
			clusters = append(clusters, c)
		}
		order := append([]*holding(nil), m.order...)
		m.mu.Unlock()
		var errs []error
		for _, c := range clusters {
			if err := c.Shutdown(); err != nil {
				errs = append(errs, fmt.Errorf("raft %s: %w", c.Name(), err))
			}
		}
		for _, h := range order {
			var err error
			if h.replica != nil {
				err = h.replica.Close()
			} else {
				err = h.db.Close()
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", dbKey(h.group, h.name), err))
			}
		}
		m.shutdown = errors.Join(errs...)
	})
	return m.shutdown
}
