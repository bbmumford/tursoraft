// Package replica keeps an encrypted embedded replica of one shared database.
//
// Reads are answered from the local replica file. A ReadWrite replica sends
// every write to the primary before the call returns, so the primary alone
// orders writes from every process holding the database; a ReadOnly replica
// refuses every write before the engine sees it (go-libsql WithReadOnly). A
// sync loop pulls the primary's newer frames into the file.
//
// Config is a replica's spec: its file, its primary, its mode, where its
// credential comes from, its key and, for a scope database, the scope it must
// hold. Every part is required and every refusal fails the open: there is no
// default mode, no fallback to an older credential, and no wider replica.
//
// The pool proves foreign keys on each physical connection when it opens, and
// refuses every statement that would turn them off, for the connection's whole
// life (policypool.ForeignKeysRequiredAtConnect). It does not prove them again
// when a connection is reused: the engine sends PRAGMA foreign_keys to the
// primary, so a proof on every reuse would cost each pooled statement a round
// trip to the primary and fail every pooled read while the primary is
// unreachable (TestPolicyProofReachesThePrimary measures both). The setting
// cannot lapse between proofs: a write runs on the primary under the
// connection's setting, a session the libSQL primary starts again begins with
// foreign keys on, and the guard refuses the one statement that turns them
// off.
//
// A physical connection opened while the primary is unreachable fails its
// proof, so reads during an outage are served by the connections proved
// before it: the pool keeps up to PoolSize of them idle and retires none for
// age.
//
// The engine is handed the key on every open, yet when the primary answers
// libSQL's HTTP sync protocol the engine opens the file without it and writes
// it unencrypted. So the file is checked after every open, and a file left
// unencrypted is closed and refused (ErrPlaintextReplica): it is never
// served.
//
// Consumed by the Manager for every database of a shared group, and by
// connectors.MakeConnector given a primary URL. The replica file is a cache:
// deleting it costs one download, which is why a plaintext file found under a
// configured key before an open is deleted rather than refused.
package replica

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bbmumford/tursoraft/database"
	"github.com/bbmumford/tursoraft/localdb"
)

// Defaults and bounds.
const (
	// DefaultSyncInterval is how often the loop pulls when Config gives none.
	DefaultSyncInterval = 5 * time.Second
	// DefaultOpenTimeout is how long Open keeps retrying an unreachable
	// primary when Config gives no bound.
	DefaultOpenTimeout = 60 * time.Second
	// SyncGap is the least time between the starts of two pulls requested
	// through Sync. A session store asks for a pull on every unknown cookie,
	// and cookies arrive faster than a pull completes; the gap bounds the
	// pulls to four a second however many requests arrive.
	SyncGap = 250 * time.Millisecond
	// maxPullBatch bounds how many waiting callers one pull answers. Callers
	// beyond it share the next pull, SyncGap later; without a bound a stream
	// of callers faster than the loop takes them holds the pull off.
	maxPullBatch = 1024
	// closeWait is how long Close waits for a pull in progress. The native
	// pull takes no context, so one against a primary that stopped answering
	// may outlast it.
	closeWait = 10 * time.Second
	// staleAfter is how many sync intervals without a successful pull make a
	// replica stale.
	staleAfter = 3
	// PoolSize is how many proved connections the pool keeps idle, and so how
	// many reads an outage serves at once: a read that needs one more opens a
	// connection whose proof needs the primary, and fails. database/sql keeps
	// two by default. Open connections are not capped, because a write holds
	// its connection for a round trip to the primary, and a cap would make the
	// process's reads wait behind PoolSize writes in flight.
	PoolSize = 8
)

// Errors.
var (
	// ErrReplicaNeedsCGO is what Open returns in a build without cgo: the
	// replica engine is a C library.
	ErrReplicaNeedsCGO = errors.New("tursoraft/replica: an embedded replica needs a cgo build")
	// ErrKeyLength means the key is not exactly 32 bytes.
	ErrKeyLength = errors.New("tursoraft/replica: key must be exactly 32 bytes")
	// ErrNoURL means Config has no primary URL.
	ErrNoURL = errors.New("tursoraft/replica: primary url is empty")
	// ErrRelativePath means the replica path is not absolute.
	ErrRelativePath = errors.New("tursoraft/replica: path must be absolute")
	// ErrClosed is what Sync returns once Close has begun.
	ErrClosed = errors.New("tursoraft/replica: closed")
	// ErrCloseTimedOut means Close returned while a pull was still inside the
	// native library. The pool closes when that pull returns.
	ErrCloseTimedOut = errors.New("tursoraft/replica: a pull in progress outlasted close; the pool closes when it returns")
	// ErrPlaintextReplica means the engine left the replica file unencrypted
	// although it was given the key, or in a state that cannot be told apart
	// from that, or that the file could not be read to check. The engine is
	// closed and nothing is served from the file.
	ErrPlaintextReplica = errors.New("tursoraft/replica: the engine left the replica file unencrypted under a configured key")
	// ErrNoMode means Config.Mode is neither ReadWrite nor ReadOnly.
	ErrNoMode = errors.New("tursoraft/replica: the spec names no mode")
	// ErrNoCredential means Config.Credentials is nil.
	ErrNoCredential = errors.New("tursoraft/replica: the spec names no credential source")
	// ErrGrantExpired means the credential's NotAfter, or its token's exp,
	// has passed.
	ErrGrantExpired = errors.New("tursoraft/replica: the grant has expired")
	// ErrGrantMismatch means the credential names another database than the
	// spec's scope.
	ErrGrantMismatch = errors.New("tursoraft/replica: the grant is for another database")
	// ErrWriteCapableToken means a ReadOnly spec was handed a token whose
	// claims do not limit it to reads.
	ErrWriteCapableToken = errors.New("tursoraft/replica: a read-only replica was handed a token that can write")
	// ErrTokenOutlivesGrant means a ReadOnly spec was handed a token with no
	// exp, or one whose exp lies further ahead than one grant lifetime.
	ErrTokenOutlivesGrant = errors.New("tursoraft/replica: a read-only replica's token outlives a grant")
	// ErrScopeMismatch means the scope database's replicaScope row is not
	// the spec's scope, or the spec's scope is incomplete.
	ErrScopeMismatch = errors.New("tursoraft/replica: the scope database does not hold the spec's scope")
	// ErrReadOnlyUnproved means a ReadOnly replica's connection did not refuse
	// the write probe itself, so its engine was not opened read-only.
	ErrReadOnlyUnproved = errors.New("tursoraft/replica: a read-only connection did not refuse a write locally")
)

// Mode is whether a replica may send writes to its primary. The zero value
// is no mode, and Open refuses it.
type Mode uint8

const (
	// ReadWrite sends every write to the primary: a host's replica of a
	// database it writes.
	ReadWrite Mode = iota + 1
	// ReadOnly refuses every write before the engine sees it, pulls with a
	// read-only token and proves both at open: a device's replica of its
	// scope databases.
	ReadOnly
)

// GrantLifetime is the longest a read-only replica's token may run: a
// device's sync grant lasts 15 minutes. tokenSkew is the clock difference
// between the minting host and this one that the token check allows.
const (
	GrantLifetime = 15 * time.Minute
	tokenSkew     = time.Minute
)

// Credential is what a replica authenticates its pulls with.
type Credential struct {
	// Token is the primary's bearer token. Empty sends none, which only a
	// primary with authentication off accepts.
	Token string
	// NotAfter is when the grant ends; zero for a credential that does not
	// expire, such as a host's static token.
	NotAfter time.Time
	// LeaseNotAfter is the end of the offline lease that came with the
	// grant; zero when none came.
	LeaseNotAfter time.Time
	// GrantID names the grant; empty for a static credential.
	GrantID string
	// Database is the database the grant names; empty for a static
	// credential. A spec with a Scope requires it to equal Scope.Database.
	Database string
	// Epoch is the segment's epoch the grant was issued for; zero for a
	// static credential. The replica carries it; its holder compares it with
	// the epoch its file records.
	Epoch uint64
}

// CredentialSource yields the credential a replica opens with. Open calls it
// once; the replica holds what it returned.
type CredentialSource interface {
	Credential(ctx context.Context) (Credential, error)
}

// StaticCredential is the source of a token that does not expire: a host's
// per-database token released from its vault.
func StaticCredential(token string) CredentialSource { return staticCredential(token) }

type staticCredential string

func (s staticCredential) Credential(context.Context) (Credential, error) {
	return Credential{Token: string(s)}, nil
}

// Scope is the scope database a replica must hold: its name and the digest
// its replicaScope row carries. The zero Scope is a whole database.
type Scope struct {
	Database string
	Digest   []byte
}

func (s Scope) isZero() bool { return s.Database == "" && s.Digest == nil }

// Config describes one replica.
type Config struct {
	// Path is the replica file, absolute.
	Path string
	// URL is the primary, for example libsql://<db>-<org>.turso.io.
	URL string
	// Mode is ReadWrite or ReadOnly; there is no default.
	Mode Mode
	// Credentials yields the token the replica pulls with; required.
	Credentials CredentialSource
	// AuthToken is a static token for a ReadWrite replica, accepted for a
	// caller that names neither Mode nor Credentials: such a Config opens
	// ReadWrite under StaticCredential(AuthToken). Setting it beside Mode or
	// Credentials is refused.
	//
	// Deprecated: set Mode: ReadWrite and Credentials: StaticCredential(token).
	AuthToken string
	// Scope is the scope database the replica must hold; zero for a whole
	// database.
	Scope Scope
	// Key encrypts the replica file and its WAL; exactly 32 bytes.
	Key []byte
	// SyncInterval is the pull period; zero means DefaultSyncInterval.
	SyncInterval time.Duration
	// OpenTimeout bounds Open's retries; zero means DefaultOpenTimeout.
	OpenTimeout time.Duration
	// Logger receives plaintext deletions, retries and sync failures; nil
	// means slog.Default.
	Logger *slog.Logger
}

// Stats is one replica's sync state.
type Stats struct {
	// Frame is the replica's frame number after the last successful pull.
	Frame uint64
	// LastPull is the time of the last successful pull, including the one
	// Open made.
	LastPull time.Time
	// Stale means no pull has succeeded for three sync intervals.
	Stale bool
	// Err is the last pull's error, nil when it succeeded.
	Err error
}

// SyncStats maps s onto the shape database.SyncStats keeps.
func (s Stats) SyncStats() database.SyncStats {
	var pulled int64
	if !s.LastPull.IsZero() {
		pulled = s.LastPull.Unix()
	}
	return database.SyncStats{LastPullUnixTime: pulled, Revision: strconv.FormatUint(s.Frame, 10)}
}

// native is the replica engine's connector as this package drives it.
type native interface {
	driver.Connector
	Close() error
	// Sync pulls from the primary and returns the frame number reached. It
	// takes no context and may block for as long as the primary does.
	Sync() (uint64, error)
}

// openNative opens the engine's connector. The cgo build sets it; tests
// replace it.
var openNative = openEngine

// Replica is one open replica.
type Replica struct {
	cfg      Config
	log      *slog.Logger
	interval time.Duration
	native   native
	db       *sql.DB
	// readPosition reads the source position the scope database records;
	// nil for a whole database.
	readPosition func(context.Context) (uint64, error)

	reqs chan chan error
	stop chan struct{}
	done chan struct{}

	closeOnce sync.Once
	closeErr  error

	mu       sync.Mutex
	cred     Credential
	position uint64
	watchers []chan uint64
	frame    uint64
	lastPull time.Time
	lastErr  error
}

// Open validates cfg, takes and checks its credential, deletes a plaintext
// replica file, opens the replica, retrying an unreachable primary until the
// open timeout, wraps it in a pool, checks a scope database's replicaScope
// row against cfg.Scope, and starts the sync loop.
//
// A ReadWrite pool proves foreign keys on every connection; a ReadOnly pool
// proves on every connection that the engine refuses a write itself.
//
// There is no fallback: a primary that stays unreachable fails Open, because
// a server that served from a replica it could not open would answer from
// nothing.
func Open(ctx context.Context, cfg Config) (*Replica, error) {
	if !Supported {
		return nil, ErrReplicaNeedsCGO
	}
	cfg, err := cfg.withStaticToken()
	if err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	cred, err := cfg.Credentials.Credential(ctx)
	if err != nil {
		return nil, fmt.Errorf("tursoraft/replica: credential for %s: %w", cfg.Path, err)
	}
	if err := cfg.admit(cred, time.Now()); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, fmt.Errorf("tursoraft/replica: %w", err)
	}
	if err := removePlaintext(cfg.Path, log); err != nil {
		return nil, err
	}
	n, err := openRetrying(ctx, cfg, cred.Token, log)
	if err != nil {
		return nil, err
	}
	if err := refuseUnencrypted(cfg.Path, n, log); err != nil {
		return nil, err
	}
	db, err := openPool(ctx, cfg.Mode, n)
	if err != nil {
		n.Close()
		return nil, fmt.Errorf("tursoraft/replica: pool for %s: %w", cfg.Path, err)
	}
	db.SetMaxIdleConns(PoolSize)
	if err := checkScope(ctx, db, cfg.Scope); err != nil {
		db.Close()
		return nil, err
	}
	r := &Replica{
		cfg: cfg, log: log, interval: cfg.syncInterval(), native: n, db: db, cred: cred,
		reqs: make(chan chan error),
		stop: make(chan struct{}),
		done: make(chan struct{}),
		// The engine pulled once inside the open.
		lastPull: time.Now(),
	}
	if !cfg.Scope.isZero() {
		r.readPosition = func(ctx context.Context) (uint64, error) { return queryPosition(ctx, db, cfg.Scope.Digest) }
		p, err := r.readPosition(ctx)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("%w: %s records no source position: %v", ErrScopeMismatch, cfg.Scope.Database, err)
		}
		r.position = p
	}
	go r.loop()
	return r, nil
}

// withStaticToken turns a Config that sets only AuthToken into the spec it
// stands for: Mode ReadWrite and StaticCredential(AuthToken).
func (c Config) withStaticToken() (Config, error) {
	if c.AuthToken == "" {
		return c, nil
	}
	if c.Mode != 0 || c.Credentials != nil {
		return c, errMixedToken
	}
	c.Mode, c.Credentials = ReadWrite, StaticCredential(c.AuthToken)
	return c, nil
}

// errMixedToken refuses a Config that sets AuthToken beside Mode or
// Credentials, which would leave two answers to which token it pulls with.
var errMixedToken = errors.New("tursoraft/replica: AuthToken is set beside Mode or Credentials")

func (c Config) validate() error {
	if c.Path == "" || !filepath.IsAbs(c.Path) {
		return fmt.Errorf("%w: %q", ErrRelativePath, c.Path)
	}
	if c.URL == "" {
		return ErrNoURL
	}
	if len(c.Key) != 32 {
		return ErrKeyLength
	}
	if c.Mode != ReadWrite && c.Mode != ReadOnly {
		return ErrNoMode
	}
	if c.Credentials == nil {
		return ErrNoCredential
	}
	if !c.Scope.isZero() && (c.Scope.Database == "" || len(c.Scope.Digest) != sha256.Size) {
		return fmt.Errorf("%w: a scope names its database and a %d-byte digest", ErrScopeMismatch, sha256.Size)
	}
	return nil
}

// admit checks a credential against the spec at now, before the engine is
// reached: an expired grant, a grant for another database than the scope's,
// and its token (checkToken).
func (c Config) admit(cred Credential, now time.Time) error {
	if !cred.NotAfter.IsZero() && !now.Before(cred.NotAfter) {
		return fmt.Errorf("%w: its NotAfter %s has passed", ErrGrantExpired, cred.NotAfter.UTC().Format(time.RFC3339))
	}
	if !c.Scope.isZero() && cred.Database != c.Scope.Database {
		return fmt.Errorf("%w: the grant names %q, the spec %q", ErrGrantMismatch, cred.Database, c.Scope.Database)
	}
	return c.checkToken(cred.Token, now)
}

func (r *Replica) openTimeout() time.Duration {
	if r.cfg.OpenTimeout > 0 {
		return r.cfg.OpenTimeout
	}
	return DefaultOpenTimeout
}

func (c Config) syncInterval() time.Duration {
	if c.SyncInterval > 0 {
		return c.SyncInterval
	}
	return DefaultSyncInterval
}

// replicaFiles are the suffixes of every file the engine keeps beside a
// replica: the database, its WAL and shared-memory index, and the replication
// state the engine records for the pull.
var replicaFiles = []string{"", "-wal", "-shm", "-client_wal_index", "-info"}

// removePlaintext deletes the replica and its companion files when the
// database file begins with the plaintext SQLite header. The engine would
// otherwise open it unencrypted, or fail on it, under a configured key.
func removePlaintext(path string, log *slog.Logger) error {
	plain, err := localdb.IsPlaintext(path)
	if err != nil {
		return fmt.Errorf("tursoraft/replica: check %s: %w", path, err)
	}
	if !plain {
		return nil
	}
	for _, suffix := range replicaFiles {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("tursoraft/replica: remove plaintext replica: %w", err)
		}
	}
	log.Warn("tursoraft: deleted a plaintext replica found under a configured key; it downloads again", "path", path)
	return nil
}

// sqliteHeader begins every unencrypted SQLite database file.
var sqliteHeader = []byte("SQLite format 3\x00")

// infoSuffix names the companion file libSQL's HTTP sync protocol keeps
// beside a replica; the frame-replication protocol never writes it.
const infoSuffix = "-info"

// refuseUnencrypted checks the replica file the engine has just opened and
// pulled into, and closes the engine and refuses when the file is to be taken
// as unencrypted (unencrypted). The file is left where it is: the next Open's
// removePlaintext deletes one that begins with the unencrypted header before
// the engine opens it.
func refuseUnencrypted(path string, n native, log *slog.Logger) error {
	why, err := unencrypted(path)
	if err == nil && why == "" {
		return nil
	}
	n.Close()
	if err != nil {
		why = "it cannot be checked: " + err.Error()
	}
	log.Error("tursoraft: refused a replica the engine left unencrypted; it is not served", "path", path, "reason", why)
	return fmt.Errorf("%w: %s: %s", ErrPlaintextReplica, path, why)
}

// unencrypted reports why the file at path is to be taken as unencrypted, or
// "" when it is not. A file is unencrypted when it begins with the SQLite
// header, and is taken as unencrypted when the HTTP sync protocol's companion
// file sits beside a file too short to hold a header to judge: that protocol
// keeps no local key, and its pages may so far be in the WAL only. An
// encrypted file has no header at all, so a file at least a header long that
// does not begin with it is kept, whichever protocol wrote it.
func unencrypted(path string) (string, error) {
	head := make([]byte, len(sqliteHeader))
	read := 0
	f, err := os.Open(path)
	switch {
	case err == nil:
		read, err = io.ReadFull(f, head)
		f.Close()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return "", err
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	if read == len(head) && bytes.Equal(head, sqliteHeader) {
		return "the file begins with the unencrypted SQLite header", nil
	}
	_, err = os.Stat(path + infoSuffix)
	switch {
	case err == nil && read < len(head):
		return "the HTTP sync protocol's " + infoSuffix + " file sits beside a file with no header to judge", nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	return "", nil
}

// openRetrying opens the engine's connector until it succeeds, fails with an
// error no retry can change, or the open timeout passes.
func openRetrying(ctx context.Context, cfg Config, token string, log *slog.Logger) (native, error) {
	timeout := cfg.OpenTimeout
	if timeout <= 0 {
		timeout = DefaultOpenTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	backoff := 250 * time.Millisecond
	for attempt := 1; ; attempt++ {
		n, err := openDetached(ctx, cfg, token)
		if err == nil {
			return n, nil
		}
		if permanent(err) {
			return nil, fmt.Errorf("tursoraft/replica: open %s: %w", cfg.Path, err)
		}
		log.Warn("tursoraft: replica open failed; retrying", "path", cfg.Path, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("tursoraft/replica: open %s: primary unreachable for %v: %w", cfg.Path, timeout, err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

// openDetached runs the native open, which takes no context and blocks on the
// first pull, and stops waiting when ctx ends. An open that completes after
// that is closed as soon as it returns.
func openDetached(ctx context.Context, cfg Config, token string) (native, error) {
	type result struct {
		n   native
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := openNative(cfg, token)
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-ctx.Done():
		go func() {
			if res := <-ch; res.n != nil {
				res.n.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// permanentText are the engine's messages for failures a retry cannot change:
// a file under another key, and a refused credential.
var permanentText = []string{
	"not a database",
	"decrypt",
	"unauthorized",
	"status=401",
	"permissiondenied",
	"not authorized",
}

func permanent(err error) bool {
	text := strings.ToLower(err.Error())
	for _, p := range permanentText {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

// DB returns the replica's pool. On a ReadWrite replica every connection has
// foreign keys on and opens write transactions with BEGIN IMMEDIATE; on a
// ReadOnly replica every connection refuses writes and BeginTx needs
// sql.TxOptions{ReadOnly: true}.
func (r *Replica) DB() *sql.DB { return r.db }

// Stats reports the replica's sync state.
func (r *Replica) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{
		Frame:    r.frame,
		LastPull: r.lastPull,
		Stale:    time.Since(r.lastPull) > staleAfter*r.interval,
		Err:      r.lastErr,
	}
}

// SyncStats is Stats in the shape database.SyncStats keeps.
func (r *Replica) SyncStats() database.SyncStats { return r.Stats().SyncStats() }

// Sync pulls from the primary and returns when a pull that started after this
// call has finished, or ctx ends.
//
// Every pull runs on the sync loop, one at a time: callers waiting together
// share one pull, including every caller that arrived while the previous pull
// ran, and a pull starts no sooner than SyncGap after the last one started. A
// caller whose ctx ends stops waiting; the pull still completes.
func (r *Replica) Sync(ctx context.Context) error {
	w := make(chan error, 1)
	select {
	case r.reqs <- w:
	case <-r.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-w:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// loop runs every pull: one per sync interval, jittered, and one for each
// batch of Sync callers. It is the only goroutine that calls the native pull,
// so no pull runs concurrently with another or with the connector's close.
func (r *Replica) loop() {
	defer close(r.done)
	next := time.NewTimer(0)
	defer next.Stop()
	var gap <-chan time.Time
	var waiting []chan error
	var lastStart time.Time
	for {
		due := false
		select {
		case <-r.stop:
			for _, w := range waiting {
				w <- ErrClosed
			}
			return
		case w := <-r.reqs:
			waiting = append(waiting, w)
		case <-next.C:
			due = true
		case <-gap:
			gap = nil
		}
		if !due && len(waiting) > 0 {
			if wait := SyncGap - time.Since(lastStart); wait > 0 {
				if gap == nil {
					gap = time.After(wait)
				}
				continue
			}
			due = true
		}
		if !due {
			continue
		}
		// Callers that arrived while the last pull ran are each blocked on
		// their send, and the gap since that pull started has passed, so taking
		// one at a time would give each its own pull, one after another. Those
		// ready now share this pull instead, at most maxPullBatch of them, so
		// callers arriving faster than they are taken cannot hold the pull off;
		// the rest share the next one. Close is read while they are taken, so
		// it is not held behind them.
	take:
		for len(waiting) < maxPullBatch {
			select {
			case <-r.stop:
				for _, w := range waiting {
					w <- ErrClosed
				}
				return
			case w := <-r.reqs:
				waiting = append(waiting, w)
			default:
				break take
			}
		}
		batch := waiting
		waiting = nil
		lastStart = time.Now()
		err := r.pull()
		for _, w := range batch {
			w <- err
		}
		next.Reset(r.jittered())
	}
}

// jittered spreads the pulls of every process holding the database across
// ±20% of the interval.
func (r *Replica) jittered() time.Duration {
	span := r.interval / 5
	return r.interval - span + time.Duration(rand.Int64N(int64(2*span)+1))
}

func (r *Replica) pull() error {
	frame, err := r.native.Sync()
	r.mu.Lock()
	failing := r.lastErr != nil
	if err == nil {
		r.frame, r.lastPull, r.lastErr = frame, time.Now(), nil
	} else {
		r.lastErr = err
	}
	r.mu.Unlock()
	switch {
	case err != nil && !failing:
		r.log.Warn("tursoraft: replica pull failed; writes fail until the primary answers", "path", r.cfg.Path, "err", err)
	case err == nil && failing:
		r.log.Info("tursoraft: replica pull recovered", "path", r.cfg.Path, "frame", frame)
	}
	if err == nil {
		r.refreshPosition()
	}
	return err
}

// Close stops the sync loop and then closes the pool, which closes the
// connector. The order matters: closing the connector while a pull runs on it
// frees memory the pull is using.
func (r *Replica) Close() error {
	r.closeOnce.Do(func() {
		close(r.stop)
		select {
		case <-r.done:
			r.closeErr = r.db.Close()
		case <-time.After(closeWait):
			go func() {
				<-r.done
				r.db.Close()
			}()
			r.closeErr = ErrCloseTimedOut
		}
	})
	return r.closeErr
}
