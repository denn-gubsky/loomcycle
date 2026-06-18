package builtin

import (
	"encoding/json"
	"sort"
)

// mcpWrapperSchemas maps each op-dispatched builtin tool — by the
// lowercase name under which the LoomCycle MCP server exposes it as a
// meta-tool — to that tool's canonical input schema const.
//
// internal/api/mcp's tool catalogue resolves these so the advertised
// MCP inputSchema is sourced from the tool's REAL validation schema
// rather than restated. The wrappers forward their arguments 1:1 to the
// underlying builtin (e.g. `memory` → connector.Memory → *Memory.Call,
// validated against memoryInputSchema), so the canonical schema is
// exactly what the MCP client should send. Previously each wrapper
// advertised a bare {"type":"object"}, hiding the `op` enum + every
// property from clients introspecting the server.
//
// When a new op-dispatched builtin is exposed as an MCP wrapper, add it
// here; TestBuiltinWrapperSchemas_CoverAllWrappers (in internal/api/mcp)
// fails if a wrapper descriptor has no entry.
var mcpWrapperSchemas = map[string]string{
	"memory":           memoryInputSchema,
	"channel":          channelInputSchema,
	"agentdef":         agentDefInputSchema,
	"skilldef":         skillDefInputSchema,
	"mcpserverdef":     mcpServerDefInputSchema,
	"scheduledef":      scheduleDefInputSchema,
	"a2aservercarddef": a2aServerCardDefInputSchema,
	"a2aagentdef":      a2aAgentDefInputSchema,
	"webhookdef":       webhookDefInputSchema,
	"memorybackenddef": memoryBackendDefInputSchema,
	"operatortokendef": operatorTokenDefInputSchema,
	"volumedef":        volumeDefInputSchema,
	"evaluation":       evaluationInputSchema,
	"context":          contextInputSchema,
}

// MCPWrapperInputSchema returns the canonical input schema for the
// op-dispatched builtin tool exposed under the given MCP meta-tool name,
// and whether such a wrapper exists. The bytes are the tool's own
// validation schema, so callers may advertise them verbatim.
func MCPWrapperInputSchema(name string) (json.RawMessage, bool) {
	s, ok := mcpWrapperSchemas[name]
	if !ok {
		return nil, false
	}
	return json.RawMessage(s), true
}

// MCPWrapperNames returns the sorted names of every op-dispatched builtin
// tool exposed as an MCP meta-tool wrapper. The MCP server's
// catalogue-coverage test iterates this so a newly-exposed wrapper that
// forgot to source its schema fails loudly.
func MCPWrapperNames() []string {
	names := make([]string, 0, len(mcpWrapperSchemas))
	for name := range mcpWrapperSchemas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
