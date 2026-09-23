package localdb_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bbmumford/tursoraft/localdb"
)

func key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	rand.Read(k)
	return k
}

func open(t *testing.T, path string, k []byte) *sql.DB {
	t.Helper()
	db, err := localdb.Open(context.Background(), path, k)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// An encrypted file takes a schema with a foreign key, every
// pooled connection has foreign keys on, and an orphan is refused on each.
func TestForeignKeysOnEveryPooledConnection(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "fk.db"), key(t))
	if _, err := db.Exec(`CREATE TABLE p(id INTEGER PRIMARY KEY);
		CREATE TABLE c(id INTEGER PRIMARY KEY, pid INTEGER REFERENCES p(id));
		INSERT INTO p VALUES (1); INSERT INTO c VALUES (1, 1)`); err != nil {
		t.Fatalf("create the schema: %v", err)
	}
	ctx := context.Background()
	db.SetMaxIdleConns(8)
	var conns []*sql.Conn
	for i := 0; i < 8; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		var fk int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Errorf("connection %d has foreign_keys=%d", i, fk)
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO c VALUES (?, 999)", 100+i); err == nil {
			t.Errorf("connection %d accepted an orphan", i)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

// The file carries no plaintext header and no schema text.
func TestLocalFileIsEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enc.db")
	db := open(t, path, key(t))
	if _, err := db.Exec("CREATE TABLE marker_table(note TEXT); INSERT INTO marker_table VALUES ('marker-row-text')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(path + suffix)
		if err != nil || len(raw) == 0 {
			continue
		}
		if bytes.HasPrefix(raw, []byte("SQLite format 3")) {
			t.Errorf("%s begins with the plaintext header", filepath.Base(path+suffix))
		}
		for _, m := range []string{"CREATE TABLE", "marker_table", "marker-row-text"} {
			if bytes.Contains(raw, []byte(m)) {
				t.Errorf("%s contains %q", filepath.Base(path+suffix), m)
			}
		}
	}
}

// Another key fails at open, not at a later query.
func TestWrongKeyFailsAtOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.db")
	db := open(t, path, key(t))
	if _, err := db.Exec("CREATE TABLE t(x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db2, err := localdb.Open(context.Background(), path, key(t)); err == nil {
		db2.Close()
		t.Fatal("a file opened under another key")
	}
}

// tursogo refuses a keyed open unless the encryption feature is named,
// which is why DSN names it.
func TestEncryptionNeedsTheExperimentalFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e6.db")
	k := key(t)
	db := open(t, path, k)
	if _, err := db.Exec("CREATE TABLE t(x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if !strings.Contains(localdb.DSN(path, k), "encryption") {
		t.Fatal("DSN does not name the encryption feature")
	}
	raw, err := sql.Open("turso", path+"?encryption_cipher=aes256gcm&encryption_hexkey="+hex.EncodeToString(k))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow("SELECT count(*) FROM t").Scan(&n); err == nil {
		t.Error("a keyed open without the encryption feature read the file")
	}
}

// A plaintext file under a configured key is refused and left in place: a
// local file is the only copy of its rows.
func TestPlaintextFileIsRefusedAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.db")
	plain, err := localdb.OpenPlaintext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec("CREATE TABLE keep(x INTEGER); INSERT INTO keep VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	plain.Close()
	_, err = localdb.Open(context.Background(), path, key(t))
	if !errors.Is(err, localdb.ErrPlaintext) {
		t.Fatalf("got %v, want ErrPlaintext", err)
	}
	if ok, _ := localdb.IsPlaintext(path); !ok {
		t.Error("the plaintext file was altered or removed")
	}
}

// Keys of the wrong length and relative paths are refused before the file is
// touched.
func TestBadKeyAndRelativePathAreRefused(t *testing.T) {
	if _, err := localdb.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), make([]byte, 16)); !errors.Is(err, localdb.ErrKeyLength) {
		t.Errorf("16-byte key: got %v", err)
	}
	if _, err := localdb.Open(context.Background(), "rel/x.db", key(t)); !errors.Is(err, localdb.ErrRelativePath) {
		t.Errorf("relative path: got %v", err)
	}
}
