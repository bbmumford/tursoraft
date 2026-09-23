// Command basic opens one shared database as an encrypted embedded replica
// and one local file, writes and reads both, and shuts down.
//
//	TURSORAFT_URL=libsql://orbtr-sessions-orbtr.turso.io \
//	TURSORAFT_TOKEN=<database token> \
//	TURSORAFT_KEY=$(head -c32 /dev/urandom | base64) \
//	go run ./examples/basic
//
// The url and a database-scoped token come from tursoraft-admin ensure and
// tursoraft-admin token. The key is the group's 32-byte key, base64.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/bbmumford/tursoraft/manager"
)

func main() {
	dir, err := filepath.Abs("tursoraft-example")
	if err != nil {
		log.Fatal(err)
	}
	key := os.Getenv("TURSORAFT_KEY")
	cfg := manager.ManagerConfig{
		Groups: []manager.GroupConfig{
			{
				GroupName:     "sessions",
				DBNames:       []string{"orbtr-sessions"},
				EncryptionKey: key,
				Databases: []manager.DatabaseConfig{{
					Name: "orbtr-sessions", URL: os.Getenv("TURSORAFT_URL"), AuthToken: os.Getenv("TURSORAFT_TOKEN"),
				}},
			},
			{GroupName: "local", DBNames: []string{"cache"}, PureLocal: true, EncryptionKey: key},
		},
		RaftConfig:   manager.RaftConfig{LocalDir: dir},
		SyncInterval: 5 * time.Second,
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx := context.Background()
	mgr, err := manager.NewManager(ctx, cfg)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer mgr.Shutdown()

	db, err := mgr.Database("sessions", "orbtr-sessions")
	if err != nil {
		log.Fatal(err)
	}
	// Idempotent schema: every instance applies it at boot.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS visits(id INTEGER PRIMARY KEY, at TEXT NOT NULL)`); err != nil {
		log.Fatal(err)
	}
	// The write reaches the primary before it returns.
	if _, err := db.ExecContext(ctx, `INSERT INTO visits(at) VALUES (?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM visits`).Scan(&n); err != nil {
		log.Fatal(err)
	}
	h, err := mgr.Health("sessions", "orbtr-sessions")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("visits: %d (replica frame %d, last pull %s)\n", n, h.Frame, h.LastPull.Format(time.RFC3339))

	local, err := mgr.Database("local", "cache")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := local.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS kv(k TEXT PRIMARY KEY, v TEXT)`); err != nil {
		log.Fatal(err)
	}
}
