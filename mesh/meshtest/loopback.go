package meshtest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bbmumford/tursoraft/mesh"
)

// Loopback timing. A record whose heartbeat is older than loopbackStale is a
// process that died without withdrawing, which is how a SIGKILLed test child
// leaves the mesh.
const (
	loopbackBeat  = 200 * time.Millisecond
	loopbackStale = 1500 * time.Millisecond
	loopbackPoll  = 100 * time.Millisecond
)

// loopbackRecord is one member's file in the shared directory.
type loopbackRecord struct {
	ID      string                       `json:"id"`
	Service string                       `json:"service"`
	Addr    string                       `json:"addr"`
	Beat    int64                        `json:"beat"`
	Meta    map[string]map[string]string `json:"meta"`
}

// Loopback is a mesh member reachable over TCP on 127.0.0.1. Members find each
// other through JSON records in a directory they share, so members in
// different processes form one mesh. It implements mesh.Mesh.
type Loopback struct {
	dir, id, service string
	ln               net.Listener

	wmu       sync.Mutex // serialises record writes, which share one temporary file
	mu        sync.Mutex
	closed    bool
	meta      map[string]map[string]string
	listeners map[string]*lbListener
	conns     map[*lbConn]struct{}
	subs      map[chan struct{}]struct{}

	stop chan struct{}
	wg   sync.WaitGroup
}

var _ mesh.Mesh = (*Loopback)(nil)

// NewLoopback joins the mesh whose records live in dir.
func NewLoopback(dir, id, service string) (*Loopback, error) {
	if strings.ContainsAny(id, "/\\") {
		return nil, fmt.Errorf("meshtest: node id %q contains a path separator", id)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l := &Loopback{
		dir: dir, id: id, service: service, ln: ln,
		meta:      map[string]map[string]string{},
		listeners: map[string]*lbListener{},
		conns:     map[*lbConn]struct{}{},
		subs:      map[chan struct{}]struct{}{},
		stop:      make(chan struct{}),
	}
	if err := l.writeRecord(); err != nil {
		ln.Close()
		return nil, err
	}
	l.wg.Add(3)
	go l.acceptLoop()
	go l.beatLoop()
	go l.pollLoop()
	return l, nil
}

// LocalID implements mesh.Mesh.
func (l *Loopback) LocalID() string { return l.id }

// LocalService implements mesh.Mesh.
func (l *Loopback) LocalService() string { return l.service }

// Publish implements mesh.Mesh.
func (l *Loopback) Publish(key string, meta map[string]string) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return net.ErrClosed
	}
	if meta == nil {
		delete(l.meta, key)
	} else {
		cp := make(map[string]string, len(meta))
		for k, v := range meta {
			cp[k] = v
		}
		l.meta[key] = cp
	}
	l.mu.Unlock()
	return l.writeRecord()
}

func (l *Loopback) writeRecord() error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	l.mu.Lock()
	rec := loopbackRecord{ID: l.id, Service: l.service, Addr: l.ln.Addr().String(), Beat: time.Now().UnixNano(), Meta: l.meta}
	raw, err := json.Marshal(rec)
	l.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := filepath.Join(l.dir, "."+l.id+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(l.dir, l.id+".json"))
}

func (l *Loopback) records() []loopbackRecord {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil
	}
	now := time.Now()
	var out []loopbackRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(l.dir, e.Name()))
		if err != nil {
			continue
		}
		var rec loopbackRecord
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		if now.Sub(time.Unix(0, rec.Beat)) > loopbackStale {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Members implements mesh.Mesh.
func (l *Loopback) Members(key string) []mesh.Member {
	var out []mesh.Member
	for _, rec := range l.records() {
		meta, ok := rec.Meta[key]
		if !ok {
			continue
		}
		out = append(out, mesh.Member{NodeID: rec.ID, Service: rec.Service, Meta: meta})
	}
	return out
}

// Dial implements mesh.Mesh.
func (l *Loopback) Dial(ctx context.Context, nodeID, key string) (mesh.Conn, error) {
	var addr string
	for _, rec := range l.records() {
		if rec.ID == nodeID {
			addr = rec.Addr
		}
	}
	if addr == "" {
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, nodeID)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetWriteDeadline(dl)
	}
	if _, err := fmt.Fprintf(c, "%s\n%s\n", key, l.id); err != nil {
		c.Close()
		return nil, err
	}
	c.SetWriteDeadline(time.Time{})
	return l.track(c, nodeID)
}

func (l *Loopback) track(c net.Conn, remote string) (*lbConn, error) {
	lc := &lbConn{Conn: c, owner: l, remote: remote}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		c.Close()
		return nil, net.ErrClosed
	}
	l.conns[lc] = struct{}{}
	return lc, nil
}

// Listen implements mesh.Mesh.
func (l *Loopback) Listen(key string) (mesh.Listener, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, net.ErrClosed
	}
	if _, ok := l.listeners[key]; ok {
		return nil, fmt.Errorf("meshtest: %s already listens on %s", l.id, key)
	}
	ln := &lbListener{owner: l, key: key, ch: make(chan mesh.Conn), done: make(chan struct{})}
	l.listeners[key] = ln
	return ln, nil
}

// Changes implements mesh.Mesh.
func (l *Loopback) Changes() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.subs, ch)
			l.mu.Unlock()
		})
	}
}

func (l *Loopback) acceptLoop() {
	defer l.wg.Done()
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.route(c)
		}()
	}
}

// route reads the two-line header a dialer sends and hands the stream to the
// listener for its key.
func (l *Loopback) route(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	key, err1 := r.ReadString('\n')
	from, err2 := r.ReadString('\n')
	if err := errors.Join(err1, err2); err != nil {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})
	c = &readerConn{Conn: c, r: r}
	key, from = strings.TrimSuffix(key, "\n"), strings.TrimSuffix(from, "\n")
	l.mu.Lock()
	ln := l.listeners[key]
	l.mu.Unlock()
	if ln == nil {
		c.Close()
		return
	}
	lc, err := l.track(c, from)
	if err != nil {
		return
	}
	select {
	case ln.ch <- lc:
	case <-ln.done:
		lc.Close()
	case <-l.stop:
		lc.Close()
	}
}

func (l *Loopback) beatLoop() {
	defer l.wg.Done()
	t := time.NewTicker(loopbackBeat)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.writeRecord()
		}
	}
}

func (l *Loopback) pollLoop() {
	defer l.wg.Done()
	t := time.NewTicker(loopbackPoll)
	defer t.Stop()
	var last string
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		h := sha256.New()
		for _, rec := range l.records() {
			rec.Beat, rec.Addr = 0, ""
			raw, _ := json.Marshal(rec)
			h.Write(raw)
		}
		d := hex.EncodeToString(h.Sum(nil))
		if d == last {
			continue
		}
		last = d
		l.mu.Lock()
		for ch := range l.subs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		l.mu.Unlock()
	}
}

// Close withdraws the member: its record is removed, its listeners stop and
// its streams close.
func (l *Loopback) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.stop)
	var ls []*lbListener
	for _, x := range l.listeners {
		ls = append(ls, x)
	}
	var cs []*lbConn
	for c := range l.conns {
		cs = append(cs, c)
	}
	l.mu.Unlock()
	l.ln.Close()
	for _, x := range ls {
		x.Close()
	}
	for _, c := range cs {
		c.Close()
	}
	l.wg.Wait()
	os.Remove(filepath.Join(l.dir, l.id+".json"))
	return nil
}

// readerConn keeps the bytes route's header reader buffered past the header,
// which a dialer that writes immediately after the header puts in the same
// TCP segment.
type readerConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

type lbListener struct {
	owner *Loopback
	key   string
	ch    chan mesh.Conn
	done  chan struct{}
	once  sync.Once
}

func (x *lbListener) Accept() (mesh.Conn, error) {
	select {
	case c := <-x.ch:
		return c, nil
	case <-x.done:
		return nil, net.ErrClosed
	}
}

func (x *lbListener) Close() error {
	x.once.Do(func() {
		x.owner.mu.Lock()
		if x.owner.listeners[x.key] == x {
			delete(x.owner.listeners, x.key)
		}
		x.owner.mu.Unlock()
		close(x.done)
	})
	return nil
}

func (x *lbListener) Addr() net.Addr { return x.owner.ln.Addr() }

type lbConn struct {
	net.Conn
	owner  *Loopback
	remote string
	once   sync.Once
}

func (c *lbConn) RemoteID() string { return c.remote }

func (c *lbConn) Close() error {
	var err error
	c.once.Do(func() {
		c.owner.mu.Lock()
		delete(c.owner.conns, c)
		c.owner.mu.Unlock()
		err = c.Conn.Close()
	})
	return err
}
