// Package hostenv builds the environment loomcycle hands a host child process.
//
// A child inherits nothing by default: loomcycle's own environment holds
// provider keys, the bearer token and the database DSN, and a child that can
// print its environment would hand them to whoever reads its output. Every
// host-exec path (the Bash tool, the Bashbox host-command fallback, the
// transcription helper commands) builds its child's env here, so the paths
// cannot drift apart on what leaks.
package hostenv

import "os"

// Scrubbed is the environment for a host child process: PATH always (most
// binaries break without it) plus the names in allowed that are set in this
// process's environment, in the order given. Nothing else is passed.
func Scrubbed(allowed []string) []string {
	out := []string{}
	if v := os.Getenv("PATH"); v != "" {
		out = append(out, "PATH="+v)
	}
	for _, name := range allowed {
		if v := os.Getenv(name); v != "" {
			out = append(out, name+"="+v)
		}
	}
	return out
}
