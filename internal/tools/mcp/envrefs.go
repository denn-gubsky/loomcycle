package mcp

import (
	"fmt"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// EnvRefLocations names each connection field of an MCP server definition that
// holds a ${NAME} token (config.HasEnvRef): a field the pool expands from this
// process's environment when it dials the server, so the value of an operator
// env var leaves with the request. Field names only, never a value. Sorted, so
// a message or an audit line built from it is stable.
//
// A nested default counts: ${run.credentials.x:-${LOOMCYCLE_Y}} holds
// ${LOOMCYCLE_Y}, which is expanded at dial before the request-time ${run.*}
// substitution sees it.
func EnvRefLocations(url string, headers map[string]string, command string, args []string, env map[string]string) []string {
	var out []string
	if config.HasEnvRef(url) {
		out = append(out, "url")
	}
	for k, v := range headers {
		if config.HasEnvRef(v) {
			out = append(out, "headers."+k)
		}
	}
	if config.HasEnvRef(command) {
		out = append(out, "command")
	}
	for i, a := range args {
		if config.HasEnvRef(a) {
			out = append(out, fmt.Sprintf("args[%d]", i))
		}
	}
	for k, v := range env {
		if config.HasEnvRef(v) {
			out = append(out, "env."+k)
		}
	}
	sort.Strings(out)
	return out
}
