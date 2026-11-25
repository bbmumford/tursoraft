// Package sqldtest starts a local sqld for a test: the libSQL server a shared
// database's primary runs on, on 127.0.0.1 and a free port, with its data in
// the test's temporary directory. Only the process it started is ever
// stopped.
//
// Consumed by every tursoraft test that needs a primary. It sits outside
// internal/ so that a test in another module that runs tursoraft against a
// primary, such as a mesh runtime's cluster test, can pick the binary, wait
// for readiness and stop the process the same way instead of keeping its own
// sqld launcher.
//
// The binary is $TURSORAFT_SQLD, else the newest of /opt/homebrew/bin/sqld,
// sqld on PATH and each ~/.local/share/libsql-server/<version>/sqld (see
// Binary); with none the test is skipped.
//
// Start runs sqld with authentication off. StartWithAuth runs it with JWT
// authentication under a fresh Ed25519 key, for the tests of what a token
// lets a replica do; its Signer mints the tokens.
package sqldtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
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
	args    []string // added to every start, Restart included
	Version string   // e.g. "0.24.32"

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
}

// homebrewSqld is where Homebrew links sqld on Apple silicon. It is a
// candidate even when a test's PATH leaves /opt/homebrew/bin out, and a
// variable so the selection tests can point it at a stand-in.
var homebrewSqld = "/opt/homebrew/bin/sqld"

// releaseGlob, under the user's home directory, matches the sqld of each
// unpacked libsql-server release, one directory per version. A release
// installed there sits beside the package manager's sqld without shadowing
// the sqld on PATH.
const releaseGlob = ".local/share/libsql-server/*/sqld"

// Binary returns the sqld the tests use, or "" when there is none.
//
// $TURSORAFT_SQLD, when set, is used as given. Otherwise the candidates are
// /opt/homebrew/bin/sqld, sqld on PATH and each
// ~/.local/share/libsql-server/<version>/sqld, and Binary takes the one whose
// --version names the newest release. A candidate whose version cannot be read
// ranks below every one whose version can, and a tie keeps the earlier
// candidate. The newest wins because RequireConcurrentTransactions skips every
// test that holds two replicas' write transactions at once on a sqld before
// 0.24, so taking the first candidate found would skip those tests whenever an
// older sqld is found first.
func Binary() string {
	if p := os.Getenv("TURSORAFT_SQLD"); p != "" {
		return p
	}
	best, bestVersion, bestKnown := "", [3]int{}, false
	for _, p := range candidates() {
		v, known := binaryVersion(p)
		if best == "" || known && (!bestKnown || newer(v, bestVersion)) {
			best, bestVersion, bestKnown = p, v, known
		}
	}
	return best
}

// candidates lists the existing sqld files Binary chooses among, each once,
// in its tie-break order.
func candidates() []string {
	var out []string
	add := func(p string) {
		for _, q := range out {
			if q == p {
				return
			}
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			out = append(out, p)
		}
	}
	add(homebrewSqld)
	if p, err := exec.LookPath("sqld"); err == nil {
		add(p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		matches, _ := filepath.Glob(filepath.Join(home, releaseGlob))
		for _, p := range matches {
			add(p)
		}
	}
	return out
}

// versionBound is how long one "<bin> --version" run is given. It is a
// variable so the slow-first-run case can shorten it instead of holding a
// stand-in open for the whole bound.
var versionBound = 10 * time.Second

// versionLine runs "<bin> --version" under versionBound and reports whether
// the bound, rather than the binary itself, ended the run.
func versionLine(bin string) (out []byte, timedOut bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionBound)
	defer cancel()
	out, err = exec.CommandContext(ctx, bin, "--version").Output()
	return out, errors.Is(ctx.Err(), context.DeadlineExceeded), err
}

// binaryVersion runs "<bin> --version", which sqld answers with a line such
// as "sqld sqld 0.24.32 (40c272de 2025-02-14)", and returns its first
// major.minor.patch field. known is false when the binary fails, does not
// answer within two runs of versionBound, or prints no such field.
//
// A binary this host has not run before answers its first --version far
// slower than its later ones, so a run the bound cut off is retried once:
// that cut-off run leaves the binary warm. Reporting it unreadable instead
// would leave Binary with an older candidate, and
// RequireConcurrentTransactions would then skip every test that holds two
// replicas' write transactions at once.
func binaryVersion(bin string) (v [3]int, known bool) {
	out, timedOut, err := versionLine(bin)
	if timedOut {
		out, _, err = versionLine(bin)
	}
	if err != nil {
		return v, false
	}
	for _, f := range strings.Fields(string(out)) {
		var a, b, c int
		if _, err := fmt.Sscanf(f, "%d.%d.%d", &a, &b, &c); err == nil {
			return [3]int{a, b, c}, true
		}
	}
	return v, false
}

// newer reports whether version a is later than version b.
func newer(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// Start starts sqld and stops it when the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	return start(t, nil)
}

// Signer mints the tokens a Server from StartWithAuth accepts.
type Signer struct{ key ed25519.PrivateKey }

// Token returns an EdDSA JWT with claims as its payload, signed by the key
// the server verifies with. sqld reads "a" (ro, rw or roa), "p" (scopes),
// "id" (a namespace) and "exp"; a token with no "a" and no "p" has full
// access.
func (s *Signer) Token(claims map[string]any) string {
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(fmt.Sprintf("sqldtest: claims do not encode: %v", err))
	}
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	return signing + "." + enc.EncodeToString(ed25519.Sign(s.key, []byte(signing)))
}

// StartWithAuth starts sqld with --auth-jwt-key-file holding the public half
// of a new Ed25519 key, so every request needs a token the returned Signer
// minted. sqld checks a token's exp with a 60 s leeway.
func StartWithAuth(t testing.TB) (*Server, *Signer) {
	t.Helper()
	return StartWithAuthFrom(t, "")
}

// StartWithAuthFrom is StartWithAuth over a primary whose default database
// starts as a copy of the SQLite file at seed, its header included; an empty
// seed starts an empty database. sqld refuses PRAGMA application_id and
// user_version as statements, so a seeded file is how a primary's page 1
// carries a file identity, as a scope database created from a stamped file
// does.
func StartWithAuthFrom(t testing.TB, seed string) (*Server, *Signer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "jwt.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return startFrom(t, []string{"--auth-jwt-key-file", keyFile}, seed), &Signer{key: priv}
}

func start(t testing.TB, args []string) *Server {
	t.Helper()
	return startFrom(t, args, "")
}

// startFrom starts sqld on a new data directory whose default database is a
// copy of seed, when seed is not empty.
func startFrom(t testing.TB, args []string, seed string) *Server {
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
	s := &Server{t: t, bin: bin, dir: t.TempDir(), addr: addr, args: args}
	if seed != "" {
		data, err := os.ReadFile(seed)
		if err != nil {
			t.Fatal(err)
		}
		dbs := filepath.Join(s.dir, "data.sqld", "dbs", "default")
		if err := os.MkdirAll(dbs, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dbs, "data"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
	cmd := exec.Command(s.bin, append([]string{"--no-welcome", "--db-path", filepath.Join(s.dir, "data.sqld"), "--http-listen-addr", s.addr}, s.args...)...)
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
		t.Skipf("sqld %s cannot run two replicas' write transactions concurrently; set TURSORAFT_SQLD to sqld 0.24 or later, or unpack one as ~/.local/share/libsql-server/<version>/sqld", s.Version)
	}
}
