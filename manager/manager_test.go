package manager

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/bbmumford/tursoraft/database"
)

// A pureLocal group opens encrypted local files that work at once; they have
// no replica to report on; with no raft block IsLeader keeps its old meaning,
// true; and Shutdown leaves no goroutine behind.
func TestLocalGroupOpensAndShutsDownClean(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m, err := NewManager(context.Background(), ManagerConfig{
		Groups:     []GroupConfig{{GroupName: "mesh", DBNames: []string{"lad"}, PureLocal: true, EncryptionKey: b64Key()}},
		RaftConfig: RaftConfig{LocalDir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := m.Database("mesh", "lad")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SyncStats(context.Background(), "mesh", "lad"); !errors.Is(err, database.ErrSyncStatsUnavailable) {
		t.Errorf("SyncStats on a local file: got %v, want ErrSyncStatsUnavailable", err)
	}
	if !m.IsLeader() {
		t.Error("IsLeader with no cluster declared must be true")
	}
	if _, err := m.Database("mesh", "other"); !errors.Is(err, ErrUnknownDatabase) {
		t.Errorf("unknown database: got %v", err)
	}
	if _, err := m.Job("mesh", "lad", "sweep", nil); !errors.Is(err, ErrNoCluster) {
		t.Errorf("a job on a group with no raft block: got %v", err)
	}
	if err := m.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(); err != nil {
		t.Errorf("a second Shutdown: %v", err)
	}
}

// Files of different kinds never share a path, whatever the group and
// database are called: a group "raft" holding a database "logs" would
// otherwise put the database where Raft keeps its log.
func TestPathsKeepDatabasesAndRaftStateApart(t *testing.T) {
	dir := "/data/tursoraft"
	paths := map[string]bool{
		replicaPath(dir, "raft", "logs"):            true,
		localPath(dir, "raft", "logs"):              true,
		raftDir(dir, "raft", "logs") + "/logs.db":   true,
		raftDir(dir, "raft", "logs") + "/stable.db": true,
	}
	if len(paths) != 4 {
		t.Fatalf("paths collide: %v", paths)
	}
}

// manager/ never imports the platform API client: nothing at boot may need
// an organisation token.
func TestManagerDoesNotImportThePlatformClient(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasSuffix(dep, "tursoraft/internal/turso") {
			t.Fatalf("manager depends on %s", dep)
		}
	}
}

// Shutdown on a manager with nothing opened returns at once.
func TestEmptyManager(t *testing.T) {
	m, err := NewManager(context.Background(), ManagerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown of an empty manager did not return")
	}
}
