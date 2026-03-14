package manager_test

import (
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/manager"
)

func TestDefaultManagerConfig(t *testing.T) {
	cfg := manager.DefaultManagerConfig()

	if cfg.RaftConfig.LocalDir != "./meshdb_data" {
		t.Errorf("expected LocalDir './meshdb_data', got '%s'", cfg.RaftConfig.LocalDir)
	}
	if cfg.RaftConfig.SnapshotDir != "./meshdb_snapshots" {
		t.Errorf("expected SnapshotDir './meshdb_snapshots', got '%s'", cfg.RaftConfig.SnapshotDir)
	}
	if cfg.RaftConfig.RaftBindAddr != ":8300" {
		t.Errorf("expected RaftBindAddr ':8300', got '%s'", cfg.RaftConfig.RaftBindAddr)
	}
	if cfg.PeerEnvVar != "MESHDB_PEERS" {
		t.Errorf("expected PeerEnvVar 'MESHDB_PEERS', got '%s'", cfg.PeerEnvVar)
	}
	if cfg.SyncInterval != 6*time.Hour {
		t.Errorf("expected SyncInterval 6h, got %v", cfg.SyncInterval)
	}
	if cfg.SnapMinGap != 2*time.Minute {
		t.Errorf("expected SnapMinGap 2m, got %v", cfg.SnapMinGap)
	}
	if cfg.UseEmbedded {
		t.Error("expected UseEmbedded to be false by default")
	}
}

func TestManagerConfigValidate_MissingAPIToken(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			OrgName: "test-org",
			// APIToken missing
		},
		Groups: []manager.GroupConfig{
			{GroupName: "test", AuthToken: "token", DBNames: []string{"db1"}},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     "/tmp/test",
			SnapshotDir:  "/tmp/snapshots",
			RaftBindAddr: ":8300",
		},
		UseEmbedded: false, // Non-embedded requires API token
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected validation error for missing API token")
	}
}

func TestManagerConfigValidate_MissingOrgName(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: "test-token",
			// OrgName missing
		},
		Groups: []manager.GroupConfig{
			{GroupName: "test", AuthToken: "token", DBNames: []string{"db1"}},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     "/tmp/test",
			SnapshotDir:  "/tmp/snapshots",
			RaftBindAddr: ":8300",
		},
		UseEmbedded: false,
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected validation error for missing org name")
	}
}

func TestManagerConfigValidate_EmptyGroups(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: "test-token",
			OrgName:  "test-org",
		},
		Groups: []manager.GroupConfig{}, // Empty
		RaftConfig: manager.RaftConfig{
			LocalDir:     "/tmp/test",
			SnapshotDir:  "/tmp/snapshots",
			RaftBindAddr: ":8300",
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected validation error for empty groups")
	}
}

func TestManagerConfigValidate_MissingGroupName(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: "test-token",
			OrgName:  "test-org",
		},
		Groups: []manager.GroupConfig{
			{GroupName: "", AuthToken: "token", DBNames: []string{"db1"}},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     "/tmp/test",
			SnapshotDir:  "/tmp/snapshots",
			RaftBindAddr: ":8300",
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected validation error for missing group name")
	}
}

func TestManagerConfigValidate_MissingAuthToken(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: "test-token",
			OrgName:  "test-org",
		},
		Groups: []manager.GroupConfig{
			{GroupName: "test", AuthToken: "", DBNames: []string{"db1"}},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     "/tmp/test",
			SnapshotDir:  "/tmp/snapshots",
			RaftBindAddr: ":8300",
		},
		UseEmbedded: false,
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected validation error for missing auth token")
	}
}

func TestManagerConfigValidate_EmptyDBNames(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: "test-token",
			OrgName:  "test-org",
		},
		Groups: []manager.GroupConfig{
			{GroupName: "test", AuthToken: "token", DBNames: []string{}},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     "/tmp/test",
			SnapshotDir:  "/tmp/snapshots",
			RaftBindAddr: ":8300",
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected validation error for empty DB names")
	}
}

func TestManagerConfigValidate_MissingRaftConfig(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: "test-token",
			OrgName:  "test-org",
		},
		Groups: []manager.GroupConfig{
			{GroupName: "test", AuthToken: "token", DBNames: []string{"db1"}},
		},
		RaftConfig: manager.RaftConfig{
			// Missing LocalDir, SnapshotDir, RaftBindAddr
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected validation error for missing raft config")
	}
}

func TestManagerConfigValidate_EmbeddedModeNoAPIToken(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			// No API token needed in embedded mode
		},
		Groups: []manager.GroupConfig{
			{GroupName: "test", DBNames: []string{"db1"}},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     "/tmp/test",
			SnapshotDir:  "/tmp/snapshots",
			RaftBindAddr: ":8300",
		},
		UseEmbedded: true, // Embedded mode doesn't require Turso credentials
	}

	err := cfg.Validate()
	if err != nil {
		t.Errorf("expected no error in embedded mode, got: %v", err)
	}
}

func TestManagerConfigValidate_PureLocalGroupNoAuthToken(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: "test-token",
			OrgName:  "test-org",
		},
		Groups: []manager.GroupConfig{
			{GroupName: "local", DBNames: []string{"cache"}, PureLocal: true},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     "/tmp/test",
			SnapshotDir:  "/tmp/snapshots",
			RaftBindAddr: ":8300",
		},
		UseEmbedded: false,
	}

	err := cfg.Validate()
	if err != nil {
		t.Errorf("expected no error for pure-local group without auth token, got: %v", err)
	}
}

func TestManagerConfigValidate_Success(t *testing.T) {
	cfg := manager.ManagerConfig{
		TursoConfig: manager.TursoConfig{
			APIToken: "test-token",
			OrgName:  "test-org",
		},
		Groups: []manager.GroupConfig{
			{GroupName: "test", AuthToken: "token", DBNames: []string{"db1"}},
		},
		RaftConfig: manager.RaftConfig{
			LocalDir:     "/tmp/test",
			SnapshotDir:  "/tmp/snapshots",
			RaftBindAddr: ":8300",
		},
	}

	err := cfg.Validate()
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}
