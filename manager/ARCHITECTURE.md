# tursoraft architecture

## Objects

| Object | Where | Authoritative | If lost |
|---|---|---|---|
| Primary | libSQL (Turso Cloud or sqld) | Yes | Turso point-in-time recovery |
| Replica files | `<LocalDir>/replicas/<group>/<db>.db` and companions | No; a cache | Deleted and downloaded again |
| Local file | `<LocalDir>/local/<group>/<db>.db` | Yes, for its one process | That process's local state |
| Raft cluster | One per shared database with a raft block, mesh key `tursoraft/<group>/<db>` | Assignments and a cluster ID; no rows | Voters rejoin; if every voter loses state a new cluster forms and an operator runs `fence-adopt` |
| Raft state | Voters: `<LocalDir>/raft/<group>/<db>/` (BoltDB `logs.db`, `stable.db`, `snapshots/`). Job hosts: memory | — | A voter needs its volume; a host rejoins |
| Fence row | `_tursoraft_fence(job PK, cluster_id, epoch, node, updated_at)` in the primary | Yes; orders a job's writers | — |
| Group key | 32 bytes, base64 in the group's config | — | Replica: resync. Local file: unreadable |
| Member key | HKDF-SHA256(group key, salt `<group>/<db>`, info `tursoraft/member/v1`) | — | Derived; never stored |

## Who is in a cluster

A group's raft block names `voterServices`, `voterCount`, `bootstrapExpect` and
`jobs{name: [host services]}`. A process takes part when its mesh service is a
voter service, a host service of any job, or both:

- A **voter** keeps its Raft state on the volume and may vote.
- A **job host** keeps its Raft state in memory and never votes.
- Any other process that holds the database has no seat: it writes the
  database but runs no Raft node for it.

Membership is decided from the service the mesh authenticates, never from what
a member publishes about itself.

### Member proof

Every Raft stream, in both directions, opens with a proof that the peer holds
the group key: each side sends a 32-byte nonce, then one HMAC-SHA256 per key
it holds, under the member key, over
`"tursoraft-member-v1" ‖ cluster ‖ lower NodeID ‖ higher NodeID ‖ lower nonce ‖ higher nonce ‖ sender NodeID`
(each field length-prefixed), then the cluster ID it has applied. Both NodeIDs
come from the mesh (`Conn.RemoteID`), and both nonces are fresh, so a proof
verifies on no other stream. A peer is accepted when any of its proofs
verifies under any of this side's keys, so a member configured with a new key
and the prior one interoperates with a member holding only the prior one.
Members that have applied different cluster IDs refuse each other.

The leader probes a member with this handshake before adding it, so a process
with the mesh network key but not the group key never enters a configuration.

### Published metadata

Each member publishes under the cluster key: `role`, `jobs` (the jobs it has
registered), `state` (`1` once it holds any Raft state), `cluster` (its
applied cluster ID), `spec` (sha256 of the raft block), `fsmv` (the entry
version it applies), `idx` (its applied index) and, while it holds no state,
`boot` (its bootstrap proposal). A member whose `spec`, `fsmv` or `cluster`
disagrees is ignored and alerted once.

## Bootstrap

A voter with Raft state never bootstraps. A voter with none forms the cluster
only when:

1. no voter on the mesh advertises `state=1`;
2. at least `bootstrapExpect` voters with no state and the same `spec` are
   visible, and this voter is among the `bootstrapExpect` lowest NodeIDs, the
   proposed set;
3. every member of the proposed set publishes the same proposal, and has for
   the settle time (2 s).

The members of the set then call `BootstrapCluster` with the set as voters
(`ErrCantBootstrap` ignored). The published agreement keeps two voters that see
different parts of the mesh from bootstrapping two configurations that share a
first log index and term, which Raft would treat as one log. The first leader
applies a new random cluster ID. Hosts never bootstrap.

## Reconciler

The leader reconciles every 10 s and whenever the mesh changes (at most every
100 ms), making at most one membership change per pass, or one swap (step 4):

1. A live participant missing from the configuration is probed, then added as
   a non-voter.
2. A non-voter absent for 2 minutes is removed.
3. A voter absent for 10 minutes is removed only while a caught-up replacement
   is live and the live voters would still be a quorum. A dead voter left in
   the configuration counts against every election.
4. While there are fewer than `voterCount` voters, a caught-up voter-eligible
   non-voter is promoted, preferring the service furthest below its floor.
   The floor of a service is 2, or its number of live eligible members when
   fewer, and applies only when more than one service votes; a service below
   it gets a voter from a service above it (promote, then demote).
5. Each job stays with its live holder, else goes to the live member with the
   lowest NodeID that is in the configuration, hosts the job, and has
   registered it: `Assign{v:1, job, node}`. The epoch is that entry's log
   index.

"Live" means publishing under the cluster key with the same spec, entry
version and cluster ID, and not failing the leader's heartbeats for longer
than the hold time. "Caught up" means within 1024 entries of the leader.

## Jobs and fencing

The FSM applies entries in memory, takes no lock but its own, and wakes the
job runner and the control loop through a channel. The runner starts a job's
body with `Fence{cluster, job, epoch, node}` when the assignment names this
process, the FSM has not halted, and this process is the leader or heard from
one within the hold time (2 s); otherwise it cancels the body's context. A
holder cut off from the leader stops by itself.

`Fence.Tx` runs `BEGIN IMMEDIATE`; upserts the fence row where the cluster ID
matches and the new epoch is at least the recorded one; requires one affected
row, else `ErrFenced` (a newer epoch wrote) or `ErrForeignCluster`; runs the
body; commits. The fence table is created inside the transaction the first
time it is missing.

## Entry versions

Entries carry `v`. An entry of another version, or of an unknown op, halts the
member's FSM: it refuses that entry and every later one, stops its jobs and
alerts. This build proposes only version 1, and a member publishing a lower
`fsmv` is not counted for bootstrap, added or assigned. Nothing holds a newer
version's entries back until every member of the configuration advertises
that version, so a build that introduces a second version must add that gate
before it proposes one.

## Timing

| Setting | Value | Why |
|---|---|---|
| Heartbeat, election timeout | 1 s | A follower with a leader refuses every vote until its own timer (1–2 timeouts) fires; failover follows the slowest follower |
| Leader lease | 500 ms | A leader cut off from a quorum steps down |
| Hold | 2 s | A holder without leader contact stops its job |
| Reconcile | 10 s, and on mesh change | |
| Host absent / voter absent | 2 min / 10 min | |
| Settle | 2 s | Bootstrap agreement |

## Shutdown

`Manager.Shutdown`: for each cluster, stop the control, runner and observer
loops (the control loop may be waiting on a Raft future, and Raft does not
answer a future whose entry it committed but had not applied when it shut
down); cancel the jobs and wait for their bodies; withdraw the published
metadata; hand off leadership; `raft.Shutdown()`; close the BoltDB stores;
close the transport, which closes every stream. Then stop each replica's sync
loop and close its pool, which closes the connector, and close local files.

## Failure modes

| Situation | What happens | Recovery |
|---|---|---|
| Primary unreachable at boot | Replica open retries for 60 s, then `NewManager` fails | Next start |
| Primary unreachable while running | Pulls fail; writes fail; pooled reads fail too (the foreign-key proof on each reuse needs the primary); fenced jobs fail their transactions | Pulls and writes recover on their own |
| Raft leader lost | New leader in about 1–2 s; jobs of a dead holder reassigned | Automatic |
| Stale job holder | Its next `Fence.Tx` affects no row: `ErrFenced` | Automatic |
| A quorum of voters lost | No leader; jobs stop; `Database()` unaffected | Restore voters |
| Every voter loses state | New cluster ID; `Fence.Tx` returns `ErrForeignCluster` | `tursoraft-admin fence-adopt` |
| Entry of an unknown version | Member halts and alerts | Upgrade the member |
| Peer without the group key | Member proof fails; stream closed; never added | — |
| Plaintext replica file under a key | Deleted at boot and downloaded again | Automatic |
| Replica under another key | Open fails ("file is not a database") | Fix the key or delete the replica |
| Transaction held about 5 s | Primary rolls it back | Smaller steps |
| Read then write outside a transaction | Lost update | One `BeginTx` or one statement |
| Shared group in a build without cgo | `ErrReplicaNeedsCGO` | Build with cgo |
