// Command cluster runs one member of a Raft cluster that decides which
// process runs a singleton job, and runs that job's writes through the fence.
//
// Start three members on one machine, each in its own terminal:
//
//	for id in n1 n2 n3; do
//	  TURSORAFT_URL=http://127.0.0.1:8080 TURSORAFT_TOKEN=local \
//	  TURSORAFT_KEY=<the same base64 32-byte key> \
//	  go run ./examples/cluster -id $id &
//	done
//
// The members find each other through meshtest's loopback mesh, a stand-in
// for the mesh runtime a deployment passes in ManagerConfig.Mesh. Kill the
// member printing "holding" and another takes the job over.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/bbmumford/tursoraft/fence"
	"github.com/bbmumford/tursoraft/manager"
	"github.com/bbmumford/tursoraft/mesh/meshtest"
)

func main() {
	id := flag.String("id", "n1", "this member's node id")
	flag.Parse()
	root, err := filepath.Abs("tursoraft-cluster-example")
	if err != nil {
		log.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "mesh"), 0o700)
	mesh, err := meshtest.NewLoopback(filepath.Join(root, "mesh"), *id, "node.orbtr.ai")
	if err != nil {
		log.Fatal(err)
	}
	defer mesh.Close()

	vol := filepath.Join(root, *id)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	mgr, err := manager.NewManager(ctx, manager.ManagerConfig{
		Groups: []manager.GroupConfig{{
			GroupName:     "control",
			DBNames:       []string{"orbtr-control"},
			EncryptionKey: os.Getenv("TURSORAFT_KEY"),
			Databases: []manager.DatabaseConfig{{
				Name: "orbtr-control", URL: os.Getenv("TURSORAFT_URL"), AuthToken: os.Getenv("TURSORAFT_TOKEN"),
			}},
			Raft: &manager.RaftSpec{
				VoterServices:   []string{"node.orbtr.ai"},
				VoterCount:      3,
				BootstrapExpect: 3,
				Jobs:            map[string][]string{"projector": {"node.orbtr.ai"}},
			},
		}},
		RaftConfig: manager.RaftConfig{LocalDir: filepath.Join(vol, "tursoraft")},
		VolumeDir:  vol,
		Mesh:       mesh,
	})
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer mgr.Shutdown()

	db, err := mgr.Database("control", "orbtr-control")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS projected(id INTEGER PRIMARY KEY, node TEXT, epoch INTEGER)`); err != nil {
		log.Fatal(err)
	}
	if _, err := mgr.Job("control", "orbtr-control", "projector", func(ctx context.Context, f fence.Fence) error {
		fmt.Printf("%s holding projector at epoch %d\n", *id, f.Epoch)
		for {
			// Every write of the job goes through the fence: a holder that
			// lost the job fails here with fence.ErrFenced.
			err := f.Tx(ctx, db, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO projected(node, epoch) VALUES (?, ?)`, *id, int64(f.Epoch))
				return err
			})
			if err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
	}); err != nil {
		log.Fatal(err)
	}
	<-ctx.Done()
}
