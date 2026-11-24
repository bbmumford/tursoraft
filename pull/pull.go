// Package pull is the pull-trigger port: the one call a subscription makes
// when it learns of a change, to bring the device's replica of a segment up
// to that change.
//
// A segment is named by its scope database's name, and a change by its
// source position: the increasing position the projector records in a
// segment's replicaProgress row as it passes each change of the source
// database, whether or not the segment admits it. So one position serves
// every member of a shared Space, and a wait ends for a segment the change
// never enters. No libSQL type, token or URL crosses the port.
//
// replica.Set implements Trigger over a device's open replicas. The package
// imports nothing beyond the standard library, so a consumer can hold a
// Trigger, and test against a fake one, without linking the replica engine.
package pull

import (
	"context"
	"errors"
)

// ErrUnknownSegment means no replica of the named segment is held: the
// caller's grants no longer name it, or name it under a newer database.
var ErrUnknownSegment = errors.New("pull: no replica of that segment is held")

// Trigger brings segment replicas up to a source position.
type Trigger interface {
	// RequestPull wakes the pull of the replica of segment and returns nil
	// once that replica has recorded atLeastPosition; ctx's error when ctx
	// ends first; ErrUnknownSegment when no replica of segment is held; and
	// the replica's own error when it can pull no further (it is closed, its
	// grant has ended). Concurrent requests share pulls.
	RequestPull(ctx context.Context, segment string, atLeastPosition uint64) error
}
