// Package hostenv builds the environment loomcycle hands a host child process.
//
// A child inherits nothing by default: loomcycle's own environment holds
// provider keys, the bearer token and the database DSN, and a child that can
// print its environment would hand them to whoever reads its output. Every
// host-exec path (the Bash tool, the Bashbox host-command fallback, the
// transcription helper commands) builds its child's env here, so the paths
// cannot drift apart on what leaks.
package hostenv

import (
	"os"
	"strings"
)

// collectorHeaders reports whether name is an OTLP collector-header variable
// (OTEL_EXPORTER_OTLP_HEADERS and its per-signal forms, or the retired
// LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS). Matched here rather than through
// internal/config so this package stays an import leaf.
func collectorHeaders(name string) bool {
	up := strings.ToUpper(name)
	return (strings.HasPrefix(up, "OTEL_EXPORTER_OTLP") || strings.HasPrefix(up, "LOOMCYCLE_OTEL_EXPORTER_OTLP")) &&
		strings.HasSuffix(up, "_HEADERS")
}

// Scrubbed is the environment for a host child process: PATH always (most
// binaries break without it) plus the names in allowed that are set in this
// process's environment, in the order given. Nothing else is passed, and the
// OTLP collector headers never are, even when listed: they are loomcycle's
// collector credentials, not a setting a child could need.
func Scrubbed(allowed []string) []string {
	out := []string{}
	if v := os.Getenv("PATH"); v != "" {
		out = append(out, "PATH="+v)
	}
	for _, name := range allowed {
		if collectorHeaders(name) {
			continue
		}
		if v := os.Getenv(name); v != "" {
			out = append(out, name+"="+v)
		}
	}
	return out
}
