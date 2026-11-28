# Changelog

## [0.1.0](https://github.com/bbmumford/tursoraft/compare/v0.0.6...v0.1.0) (2026-11-28)


### Features

* a replica opens from a spec naming its mode, its credential source and its scope, and refuses a token it must not pull under before the engine is reached ([88f15bc](https://github.com/bbmumford/tursoraft/commit/88f15bc5ef8eea8d0a4b8ae8c8c47fcf2faaa0ba))
* a replica renews an expiring credential inside its sync loop by reopening the engine on the same file, and stops pulling once a credential lapses ([68f173f](https://github.com/bbmumford/tursoraft/commit/68f173fa1f85f2fefae2df50b41204d7d8d1ab1b))
* a scope replica follows its source position, RequestPull waits on the sync loop until it reaches one, and replica.Set routes such requests by segment ([3217a62](https://github.com/bbmumford/tursoraft/commit/3217a62c489a1895b03dbc630dcaf84dab2746c4))
* a scope replica opens only when its header carries the scope database file identity its primary was stamped with, and the copy is never stamped ([5f9a481](https://github.com/bbmumford/tursoraft/commit/5f9a48128f87bfab90995eb10417bdf04465e6b8))
* localdb.OpenFile stamps a new local file with its caller's kind and refuses any other, and a pure-local group's databases carry the tursoraftLocal kind ([6bc2596](https://github.com/bbmumford/tursoraft/commit/6bc2596294426813cc4823dff8f1abfc39a4b45a))
* with no live grant a replica serves reads only while its offline lease runs, judged by the holder's clock and held to 30 days ([7e34b17](https://github.com/bbmumford/tursoraft/commit/7e34b1747d21c385077de0d6fd18547fc4a2d6c0))


### Bug Fixes

* a grant renewal retires the old engine's connections before it closes the engine ([cf23a14](https://github.com/bbmumford/tursoraft/commit/cf23a148dd51b689d7b364b8d5a5884b0a8633bd))
* a read-only scope grant may name its database by the primary's own id, which is the only database claim Turso's database tokens carry ([d3b5f4b](https://github.com/bbmumford/tursoraft/commit/d3b5f4b0c3caf764b9ce2eabeaad261f8cef2411))
* a replica handle's pool is closed only by the replica ([27a1f48](https://github.com/bbmumford/tursoraft/commit/27a1f48b76d7a5fb727c8f31e2d730496feb7ddd))
* a replica refused as unencrypted is deleted at once with its WAL, shared-memory and companion files ([278c87d](https://github.com/bbmumford/tursoraft/commit/278c87d13d85efd4d6b2f2a2aae1d62a4747bcf9))
* a replica refuses a scoped token that names no database, with ErrGrantMismatch, so its check that every named database is the scope's cannot pass vacuously ([5367051](https://github.com/bbmumford/tursoraft/commit/53670511f4798ddd3ecad1a2ae4082f386d4904d))
* a replica the engine leaves unencrypted is refused after the open instead of served ([feeb8ed](https://github.com/bbmumford/tursoraft/commit/feeb8edc8964c40bf4e58c0dbdd53c4074296029))
* **camelcase:** corrective sweep for pkg-tursoraft ([bbad109](https://github.com/bbmumford/tursoraft/commit/bbad10971258525803c3681f9d3b756dbfaada31))
* **database:** surface the embedded replica's sync divergence, and correct the write-path comment ([59eba5c](https://github.com/bbmumford/tursoraft/commit/59eba5c0a80d7fd66460faf60e895b449dff12fe))
* encrypted replicas, local files and fenced singleton jobs over a Raft cluster per shared database ([5e8ddd9](https://github.com/bbmumford/tursoraft/commit/5e8ddd909300417b6085f860d4461e059f7b1956))
* **manager:** IsLeader's comment names the consumers that still gate on it ([5494bd8](https://github.com/bbmumford/tursoraft/commit/5494bd8f42f703df509e50c50a0eeb0d2d74fbdd))
* **meshtest:** each Isolate makes a new island ([99580f2](https://github.com/bbmumford/tursoraft/commit/99580f2ab906770df3f3db13dc9a7fa87d82103b))
* move tursogo to v0.8.4, its stable release ([709789b](https://github.com/bbmumford/tursoraft/commit/709789b26f28ec1f399cc22c01adc8467d065c0e))
* raft blocks name mesh services, not domains; IsLeader's comment names its one remaining consumer ([c79315c](https://github.com/bbmumford/tursoraft/commit/c79315c2c76e44809092137219475aeed9705515))
* **replica:** callers that queue behind a slow pull share the next one ([d663a74](https://github.com/bbmumford/tursoraft/commit/d663a74064c98a0bfd1078205fef719e468b4415))
* **replica:** one pull answers at most a batch of waiting callers, and Close is read while they are taken ([08cace0](https://github.com/bbmumford/tursoraft/commit/08cace0fff46c6ea7c7490d3a7698175e6e9524d))
* **replica:** orphan checks require a foreign-key failure; MeshKey and sqldtest docs name no consumer that does not use them ([ac61845](https://github.com/bbmumford/tursoraft/commit/ac61845c5473f1743cf11178e03b7bee4518f0ee))
* **replica:** prove foreign keys at connect and serve reads through an outage; export MeshKey; MakeConnector requires a key; sqldtest is public ([4069b16](https://github.com/bbmumford/tursoraft/commit/4069b164ef2f81695da149b92e0d252aa4eb7343))
* report a replica's sync divergence without exposing its handle ([ab12097](https://github.com/bbmumford/tursoraft/commit/ab12097a4a657c2b8bb70111fdb67fa446c9c618))
* RequestPull with no deadline stops after DefaultRequestPullTimeout with ErrPositionNotReached ([3cd4835](https://github.com/bbmumford/tursoraft/commit/3cd4835c51bbc30a94bb887587f6137e8da174b8))
* **sqldtest:** a release whose first --version runs past the bound is read on a second run, so Binary still ranks the newest sqld ([3bb25fb](https://github.com/bbmumford/tursoraft/commit/3bb25fbff93fb1470caeab9e49009eb5a706f960))
* **sqldtest:** Binary takes $TURSORAFT_SQLD, else the newest of Homebrew's sqld, sqld on PATH and each ~/.local/share/libsql-server/&lt;version&gt;/sqld, so the concurrent-transaction tests run wherever sqld 0.24 is installed ([5aadfc4](https://github.com/bbmumford/tursoraft/commit/5aadfc4594128cd871401a5ecfe622c183c7d311))
* the deletion before an open judges page 1 as the check after an open does, so a replica refused for an unencrypted page 1 in the WAL is replaced ([beb567a](https://github.com/bbmumford/tursoraft/commit/beb567a7d17deb6b08f1802b09f8fb25b418fc68))
* the pool's Connect waits out an engine that refuses a new connection because the database is locked ([f2af4e4](https://github.com/bbmumford/tursoraft/commit/f2af4e4859f97e309da125c31b6e2b2d954c9b3c))
* the unencrypted-replica check judges page 1 in the database file or, before the engine wrote it there, in the WAL, and no longer reads the -info file as a sign ([7c6a72a](https://github.com/bbmumford/tursoraft/commit/7c6a72a281880d0c00ccb87b005a4496632f7292))
* the unencrypted-replica check reads every WAL frame of page 1, also when the database file holds an encrypted page 1 ([0d76321](https://github.com/bbmumford/tursoraft/commit/0d76321d94dbbafa69194974c8e33c0db4035a81))
* tursoraft-admin token mints a token that expires after one day unless another lifetime is asked for ([c7e03e6](https://github.com/bbmumford/tursoraft/commit/c7e03e647f9d5dfdb94fb5de252e7fae145c54d0))
