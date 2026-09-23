// Package mesh is the transport contract tursoraft's Raft clusters run over.
//
// tursoraft defines it and stays free of any mesh implementation; a mesh
// runtime (loom in production, meshtest in tests) implements it. Everything a
// cluster needs from the network is here: who this process is, which peers
// publish under a cluster key, and authenticated streams to them.
package mesh

import (
	"context"
	"net"
)

// Member is one process visible on the mesh under a key.
type Member struct {
	// NodeID is the member's mesh identity, authenticated by the mesh.
	NodeID string
	// Service is the endpoint service the member runs (for example
	// "node.orbtr.ai"), authenticated by the mesh. Voter eligibility is decided
	// from this field, never from Meta.
	Service string
	// Meta is what the member last published under the key. It is the member's
	// own claim about its Raft state.
	Meta map[string]string
}

// Conn is one bidirectional stream to a peer.
//
// RemoteID must be the NodeID the transport authenticated for the far end,
// not a value the peer sent inside the stream: the member proof binds both
// NodeIDs, and a peer that could choose its own RemoteID could replay a proof
// made for another pair.
//
// Deadlines set with SetDeadline, SetReadDeadline and SetWriteDeadline must
// be honoured. The cluster bounds its handshake and every Raft RPC with them,
// and a stream that ignores a write deadline hangs the Raft goroutine writing
// to a stalled peer.
type Conn interface {
	net.Conn
	RemoteID() string
}

// Listener yields the streams peers open to this process under one key.
type Listener interface {
	Accept() (Conn, error)
	Close() error
	Addr() net.Addr
}

// Mesh is the network as a cluster sees it.
type Mesh interface {
	// LocalID is this process's NodeID.
	LocalID() string
	// LocalService is the service this process runs, as peers see it in
	// Member.Service.
	LocalService() string
	// Publish replaces this process's metadata under key. A nil meta
	// withdraws it.
	Publish(key string, meta map[string]string) error
	// Members lists the processes currently publishing under key, this one
	// included once it has published.
	Members(key string) []Member
	// Dial opens a stream to nodeID for key.
	Dial(ctx context.Context, nodeID, key string) (Conn, error)
	// Listen accepts the streams peers open to this process for key. Only one
	// listener per key may be open at a time.
	Listen(key string) (Listener, error)
	// Changes returns a channel that receives a value, coalesced, whenever
	// the membership or published metadata of any key may have changed, and a
	// function that stops the subscription.
	Changes() (<-chan struct{}, func())
}
