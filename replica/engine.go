package replica

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
)

// errReopening is what a new connection gets while the loop reopens the
// engine under a renewed grant; the connections already open keep serving.
var errReopening = errors.New("tursoraft/replica: the engine is being reopened under a renewed grant")

// renewalTime is when a credential fetched at fetched is renewed: two thirds
// of the way to its NotAfter (10 minutes into a 15-minute grant), or never for
// a credential that does not expire.
func renewalTime(c Credential, fetched time.Time) time.Time {
	if c.NotAfter.IsZero() {
		return time.Time{}
	}
	return fetched.Add(c.NotAfter.Sub(fetched) * 2 / 3)
}

// renew asks the credential source for the next grant, checks it as Open
// does, retires the old engine's connections, closes it, and reopens the
// engine under the new grant. It runs on the loop, so it waits for
// the pull in progress and holds the next one until it returns; the callers
// waiting for that pull keep waiting. A grant that cannot be had or is refused
// is asked for again one sync interval later, until the held grant ends, after
// which every pull fails with ErrGrantExpired (engineForPull): the replica
// never pulls under an older grant than the newest it took.
func (r *Replica) renew() {
	ctx, cancel := context.WithTimeout(context.Background(), r.openTimeout())
	defer cancel()
	next, err := r.cfg.Credentials.Credential(ctx)
	fetched := time.Now()
	if err == nil {
		err = r.cfg.admit(next, fetched)
	}
	if err != nil {
		r.mu.Lock()
		r.renewAt = fetched.Add(r.interval)
		r.mu.Unlock()
		r.log.Warn("tursoraft: grant renewal failed; asking again until the grant ends", "path", r.cfg.Path, "err", err)
		return
	}
	// The old engine's connections are retired before it is closed: with
	// retiredBelow past their generation, each reports itself invalid and
	// database/sql discards it rather than run a read on a closed engine while
	// the new one opens.
	r.emu.Lock()
	old := r.engine
	r.engine = nil
	r.retiredBelow.Store(r.gen.Load() + 1)
	r.emu.Unlock()
	r.mu.Lock()
	r.cred, r.renewAt = boundLease(next, fetched), renewalTime(next, fetched)
	r.mu.Unlock()
	if old != nil {
		old.Close()
	}
	if err := r.openEngineAgain(next.Token); err != nil {
		r.log.Warn("tursoraft: reopening the replica under its renewed grant failed; the next pull tries again", "path", r.cfg.Path, "err", err)
	}
}

// openEngineAgain opens the engine on the replica's file under token and
// puts it in place. The file, its -client_wal_index and the frame they record
// stay, so the new engine resumes from them rather than downloading again,
// and nothing deletes the file first: a file the engine leaves unencrypted is
// refused and deleted after the open (refuseUnencrypted), which ends the
// replica.
func (r *Replica) openEngineAgain(token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.openTimeout())
	defer cancel()
	n, err := openDetached(ctx, r.cfg, token)
	if err != nil {
		return err
	}
	if err := refuseUnencrypted(r.cfg.Path, n, r.log); err != nil {
		r.emu.Lock()
		r.failed = err
		r.gen.Add(1)
		r.emu.Unlock()
		return err
	}
	r.emu.Lock()
	r.engine = n
	r.gen.Add(1)
	r.emu.Unlock()
	return nil
}

// engineForPull is the engine the next pull runs on: none once the held
// grant has ended or a reopened engine was refused, and the engine opened
// again when an earlier reopen failed.
func (r *Replica) engineForPull() (native, error) {
	r.mu.Lock()
	cred := r.cred
	r.mu.Unlock()
	r.emu.RLock()
	n, failed := r.engine, r.failed
	r.emu.RUnlock()
	if failed != nil {
		return nil, failed
	}
	if !cred.NotAfter.IsZero() && !time.Now().Before(cred.NotAfter) {
		return nil, ErrGrantExpired
	}
	if n != nil {
		return n, nil
	}
	if err := r.openEngineAgain(cred.Token); err != nil {
		return nil, err
	}
	r.emu.RLock()
	defer r.emu.RUnlock()
	if r.engine == nil {
		return nil, errReopening
	}
	return r.engine, nil
}

// currentConnector is the pool's connector. Every connection it makes is on
// the engine the replica holds when it connects; a connection made on an
// engine the loop has since replaced reports itself invalid, so database/sql
// closes it when it is next returned or reused, and the pool's connections
// move onto the new engine without the pool being replaced. Close closes the
// engine held then, which db.Close calls after the loop has stopped.
type currentConnector struct{ r *Replica }

// connectBusyWait bounds how long Connect waits out an engine that refuses a
// new connection because the database is locked, or the loop's reopen of the
// engine under a renewed grant. On libSQL's HTTP sync path
// the engine runs a statement as it opens each connection, and SQLite refuses
// it with "database is locked" while the loop's pull holds the database; every
// renewal swap retires the pool's connections, so the reads after it open new
// ones while the loop pulls. Tests shorten it.
var connectBusyWait = 5 * time.Second

// Connect opens a connection on the current engine (connectOnce), opening it
// again, with backoff and without holding emu, while the engine refuses
// because the database is locked or the loop is reopening the engine under a
// renewed grant, until connectBusyWait has passed or ctx ends; any other
// refusal is returned at once. A renewal retires the old engine's connections
// before it closes that engine, so the reads that follow open new ones, and
// they wait here for the new engine rather than fail while it opens.
func (c currentConnector) Connect(ctx context.Context) (driver.Conn, error) {
	deadline := time.Now().Add(connectBusyWait)
	backoff := 2 * time.Millisecond
	for {
		conn, err := c.connectOnce(ctx)
		if err == nil || !(databaseLocked(err) || errors.Is(err, errReopening)) || !time.Now().Before(deadline) {
			return conn, err
		}
		wait := time.NewTimer(min(backoff, time.Until(deadline)))
		select {
		case <-ctx.Done():
			wait.Stop()
			return nil, fmt.Errorf("%w (the engine refused the connection: %v)", ctx.Err(), err)
		case <-wait.C:
		}
		backoff = min(backoff*2, 50*time.Millisecond)
	}
}

// databaseLocked reports whether err is SQLite's refusal while another
// connection holds the database.
func databaseLocked(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "database is locked")
}

// connectOnce opens one connection on the engine the replica holds now.
func (c currentConnector) connectOnce(ctx context.Context) (driver.Conn, error) {
	r := c.r
	r.emu.RLock()
	defer r.emu.RUnlock()
	if r.failed != nil {
		return nil, r.failed
	}
	if r.engine == nil {
		return nil, errReopening
	}
	if err := r.leaseError(); err != nil {
		return nil, err
	}
	gen := r.gen.Load()
	conn, err := r.engine.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &engineConn{Conn: conn, r: r, gen: gen}, nil
}

func (c currentConnector) Driver() driver.Driver { return c.r.drv }

func (c currentConnector) Close() error {
	c.r.emu.Lock()
	n := c.r.engine
	c.r.engine = nil
	c.r.emu.Unlock()
	if n == nil {
		return nil
	}
	return n.Close()
}

// engineConn is one connection of the engine numbered gen. It forwards the
// statement methods of the engine's connection, which database/sql would
// otherwise not see through the wrapper, each behind the offline-lease gate
// (Replica.leaseError): once the grant and its lease have ended, every
// statement and transaction is refused with ErrLeaseEnded before the engine
// is asked, on the connections already open as on new ones.
type engineConn struct {
	driver.Conn
	r   *Replica
	gen uint64
}

// IsValid is false once the loop has retired the connection's engine or put
// another engine in place.
func (c *engineConn) IsValid() bool {
	if c.r.gen.Load() != c.gen || c.gen < c.r.retiredBelow.Load() {
		return false
	}
	v, ok := c.Conn.(driver.Validator)
	return !ok || v.IsValid()
}

// ResetSession has database/sql discard a connection of a replaced engine
// before it hands it out again.
func (c *engineConn) ResetSession(ctx context.Context) error {
	if c.r.gen.Load() != c.gen || c.gen < c.r.retiredBelow.Load() {
		return driver.ErrBadConn
	}
	if rs, ok := c.Conn.(driver.SessionResetter); ok {
		return rs.ResetSession(ctx)
	}
	return nil
}

func (c *engineConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.r.leaseError(); err != nil {
		return nil, err
	}
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *engineConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.r.leaseError(); err != nil {
		return nil, err
	}
	if q, ok := c.Conn.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *engineConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := c.r.leaseError(); err != nil {
		return nil, err
	}
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

// Prepare is PrepareContext with no context, behind the same gate.
func (c *engineConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *engineConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.r.leaseError(); err != nil {
		return nil, err
	}
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}
