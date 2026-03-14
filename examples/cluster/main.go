// Example: Multi-node TursoRaft cluster with high availability
//
// This demonstrates configuring a multi-node cluster for production
// environments with proper peer discovery and failover.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bbmumford/tursoraft/manager"
)

func main() {
	nodeID := os.Getenv("NODE_ID")
	if nodeID == "" {
		nodeID = "node-1"
	}

	// Production-ready configuration
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: os.Getenv("TURSO_API_TOKEN"),
			OrgName:  os.Getenv("TURSO_ORG_NAME"),
		},
		Groups: []manager.GroupConfig{
			{
				GroupName: "production",
				AuthToken: os.Getenv("TURSO_DB_TOKEN"),
				DBNames:   []string{"app-data", "user-sessions", "audit-logs"},
			},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     fmt.Sprintf("./data/raft/%s", nodeID),
			SnapshotDir:  fmt.Sprintf("./data/raft/%s/snapshots", nodeID),
			RaftBindAddr: getEnvOrDefault("RAFT_BIND_ADDR", ":8300"),
		},
		// Peer discovery via environment variable
		// Format: "node-1=10.0.0.1:8300,node-2=10.0.0.2:8300,node-3=10.0.0.3:8300"
		PeerEnvVar: "TURSORAFT_PEERS",

		// Background sync settings
		SyncInterval: 5 * time.Minute,
		SnapMinGap:   2 * time.Minute,
	}

	// Validate
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config validation failed: %v", err)
	}

	// Create manager
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr, err := manager.NewManager(ctx, cfg)
	if err != nil {
		log.Fatalf("failed to create manager: %v", err)
	}

	// Start health/status HTTP server
	go startStatusServer(mgr, nodeID, ":8080")

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	log.Println("Shutting down...")

	// Graceful shutdown
	if err := mgr.Shutdown(); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}

func startStatusServer(mgr *manager.Manager, nodeID, addr string) {
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Cluster status endpoint
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		leaderAddr, _ := mgr.GetLeader()
		peers, _ := mgr.GetPeers()

		status := map[string]interface{}{
			"is_leader": mgr.IsLeader(),
			"node_id":   nodeID,
			"leader":    leaderAddr,
			"peers":     peers,
			"timestamp": time.Now().UTC(),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})

	// Ready check - simple health for now
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		// Check if we can reach leader (indicates cluster is operational)
		_, err := mgr.GetLeader()
		if err == nil {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		}
	})

	log.Printf("Status server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("status server error: %v", err)
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
