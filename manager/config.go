package manager

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bbmumford/tursoraft/internal/cluster"
	"github.com/bbmumford/tursoraft/mesh"
	"github.com/bbmumford/tursoraft/replica"
)

// Validation errors. Validate wraps each with the group or database it names.
var (
	ErrBadName          = errors.New("tursoraft: group, database or job name must match ^[a-z0-9][a-z0-9_-]*$")
	ErrDuplicateName    = errors.New("tursoraft: name declared twice")
	ErrBadEncryptionKey = errors.New("tursoraft: encryptionKey must be base64 of exactly 32 bytes")
	ErrNoURL            = errors.New("tursoraft: shared database has no url")
	ErrNoToken          = errors.New("tursoraft: shared database has no auth token")
	ErrRelativeDir      = errors.New("tursoraft: localDir must be an absolute path")
	ErrBadRaft          = errors.New("tursoraft: raft block is invalid")
	ErrNoMesh           = errors.New("tursoraft: a group with a raft block needs a mesh")
	ErrVoterOffVolume   = errors.New("tursoraft: a voter's raft state must sit under volumeDir")
	// ErrReplicaNeedsCGO is what NewManager returns when a build without cgo
	// declares a shared group.
	ErrReplicaNeedsCGO = replica.ErrReplicaNeedsCGO
)

// nameRE is the shape of every group, database and job name. Names become path
// elements and mesh keys, so the pattern admits no separator, dot or upper case.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// KeyLen is the only group key length accepted.
const KeyLen = 32

// ManagerConfig configures a Manager.
type ManagerConfig struct {
	// Groups are the database groups this process holds.
	Groups []GroupConfig `json:"groups"`
	// RaftConfig carries LocalDir, under which every file lives:
	// replicas/<group>/<db>.db, local/<group>/<db>.db and raft/<group>/<db>/.
	RaftConfig RaftConfig `json:"raftConfig"`
	// VolumeDir is the root of the persistent volume. A process whose service
	// votes in any cluster must set it and keep LocalDir under it, because a
	// voter that loses its Raft state on restart rejoins as a stranger.
	VolumeDir string `json:"volumeDir"`
	// SyncInterval is the pull period of a group that sets none; zero means
	// five seconds.
	SyncInterval time.Duration `json:"syncInterval"`
	// OpenTimeout bounds each replica's first open; zero means sixty seconds.
	OpenTimeout time.Duration `json:"openTimeout"`
	// Mesh is the network the Raft clusters run over. Required when any group
	// declares a raft block.
	Mesh mesh.Mesh `json:"-"`
	// Logger receives the manager's alerts; nil means slog.Default.
	Logger *slog.Logger `json:"-"`

	// TursoConfig is ignored: a Manager opens every shared database from its
	// url and token and never calls the platform API.
	//
	// Deprecated: ignored; loom's node runtime fills it.
	TursoConfig TursoConfig `json:"tursoConfig"`
	// PeerEnvVar is ignored: members find each other on the mesh.
	//
	// Deprecated: ignored; loom's mesh ledger sets it.
	PeerEnvVar string `json:"peerEnvVar"`
	// SnapMinGap is ignored: Raft snapshots hold assignments, not rows.
	//
	// Deprecated: ignored; loom's node runtime sets it.
	SnapMinGap time.Duration `json:"snapMinGap"`
	// UseEmbedded is ignored: every shared database is an embedded replica
	// and every pureLocal database a local file.
	//
	// Deprecated: ignored; loom's node runtime sets it.
	UseEmbedded bool `json:"useEmbedded"`

	// timing overrides the cluster timing; tests shorten it.
	timing cluster.Timing
}

// RaftConfig carries the directory every file lives under.
type RaftConfig struct {
	// LocalDir is the absolute directory for replicas, local files and Raft
	// state.
	LocalDir string `json:"localDir"`
	// RaftBindAddr is ignored: Raft runs over ManagerConfig.Mesh.
	//
	// Deprecated: ignored; loom's node runtime sets it.
	RaftBindAddr string `json:"raftBindAddr"`
	// SnapshotDir is ignored: Raft snapshots sit under LocalDir.
	//
	// Deprecated: ignored; loom's node runtime sets it.
	SnapshotDir string `json:"snapshotDir"`
}

// TursoConfig is the platform account. A Manager never reads it; the
// tursoraft-admin command uses its own flags.
//
// Deprecated: ignored; loom's node runtime fills ManagerConfig.TursoConfig.
type TursoConfig struct {
	APIToken string `json:"apiToken"`
	OrgName  string `json:"orgName"`
	BaseURL  string `json:"baseUrl"`
}

// GroupConfig is one database group.
type GroupConfig struct {
	// GroupName names the group.
	GroupName string `json:"groupName"`
	// AuthToken is the token of each shared database whose Databases entry
	// carries none.
	AuthToken string `json:"authToken"`
	// DBNames lists the group's databases. In a shared group each must also
	// appear in Databases, which carries its url.
	DBNames []string `json:"dbNames"`
	// EncryptionKey is base64 of exactly 32 bytes; every file of the group is
	// encrypted under it and Raft members prove they hold it.
	EncryptionKey string `json:"encryptionKey"`
	// PureLocal makes every database of the group a local file this process
	// alone writes, with no primary.
	PureLocal bool `json:"pureLocal"`

	// PriorEncryptionKey is the group's previous key during a rotation. It is
	// accepted in the Raft member proof only.
	PriorEncryptionKey string `json:"priorEncryptionKey,omitempty"`
	// Databases carries each shared database's primary url and optional
	// database-scoped token.
	Databases []DatabaseConfig `json:"databases,omitempty"`
	// SyncInterval is this group's pull period; zero means the manager's.
	SyncInterval time.Duration `json:"syncInterval,omitempty"`
	// Raft declares the group's singleton jobs; nil means it has none.
	Raft *RaftSpec `json:"raft,omitempty"`
}

// DatabaseConfig is one shared database's primary.
type DatabaseConfig struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	AuthToken string `json:"authToken,omitempty"`
}

// RaftSpec is a group's raft block: which services vote on who runs each
// singleton job, and which services may run it.
type RaftSpec struct {
	VoterServices   []string            `json:"voterServices"`
	VoterCount      int                 `json:"voterCount"`
	BootstrapExpect int                 `json:"bootstrapExpect"`
	Jobs            map[string][]string `json:"jobs"`
}

func (r RaftSpec) spec() cluster.Spec {
	return cluster.Spec{VoterServices: r.VoterServices, VoterCount: r.VoterCount, BootstrapExpect: r.BootstrapExpect, Jobs: r.Jobs}
}

// dbSpec is one resolved database of a group.
type dbSpec struct {
	name, url, token string
}

// databases resolves the group's databases in declaration order: DBNames
// first, then any name only Databases carries.
func (g GroupConfig) databases() []dbSpec {
	byName := map[string]DatabaseConfig{}
	for _, d := range g.Databases {
		byName[d.Name] = d
	}
	var out []dbSpec
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		d := byName[name]
		token := d.AuthToken
		if token == "" {
			token = g.AuthToken
		}
		out = append(out, dbSpec{name: name, url: d.URL, token: token})
	}
	for _, n := range g.DBNames {
		add(n)
	}
	for _, d := range g.Databases {
		add(d.Name)
	}
	return out
}

// keys decodes the group's current key and, when set, its prior one.
func (g GroupConfig) keys() ([][]byte, error) {
	key, err := decodeKey(g.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("group %q: %w", g.GroupName, err)
	}
	out := [][]byte{key}
	if g.PriorEncryptionKey != "" {
		prior, err := decodeKey(g.PriorEncryptionKey)
		if err != nil {
			return nil, fmt.Errorf("group %q priorEncryptionKey: %w", g.GroupName, err)
		}
		out = append(out, prior)
	}
	return out, nil
}

func decodeKey(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(key) != KeyLen {
		return nil, ErrBadEncryptionKey
	}
	return key, nil
}

// Validate checks everything NewManager needs before it touches the network
// or the disk: absolute directories, names that are safe path elements, a
// 32-byte key for every group, a url and a token for every shared database, a
// well-formed raft block over exactly one shared database, a mesh for every
// raft block, and a voter's state under the volume.
func (cfg ManagerConfig) Validate() error {
	if len(cfg.Groups) == 0 {
		return nil
	}
	dir := cfg.RaftConfig.LocalDir
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: %q", ErrRelativeDir, dir)
	}
	seen := map[string]bool{}
	for _, g := range cfg.Groups {
		if !nameRE.MatchString(g.GroupName) {
			return fmt.Errorf("%w: group %q", ErrBadName, g.GroupName)
		}
		if seen[g.GroupName] {
			return fmt.Errorf("%w: group %q", ErrDuplicateName, g.GroupName)
		}
		seen[g.GroupName] = true
		if err := cfg.validateGroup(g); err != nil {
			return err
		}
	}
	return nil
}

func (cfg ManagerConfig) validateGroup(g GroupConfig) error {
	if _, err := g.keys(); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, n := range g.DBNames {
		if names[n] {
			return fmt.Errorf("%w: group %q database %q", ErrDuplicateName, g.GroupName, n)
		}
		names[n] = true
	}
	withURL := map[string]bool{}
	for _, d := range g.Databases {
		if withURL[d.Name] {
			return fmt.Errorf("%w: group %q database %q", ErrDuplicateName, g.GroupName, d.Name)
		}
		withURL[d.Name] = true
	}
	dbs := g.databases()
	for _, d := range dbs {
		if !nameRE.MatchString(d.name) {
			return fmt.Errorf("%w: group %q database %q", ErrBadName, g.GroupName, d.name)
		}
		if g.PureLocal {
			continue
		}
		if !replica.Supported {
			return fmt.Errorf("group %q: %w", g.GroupName, ErrReplicaNeedsCGO)
		}
		if d.url == "" {
			return fmt.Errorf("%w: group %q database %q", ErrNoURL, g.GroupName, d.name)
		}
		if d.token == "" {
			return fmt.Errorf("%w: group %q database %q", ErrNoToken, g.GroupName, d.name)
		}
	}
	if g.Raft == nil {
		return nil
	}
	return cfg.validateRaft(g, len(dbs))
}

func (cfg ManagerConfig) validateRaft(g GroupConfig, databases int) error {
	r := g.Raft
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: group %q: %s", ErrBadRaft, g.GroupName, fmt.Sprintf(format, a...))
	}
	switch {
	case g.PureLocal:
		return bad("a pureLocal group has no shared database to fence")
	case databases != 1:
		return bad("the group must hold exactly one database, it holds %d", databases)
	case len(r.VoterServices) == 0:
		return bad("voterServices is empty")
	case r.VoterCount < 1:
		return bad("voterCount must be at least 1")
	case r.BootstrapExpect < 1 || r.BootstrapExpect > r.VoterCount:
		return bad("bootstrapExpect must be between 1 and voterCount")
	}
	for name, hosts := range r.Jobs {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("%w: group %q job %q", ErrBadName, g.GroupName, name)
		}
		if len(hosts) == 0 {
			return bad("job %q names no host service", name)
		}
	}
	if cfg.Mesh == nil {
		return fmt.Errorf("%w: group %q", ErrNoMesh, g.GroupName)
	}
	if !r.spec().IsVoterService(cfg.Mesh.LocalService()) {
		return nil
	}
	vol := cfg.VolumeDir
	rel, err := filepath.Rel(vol, cfg.RaftConfig.LocalDir)
	if vol == "" || !filepath.IsAbs(vol) || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: group %q localDir %q volumeDir %q", ErrVoterOffVolume, g.GroupName, cfg.RaftConfig.LocalDir, vol)
	}
	return nil
}

// Paths under LocalDir. Each kind of file has its own prefix, so a database
// and Raft's own files never share a name whatever the group and database are
// called.
func replicaPath(dir, group, db string) string {
	return filepath.Join(dir, "replicas", group, db+".db")
}

func localPath(dir, group, db string) string {
	return filepath.Join(dir, "local", group, db+".db")
}

func raftDir(dir, group, db string) string {
	return filepath.Join(dir, "raft", group, db)
}
