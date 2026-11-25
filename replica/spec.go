package replica

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bbmumford/filekind"
	"github.com/bbmumford/policypool"
)

// tokenClaims are the claims of a Turso token (an EdDSA JWT) the replica
// reads: "a" (ro, rw or roa), "p" (scopes, each naming its namespaces in
// "ns"), "id" (a namespace) and "exp".
type tokenClaims struct {
	A   *string                    `json:"a"`
	P   map[string]json.RawMessage `json:"p"`
	ID  *string                    `json:"id"`
	Exp *json.Number               `json:"exp"`
}

// decodeClaims reads a token's claims without checking its signature: the
// holder has no key to check it with, and the primary checks it on every
// request. ok is false for a token that is not a JWT with a JSON payload.
func decodeClaims(token string) (claims tokenClaims, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, false
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return claims, false
	}
	return claims, true
}

// checkToken refuses, before the engine is reached, a token the replica must
// not open under. The C library opens a replica under no token or an expired
// one and fails only at the first pull, as a handshake timeout Open would
// retry until its timeout, so these are refused here: a missing token
// (ErrNoCredential), and a JWT whose exp has passed (ErrGrantExpired). A
// ReadOnly spec accepts only a JWT whose claims limit it to reads for at most
// one grant lifetime: "a" is "ro"; "p", when present, holds only the "ro" and
// "roa" scopes (ErrWriteCapableToken); "exp" is present and no further ahead
// than GrantLifetime plus tokenSkew (ErrTokenOutlivesGrant); and for a scope
// database, every database its "id" and its scopes' "ns" lists name is that
// database, named by Scope.Database or by the grant's databaseID
// (ErrGrantMismatch). sqld's tokens name a namespace by name; Turso's minted
// database tokens name the database only by Turso's id for it, which the
// holder cannot derive from the scope, so the grant carries it.
//
// The read-only checks are the third of a read-only replica's refusals of a
// write, beside the statement gate and the primary's own refusal of a write
// under an "ro" token; a bounded exp means a leaked token stops pulling within
// one grant lifetime; and the namespace check stands because the primary's
// replication path accepts a token signed by its key for any namespace it
// serves, whichever namespace the token names.
func (c Config) checkToken(token, databaseID string, now time.Time) error {
	if token == "" {
		return fmt.Errorf("%w: the credential carries no token", ErrNoCredential)
	}
	claims, jwt := decodeClaims(token)
	if jwt && claims.Exp != nil {
		if exp, err := claims.Exp.Float64(); err == nil && !now.Before(time.Unix(int64(exp), 0)) {
			return fmt.Errorf("%w: the token's exp %s has passed", ErrGrantExpired, time.Unix(int64(exp), 0).UTC().Format(time.RFC3339))
		}
	}
	if c.Mode != ReadOnly {
		return nil
	}
	if !jwt {
		return fmt.Errorf("%w: the token is not a JWT whose claims can be read", ErrWriteCapableToken)
	}
	if claims.A == nil || *claims.A != "ro" {
		return fmt.Errorf("%w: its \"a\" claim is not \"ro\"", ErrWriteCapableToken)
	}
	for scope := range claims.P {
		if scope != "ro" && scope != "roa" {
			return fmt.Errorf("%w: its \"p\" claim names the %q scope", ErrWriteCapableToken, scope)
		}
	}
	if claims.Exp == nil {
		return fmt.Errorf("%w: it has no exp", ErrTokenOutlivesGrant)
	}
	exp, err := claims.Exp.Float64()
	if err != nil {
		return fmt.Errorf("%w: its exp is not a number", ErrTokenOutlivesGrant)
	}
	if expires := time.Unix(int64(exp), 0); expires.After(now.Add(GrantLifetime + tokenSkew)) {
		return fmt.Errorf("%w: its exp is %v ahead, more than %v", ErrTokenOutlivesGrant, expires.Sub(now).Round(time.Second), GrantLifetime+tokenSkew)
	}
	if c.Scope.isZero() {
		return nil
	}
	names := func(db string) bool { return db == c.Scope.Database || databaseID != "" && db == databaseID }
	if claims.ID != nil && !names(*claims.ID) {
		return fmt.Errorf("%w: the token's id claim names %q, the spec %q and the grant's database id %q", ErrGrantMismatch, *claims.ID, c.Scope.Database, databaseID)
	}
	for scope, raw := range claims.P {
		var body struct {
			NS []string `json:"ns"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("%w: its %q scope does not decode", ErrWriteCapableToken, scope)
		}
		for _, ns := range body.NS {
			if !names(ns) {
				return fmt.Errorf("%w: the token's %q scope names %q, the spec %q and the grant's database id %q", ErrGrantMismatch, scope, ns, c.Scope.Database, databaseID)
			}
		}
	}
	return nil
}

// scopeIdentity is the file identity every scope database carries in its
// header: filekind's scopeReplica kind at the scope database format this
// package reads. The primary is stamped once, when the scope database is
// created, and a replica's page 1 is its primary's, so a replica is checked
// against it and never stamped (CheckOnly): an unstamped replica means its
// primary was never stamped, and a stamp on the copy alone would be taken away
// by the primary's next page 1.
var scopeIdentity = policypool.Identity{Kind: filekind.ScopeReplica, Generation: scopeGeneration, CheckOnly: true}

// scopeGeneration is the scope database format generation this package reads:
// filekind's registry row for scopeReplica (TestScopeIdentityIsTheRegistrysRow
// holds the two together).
const scopeGeneration = 1

// openPool wraps the engine's connector in the pool a spec needs, proves its
// first connection and, for a scope database, checks the file identity.
// ReadWrite proves foreign keys on every connection
// (policypool.ForeignKeysRequiredAtConnect), through OpenFileConnector with
// scopeIdentity for a scope database, so its connections also refuse every
// header assignment. ReadOnly proves on every connection that the engine
// refuses a write itself, which a pool of connections that never write needs
// instead: the foreign-key proof's PRAGMA foreign_keys is a statement libSQL
// sends to the primary, so the read-only gate refuses it, and policypool checks
// that pool's file with CheckIdentity. A refused identity is
// policypool.ErrFileIdentity, with the pool closed.
func openPool(ctx context.Context, cfg Config, base driver.Connector) (*sql.DB, error) {
	if cfg.Mode == ReadWrite {
		if cfg.Scope.isZero() {
			return policypool.OpenConnector(ctx, base, policypool.ForeignKeysRequiredAtConnect)
		}
		return policypool.OpenFileConnector(ctx, base, policypool.ForeignKeysRequiredAtConnect, scopeIdentity)
	}
	db := sql.OpenDB(readOnlyConnector{base})
	c, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	c.Close()
	if !cfg.Scope.isZero() {
		if err := policypool.CheckIdentity(ctx, db, scopeIdentity); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// readOnlyProbe is the write each read-only connection must refuse locally.
// libSQL's replica connection classes it a write and sends it to the
// primary, and SQLite refuses to modify sqlite_schema on any database, so a
// connection whose gate was missing changes nothing on the primary and fails
// the proof with an error that is not the gate's.
const readOnlyProbe = "DELETE FROM sqlite_schema WHERE 0"

// readOnlyConnector is a ReadOnly replica's pool connector: every connection
// it hands to database/sql has refused readOnlyProbe with the engine's own
// read-only refusal (refusedLocally).
type readOnlyConnector struct{ base driver.Connector }

func (c readOnlyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("%w: the engine's connection runs no statements", ErrReadOnlyUnproved)
	}
	if _, err := execer.ExecContext(ctx, readOnlyProbe, nil); !refusedLocally(err) {
		conn.Close()
		if err == nil {
			return nil, fmt.Errorf("%w: the probe ran", ErrReadOnlyUnproved)
		}
		return nil, fmt.Errorf("%w: %v", ErrReadOnlyUnproved, err)
	}
	return conn, nil
}

func (c readOnlyConnector) Driver() driver.Driver { return c.base.Driver() }

// Close closes the engine when the pool closes, as policypool's connector
// does for a ReadWrite pool.
func (c readOnlyConnector) Close() error {
	if closer, ok := c.base.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// checkScope reads a scope database's replicaScope row and requires exactly
// one, carrying scope.Digest. A whole-database spec (the zero Scope) checks
// nothing.
func checkScope(ctx context.Context, db *sql.DB, scope Scope) error {
	if scope.isZero() {
		return nil
	}
	rows, err := db.QueryContext(ctx, "SELECT scopeDigest FROM replicaScope")
	if err != nil {
		return fmt.Errorf("%w: read replicaScope of %s: %v", ErrScopeMismatch, scope.Database, err)
	}
	defer rows.Close()
	var digests [][]byte
	for rows.Next() {
		var d []byte
		if err := rows.Scan(&d); err != nil {
			return fmt.Errorf("%w: read replicaScope of %s: %v", ErrScopeMismatch, scope.Database, err)
		}
		digests = append(digests, d)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: read replicaScope of %s: %v", ErrScopeMismatch, scope.Database, err)
	}
	if len(digests) != 1 {
		return fmt.Errorf("%w: %s holds %d replicaScope rows, want 1", ErrScopeMismatch, scope.Database, len(digests))
	}
	if !bytes.Equal(digests[0], scope.Digest) {
		return fmt.Errorf("%w: %s holds another scope's digest", ErrScopeMismatch, scope.Database)
	}
	return nil
}
