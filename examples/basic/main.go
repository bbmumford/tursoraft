// Example: Basic TursoRaft distributed database cluster
//
// This demonstrates setting up a distributed Turso/libraryQL cluster with
// Raft consensus for strong consistency across nodes.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/bbmumford/tursoraft/manager"
)

func main() {
	// Create configuration using the manager.ManagerConfig type
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: os.Getenv("TURSO_API_TOKEN"),
			OrgName:  os.Getenv("TURSO_ORG_NAME"),
		},
		Groups: []manager.GroupConfig{
			{
				GroupName: "primary",
				AuthToken: os.Getenv("TURSO_DB_TOKEN"),
				DBNames:   []string{"users", "sessions"},
			},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     "./data/tursoraft",
			SnapshotDir:  "./data/tursoraft/snapshots",
			RaftBindAddr: ":8300",
		},
		PeerEnvVar:   "TURSORAFT_PEERS",
		SyncInterval: 5 * time.Minute,
		SnapMinGap:   2 * time.Minute,
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	// Create and start manager
	ctx := context.Background()
	mgr, err := manager.NewManager(ctx, cfg)
	if err != nil {
		log.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Shutdown()

	// Get database handle using the Database method
	db, err := mgr.Database("primary", "users")
	if err != nil {
		log.Fatalf("failed to get database: %v", err)
	}

	// Query using the sql.DB handle
	rows, err := db.Query("SELECT id, name FROM users LIMIT 10")
	if err != nil {
		log.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	fmt.Println("Users:")
	for rows.Next() {
		var id int
		var name string
		rows.Scan(&id, &name)
		fmt.Printf("  %d: %s\n", id, name)
	}

	// Check if this node is the leader
	if mgr.IsLeader() {
		fmt.Println("This node is the Raft leader")
	}

	// Alternative: Use Exec/Query methods with group/db routing
	err = mgr.Exec(ctx, "primary", "sessions", "DELETE FROM sessions WHERE expired_at < ?", time.Now().Unix())
	if err != nil {
		log.Printf("cleanup failed: %v", err)
	}
}
