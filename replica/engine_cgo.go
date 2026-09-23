//go:build cgo

package replica

import (
	"encoding/base64"

	libsql "github.com/bbmumford/go-libsql"
)

// Supported reports whether this build can open a replica.
const Supported = true

// engine adapts go-libsql's connector to native.
type engine struct{ *libsql.Connector }

// Sync pulls once. go-libsql marks Connector.Sync deprecated in favour of a
// different engine; it is the embedded-replica pull this package is built on.
func (e engine) Sync() (uint64, error) {
	rep, err := e.Connector.Sync()
	if err != nil {
		return 0, err
	}
	return uint64(rep.FrameNo), nil
}

// openEngine opens the replica with the options every holder of a shared
// database uses:
//   - WithEncryption(base64(key)): the replica file and its WAL are
//     encrypted; go-libsql takes the key as a NUL-terminated string, so the
//     raw bytes are passed in base64.
//   - WithReadYourWrites(true): a write is visible to the writer's next read
//     before the next pull.
//   - WithRawText(true): TEXT that looks like a date is returned as a
//     string, as it was written.
//   - WithImmediateTransactions(true): a transaction that can write begins
//     with BEGIN IMMEDIATE on the primary, so two replicas that read then
//     write in a transaction serialise instead of losing an update.
//
// WithSyncInterval is never passed: go-libsql's own sync goroutines outlive
// Close, and the loop in this package pulls instead.
func openEngine(cfg Config) (native, error) {
	opts := []libsql.Option{
		libsql.WithEncryption(base64.StdEncoding.EncodeToString(cfg.Key)),
		libsql.WithReadYourWrites(true),
		libsql.WithRawText(true),
		libsql.WithImmediateTransactions(true),
	}
	if cfg.AuthToken != "" {
		opts = append(opts, libsql.WithAuthToken(cfg.AuthToken))
	}
	c, err := libsql.NewEmbeddedReplicaConnector(cfg.Path, cfg.URL, opts...)
	if err != nil {
		return nil, err
	}
	return engine{c}, nil
}
