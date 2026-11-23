package builtin

import (
	"encoding/json"
	"sort"
)

// WorkspaceMCPSchema is the Workspace tool's input schema cut to what it
// takes over loomcycle's MCP server (RFC GW D35): the op enum is
// WorkspaceMCPOps, and the properties are those ops' arguments. Derived from
// the tool's own schema and argument table, so the two cannot drift; ok is
// false when the tool's schema cannot be read.
func WorkspaceMCPSchema() (json.RawMessage, bool) {
	var s map[string]json.RawMessage
	if err := json.Unmarshal((&Workspace{}).InputSchema(), &s); err != nil {
		return nil, false
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(s["properties"], &props); err != nil {
		return nil, false
	}
	keep := map[string]bool{}
	for _, op := range WorkspaceMCPOps {
		for _, arg := range workspaceOpArgs[op] {
			keep[arg] = true
		}
	}
	out := map[string]json.RawMessage{}
	for name, p := range props {
		if keep[name] {
			out[name] = p
		}
	}
	ops := append([]string(nil), WorkspaceMCPOps...)
	sort.Strings(ops)
	op, err := json.Marshal(map[string]any{"type": "string", "enum": ops})
	if err != nil {
		return nil, false
	}
	out["op"] = op
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	s["properties"] = b
	res, err := json.Marshal(s)
	if err != nil {
		return nil, false
	}
	return res, true
}
