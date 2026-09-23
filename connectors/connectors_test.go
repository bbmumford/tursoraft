package connectors

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bbmumford/tursoraft/localdb"
)

// loom's node runtime opens its LAD file with MakeConnector(path, "", "", "");
// that call must keep opening a working local file.
func TestKeylessLocalFileOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lad.db")
	h, err := MakeConnector(path, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Database().Exec("CREATE TABLE lad_records(topic TEXT)"); err != nil {
		t.Fatal(err)
	}
	var fk int
	if err := h.Database().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v", fk, err)
	}
}

// With a key the local file is encrypted; a key that is not base64 of 32
// bytes is refused.
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
	h.Close()
	if plain, _ := localdb.IsPlaintext(path); plain {
		t.Error("a keyed local file is plaintext")
	}
	if _, err := MakeConnector(filepath.Join(t.TempDir(), "x.db"), "", "", base64.StdEncoding.EncodeToString(k[:16])); err == nil {
		t.Error("a 16-byte key was accepted")
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
