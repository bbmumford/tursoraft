//go:build cgo

package replica

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	libsql "github.com/bbmumford/go-libsql"
)

// unsignedToken is a JWT with claims as its payload and a signature nothing
// checks: the replica's token check reads the claims only.
func unsignedToken(claims map[string]any) string {
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	return enc.EncodeToString(h) + "." + enc.EncodeToString(c) + "." + enc.EncodeToString([]byte("unchecked"))
}

// openCatching runs Open and turns a panic into an error, so a check that is
// missing shows as a wrong error rather than a crash.
func openCatching(cfg Config) (r *Replica, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("Open panicked: %v", p)
		}
	}()
	return Open(context.Background(), cfg)
}

func baseSpec(t *testing.T) Config {
	return Config{
		Path: filepath.Join(t.TempDir(), "r.db"), URL: "http://127.0.0.1:1", Key: make([]byte, 32),
		Mode: ReadWrite, Credentials: StaticCredential("t"), OpenTimeout: time.Second,
	}
}

func TestOpenRefusesAnIncompleteSpec(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
		want error
	}{
		{"no mode", func(c *Config) { c.Mode = 0 }, ErrNoMode},
		{"unknown mode", func(c *Config) { c.Mode = 7 }, ErrNoMode},
		{"no credential source", func(c *Config) { c.Credentials = nil }, ErrNoCredential},
		{"scope without a digest", func(c *Config) { c.Scope = Scope{Database: "orbtr-r-x"} }, ErrScopeMismatch},
		{"scope with a short digest", func(c *Config) { c.Scope = Scope{Database: "orbtr-r-x", Digest: make([]byte, 16)} }, ErrScopeMismatch},
		{"scope without a database", func(c *Config) { c.Scope = Scope{Digest: make([]byte, 32)} }, ErrScopeMismatch},
	}
	for _, tc := range cases {
		cfg := baseSpec(t)
		tc.edit(&cfg)
		r, err := openCatching(cfg)
		if r != nil {
			r.Close()
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestTokenCheck(t *testing.T) {
	now := time.Now()
	in := func(d time.Duration) int64 { return now.Add(d).Unix() }
	ro := Config{Mode: ReadOnly}
	rw := Config{Mode: ReadWrite}
	scoped := Config{Mode: ReadOnly, Scope: Scope{Database: "orbtr-r-aa", Digest: make([]byte, 32)}}
	cases := []struct {
		name  string
		cfg   Config
		token string
		want  error // nil: accepted
	}{
		{"ro, exp in 10 min", ro, unsignedToken(map[string]any{"a": "ro", "exp": in(10 * time.Minute)}), nil},
		{"ro, exp at the lifetime plus skew", ro, unsignedToken(map[string]any{"a": "ro", "exp": in(GrantLifetime + tokenSkew - time.Second)}), nil},
		{"ro with ro and roa scopes", ro, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Minute), "p": map[string]any{"ro": map[string]any{"tags": []string{"x"}}, "roa": map[string]any{}}}), nil},
		{"no a: full access", ro, unsignedToken(map[string]any{"exp": in(time.Minute)}), ErrWriteCapableToken},
		{"a rw", ro, unsignedToken(map[string]any{"a": "rw", "exp": in(time.Minute)}), ErrWriteCapableToken},
		{"a roa", ro, unsignedToken(map[string]any{"a": "roa", "exp": in(time.Minute)}), ErrWriteCapableToken},
		{"p rw", ro, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Minute), "p": map[string]any{"rw": map[string]any{}}}), ErrWriteCapableToken},
		{"p rwa", ro, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Minute), "p": map[string]any{"rwa": map[string]any{}}}), ErrWriteCapableToken},
		{"p ddl", ro, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Minute), "p": map[string]any{"ddl": map[string]any{}}}), ErrWriteCapableToken},
		{"no exp", ro, unsignedToken(map[string]any{"a": "ro"}), ErrTokenOutlivesGrant},
		{"exp an hour ahead", ro, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Hour)}), ErrTokenOutlivesGrant},
		{"exp past", ro, unsignedToken(map[string]any{"a": "ro", "exp": in(-time.Second)}), ErrGrantExpired},
		{"not a JWT", ro, "plain-token", ErrWriteCapableToken},
		{"claims not base64url", ro, "a.%%%.c", ErrWriteCapableToken},
		{"read-only, no token", ro, "", ErrNoCredential},
		{"read-write, no token", rw, "", ErrNoCredential},
		{"read-write, a static token", rw, "db-token", nil},
		{"read-write, a JWT with no exp", rw, unsignedToken(map[string]any{"a": "rw"}), nil},
		{"read-write, an expired JWT", rw, unsignedToken(map[string]any{"exp": in(-10 * time.Minute)}), ErrGrantExpired},
		{"scope, id names it", scoped, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Minute), "id": "orbtr-r-aa"}), nil},
		{"scope, id names another", scoped, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Minute), "id": "orbtr-r-bb"}), ErrGrantMismatch},
		{"scope, ns names it", scoped, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Minute), "p": map[string]any{"ro": map[string]any{"ns": []string{"orbtr-r-aa"}}}}), nil},
		{"scope, ns names another", scoped, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Minute), "p": map[string]any{"ro": map[string]any{"ns": []string{"orbtr-r-aa", "orbtr-r-bb"}}}}), ErrGrantMismatch},
		{"whole database, id names one", ro, unsignedToken(map[string]any{"a": "ro", "exp": in(time.Minute), "id": "orbtr-r-bb"}), nil},
	}
	for _, tc := range cases {
		err := tc.cfg.checkToken(tc.token, "", now)
		if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

type fixedSource struct {
	c   Credential
	err error
}

func (f fixedSource) Credential(context.Context) (Credential, error) { return f.c, f.err }

func TestAdmitCredential(t *testing.T) {
	now := time.Now()
	ro := unsignedToken(map[string]any{"a": "ro", "exp": now.Add(10 * time.Minute).Unix()})
	scope := Scope{Database: "orbtr-r-aa", Digest: make([]byte, 32)}
	cases := []struct {
		name string
		cfg  Config
		cred Credential
		want error
	}{
		{"static read-write", Config{Mode: ReadWrite}, Credential{Token: "anything"}, nil},
		{"read-only grant for its scope", Config{Mode: ReadOnly, Scope: scope}, Credential{Token: ro, Database: "orbtr-r-aa", NotAfter: now.Add(time.Minute)}, nil},
		{"grant past NotAfter", Config{Mode: ReadOnly, Scope: scope}, Credential{Token: ro, Database: "orbtr-r-aa", NotAfter: now.Add(-time.Second)}, ErrGrantExpired},
		{"grant for another database", Config{Mode: ReadOnly, Scope: scope}, Credential{Token: ro, Database: "orbtr-r-bb"}, ErrGrantMismatch},
		{"grant naming no database", Config{Mode: ReadOnly, Scope: scope}, Credential{Token: ro}, ErrGrantMismatch},
		{"read-only spec, write token", Config{Mode: ReadOnly}, Credential{Token: unsignedToken(map[string]any{"exp": now.Add(time.Minute).Unix()})}, ErrWriteCapableToken},
	}
	for _, tc := range cases {
		err := tc.cfg.admit(tc.cred, now)
		if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestGrantNamesTheDatabaseByThePrimarysID holds the token check to the claims
// Turso mints: a database token's only database claim is "id", Turso's own id
// for the database (measured on minted tokens: keys a, exp, iat, id, kid, rid),
// so a grant for a scope database carries that id beside the name.
func TestGrantNamesTheDatabaseByThePrimarysID(t *testing.T) {
	now := time.Now()
	scope := Config{Mode: ReadOnly, Scope: Scope{Database: "orbtr-r-aa", Digest: make([]byte, 32)}}
	turso := func(id string) string {
		return unsignedToken(map[string]any{"a": "ro", "exp": now.Add(15 * time.Minute).Unix(), "iat": now.Unix(), "id": id, "kid": "k", "rid": "r"})
	}
	const idA, idB = "01a21312-5401-7f20-a59e-97d786c52a9d", "01a21312-6701-7dbc-ad94-8ccd1f35bd7c"
	cases := []struct {
		name string
		cred Credential
		want error
	}{
		{"id is the grant's database id", Credential{Token: turso(idA), Database: "orbtr-r-aa", DatabaseID: idA}, nil},
		{"id is another database's id", Credential{Token: turso(idB), Database: "orbtr-r-aa", DatabaseID: idA}, ErrGrantMismatch},
		{"grant carries no database id", Credential{Token: turso(idA), Database: "orbtr-r-aa"}, ErrGrantMismatch},
		{"ns names the grant's database id", Credential{Token: unsignedToken(map[string]any{"a": "ro", "exp": now.Add(time.Minute).Unix(), "p": map[string]any{"ro": map[string]any{"ns": []string{idA}}}}), Database: "orbtr-r-aa", DatabaseID: idA}, nil},
		{"ns names another id", Credential{Token: unsignedToken(map[string]any{"a": "ro", "exp": now.Add(time.Minute).Unix(), "p": map[string]any{"ro": map[string]any{"ns": []string{idA, idB}}}}), Database: "orbtr-r-aa", DatabaseID: idA}, ErrGrantMismatch},
		{"id names the scope by name, any database id", Credential{Token: turso("orbtr-r-aa"), Database: "orbtr-r-aa", DatabaseID: idB}, nil},
	}
	for _, tc := range cases {
		err := scope.admit(tc.cred, now)
		if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// fakeEngine is a native whose connections refuse or run every statement as
// told.
type fakeEngine struct {
	execErr error // what a connection's ExecContext returns
	closed  atomic.Bool
	syncs   atomic.Int64
}

func (f *fakeEngine) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{execErr: f.execErr}, nil
}
func (f *fakeEngine) Driver() driver.Driver { return nil }
func (f *fakeEngine) Close() error          { f.closed.Store(true); return nil }
func (f *fakeEngine) Sync() (uint64, error) { f.syncs.Add(1); return 1, nil }

type fakeConn struct{ execErr error }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("fake: no prepare") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("fake: no begin") }
func (c *fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(0), nil
}

func withFakeEngine(t *testing.T, f *fakeEngine) *atomic.Int64 {
	t.Helper()
	opens := &atomic.Int64{}
	saved := openNative
	openNative = func(Config, string) (native, error) {
		opens.Add(1)
		return f, nil
	}
	t.Cleanup(func() { openNative = saved })
	return opens
}

// The credential is checked before the engine opens, so a refused token never
// reaches the primary.
func TestOpenRefusesACredentialBeforeTheEngine(t *testing.T) {
	opens := withFakeEngine(t, &fakeEngine{execErr: libsql.ErrReadOnlyReplica})
	cfg := baseSpec(t)
	cfg.Mode = ReadOnly
	cfg.Credentials = StaticCredential(unsignedToken(map[string]any{"exp": time.Now().Add(time.Minute).Unix()}))
	if _, err := openCatching(cfg); !errors.Is(err, ErrWriteCapableToken) {
		t.Errorf("write-capable token: err = %v", err)
	}
	failing := errors.New("the grant host is unreachable")
	cfg.Credentials = fixedSource{err: failing}
	if _, err := openCatching(cfg); !errors.Is(err, failing) {
		t.Errorf("failing source: err = %v", err)
	}
	if n := opens.Load(); n != 0 {
		t.Errorf("the engine opened %d times for refused credentials", n)
	}
}

// A ReadOnly pool hands out only connections that refused the write probe
// with go-libsql's own refusal; an engine whose connections ran it, or failed
// it some other way, fails the open.
func TestReadOnlyPoolProvesTheGate(t *testing.T) {
	ro := StaticCredential(unsignedToken(map[string]any{"a": "ro", "exp": time.Now().Add(10 * time.Minute).Unix()}))
	for _, tc := range []struct {
		name    string
		execErr error
		ok      bool
	}{
		{"refused by the gate", fmt.Errorf("wrapped: %w", libsql.ErrReadOnlyReplica), true},
		{"ran", nil, false},
		{"failed otherwise", errors.New("table sqlite_master may not be modified"), false},
	} {
		f := &fakeEngine{execErr: tc.execErr}
		withFakeEngine(t, f)
		cfg := baseSpec(t)
		cfg.Mode, cfg.Credentials = ReadOnly, ro
		r, err := openCatching(cfg)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: err = %v, want an open replica", tc.name, err)
				continue
			}
			r.Close()
			continue
		}
		if r != nil {
			r.Close()
		}
		if !errors.Is(err, ErrReadOnlyUnproved) {
			t.Errorf("%s: err = %v, want ErrReadOnlyUnproved", tc.name, err)
		}
		if !f.closed.Load() {
			t.Errorf("%s: the engine was left open after the refused open", tc.name)
		}
	}
}

// A Config that sets only AuthToken opens ReadWrite under that static token;
// one that sets it beside Mode or Credentials is refused before the engine.
func TestAuthTokenAloneOpensReadWrite(t *testing.T) {
	var tokens []string
	saved := openNative
	openNative = func(cfg Config, token string) (native, error) {
		tokens = append(tokens, token)
		c, err := libsql.NewLocalConnector(cfg.Path)
		if err != nil {
			return nil, err
		}
		return fileEngine{Connector: c, closed: &atomic.Bool{}}, nil
	}
	t.Cleanup(func() { openNative = saved })
	r, err := openCatching(Config{Path: filepath.Join(t.TempDir(), "r.db"), URL: "http://127.0.0.1:1", AuthToken: "db-token", Key: make([]byte, 32), OpenTimeout: time.Second})
	if err != nil {
		t.Fatalf("a Config with only AuthToken: err = %v", err)
	}
	mode := r.cfg.Mode
	r.Close()
	if mode != ReadWrite || len(tokens) != 1 || tokens[0] != "db-token" {
		t.Errorf("opened with mode %v and tokens %q, want ReadWrite under db-token", mode, tokens)
	}
	for _, cfg := range []Config{
		{Path: filepath.Join(t.TempDir(), "m.db"), URL: "http://127.0.0.1:1", AuthToken: "a", Mode: ReadWrite, Key: make([]byte, 32)},
		{Path: filepath.Join(t.TempDir(), "c.db"), URL: "http://127.0.0.1:1", AuthToken: "a", Credentials: StaticCredential("b"), Key: make([]byte, 32)},
	} {
		if _, err := openCatching(cfg); !errors.Is(err, errMixedToken) {
			t.Errorf("AuthToken beside Mode or Credentials: err = %v, want errMixedToken", err)
		}
	}
	if len(tokens) != 1 {
		t.Errorf("the engine opened %d times, want once", len(tokens))
	}
}
