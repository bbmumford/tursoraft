// Package fence orders the writes of a singleton job across processes.
//
// A Raft cluster assigns each job to one process and stamps the assignment
// with an epoch: the log index that made it. For about one election timeout
// after a failover the old holder may still believe it holds the job, so the
// assignment alone cannot keep two processes from writing. Every write a job
// makes therefore runs in Tx, which first records the job's epoch in the
// fence row of the same database, in the same transaction, and refuses when a
// newer epoch is already recorded. The primary serialises the transactions,
// so a stale holder's write fails instead of interleaving with the new
// holder's.
package fence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors Tx returns instead of running fn.
var (
	// ErrFenced means a newer epoch of the job has written: this holder lost
	// the job and must stop.
	ErrFenced = errors.New("tursoraft/fence: a newer holder of this job has written")
	// ErrForeignCluster means the fence row was written by another Raft
	// cluster for this database — the cluster was re-formed from empty state.
	// An operator adopts the new cluster with tursoraft-admin fence-adopt once
	// the old one is known to be gone.
	ErrForeignCluster = errors.New("tursoraft/fence: the fence row belongs to another cluster")
	// ErrInvalid means the fence is missing its cluster, job or epoch.
	ErrInvalid = errors.New("tursoraft/fence: fence has no cluster, job or epoch")
)

// Table is the fence table, one row per job.
const Table = "_tursoraft_fence"

// CreateTableSQL creates the fence table when it is absent.
const CreateTableSQL = `CREATE TABLE IF NOT EXISTS ` + Table + ` (
	job        TEXT PRIMARY KEY,
	cluster_id TEXT NOT NULL,
	epoch      INTEGER NOT NULL,
	node       TEXT NOT NULL,
	updated_at INTEGER NOT NULL
)`

// upsertSQL records the epoch when the row is absent, or when it belongs to
// the same cluster and holds an epoch no newer than this one. An equal epoch
// is accepted because the same holder writes many times under one epoch.
const upsertSQL = `INSERT INTO ` + Table + ` (job, cluster_id, epoch, node, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(job) DO UPDATE SET epoch = excluded.epoch, node = excluded.node, updated_at = excluded.updated_at
WHERE ` + Table + `.cluster_id = excluded.cluster_id AND excluded.epoch >= ` + Table + `.epoch`

// AdoptSQL moves every fence row to a new cluster ID and resets its epoch, so
// the new cluster's first assignments pass. tursoraft-admin fence-adopt runs
// it with the new cluster ID as its one argument.
const AdoptSQL = `UPDATE ` + Table + ` SET cluster_id = ?, epoch = 0`

// Fence is one holder's right to write for a job.
type Fence struct {
	// Cluster is the Raft cluster ID that made the assignment.
	Cluster string
	// Job is the job name, the fence row's key.
	Job string
	// Epoch is the log index of the assignment.
	Epoch uint64
	// Node is the holder's NodeID, recorded for operators.
	Node string
}

// Tx runs fn in one transaction on db after recording f in the fence row.
//
// db must begin its transactions with BEGIN IMMEDIATE, so that the primary
// grants the write lock before the fence row is read; tursoraft's replica
// pools do. The transaction is committed when fn returns nil and rolled back
// otherwise.
//
// Keep fn short: the primary rolls back a write transaction held open for
// about five seconds, and every other writer to the database waits for it.
func (f Fence) Tx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	if f.Cluster == "" || f.Job == "" || f.Epoch == 0 {
		return ErrInvalid
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f.claim(ctx, tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// claim records f's epoch, creating the fence table the first time a
// database's job writes. The table is created inside the same transaction
// rather than once per pool, because a transaction that rolls back takes its
// CREATE TABLE with it.
func (f Fence) claim(ctx context.Context, tx *sql.Tx) error {
	args := []any{f.Job, f.Cluster, int64(f.Epoch), f.Node, time.Now().UnixMilli()}
	res, err := tx.ExecContext(ctx, upsertSQL, args...)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		if _, err := tx.ExecContext(ctx, CreateTableSQL); err != nil {
			return fmt.Errorf("tursoraft/fence: create %s: %w", Table, err)
		}
		res, err = tx.ExecContext(ctx, upsertSQL, args...)
	}
	if err != nil {
		return fmt.Errorf("tursoraft/fence: record epoch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("tursoraft/fence: record epoch: %w", err)
	}
	if n == 1 {
		return nil
	}
	var cluster string
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT cluster_id, epoch FROM `+Table+` WHERE job = ?`, f.Job).Scan(&cluster, &epoch); err != nil {
		return fmt.Errorf("tursoraft/fence: read fence row: %w", err)
	}
	if cluster != f.Cluster {
		return fmt.Errorf("%w: job %s is fenced by cluster %s, this holder is in %s", ErrForeignCluster, f.Job, cluster, f.Cluster)
	}
	return fmt.Errorf("%w: job %s is at epoch %d, this holder has %d", ErrFenced, f.Job, epoch, f.Epoch)
}
