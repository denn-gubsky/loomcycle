package mcp

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The `identity` meta-tool is the in-run Identity tool, plus op=tenants: the
// admin's cross-tenant listing the former `directory` tool offered here and
// nowhere in a run. Its schema and description are the builtin's own with
// that one op added, so the two cannot drift on anything else.

// identityMCPSchema is the Identity tool's input schema with "tenants" added
// to the op enum.
func identityMCPSchema() json.RawMessage {
	base := builtinSchema("identity")
	var s map[string]any
	if err := json.Unmarshal(base, &s); err != nil {
		return base
	}
	props, _ := s["properties"].(map[string]any)
	op, _ := props["op"].(map[string]any)
	enum, _ := op["enum"].([]any)
	if op == nil || enum == nil {
		return base
	}
	op["enum"] = append(enum, "tenants")
	op["description"] = "Which lookup to run. tenants (operator-admin only) lists the tenants that have run anything, with counts."
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return base
	}
	return json.RawMessage(bytes.TrimSpace(buf.Bytes()))
}

// identityDescription is the Identity tool's description, pointed at this
// transport's names, with op=tenants and the former tool's name.
func identityDescription() string {
	desc := (&builtin.Identity{}).Description()
	desc = strings.ReplaceAll(desc, "Context op=self", "the `context` tool's op=self")
	return desc + " On this server it also takes op=tenants, which lists the tenants with run activity and requires an operator-admin token." +
		" It replaces the former `directory` tool: directory op=users is op=users (active_only for the activity view), op=inspect is op=held."
}
