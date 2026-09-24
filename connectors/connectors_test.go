package connectors

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bbmumford/tursoraft/localdb"
)

// An empty key is refused, for a local file and for a replica alike, before
// anything is created: there is no plaintext open.
func TestAnEmptyKeyIsRefused(t *testing.T) {
	dir := t.TempDir()
	for _, primary := range []string{"", "http://127.0.0.1:1"} {
		h, err := MakeConnector(filepath.Join(dir, "lad.db"), primary, "", "")
		if err == nil {
			h.Close()
			t.Fatalf("primary %q: an empty key opened a database", primary)
		}
		if !errors.Is(err, ErrNoKey) {
			t.Errorf("primary %q: got %v, want ErrNoKey", primary, err)
		}
		if !strings.Contains(err.Error(), "encryptionKey") {
			t.Errorf("primary %q: the error does not name the parameter: %v", primary, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused open left %d files", len(entries))
	}
}

// With a 32-byte key the local file opens, proves foreign keys and is
// encrypted; a key that is not base64 of 32 bytes is refused.
func TestKeyedLocalFileIsEncrypted(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	path := filepath.Join(t.TempDir(), "k.db")
	h, err := MakeConnector(path, "", "", base64.StdEncoding.EncodeToString(k))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Database().Exec("CREATE TABLE t(x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	var fk int
	if err := h.Database().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v", fk, err)
	}
	h.Close()
	if plain, _ := localdb.IsPlaintext(path); plain {
		t.Error("a keyed local file is plaintext")
	}
	if _, err := MakeConnector(filepath.Join(t.TempDir(), "x.db"), "", "", base64.StdEncoding.EncodeToString(k[:16])); !errors.Is(err, ErrBadKey) {
		t.Errorf("a 16-byte key: got %v, want ErrBadKey", err)
	}
}

// hranaClient is the module of libsql-client-go, which registers database/sql
// driver "libsql". The partition-database pools that open "libsql" (cortex
// tenant/tenantdb, constellation domain/identity/provisioning/tenantdb) import
// it themselves, and this module opens its databases through go-libsql and
// tursogo, so no package or test here needs it.
const hranaClient = "github.com/tursodatabase/libsql-client-go"

// moduleDeps is every package that a package or test of this module depends
// on, as the go command resolves them from the module root.
var moduleDeps = sync.OnceValues(func() ([]string, error) {
	cmd := exec.Command("go", "list", "-deps", "-test", "./...")
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go list: %w\n%s", err, out)
	}
	return strings.Fields(string(out)), nil
})

// No package or test of this module depends on libsql-client-go, so linking
// any tursoraft package registers no "libsql" driver.
func TestNothingDependsOnTheHranaClient(t *testing.T) {
	deps, err := moduleDeps()
	if err != nil {
		t.Fatal(err)
	}
	var hrana []string
	for _, dep := range deps {
		if dep == hranaClient || strings.HasPrefix(dep, hranaClient+"/") {
			hrana = append(hrana, dep)
		}
	}
	if len(hrana) != 0 {
		t.Errorf("packages of %s in this module's dependencies: %v", hranaClient, hrana)
	}
}

// The listing TestNothingDependsOnTheHranaClient reads covers the packages
// that open databases: it holds policypool, which replica and localdb import,
// so an empty or partial listing cannot pass for a clean one.
func TestTheDependencyListingReachesTheOpeners(t *testing.T) {
	deps, err := moduleDeps()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(deps, "github.com/bbmumford/policypool") {
		t.Errorf("the listing of %d packages does not hold policypool", len(deps))
	}
}
