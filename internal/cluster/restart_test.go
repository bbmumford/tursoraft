package cluster

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/mesh/meshtest"
)

// Restarting every voter in the same process on the same directories keeps
// the cluster: each finds its state, none bootstraps again, and the cluster
// ID is the one it had.
func TestRestartInProcessKeepsTheCluster(t *testing.T) {
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 3, BootstrapExpect: 3, Jobs: map[string][]string{"sweep": {svcAI}}}
	e := newEnv(t, spec, fastTiming())
	ids := []string{"a1", "a2", "a3"}
	for _, id := range ids {
		e.register(e.start(id, svcAI), "sweep")
	}
	eventually(t, 20*time.Second, "a held job", func() bool { return len(e.holders("sweep")) == 1 })
	first := e.live()[0].c.ClusterID()
	for _, n := range e.live() {
		e.stop(n)
	}
	for _, id := range ids {
		n := e.start(id, svcAI)
		if n.c.raft.LastIndex() == 0 {
			t.Errorf("%s restarted with no raft state", id)
		}
		e.register(n, "sweep")
	}
	eventually(t, 20*time.Second, "a held job after restart", func() bool { return len(e.holders("sweep")) == 1 })
	for _, n := range e.live() {
		if got := n.c.ClusterID(); got != first {
			t.Errorf("%s is in cluster %s after restart, was %s", n.id, got, first)
		}
	}
}

// A second Start on raft state another Start in this process still holds
// fails instead of waiting forever on the file lock.
func TestSecondOpenOfTheSameStateFails(t *testing.T) {
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 1, BootstrapExpect: 1, Jobs: map[string][]string{}}
	e := newEnv(t, spec, fastTiming())
	n := e.start("a1", svcAI)
	m := e.hub.Node("a1-again", svcAI)
	defer m.Close()
	done := make(chan error, 1)
	go func() {
		c, err := Start(Config{Group: "control", DB: "orbtr-control", Spec: spec, Mesh: m, Keys: e.keys, Dir: n.dir, Logger: quietLogger(t), Timing: e.timing})
		if err == nil {
			c.Shutdown()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a second Start opened state another Start holds")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a second Start on held state did not return")
	}
}

// childEnv names the environment a re-executed test binary reads to run one
// cluster member.
const childEnv = "TURSORAFT_CLUSTER_CHILD"

// Voters in separate processes, killed with SIGKILL and started again on the
// same directories, rejoin the same cluster without bootstrapping.
func TestRestartAcrossProcessesKeepsTheCluster(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		runChild()
		return
	}
	root := t.TempDir()
	meshDir := filepath.Join(root, "mesh")
	os.MkdirAll(meshDir, 0o700)
	ids := []string{"a1", "a2", "a3"}

	spawn := func(round int) map[string]*exec.Cmd {
		cmds := map[string]*exec.Cmd{}
		for _, id := range ids {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRestartAcrossProcessesKeepsTheCluster$")
			cmd.Env = append(os.Environ(),
				childEnv+"="+id,
				"TURSORAFT_CHILD_ROOT="+root,
				fmt.Sprintf("TURSORAFT_CHILD_ROUND=%d", round))
			cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			cmds[id] = cmd
		}
		t.Cleanup(func() {
			for _, c := range cmds {
				c.Process.Kill()
				c.Wait()
			}
		})
		return cmds
	}
	read := func(round int) map[string]string {
		out := map[string]string{}
		eventually(t, 30*time.Second, fmt.Sprintf("round %d reports", round), func() bool {
			for _, id := range ids {
				raw, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("%s.%d.out", id, round)))
				if err != nil {
					return false
				}
				out[id] = string(raw)
			}
			return true
		})
		return out
	}

	cmds := spawn(1)
	first := read(1)
	var clusterID string
	for id, r := range first {
		cid, _, _ := strings.Cut(r, " ")
		// A member that reports no cluster ID is not in the cluster round 2
		// then compares itself against: clusterID takes the first report that
		// carries one, so without this an empty ID agrees with every other
		// report and the round passes on a member that never applied one.
		if cid == "" {
			t.Fatalf("round 1: %s reports no cluster ID: %q", id, r)
		}
		if clusterID == "" {
			clusterID = cid
		}
		if cid != clusterID {
			t.Fatalf("round 1: %s reports cluster %s, another %s", id, cid, clusterID)
		}
	}
	for _, c := range cmds {
		c.Process.Signal(syscall.SIGKILL)
		c.Wait()
	}
	spawn(2)
	for id, r := range read(2) {
		cid, state, _ := strings.Cut(r, " ")
		if cid != clusterID {
			t.Errorf("round 2: %s is in cluster %s, was %s", id, cid, clusterID)
		}
		if state != "state=1" {
			t.Errorf("round 2: %s started with no raft state", id)
		}
	}
}

// runChild is one member process of TestRestartAcrossProcessesKeepsTheCluster.
func runChild() {
	id, root, round := os.Getenv(childEnv), os.Getenv("TURSORAFT_CHILD_ROOT"), os.Getenv("TURSORAFT_CHILD_ROUND")
	m, err := meshtest.NewLoopback(filepath.Join(root, "mesh"), id, svcAI)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	spec := Spec{VoterServices: []string{svcAI}, VoterCount: 3, BootstrapExpect: 3, Jobs: map[string][]string{}}
	c, err := Start(Config{
		Group: "control", DB: "orbtr-control", Spec: spec, Mesh: m, Keys: [][]byte{key},
		Dir: filepath.Join(root, id), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Timing: fastTiming(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	state := "state=0"
	if c.raft.LastIndex() > 0 {
		state = "state=1"
	}
	deadline := time.Now().Add(60 * time.Second)
	// Report once this member has applied the cluster ID from the log and
	// knows a leader, not from the ID it recorded before the restart.
	for (c.fsm.clusterID() == "" || c.Leader() == "") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	report := filepath.Join(root, fmt.Sprintf("%s.%s.out", id, round))
	if err := writeReport(report, []byte(c.fsm.clusterID()+" "+state)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	time.Sleep(time.Until(deadline))
	os.Exit(0)
}

// writeReport gives the parent's read a member's report only whole: the
// bytes go to a sibling file, which is then renamed onto the name read polls
// for. read takes any file it can open as the report, so a report written in
// place can be read between its creation and its bytes, as an empty cluster
// ID.
func writeReport(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
