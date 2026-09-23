package meshtest

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/mesh"
)

func exercise(t *testing.T, a, b mesh.Mesh, isolate func()) {
	t.Helper()
	if err := a.Publish("k", map[string]string{"role": "voter"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish("k", map[string]string{"role": "host"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(a.Members("k")) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("members %v", a.Members("k"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, m := range a.Members("k") {
		if m.NodeID == b.LocalID() && (m.Service != b.LocalService() || m.Meta["role"] != "host") {
			t.Errorf("b seen as %+v", m)
		}
	}
	ln, err := b.Listen("k")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan mesh.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			got <- c
		}
	}()
	c, err := a.Dial(context.Background(), b.LocalID(), "k")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The in-memory mesh's streams are synchronous pipes: a write returns
	// once the far side has read it.
	go io.WriteString(c, "ping")
	far := <-got
	defer far.Close()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(far, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read %q %v", buf, err)
	}
	if c.RemoteID() != b.LocalID() || far.RemoteID() != a.LocalID() {
		t.Errorf("remote ids %q %q", c.RemoteID(), far.RemoteID())
	}
	// A stream to a key nobody listens on fails at the dial or at its first
	// read, as it would over a network.
	if c, err := a.Dial(context.Background(), b.LocalID(), "other"); err == nil {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Error("a stream to a key nobody listens on carried data")
		}
		c.Close()
	}
	if isolate != nil {
		isolate()
		if len(a.Members("k")) != 1 {
			t.Errorf("an isolated member still sees %v", a.Members("k"))
		}
		if _, err := a.Dial(context.Background(), b.LocalID(), "k"); err == nil {
			t.Error("dialled across an isolation")
		}
	}
}

func TestMemoryMesh(t *testing.T) {
	h := NewHub()
	a, b := h.Node("a", "svc-a"), h.Node("b", "svc-b")
	defer a.Close()
	defer b.Close()
	exercise(t, a, b, func() { h.Isolate("a") })
}

func TestLoopbackMesh(t *testing.T) {
	dir := t.TempDir()
	a, err := NewLoopback(dir, "a", "svc-a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewLoopback(dir, "b", "svc-b")
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, a, b, nil)
	b.Close()
	if _, err := os.Stat(dir + "/b.json"); !os.IsNotExist(err) {
		t.Error("a closed member left its record")
	}
}
