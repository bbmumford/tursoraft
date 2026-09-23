package cluster

import (
	"testing"

	"go.uber.org/goleak"
)

// Every test shuts its members down, so no Raft, transport, job or loop
// goroutine may outlive the package's tests.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
