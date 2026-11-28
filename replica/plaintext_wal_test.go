//go:build cgo

package replica

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// Page 1 is judged in the WAL even when the database file holds an encrypted
// page 1: a reader takes page 1 from the WAL's frame of it over the file, so an
// unencrypted frame of page 1 over an encrypted file is refused, and so is an
// unencrypted frame that follows an encrypted one.
func TestAnUnencryptedPageOneInTheWALIsJudgedOverAnEncryptedFile(t *testing.T) {
	random := make([]byte, 4096)
	rand.Read(random)
	path := filepath.Join(t.TempDir(), "r.db")
	if err := os.WriteFile(path, random, 0o600); err != nil {
		t.Fatal(err)
	}
	walWithPageOne(t, path+"-wal", []uint32{1}, sqliteHeader)
	if why, err := unencrypted(path); err != nil || why == "" {
		t.Fatalf("an encrypted file under an unencrypted WAL page 1: why=%q err=%v, want refused", why, err)
	}
}
