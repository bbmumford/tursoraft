# tursoraft

tursoraft holds a process's shared and local SQL databases, and runs a Raft
cluster per shared database that decides which process runs each singleton job.

```sh
go get github.com/bbmumford/tursoraft
```

- **Shared database**: one authoritative copy, the *primary* (libSQL, in Turso
  Cloud or a sqld). Every process that holds it keeps an encrypted embedded
  *replica* (go-libsql). Reads are answered from the replica file; every write
  is sent to the primary and returns once the primary has it. Every holder
  writes; no write waits for Raft or a leader.
- **Local database**: a file one process owns, through tursogo, encrypted.
- **Raft cluster**: one per shared database that declares singleton jobs. It
  carries job assignments and a cluster ID, never rows. The leader gives each
  job to one live host and stamps the assignment with an *epoch* (the log
  index). The job's writes go through `fence.Fence.Tx`, which the primary
  orders by that epoch, so a holder that lost the job cannot write after the
  new holder has.

Raft runs over a `mesh.Mesh` the caller supplies (a mesh runtime implements
it; `mesh/meshtest` has an in-memory and a TCP-loopback one). Nothing in
tursoraft reads an environment variable or calls the Turso platform API at
boot.

## Packages

| Package | What it is |
|---|---|
| `manager` | `NewManager`, `Database`, `Job`, `Leader`, `Sync`, `SyncOnMiss`, `SyncStats`, `Health`, `Shutdown`, and the deprecated `IsLeader` |
| `replica` | One encrypted embedded replica from its spec (`Config`: mode, credential source, scope, key): the token check, open with retry, plaintext checks before and after the open, the read-only pool, sync loop, stats (cgo; a stub without) |
| `localdb` | One encrypted tursogo file through policypool |
| `fence` | `Fence.Tx`: epoch-fenced write transactions |
| `mesh`, `mesh/meshtest` | The transport contract Raft runs over, and test meshes |
| `connectors` | `MakeConnector`, one encrypted handle outside a Manager |
| `database` | `DatabaseHandle` and `SyncStats` |
| `pull` | The pull-trigger port, `pull.Trigger`: `RequestPull(ctx, segment, atLeastPosition)`; no dependency beyond the standard library |
| `sqldtest` | Starts a local sqld for a test that needs a primary; `StartWithAuth` runs it under JWT authentication with a `Signer` for its tokens |
| `cmd/tursoraft-admin` | Operator tool: `ensure`, `token`, `fence-adopt` |

Every `*sql.DB` tursoraft returns comes from `policypool`, and on every
connection a statement that would turn foreign keys off is refused.

- A **local file's** pool (`ForeignKeysRequired`) proves `foreign_keys=1` when
  each connection opens and again on every reuse: tursogo opens every
  connection with it off.
- A **replica's** pool (`ForeignKeysRequiredAtConnect`) proves it only when each
  connection opens. go-libsql sends that proof to the primary, so a proof on
  every reuse would cost each statement a round trip and fail every read while
  the primary is down. The setting cannot lapse in between: the libSQL primary
  starts every session with it on, and the guard refuses the only statement
  that turns it off.

## Configuration

```go
mgr, err := manager.NewManager(ctx, manager.ManagerConfig{
    Groups: []manager.GroupConfig{{
        GroupName:     "control",
        DBNames:       []string{"orbtr-control"},
        EncryptionKey: groupKey, // base64 of exactly 32 bytes
        Databases: []manager.DatabaseConfig{{
            Name: "orbtr-control", URL: "libsql://orbtr-control-orbtr.turso.io", AuthToken: dbToken,
        }},
        Raft: &manager.RaftSpec{
            VoterServices:   []string{"node-orbtr-ai", "node-orbtr-io"},
            VoterCount:      5,
            BootstrapExpect: 3,
            Jobs:            map[string][]string{"projector": {"api-orbtr-ai", "node-orbtr-io"}},
        },
    }},
    RaftConfig: manager.RaftConfig{LocalDir: "/data/tursoraft"},
    VolumeDir:  "/data",
    Mesh:       mesh, // implements mesh.Mesh
})
```

`Validate` (run by `NewManager`) needs no network and refuses: a relative
`LocalDir`; a group without a 32-byte key; a shared database without a url or
a token (a database's own token, else the group's); a group, database or job
name outside `^[a-z0-9][a-z0-9_-]*$`; a raft block over other than one shared
database, with no voters, or with `bootstrapExpect` outside `1..voterCount`; a
raft block with no mesh; a voter whose state is not under `VolumeDir`; and, in
a build without cgo, any shared group (`ErrReplicaNeedsCGO`).

Voter and job-host names are the service names `mesh.Mesh.LocalService`
reports (loom's node `ServiceName`), not domains. A process whose service is
neither a voter nor a host of any job gets no seat: `Validate` accepts it and
`Job` answers `ErrNoCluster`, and a block whose voters name no running service
never forms a cluster.

## Files

Everything lives under `LocalDir`, each kind under its own prefix so no two
kinds ever share a path:

| Path | What |
|---|---|
| `replicas/<group>/<db>.db` (+ `-wal`, `-shm`, `-client_wal_index`) | Replica of a shared database; a cache |
| `local/<group>/<db>.db` | Local database; the only copy of its rows |
| `raft/<group>/<db>/{logs.db,stable.db,snapshots/}` | A voter's Raft state; job hosts keep theirs in memory |

## Boot

1. Validate, with no network. Every shared database's replica is `ReadWrite`
   under the static token its config carries (`replica.StaticCredential`).
2. For each shared database: with a key configured, a replica file that begins
   with the plaintext SQLite header is deleted (it is a cache).
3. Open the replica (`WithEncryption`, `WithReadYourWrites`, `WithRawText`,
   `WithImmediateTransactions`), retrying an unreachable primary for the open
   timeout (60 s), then fail. There is no fallback. Then check the file the
   engine wrote: one that begins with the plaintext SQLite header, or that is
   too short to judge beside the `-info` file libSQL's HTTP sync protocol
   writes, is refused with `replica.ErrPlaintextReplica` and nothing is served
   from it, because a primary that answers that protocol makes the engine open
   the file without its key.
4. Wrap it with `policypool.OpenConnector(…, ForeignKeysRequiredAtConnect)`,
   keeping up to `replica.PoolSize` (8) proved connections idle, and start its
   sync loop (every sync interval, default 5 s, jittered ±20%).
5. Open local databases through tursogo, encrypted, through policypool.
6. `Database()` works from here and never waits for Raft.
7. Start each Raft cluster this process has a seat in.
8. The endpoint applies its schema, calls `Sync`, then serves.

## Read-only replicas

A device's replica of a scope database opens with `Mode: replica.ReadOnly`, a
`CredentialSource` handing out its grants and the `Scope` it must hold. Before
the engine is reached, `Open` refuses a spec with no mode or credential source,
a credential with no token, an expired grant or token, and a token whose claims
are not read-only (`a` must be `ro`, `p` may hold only `ro` and `roa`, `exp`
must lie within one 15-minute grant plus a minute, and every namespace it names
must be the scope's database). After the open, the scope database's
`replicaScope` row must carry the spec's digest. go-libsql's `WithReadOnly`
gate refuses every statement it would send to the primary, and every pooled
connection proves it refuses a write itself before it is used.

A scope replica records its source position, the `sourceCursor` of its
`replicaProgress` row, after every pull. `RequestPull(ctx, atLeastPosition)`
asks the sync loop for pulls, sharing them with every other caller, until the
replica records that position, and `Position` and `WatchPosition` read and
follow it. `replica.Set` holds a device's replicas by database name and
implements `pull.Trigger` over them, so a subscription that learns of a change
at a source position brings the segment's replica up to it.

## Jobs

```go
db, _ := mgr.Database("control", "orbtr-control")
job, err := mgr.Job("control", "orbtr-control", "projector", func(ctx context.Context, f fence.Fence) error {
    for ctx.Err() == nil {
        err := f.Tx(ctx, db, func(tx *sql.Tx) error {
            // this holder's writes for one step
            return nil
        })
        if errors.Is(err, fence.ErrFenced) {
            return err // a newer holder has written; stop
        }
        // wait for the next step or ctx
    }
    return nil
})
```

The body runs while this process holds the job and gets a context that ends
when the job moves or the manager shuts down. If it returns while the job is
still held it runs again after a backoff. For about one election timeout
after a failover two processes may both believe they hold a job; `Fence.Tx`
refuses the stale one's writes once the new one has written. Effects outside
the fenced database must be idempotent.

`Fence.Tx` begins with `BEGIN IMMEDIATE`, records the epoch in
`_tursoraft_fence`, runs the body and commits. Keep the body short: the
primary rolls back a write transaction held for about five seconds.

## Rules for code on a shared database

- A read and then a write outside one transaction loses the update another
  process makes in between. Use one `BeginTx` (which is `BEGIN IMMEDIATE`) or
  one conditional statement.
- Schema is applied by every instance at boot, not by a leader; each step must
  be idempotent and run in one transaction with its ledger check.

## Operator tool

```
TURSO_API_TOKEN=… tursoraft-admin ensure -org orbtr -group control -db orbtr-control
TURSO_API_TOKEN=… tursoraft-admin token  -org orbtr -db orbtr-control
TURSO_DB_TOKEN=…  tursoraft-admin fence-adopt -url libsql://… -cluster <new id> -confirm-old-cluster-gone
```

`ensure` refuses a database that exists in another group. `fence-adopt` is
for the case where every voter lost its Raft state and a new cluster formed:
the fence rows still name the old cluster, so the new cluster's jobs get
`fence.ErrForeignCluster` until an operator who has stopped every member of
the old cluster adopts the new ID (from `Manager.Health`).

## Building and testing

Shared groups need cgo (go-libsql is a C library). go-libsql and policypool
resolve through `replace` directives to sibling checkouts; workspaces that
use tursoraft carry the same lines in their `go.work`.

Tests that need a primary start a local sqld through `sqldtest`, on
127.0.0.1 and a free port, and stop only the process they started. The binary
is `$TURSORAFT_SQLD` when set; otherwise the newest, by `sqld --version`, of
`/opt/homebrew/bin/sqld`, `sqld` on `PATH` and each
`~/.local/share/libsql-server/<version>/sqld`; with none they skip. Tests that
hold `BEGIN IMMEDIATE` on two replicas at once need sqld 0.24 or later and skip
on older ones, which never answer the second replica.

A libsql-server release unpacked into its own version directory is found
without shadowing the `sqld` on `PATH`:

```
mkdir -p ~/.local/share/libsql-server/0.24.32
cp sqld ~/.local/share/libsql-server/0.24.32/
go test -race -count=1 -p 2 ./...
```

To run against one binary, name it:

```
TURSORAFT_SQLD=/path/to/sqld-0.24 go test -race -count=1 -p 2 ./...
```

## Known limits

- **Reads during an outage run on connections proved before it.** A new
  connection's foreign-key proof needs the primary. While the primary is down
  a replica serves reads from its file on the connections its pool already
  holds, up to `replica.PoolSize` at once; a read that needs one more fails,
  and every write fails.
- **Replica key strength.** go-libsql takes the key as the base64 passphrase
  and its cipher uses the passphrase's first 32 bytes, so the first 24 bytes
  (192 bits) of the group key protect a replica file.
- **A pull against a primary that refuses connections blocks about 3 s**
  inside the engine before failing; `Sync` callers and `Close` wait for it.
- **Raft timing.** Heartbeat and election timeouts are 1 s: a follower that
  still has a leader refuses every vote until its own timer fires, so a killed
  leader's job moves after the slowest follower's timer (1–2 s) plus the
  reassignment.

## Dependencies

- `github.com/bbmumford/go-libsql` (`replica`): the encrypted embedded replica
  of a libSQL primary.
- `github.com/bbmumford/policypool` (`localdb`, `replica`): the connection
  pools and their foreign-key proof.
- `turso.tech/database/tursogo` (local files), `github.com/hashicorp/raft`,
  `github.com/hashicorp/raft-boltdb/v2`, `go.etcd.io/bbolt` and
  `github.com/hashicorp/go-hclog` (the Raft clusters and their log store).
- `go.mod` replaces go-libsql, policypool and filekind (which policypool
  imports) with the sibling checkouts `../go-libsql`, `../policypool` and
  `../filekind`.

## Used by

Importers in the ORBTR estate:

- `github.com/bbmumford/loom/node` (`localdb`, `manager`, `mesh`,
  `sqldtest`): the mesh node's databases, with loom's mesh as Raft's transport.
- `github.com/ORBTR/Library/endpoint` (`manager`),
  `github.com/ORBTR/Library/endpoint/jobs` (`fence`, `manager`) and
  `endpoint/jobs/intents` (`fence`): every endpoint's databases and singleton
  jobs.
- `github.com/ORBTR/ai/node.orbtr.ai` (`manager`) and
  `github.com/ORBTR/cortex/internal/sqldtest` (`sqldtest`).
- Tests only: `github.com/ORBTR/api.orbtr.ai`,
  `github.com/ORBTR/Library/domain/pluginhost` and
  `github.com/ORBTR/Library/grantwrite` (`replica`, `sqldtest`).

## Platforms and build

Shared databases need cgo, because go-libsql is a C library: `fence`,
`connectors` and `cmd/tursoraft-admin` build only with cgo, and in a build
without it `replica.Supported` is false and opening a replica returns
`ErrReplicaNeedsCGO`, so a cgo-free build compiles and opens local files only. `sqldtest` builds on Unix systems. The module's `go` directive is
1.25.0.

## Licence

MIT. Copyright (c) 2026 Brandon Miller-Mumford. The full text is in
[LICENSE](LICENSE).

## Contributing

[AGENTS.md](AGENTS.md) holds the repository's two rules, for people and coding
agents alike: no temporary documents in the repository, and conventional commit
messages that stand on their own. [CLAUDE.md](CLAUDE.md) imports it for Claude
Code.
