package connectors

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// Importing this package registers driver "libsql" for the partition pools
// that open it by name.
func TestTheHranaDriverIsRegistered(t *testing.T) {
	if !slices.Contains(sql.Drivers(), "libsql") {
		t.Fatalf("drivers %v: libsql is not registered", sql.Drivers())
	}
}

// Only libsql_driver.go imports the Hrana client, so deleting that one file
// removes the dependency.
func TestOnlyThisFileImportsTheHranaClient(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	var importers []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(p, "github.com/tursodatabase/libsql-client-go") {
				rel, _ := filepath.Rel(root, path)
				importers = append(importers, rel)
			}
		}
		return nil
	})
	if len(importers) != 1 || importers[0] != filepath.Join("connectors", "libsql_driver.go") {
		t.Errorf("files importing libsql-client-go: %v", importers)
	}
}
