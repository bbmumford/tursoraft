package replica

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// unsignedJWT is a token whose claims checkToken reads; its signature is not
// checked here, the primary checks it.
func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJFZERTQSJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

// A read-only scope grant's token must name a database, and only the scope's:
// a token naming none (no "id", no scope "ns") is refused with
// ErrGrantMismatch, as one naming another database is, and one naming the
// scope's database by name or by the grant's database id passes.
func TestAReadOnlyScopeTokenMustNameTheScopesDatabase(t *testing.T) {
	now := time.Now()
	cfg := Config{Mode: ReadOnly, Scope: Scope{Database: "seg-a", Digest: make([]byte, 32)}}
	exp := now.Add(10 * time.Minute).Unix()
	for name, claims := range map[string]map[string]any{
		"no database":      {"a": "ro", "exp": exp},
		"an empty ns list": {"a": "ro", "exp": exp, "p": map[string]any{"ro": map[string]any{"ns": []string{}}}},
		"another database": {"a": "ro", "exp": exp, "id": "seg-b"},
	} {
		if err := cfg.checkToken(unsignedJWT(t, claims), "db-uuid-a", now); !errors.Is(err, ErrGrantMismatch) {
			t.Errorf("a token naming %s = %v, want ErrGrantMismatch", name, err)
		}
	}
	for name, claims := range map[string]map[string]any{
		"the scope by name": {"a": "ro", "exp": exp, "p": map[string]any{"ro": map[string]any{"ns": []string{"seg-a"}}}},
		"the database id":   {"a": "ro", "exp": exp, "id": "db-uuid-a"},
	} {
		if err := cfg.checkToken(unsignedJWT(t, claims), "db-uuid-a", now); err != nil {
			t.Errorf("a token naming %s = %v, want it accepted", name, err)
		}
	}
}
