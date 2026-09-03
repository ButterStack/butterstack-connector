// Package tools holds the per-tool executors.
//
// Every executor takes an already-validated argument map: by the time control
// reaches this package, the verb was in the compiled vocabulary, every argument
// was type-checked, and every scoped argument was inside its scope. Executors
// must not re-derive anything from the wire, and must not accept a host, port,
// URL, or credential from their caller -- all of those come from the config
// value the executor was constructed with.
package tools

import (
	"context"
	"errors"
)

// ErrNotConfigured is returned when a verb's tool section is disabled.
var ErrNotConfigured = errors.New("tools: tool is not configured")

// Executor runs one verb.
type Executor interface {
	Execute(ctx context.Context, verb string, args map[string]any, maxBytes int) (body any, bytes int, truncated bool, err error)
}

// argInt reads a validated integer argument, with a default.
func argInt(args map[string]any, name string, def int64) int64 {
	if v, ok := args[name]; ok {
		if i, ok := v.(int64); ok {
			return i
		}
	}
	return def
}

// argString reads a validated string argument.
func argString(args map[string]any, name string) string {
	if v, ok := args[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
