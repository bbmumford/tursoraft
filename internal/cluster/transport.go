package cluster

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/bbmumford/tursoraft/mesh"
)

// handshakeTimeout bounds the member proof on a new stream. A peer that
// stalls inside it holds one goroutine for this long and no more.
const handshakeTimeout = 5 * time.Second

// nodeAddr is the net.Addr of a cluster member: its NodeID. Raft reads the
// transport's local address from it, and ServerAddress = ServerID = NodeID.
type nodeAddr string

func (a nodeAddr) Network() string { return "tursoraft" }
func (a nodeAddr) String() string  { return string(a) }

// streamLayer is the raft.StreamLayer over the mesh: Dial opens a mesh stream
// to the NodeID raft names, Accept hands raft the streams peers open, and
// every stream in either direction passes the member proof first.
//
// It owns every stream it has handed out, because raft's NetworkTransport
// closes neither accepted streams on Close nor pooled outbound streams, and a
// handler goroutine blocked reading a stream outlives Shutdown until the
// stream closes.
type streamLayer struct {
	mesh    mesh.Mesh
	key     string
	ln      mesh.Listener
	hs      handshake
	log     *slog.Logger
	refused func(remote string, err error)

	accepted chan net.Conn
	closing  chan struct{}
	once     sync.Once
	wg       sync.WaitGroup

	mu    sync.Mutex
	conns map[*trackedConn]struct{}
}

func newStreamLayer(m mesh.Mesh, key string, hs handshake, log *slog.Logger, refused func(string, error)) (*streamLayer, error) {
	ln, err := m.Listen(key)
	if err != nil {
		return nil, err
	}
	s := &streamLayer{
		mesh: m, key: key, ln: ln, hs: hs, log: log, refused: refused,
		accepted: make(chan net.Conn),
		closing:  make(chan struct{}),
		conns:    map[*trackedConn]struct{}{},
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

func (s *streamLayer) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := s.hs.server(c, time.Now().Add(handshakeTimeout)); err != nil {
				c.Close()
				s.refuse(c.RemoteID(), err)
				return
			}
			tc, ok := s.track(c)
			if !ok {
				return
			}
			select {
			case s.accepted <- tc:
			case <-s.closing:
				tc.Close()
			}
		}()
	}
}

func (s *streamLayer) refuse(remote string, err error) {
	if errors.Is(err, ErrProofFailed) || errors.Is(err, ErrForeignCluster) {
		s.log.Error("tursoraft: refused a raft stream", "cluster", s.key, "peer", remote, "err", err)
	}
	if s.refused != nil {
		s.refused(remote, err)
	}
}

func (s *streamLayer) track(c mesh.Conn) (*trackedConn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closing:
		c.Close()
		return nil, false
	default:
	}
	tc := &trackedConn{Conn: c, layer: s}
	s.conns[tc] = struct{}{}
	return tc, true
}

// Accept implements net.Listener for raft.
func (s *streamLayer) Accept() (net.Conn, error) {
	select {
	case c := <-s.accepted:
		return c, nil
	case <-s.closing:
		return nil, net.ErrClosed
	}
}

// Addr implements net.Listener for raft.
func (s *streamLayer) Addr() net.Addr { return nodeAddr(s.hs.local) }

// Dial implements raft.StreamLayer.
func (s *streamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = handshakeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, _, err := s.dial(ctx, string(address))
	return c, err
}

// dial opens a mesh stream to nodeID and runs the member proof on it,
// returning the peer's standing. It gives up when ctx ends or the layer
// closes.
func (s *streamLayer) dial(ctx context.Context, nodeID string) (net.Conn, standing, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	c, err := s.mesh.Dial(ctx, nodeID, s.key)
	if err != nil {
		return nil, standing{}, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(handshakeTimeout)
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	peer, err := s.hs.client(c, deadline)
	if !stop() {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		c.Close()
		s.refuse(nodeID, err)
		return nil, standing{}, err
	}
	tc, ok := s.track(c)
	if !ok {
		return nil, standing{}, net.ErrClosed
	}
	return tc, peer, nil
}

// probe opens one stream to nodeID and runs the member proof on it, without
// handing the stream to raft, and returns the peer's standing. The reconciler
// probes a member before adding it, so a member that cannot prove the group
// key, or that already belongs to another cluster, never enters the
// configuration.
func (s *streamLayer) probe(ctx context.Context, nodeID string) (standing, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	c, peer, err := s.dial(ctx, nodeID)
	if err != nil {
		return standing{}, err
	}
	return peer, c.Close()
}

// Close stops accepting, closes every stream handed out and waits for the
// accept and handshake goroutines.
func (s *streamLayer) Close() error {
	s.once.Do(func() {
		close(s.closing)
		s.ln.Close()
		s.mu.Lock()
		conns := make([]*trackedConn, 0, len(s.conns))
		for c := range s.conns {
			conns = append(conns, c)
		}
		s.mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	s.wg.Wait()
	return nil
}

type trackedConn struct {
	mesh.Conn
	layer *streamLayer
	once  sync.Once
}

func (c *trackedConn) Close() error {
	var err error
	c.once.Do(func() {
		c.layer.mu.Lock()
		delete(c.layer.conns, c)
		c.layer.mu.Unlock()
		err = c.Conn.Close()
	})
	return err
}
