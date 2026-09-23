package connectors

// The partition-database pools of api.orbtr.ai and node.orbtr.io open driver
// "libsql" (cortex tenant/tenantdb/pool.go, constellation
// domain/identity/provisioning/tenantdb/pool.go) without importing the package
// that registers it. loom's node runtime imports this package into every
// endpoint that runs a node, so this import is where those pools get the
// driver; without it they fail at their first open with "unknown driver".
//
// TODO: delete once both tenantdb packages import the driver themselves. The
// test TestOnlyThisFileImportsTheHranaClient pins it to this one file.
import _ "github.com/tursodatabase/libsql-client-go/libsql"
