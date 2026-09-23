// Package meshtest implements mesh.Mesh for tests: an in-memory hub for
// clusters inside one process, and a TCP loopback mesh whose members find each
// other through a shared directory, for clusters spread across processes.
//
// Neither authenticates anything. RemoteID is what the hub or the dialer says
// it is, which is enough to test the cluster logic above the mesh and nothing
// more; production meshes must authenticate it.
package meshtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"

	"github.com/bbmumford/tursoraft/mesh"
)

// ErrUnreachable is what Dial returns for a node that is closed, isolated from
// the dialer, or not listening on the key.
var ErrUnreachable = errors.New("meshtest: node unreachable")

// Hub connects in-memory nodes. The zero value is not usable; call NewHub.
type Hub struct {
	mu       sync.Mutex
	nodes    map[string]*Node
	isolated map[string]int // node → island; nodes on different islands cannot see or reach each other
	islands  int            // islands made so far; each Isolate takes the next number
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{nodes: map[string]*Node{}, isolated: map[string]int{}}
}

// Node joins a node with the given identity. A NodeID already in use by a
// live node is an error in the caller's test; it panics.
func (h *Hub) Node(id, service string) *Node {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n, ok := h.nodes[id]; ok && !n.closed {
		panic(fmt.Sprintf("meshtest: node %s already joined", id))
	}
	n := &Node{
		hub:       h,
		id:        id,
		service:   service,
		meta:      map[string]map[string]string{},
		listeners: map[string]*memListener{},
		conns:     map[*memConn]struct{}{},
		subs:      map[chan struct{}]struct{}{},
	}
	h.nodes[id] = n
	h.notifyLocked()
	return n
}

// Isolate puts the given nodes on an island of their own: they see and reach
// each other, and nothing else sees or reaches them. Streams crossing the new
// boundary are closed.
func (h *Hub) Isolate(ids ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.islands++
	island := h.islands
	for _, id := range ids {
		h.isolated[id] = island
	}
	for _, n := range h.nodes {
		for c := range n.conns {
			if !h.reachableLocked(n.id, c.remote) {
				go c.Close()
			}
		}
	}
	h.notifyLocked()
}

// Heal removes every isolation.
func (h *Hub) Heal() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.isolated = map[string]int{}
	h.notifyLocked()
}

func (h *Hub) reachableLocked(a, b string) bool {
	return h.isolated[a] == h.isolated[b]
}

func (h *Hub) notifyLocked() {
	for _, n := range h.nodes {
		for ch := range n.subs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// Node is one in-memory mesh member. It implements mesh.Mesh.
type Node struct {
	hub       *Hub
	id        string
	service   string
	closed    bool
	meta      map[string]map[string]string
	listeners map[string]*memListener
	conns     map[*memConn]struct{}
	subs      map[chan struct{}]struct{}
}

var _ mesh.Mesh = (*Node)(nil)

// LocalID implements mesh.Mesh.
func (n *Node) LocalID() string { return n.id }

// LocalService implements mesh.Mesh.
func (n *Node) LocalService() string { return n.service }

// Publish implements mesh.Mesh.
func (n *Node) Publish(key string, meta map[string]string) error {
	h := n.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if n.closed {
		return net.ErrClosed
	}
	if meta == nil {
		delete(n.meta, key)
	} else {
		cp := make(map[string]string, len(meta))
		for k, v := range meta {
			cp[k] = v
		}
		n.meta[key] = cp
	}
	h.notifyLocked()
	return nil
}

// Members implements mesh.Mesh.
func (n *Node) Members(key string) []mesh.Member {
	h := n.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []mesh.Member
	for _, m := range h.nodes {
		if m.closed || !h.reachableLocked(n.id, m.id) {
			continue
		}
		meta, ok := m.meta[key]
		if !ok {
			continue
		}
		cp := make(map[string]string, len(meta))
		for k, v := range meta {
			cp[k] = v
		}
		out = append(out, mesh.Member{NodeID: m.id, Service: m.service, Meta: cp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// Dial implements mesh.Mesh.
func (n *Node) Dial(ctx context.Context, nodeID, key string) (mesh.Conn, error) {
	h := n.hub
	h.mu.Lock()
	target, ok := h.nodes[nodeID]
	if n.closed || !ok || target.closed || !h.reachableLocked(n.id, nodeID) {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, nodeID)
	}
	l, ok := target.listeners[key]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w: %s is not listening on %s", ErrUnreachable, nodeID, key)
	}
	a, b := net.Pipe()
	local := &memConn{Conn: a, owner: n, remote: nodeID}
	far := &memConn{Conn: b, owner: target, remote: n.id}
	local.peer, far.peer = far, local
	n.conns[local] = struct{}{}
	target.conns[far] = struct{}{}
	h.mu.Unlock()

	select {
	case l.ch <- far:
		return local, nil
	case <-l.done:
	case <-ctx.Done():
	}
	local.Close()
	far.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s stopped listening on %s", ErrUnreachable, nodeID, key)
}

// Listen implements mesh.Mesh.
func (n *Node) Listen(key string) (mesh.Listener, error) {
	h := n.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if n.closed {
		return nil, net.ErrClosed
	}
	if _, ok := n.listeners[key]; ok {
		return nil, fmt.Errorf("meshtest: %s already listens on %s", n.id, key)
	}
	l := &memListener{node: n, key: key, ch: make(chan mesh.Conn), done: make(chan struct{})}
	n.listeners[key] = l
	return l, nil
}

// Changes implements mesh.Mesh.
func (n *Node) Changes() (<-chan struct{}, func()) {
	h := n.hub
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	n.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(n.subs, ch)
			h.mu.Unlock()
		})
	}
}

// Close removes the node from the hub as a crash would: its metadata
// disappears, its listeners stop and every stream it holds is closed.
func (n *Node) Close() error {
	h := n.hub
	h.mu.Lock()
	if n.closed {
		h.mu.Unlock()
		return nil
	}
	n.closed = true
	n.meta = map[string]map[string]string{}
	var ls []*memListener
	for _, l := range n.listeners {
		ls = append(ls, l)
	}
	var cs []*memConn
	for c := range n.conns {
		cs = append(cs, c)
	}
	h.notifyLocked()
	h.mu.Unlock()
	for _, l := range ls {
		l.Close()
	}
	for _, c := range cs {
		c.Close()
	}
	return nil
}

type memAddr string

func (a memAddr) Network() string { return "meshtest" }
func (a memAddr) String() string  { return string(a) }

type memListener struct {
	node *Node
	key  string
	ch   chan mesh.Conn
	done chan struct{}
	once sync.Once
}

func (l *memListener) Accept() (mesh.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() {
		h := l.node.hub
		h.mu.Lock()
		if l.node.listeners[l.key] == l {
			delete(l.node.listeners, l.key)
		}
		h.mu.Unlock()
		close(l.done)
	})
	return nil
}

func (l *memListener) Addr() net.Addr { return memAddr(l.node.id) }

type memConn struct {
	net.Conn
	owner  *Node
	remote string
	peer   *memConn
	once   sync.Once
}

func (c *memConn) RemoteID() string { return c.remote }

func (c *memConn) LocalAddr() net.Addr  { return memAddr(c.owner.id) }
func (c *memConn) RemoteAddr() net.Addr { return memAddr(c.remote) }

func (c *memConn) Close() error {
	var err error
	c.once.Do(func() {
		h := c.owner.hub
		h.mu.Lock()
		delete(c.owner.conns, c)
		h.mu.Unlock()
		err = c.Conn.Close()
	})
	return err
}
