package replica

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// ErrNoPosition means the replica holds a whole database, which records no
// source position; RequestPull and WatchPosition need a scope database.
var ErrNoPosition = errors.New("tursoraft/replica: a whole-database replica records no source position")

// queryPosition reads the source position a scope database records: its
// replicaProgress row's sourceCursor, the position in the source database's
// change sequence up to which the projector has written the segment,
// including the changes the segment does not admit. It is an integer; text
// of decimal digits is read as one, and empty text is zero.
func queryPosition(ctx context.Context, db *sql.DB, digest []byte) (uint64, error) {
	var v any
	if err := db.QueryRowContext(ctx, "SELECT sourceCursor FROM replicaProgress WHERE scopeDigest = ?", digest).Scan(&v); err != nil {
		return 0, err
	}
	switch x := v.(type) {
	case int64:
		if x >= 0 {
			return uint64(x), nil
		}
	case string:
		return parsePosition(x)
	case []byte:
		return parsePosition(string(x))
	}
	return 0, fmt.Errorf("tursoraft/replica: replicaProgress.sourceCursor holds %T %v, not a position", v, v)
}

func parsePosition(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	p, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("tursoraft/replica: replicaProgress.sourceCursor %q is not a position", s)
	}
	return p, nil
}

// notePosition records the position a pull reached and passes it to every
// watcher. A position is never lowered: the projector only moves a segment's
// cursor forward.
func (r *Replica) notePosition(p uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p <= r.position {
		return
	}
	r.position = p
	for _, w := range r.watchers {
		// Each watcher's channel holds one value: a newer position replaces
		// one its receiver has not taken yet.
		select {
		case <-w:
		default:
		}
		w <- p
	}
}

// refreshPosition reads the position after a pull. A read that fails leaves
// the last position in place, so a waiting RequestPull waits on.
func (r *Replica) refreshPosition() {
	if r.readPosition == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.openTimeout())
	defer cancel()
	p, err := r.readPosition(ctx)
	if err != nil {
		r.log.Warn("tursoraft: reading the replica's source position failed", "path", r.cfg.Path, "err", err)
		return
	}
	r.notePosition(p)
}

// Position is the source position the replica recorded at its last pull;
// zero for a whole-database replica.
func (r *Replica) Position() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.position
}

// RequestPull is the pull-trigger port of one replica: it wakes the pull loop
// and returns nil once the replica has recorded atLeastPosition, at once when
// it already has. It is Sync repeated: requests that arrive while a pull runs
// share the next pull, and pulls start at least SyncGap apart, so a burst of
// requests costs one pull, and a position the primary does not hold yet is
// pulled for again until it arrives or ctx ends. It returns ctx's error when
// ctx ends first, ErrNoPosition for a whole-database replica, and the pull's
// error when the replica can pull no further (ErrClosed, ErrGrantExpired,
// ErrPlaintextReplica); any other failed pull is tried again.
func (r *Replica) RequestPull(ctx context.Context, atLeastPosition uint64) error {
	if r.readPosition == nil {
		return ErrNoPosition
	}
	for {
		if r.Position() >= atLeastPosition {
			return nil
		}
		err := r.Sync(ctx)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrClosed), errors.Is(err, ErrGrantExpired), errors.Is(err, ErrPlaintextReplica):
			return err
		}
	}
}

// WatchPosition delivers the replica's position now and then each newer
// position a pull reaches, until ctx ends or the replica closes, when the
// channel closes. A receiver that falls behind gets the newest position,
// never an older one after a newer. A whole-database replica records no
// position, and its channel is closed at once.
func (r *Replica) WatchPosition(ctx context.Context) <-chan uint64 {
	w := make(chan uint64, 1)
	if r.readPosition == nil {
		close(w)
		return w
	}
	r.mu.Lock()
	w <- r.position
	r.watchers = append(r.watchers, w)
	r.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-r.done:
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, x := range r.watchers {
			if x == w {
				r.watchers = append(r.watchers[:i], r.watchers[i+1:]...)
				break
			}
		}
		close(w)
	}()
	return w
}
