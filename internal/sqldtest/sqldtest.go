// Package sqldtest starts a local sqld for a test: the libSQL server a shared
// database's primary runs on, on 127.0.0.1 and a free port, with its data in
// the test's temporary directory. Only the process it started is ever
// stopped.
//
// The binary is $TURSORAFT_SQLD, else /opt/homebrew/bin/sqld, else sqld on
// PATH; with none the test is skipped.
package sqldtest

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Server is one sqld process.
type Server struct {
	t       testing.TB
	bin     string
	dir     string
	addr    string
	Version string // e.g. "0.24.32"

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
}

// Binary returns the sqld the tests use, or "" when there is none.
func Binary() string {
	if p := os.Getenv("TURSORAFT_SQLD"); p != "" {
		return p
	}
	if _, err := os.Stat("/opt/homebrew/bin/sqld"); err == nil {
		return "/opt/homebrew/bin/sqld"
	}
	if p, err := exec.LookPath("sqld"); err == nil {
		return p
	}
	return ""
}

// Start starts sqld and stops it when the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	bin := Binary()
	if bin == "" {
		t.Skip("no sqld: set TURSORAFT_SQLD")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	s := &Server{t: t, bin: bin, dir: t.TempDir(), addr: addr}
	s.start()
	t.Cleanup(s.Stop)
	return s
}

// URL is the primary's URL for a replica.
func (s *Server) URL() string { return "http://" + s.addr }

func (s *Server) start() {
	s.t.Helper()
	logf, err := os.OpenFile(filepath.Join(s.dir, "sqld.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command(s.bin, "--no-welcome", "--db-path", filepath.Join(s.dir, "data.sqld"), "--http-listen-addr", s.addr)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		s.t.Fatal(err)
	}
	exited := make(chan struct{})
	s.mu.Lock()
	s.cmd, s.exited = cmd, exited
	s.mu.Unlock()
	go func() {
		cmd.Wait()
		logf.Close()
		close(exited)
	}()
	deadline := time.Now().Add(20 * time.Second)
	client := http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for {
		resp, err := client.Get(s.URL() + "/version")
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				s.Version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(body)), "sqld"))
				if f := strings.Fields(s.Version); len(f) > 0 {
					s.Version = f[0]
				}
				return
			}
		}
		if time.Now().After(deadline) {
			s.Stop()
			s.t.Fatalf("sqld at %s did not answer: %v", s.addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Stop stops the process this Server started, and only it.
func (s *Server) Stop() {
	s.mu.Lock()
	cmd, exited := s.cmd, s.exited
	s.cmd = nil
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
}

// Restart starts sqld again on the same address and data.
func (s *Server) Restart() {
	s.t.Helper()
	s.Stop()
	s.start()
}

// AtLeast reports whether the server is version major.minor or later.
func (s *Server) AtLeast(major, minor int) bool {
	var a, b, c int
	if _, err := fmt.Sscanf(s.Version, "%d.%d.%d", &a, &b, &c); err != nil {
		return false
	}
	return a > major || a == major && b >= minor
}

// RequireConcurrentTransactions skips a test that holds BEGIN IMMEDIATE on
// two replicas at once: sqld before 0.24 never answers a statement inside
// one replica's transaction while a second replica waits in its own, so the
// test would hang instead of failing.
func (s *Server) RequireConcurrentTransactions(t testing.TB) {
	t.Helper()
	if !s.AtLeast(0, 24) {
		t.Skipf("sqld %s cannot run two replicas' write transactions concurrently; set TURSORAFT_SQLD to sqld 0.24 or later", s.Version)
	}
}
