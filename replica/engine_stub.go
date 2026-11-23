//go:build !cgo

package replica

// Supported reports whether this build can open a replica. The replica
// engine is a C library, so a build without cgo cannot.
const Supported = false

func openEngine(Config, string) (native, error) { return nil, ErrReplicaNeedsCGO }

func refusedLocally(error) bool { return false }
