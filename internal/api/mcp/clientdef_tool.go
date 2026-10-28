package mcp

import (
	"encoding/json"

	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// clientDefToolName is the MCP twin of POST /v1/_clientdef: the apps (OAuth
// clients) that sign people in through this server's gates.
const clientDefToolName = "clientdef"

// clientDefToolDescriptor is its tools/list entry. Model-visible text: no
// design-document letters.
func clientDefToolDescriptor() loommcp.ToolDescriptor {
	return loommcp.ToolDescriptor{
		Name: clientDefToolName,
		Description: "Register the APPS that sign people in through this server's own sign-in page — OAuth clients such as the loomboard web, extension or desktop app. " +
			"Ops: create, update, get, list, retire, verify. A client is a name (its client_id) and a client object: kind public (no secret; PKCE is required), " +
			"redirect_uris (https with a host, http only to 127.0.0.1 or [::1] on any port, or an app's private-use scheme; matched exactly, never a wildcard or fragment), " +
			"allowed_origins for browser calls to the token endpoint, access_token_ttl, refresh {idle, absolute} and restrictions every token of the app carries. " +
			"A redirect URI decides where a person's sign-in code goes, so register only addresses the app really owns. " +
			"Needs substrate:tenant: a tenant operator registers apps for its own tenant only, and such an app signs in only that tenant's people; an admin may register for any tenant, or for every tenant by leaving tenant empty. " +
			"Clients declared in the server's config are listed read-only and cannot be changed or retired here. update writes a new version and voids the app's unused sign-in codes; retire ends every sign-in made through the app at once. " +
			"Every write is audited with its author. Do NOT use it to sign anyone in: people sign in through the app, which sends them to this server.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "op":             {"type": "string", "enum": ["create", "update", "get", "list", "retire", "verify"], "description": "Operation to perform."},
    "name":           {"type": "string", "description": "The client's id (create, update, get, retire, verify): lowercase letters, digits, - and _."},
    "def_id":         {"type": "string", "description": "get: one stored version by its id instead of the name."},
    "client": {
      "type": "object",
      "description": "create / update: the client.",
      "properties": {
        "kind":             {"type": "string", "enum": ["public"]},
        "label":            {"type": "string", "description": "The name the sign-in page shows."},
        "redirect_uris":    {"type": "array", "items": {"type": "string"}},
        "allowed_origins":  {"type": "array", "items": {"type": "string"}},
        "access_token_ttl": {"type": "string", "description": "A Go duration, e.g. 1h (default)."},
        "refresh":          {"type": "object", "properties": {"idle": {"type": "string"}, "absolute": {"type": "string"}}, "description": "Go durations or whole days (7d / 30d default)."},
        "restrictions":     {"type": "object", "description": "Narrows every token the app's sign-ins mint (agents, overrides, rate, retention)."}
      },
      "required": ["kind", "redirect_uris"]
    },
    "description":    {"type": "string", "description": "create / update: what the app is."},
    "tenant":         {"type": "string", "description": "Admin only. create: the tenant the app serves (empty = every tenant). list: show one tenant's apps."},
    "content_sha256": {"type": "string", "description": "verify: the hash to compare with the version in force."}
  },
  "required": ["op"],
  "additionalProperties": false
}`),
	}
}
