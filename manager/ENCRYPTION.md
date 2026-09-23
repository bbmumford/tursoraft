# tursoraft encryption at rest

Every file tursoraft writes for a database is encrypted, and a group without a
key does not open.

## The group key

`GroupConfig.EncryptionKey` is base64 of exactly 32 random bytes:

```
head -c 32 /dev/urandom | base64
```

Every process holding the group carries the same value: replicas of one
database are opened under it, and Raft members prove they hold it. A key of
any other length, or one that does not decode, is refused by `Validate`.

## What is encrypted, and how

| File | Mechanism | Plaintext found under a key | Wrong key |
|---|---|---|---|
| Replica `<db>.db` and `-wal` | go-libsql `WithEncryption(base64(key))`, SQLite3MC AES-256-CBC per page | Deleted with its companion files and downloaded again: a replica is a cache | Open fails with "file is not a database"; the file is left for the operator |
| Local file | tursogo `experimental=encryption,triggers&encryption_cipher=aes256gcm&encryption_hexkey=<hex>` | Refused with `localdb.ErrPlaintext` and left in place: it is the only copy | Open fails when the schema is first read |
| Raft BoltDB and snapshots | Not encrypted | — | — |
| Primary | Turso's own at-rest encryption | — | — |

The tests read the files back: no plaintext SQLite header, and no table name or
row text, in the database file or its WAL.

Raft state holds NodeIDs, job names, epochs and the cluster ID; no rows.

### Replica key strength

go-libsql passes the key across the C boundary as a NUL-terminated string,
which is why a 32-byte key is given as its base64 form, and its cipher derives
the AES key from the first 32 bytes of that string. The first 32 base64
characters encode the first 24 bytes of the key, so a replica file is
protected by 192 bits of the group key.

AES-256-CBC is deterministic per page and carries no integrity check: a
tampered replica is not detected, and a resync replaces it.

## The member key

The Raft member proof is made under
`HKDF-SHA256(group key, salt = "<group>/<db>", info = "tursoraft/member/v1")`,
so a proof for one database's cluster is not a proof for another's. It is
derived at start and never stored.

## Rotating a group key

1. Give every holder the new key as `EncryptionKey` and the old one as
   `PriorEncryptionKey`. The prior key is used only for the member proof, so
   members on either key keep proving to each other while they restart.
2. A replica does not reopen under a new key: stop the process, delete its
   replica files, and start it under the new key. It downloads again.
3. A local file cannot be rekeyed in place: export it under the old key and
   import it under the new one.
4. Remove `PriorEncryptionKey` once every holder runs on the new key.

## Keys outside a Manager

`connectors.MakeConnector` requires a key as well: an empty one is
`connectors.ErrNoKey`, and one that is not base64 of 32 bytes
`connectors.ErrBadKey`, both before any file is created. Given a primary URL
it opens an encrypted replica, else an encrypted local file.
