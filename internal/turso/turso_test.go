package turso

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureAllDBs_SkipsPureLocalGroups(t *testing.T) {
	t.Helper()

	groupCalls := 0
	dbCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/organizations/test-org/groups/remote":
			groupCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"group":{"name":"remote"}}`))
		case "/v1/organizations/test-org/databases/core":
			dbCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"database":{"name":"core","primary_url":"libsql://core.test"}}`))
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTursoClient(TursoConfig{
		APIToken: "token",
		OrgName:  "test-org",
		BaseURL:  server.URL,
	})

	dbInfo, err := EnsureAllDBs(context.Background(), client, []GroupConfig{
		{GroupName: "auth-sessions", DBNames: []string{"sessions"}, PureLocal: true},
		{GroupName: "remote", DBNames: []string{"core"}},
	})
	if err != nil {
		t.Fatalf("EnsureAllDBs() error = %v", err)
	}

	if got := dbInfo["auth-sessions.sessions"].URL; got != "" {
		t.Fatalf("pure-local database URL = %q, want empty string", got)
	}
	if got := dbInfo["remote.core"].URL; got != "libsql://core.test" {
		t.Fatalf("remote database URL = %q, want libsql://core.test", got)
	}
	if groupCalls != 1 {
		t.Fatalf("group validation calls = %d, want 1", groupCalls)
	}
	if dbCalls != 1 {
		t.Fatalf("database lookup calls = %d, want 1", dbCalls)
	}
	if _, ok := dbInfo["auth-sessions.sessions"]; !ok {
		t.Fatalf("pure-local db key missing from result map")
	}
}
