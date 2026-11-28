package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakePlatform is a fake of the platform API holding databases by name.
type fakePlatform struct {
	mu      sync.Mutex
	dbs     map[string]map[string]string // name → {"Hostname", "group"}
	created []string
	status  int    // when set, every call answers it
	body    string // with this body
	auth    string
	lastReq string
}

func (p *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.auth = r.Header.Get("Authorization")
	p.lastReq = r.Method + " " + r.URL.RequestURI()
	if p.status != 0 {
		w.WriteHeader(p.status)
		io.WriteString(w, p.body)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/organizations/orbtr/databases/"):
		name := strings.TrimPrefix(r.URL.Path, "/v1/organizations/orbtr/databases/")
		d, ok := p.dbs[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"database not found"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"database": map[string]string{"Name": name, "Hostname": d["Hostname"], "group": d["group"]}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/organizations/orbtr/databases":
		var req struct{ Name, Group string }
		json.NewDecoder(r.Body).Decode(&req)
		host := req.Name + "-orbtr.turso.io"
		p.dbs[req.Name] = map[string]string{"Hostname": host, "group": req.Group}
		p.created = append(p.created, req.Name)
		json.NewEncoder(w).Encode(map[string]any{"database": map[string]string{"Name": req.Name, "Hostname": host}})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/auth/tokens"):
		json.NewEncoder(w).Encode(map[string]string{"jwt": "minted-for-" + strings.Split(r.URL.Path, "/")[5]})
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func admin(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	env := map[string]string{"TURSO_API_TOKEN": "org-token", "TURSO_API_BASE_URL": srv.URL, "TURSO_DB_TOKEN": "db-token"}
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

// A database of that name in another group is refused and nothing is
// created.
func TestEnsureRefusesADatabaseInAnotherGroup(t *testing.T) {
	p := &fakePlatform{dbs: map[string]map[string]string{"orbtr-control": {"Hostname": "x", "group": "default"}}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	code, _, errOut := admin(t, srv, "ensure", "-org", "orbtr", "-group", "control", "-db", "orbtr-control")
	if code == 0 || !strings.Contains(errOut, "another group") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if len(p.created) != 0 {
		t.Errorf("created %v", p.created)
	}
}

// A missing database is created in the group and its url printed from the
// documented Hostname.
func TestEnsureCreatesInTheGroup(t *testing.T) {
	p := &fakePlatform{dbs: map[string]map[string]string{}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	code, out, errOut := admin(t, srv, "ensure", "-org", "orbtr", "-group", "control", "-db", "orbtr-control")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if p.dbs["orbtr-control"]["group"] != "control" || !strings.Contains(out, "url: libsql://orbtr-control-orbtr.turso.io") {
		t.Errorf("created %v, printed %q", p.dbs, out)
	}
	if p.auth != "Bearer org-token" {
		t.Errorf("authorization %q", p.auth)
	}
}

// The platform's refusal reaches the operator with its body.
func TestAPlatformRefusalShowsItsBody(t *testing.T) {
	p := &fakePlatform{status: http.StatusForbidden, body: `{"error":"token lacks permission on group control"}`}
	srv := httptest.NewServer(p)
	defer srv.Close()
	code, _, errOut := admin(t, srv, "ensure", "-org", "orbtr", "-group", "control", "-db", "orbtr-control")
	if code == 0 || !strings.Contains(errOut, "403") || !strings.Contains(errOut, "token lacks permission on group control") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

// A token is minted for one database, full access unless asked otherwise, and
// expires after one day unless asked otherwise: a token that never expires is
// asked for by name.
func TestTokenIsScopedToOneDatabase(t *testing.T) {
	p := &fakePlatform{dbs: map[string]map[string]string{}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	code, out, errOut := admin(t, srv, "token", "-org", "orbtr", "-db", "orbtr-sessions")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.TrimSpace(out) != "minted-for-orbtr-sessions" {
		t.Errorf("printed %q", out)
	}
	if !strings.Contains(p.lastReq, "/databases/orbtr-sessions/auth/tokens?") || !strings.Contains(p.lastReq, "authorization=full-access") || !strings.Contains(p.lastReq, "expiration=1d") {
		t.Errorf("request %q", p.lastReq)
	}
	if code, _, errOut := admin(t, srv, "token", "-org", "orbtr", "-db", "orbtr-sessions", "-expiration", "never"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(p.lastReq, "expiration=never") {
		t.Errorf("request %q, want the never-expiring token asked for", p.lastReq)
	}
}

// fence-adopt refuses until the operator confirms the old cluster is gone,
// then sends the adopt statement with the new cluster id.
func TestFenceAdoptNeedsConfirmation(t *testing.T) {
	var got struct {
		Requests []struct {
			Type string
			Stmt struct {
				SQL  string `json:"sql"`
				Args []struct{ Type, Value string }
			}
		}
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"results":[{"type":"ok","response":{"type":"execute","result":{"affected_row_count":3}}},{"type":"ok","response":{"type":"close"}}]}`)
	}))
	defer srv.Close()
	code, _, errOut := admin(t, srv, "fence-adopt", "-url", srv.URL, "-cluster", "new-id")
	if code == 0 || !strings.Contains(errOut, "confirm-old-cluster-gone") {
		t.Fatalf("unconfirmed adopt: exit %d, stderr %q", code, errOut)
	}
	if len(got.Requests) != 0 {
		t.Fatal("an unconfirmed adopt reached the database")
	}
	code, out, errOut := admin(t, srv, "fence-adopt", "-url", srv.URL, "-cluster", "new-id", "-confirm-old-cluster-gone")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(got.Requests) == 0 || !strings.HasPrefix(got.Requests[0].Stmt.SQL, "UPDATE _tursoraft_fence") || got.Requests[0].Stmt.Args[0].Value != "new-id" {
		t.Errorf("sent %+v", got)
	}
	if auth != "Bearer db-token" || !strings.Contains(out, "3 fence rows") {
		t.Errorf("auth %q, printed %q", auth, out)
	}
}
