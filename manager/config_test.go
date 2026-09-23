package manager

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bbmumford/tursoraft/mesh/meshtest"
	"github.com/bbmumford/tursoraft/replica"
)

func b64Key() string {
	k := make([]byte, 32)
	rand.Read(k)
	return base64.StdEncoding.EncodeToString(k)
}

// sharedGroup is a valid shared group of one database with a raft block.
func sharedGroup() GroupConfig {
	return GroupConfig{
		GroupName:     "control",
		DBNames:       []string{"orbtr-control"},
		EncryptionKey: b64Key(),
		Databases:     []DatabaseConfig{{Name: "orbtr-control", URL: "libsql://orbtr-control.example", AuthToken: "db-token"}},
		Raft: &RaftSpec{
			VoterServices: []string{"node.orbtr.ai", "node.orbtr.io"}, VoterCount: 5, BootstrapExpect: 3,
			Jobs: map[string][]string{"projector": {"api.orbtr.ai", "node.orbtr.io"}},
		},
	}
}

func validConfig(t *testing.T) ManagerConfig {
	t.Helper()
	vol := t.TempDir()
	hub := meshtest.NewHub()
	n := hub.Node("n1", "node.orbtr.ai")
	t.Cleanup(func() { n.Close() })
	return ManagerConfig{
		Groups:     []GroupConfig{sharedGroup()},
		RaftConfig: RaftConfig{LocalDir: filepath.Join(vol, "tursoraft")},
		VolumeDir:  vol,
		Mesh:       n,
	}
}

func TestValidConfigPasses(t *testing.T) {
	if !replica.Supported {
		t.Skip("a shared group needs cgo")
	}
	if err := validConfig(t).Validate(); err != nil {
		t.Fatal(err)
	}
}

// Every refusal Validate makes, each before any network or disk access.
func TestValidateRefuses(t *testing.T) {
	if !replica.Supported {
		t.Skip("a shared group needs cgo")
	}
	cases := []struct {
		name string
		edit func(*ManagerConfig)
		want error
	}{
		{"relative localDir", func(c *ManagerConfig) { c.RaftConfig.LocalDir = "./data/lad" }, ErrRelativeDir},
		{"no key", func(c *ManagerConfig) { c.Groups[0].EncryptionKey = "" }, ErrBadEncryptionKey},
		{"16-byte key", func(c *ManagerConfig) {
			c.Groups[0].EncryptionKey = base64.StdEncoding.EncodeToString(make([]byte, 16))
		}, ErrBadEncryptionKey},
		{"bad prior key", func(c *ManagerConfig) { c.Groups[0].PriorEncryptionKey = "short" }, ErrBadEncryptionKey},
		{"no url", func(c *ManagerConfig) { c.Groups[0].Databases[0].URL = "" }, ErrNoURL},
		{"name only in dbNames", func(c *ManagerConfig) { c.Groups[0].Databases = nil }, ErrNoURL},
		{"no token", func(c *ManagerConfig) { c.Groups[0].Databases[0].AuthToken = "" }, ErrNoToken},
		{"group name with a separator", func(c *ManagerConfig) { c.Groups[0].GroupName = "a/b" }, ErrBadName},
		{"database name with a dot", func(c *ManagerConfig) {
			c.Groups[0].DBNames = []string{"x.db"}
			c.Groups[0].Databases[0].Name = "x.db"
		}, ErrBadName},
		{"duplicate group", func(c *ManagerConfig) { c.Groups = append(c.Groups, c.Groups[0]) }, ErrDuplicateName},
		{"raft over two databases", func(c *ManagerConfig) {
			c.Groups[0].Databases = append(c.Groups[0].Databases, DatabaseConfig{Name: "two", URL: "libsql://two", AuthToken: "t"})
		}, ErrBadRaft},
		{"raft on a pureLocal group", func(c *ManagerConfig) { c.Groups[0].PureLocal = true }, ErrBadRaft},
		{"no voters", func(c *ManagerConfig) { c.Groups[0].Raft.VoterServices = nil }, ErrBadRaft},
		{"bootstrapExpect above voterCount", func(c *ManagerConfig) { c.Groups[0].Raft.BootstrapExpect = 6 }, ErrBadRaft},
		{"job with no host", func(c *ManagerConfig) { c.Groups[0].Raft.Jobs["idle"] = nil }, ErrBadRaft},
		{"raft with no mesh", func(c *ManagerConfig) { c.Mesh = nil }, ErrNoMesh},
		{"voter with no volume", func(c *ManagerConfig) { c.VolumeDir = "" }, ErrVoterOffVolume},
		{"voter state off the volume", func(c *ManagerConfig) { c.RaftConfig.LocalDir = "/tmp/elsewhere" }, ErrVoterOffVolume},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			tc.edit(&cfg)
			if err := cfg.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// A job host that does not vote needs no volume.
func TestAHostThatDoesNotVoteNeedsNoVolume(t *testing.T) {
	if !replica.Supported {
		t.Skip("a shared group needs cgo")
	}
	cfg := validConfig(t)
	hub := meshtest.NewHub()
	api := hub.Node("api1", "api.orbtr.ai")
	defer api.Close()
	cfg.Mesh, cfg.VolumeDir = api, ""
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The fields loom's node runtime sets and a Manager ignores are accepted.
func TestIgnoredFieldsAreAccepted(t *testing.T) {
	cfg := ManagerConfig{
		Groups:      []GroupConfig{{GroupName: "mesh", DBNames: []string{"lad"}, PureLocal: true, EncryptionKey: b64Key()}},
		RaftConfig:  RaftConfig{LocalDir: t.TempDir(), RaftBindAddr: ":7331", SnapshotDir: "/x"},
		TursoConfig: TursoConfig{APIToken: "org-token", OrgName: "orbtr"},
		PeerEnvVar:  "MESHDB_PEERS",
		UseEmbedded: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
