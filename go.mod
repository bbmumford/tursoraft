module github.com/bbmumford/tursoraft

go 1.25.0

require (
	github.com/bbmumford/go-libsql v0.0.0
	github.com/bbmumford/policypool v0.0.0
	github.com/hashicorp/go-hclog v1.6.2
	github.com/hashicorp/raft v1.7.3
	github.com/hashicorp/raft-boltdb/v2 v2.3.1
	go.etcd.io/bbolt v1.3.5
	go.uber.org/goleak v1.3.0
	turso.tech/database/tursogo v0.8.0-pre.12
)

require (
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/armon/go-metrics v0.4.1 // indirect
	github.com/bbmumford/filekind v0.0.0 // indirect
	github.com/boltdb/bolt v1.3.1 // indirect
	github.com/ebitengine/purego v0.9.1 // indirect
	github.com/fatih/color v1.13.0 // indirect
	github.com/hashicorp/go-immutable-radix v1.0.0 // indirect
	github.com/hashicorp/go-metrics v0.5.4 // indirect
	github.com/hashicorp/go-msgpack/v2 v2.1.2 // indirect
	github.com/hashicorp/golang-lru v0.5.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/libsql/sqlite-antlr4-parser v0.0.0-20240327125255-dbf53b6cbf06 // indirect
	github.com/mattn/go-colorable v0.1.12 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/tursodatabase/turso-go-platform-libs v0.8.0-pre.12 // indirect
	github.com/zeebo/blake3 v0.2.4 // indirect
	golang.org/x/exp v0.0.0-20240325151524-a685a6edb6d8 // indirect
	golang.org/x/sys v0.44.0 // indirect
)

replace github.com/bbmumford/go-libsql => ../go-libsql

replace github.com/bbmumford/policypool => ../policypool

replace github.com/bbmumford/filekind => ../filekind
