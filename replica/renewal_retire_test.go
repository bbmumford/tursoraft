//go:build cgo

package replica

import (
	"context"
	"sync"
	"testing"
	"time"

	libsql "github.com/bbmumford/go-libsql"
)

// closeWatchingEngine is the fake engine with a hook its Close runs first.
type closeWatchingEngine struct {
	*fakeEngine
	onClose func()
}

func (e *closeWatchingEngine) Close() error {
	if e.onClose != nil {
		e.onClose()
	}
	return e.fakeEngine.Close()
}

// A renewal retires the old engine's connections before it closes the
// engine: by the time the old engine's Close runs, a connection made on it
// already reports itself invalid, so database/sql never runs a pooled read on
// a closed engine while the new one opens.
func TestRenewalRetiresTheOldConnectionsBeforeClosingTheEngine(t *testing.T) {
	var mu sync.Mutex
	var opened []*closeWatchingEngine
	saved := openNative
	openNative = func(Config, string) (native, error) {
		mu.Lock()
		defer mu.Unlock()
		e := &closeWatchingEngine{fakeEngine: &fakeEngine{execErr: libsql.ErrReadOnlyReplica}}
		opened = append(opened, e)
		return e, nil
	}
	t.Cleanup(func() { openNative = saved })
	r, err := Open(context.Background(), Config{
		Path: t.TempDir() + "/r.db", URL: "http://127.0.0.1:1", Key: make([]byte, 32),
		Mode: ReadOnly, Credentials: &mintingSource{life: 300 * time.Millisecond},
		SyncInterval: 50 * time.Millisecond, OpenTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	held, err := r.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	var first *engineConn
	if err := held.Raw(func(dc any) error { first = dc.(*engineConn); return nil }); err != nil {
		t.Fatal(err)
	}
	validAtClose := make(chan bool, 1)
	mu.Lock()
	opened[0].onClose = func() { validAtClose <- first.IsValid() }
	mu.Unlock()
	select {
	case valid := <-validAtClose:
		if valid {
			t.Fatal("the old engine was closed while a connection made on it still reported itself valid")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no renewal closed the first engine")
	}
}
