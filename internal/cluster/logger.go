package cluster

import (
	"bytes"
	"log/slog"

	"github.com/hashicorp/go-hclog"
)

// newHCLogger gives Raft, which logs through hclog, a logger that writes into
// log at warning level and above. Raft's info lines (every election, every
// snapshot) would otherwise flood an endpoint's log.
func newHCLogger(log *slog.Logger) hclog.Logger {
	return hclog.New(&hclog.LoggerOptions{
		Name:        "raft",
		Level:       hclog.Warn,
		Output:      slogWriter{log},
		DisableTime: true,
	})
}

// slogWriter forwards each line hclog formats to slog.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	for _, line := range bytes.Split(bytes.TrimRight(p, "\n"), []byte("\n")) {
		if len(line) > 0 {
			w.log.Warn(string(line))
		}
	}
	return len(p), nil
}
