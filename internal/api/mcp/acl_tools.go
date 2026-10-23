package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// The access-control management meta-tools (RFC EZ §3): groupdef, acldef,
// aclbinding and authz, one per family of connector.ACLManagementOps, each
// the op-dispatched twin of its HTTP routes through the shared
// implementation. The op enums are generated from that table, so a tool
// cannot offer an op the routes do not have.

// aclToolDescriptors are the four tools' tools/list entries. Model-visible
// text: no design-document letters.
func aclToolDescriptors() []loommcp.ToolDescriptor {
	return []loommcp.ToolDescriptor{
		{
			Name: connector.ACLFamilyGroupDef,
			Description: "Manage USER GROUPS in your tenant — the sets access-control lists are bound to. Ops: list, get, effective, create, delete, add_member, remove_member. " +
				"create writes a group (name, description, external_id) and rewrites one of that name; get returns its members and every group it is nested in (contained_in); effective returns every user it holds through the groups nested in it, config-declared members included. " +
				"add_member / remove_member take the group's name plus kind (user or group) and id; nesting a group inside itself is refused. " +
				"You cannot add yourself to a group unless you hold the tenant's whole authority. " +
				"Needs substrate:tenant — a tenant's members do not see this tool. Confined to your own tenant; an admin may focus another with tenant. " +
				"Every change takes effect on the next request, on every replica. Do NOT use it to say what a group may do: that is an access-control list (acldef) bound to the group (aclbinding).",
			InputSchema: aclToolSchema(connector.ACLFamilyGroupDef, `
				"name":        {"type": "string", "description": "The group (every op but list)."},
				"description": {"type": "string", "description": "create: what the group is for."},
				"external_id": {"type": "string", "description": "create: the group's id in an identity provider, when it mirrors one."},
				"kind":        {"type": "string", "enum": ["user", "group"], "description": "add_member / remove_member: what the member is."},
				"id":          {"type": "string", "description": "add_member / remove_member: the member's subject or group name."}`),
		},
		{
			Name: connector.ACLFamilyAclDef,
			Description: "Author ACCESS-CONTROL LISTS in your tenant — named rule lists of allow and deny rules over actions and resources, which take effect where they are bound (aclbinding). " +
				"Ops: create, update, put, get, list, retire, validate, catalog, names. create, update and put are one write: they store the list under its name and bump its version. " +
				"retire takes a list out of force (bindings to it then apply nothing); a later create brings it back as a new version. " +
				"validate checks rules without storing them; catalog returns the resource types and verbs a rule may name; names lists the live lists' names. " +
				"A deny always wins over an allow. Each write asks your own access-control set too: authoring a list needs author on it, retiring needs retire, and a writer who holds only delegated authority cannot change a list that binds itself. " +
				"Needs substrate:tenant — a tenant's members do not see this tool. Confined to your own tenant; an admin may focus another with tenant. " +
				"Do NOT use it to grant a list to anyone: a list does nothing until a binding names it.",
			InputSchema: aclToolSchema(connector.ACLFamilyAclDef, `
				"name":            {"type": "string", "description": "The list (create, update, put, get, retire)."},
				"rules":           {"type": "array", "items": {"type": "object"}, "description": "create / update / put / validate: the rules, each {effect: allow|deny, actions: [...], resources: [\"<type>:<selector>\"], when?: {...}}."},
				"description":     {"type": "string", "description": "create / update / put: what the list is for."},
				"include_retired": {"type": "boolean", "description": "list: include retired lists."}`),
		},
		{
			Name: connector.ACLFamilyAclBinding,
			Description: "BIND an access-control list to whoever it applies to in your tenant: a user, a group, a token, or the whole tenant. Ops: list, create, delete. " +
				"create takes acl, subject_kind (user, group, token or tenant), subject_id (not for tenant) and an optional expires_at; delete retires a binding by its id. " +
				"A binding to a list that does not exist yet applies nothing until the list is created. " +
				"You cannot bind a list to yourself, your token, a group you are in or the tenant unless you hold the tenant's whole authority. " +
				"Needs substrate:tenant — a tenant's members do not see this tool. Confined to your own tenant; an admin may focus another with tenant. " +
				"Every change takes effect on the next request, on every replica. Do NOT use it to change what a list allows or denies: that is acldef; a binding only decides whom a list applies to.",
			InputSchema: aclToolSchema(connector.ACLFamilyAclBinding, `
				"acl":             {"type": "string", "description": "create: the list to bind."},
				"subject_kind":    {"type": "string", "enum": ["user", "group", "token", "tenant"], "description": "create: what the binding applies to."},
				"subject_id":      {"type": "string", "description": "create: the user's subject, the group's name or the token's def_id; omit for tenant."},
				"expires_at":      {"type": "string", "format": "date-time", "description": "create: when the binding stops applying (RFC 3339). Omit for no expiry."},
				"id":              {"type": "string", "description": "delete: the binding's id (binding_id from create or list)."},
				"include_retired": {"type": "boolean", "description": "list: include retired bindings."}`),
		},
		{
			Name: connector.ACLFamilyAuthz,
			Description: "Ask what the ACCESS-CONTROL rules decide, without doing the thing. Ops: evaluation, evaluations, explain, simulate. " +
				"evaluation answers one question — action {name} on resource {type, id} — with decision, outcome (allow, deny or no_match) and the rules that decided; evaluations answers up to 256 (options.evaluations_semantic: execute_all, deny_on_first_deny or permit_on_first_permit); " +
				"explain returns every rule that matched or did not, and the bindings that applied; simulate answers against a DRAFT — lists and bindings you pass in, added to the stored ones — and writes nothing. " +
				"Asked about yourself by default; naming another subject (subject {id}) needs a tenant operator. A decision of false with outcome no_match means no rule spoke: in compat mode the scope a token holds still decides such a request. Do NOT use it to change a rule: it only asks; acldef and aclbinding write.",
			InputSchema: aclToolSchema(connector.ACLFamilyAuthz, `
				"subject":     {"type": "object", "properties": {"id": {"type": "string"}}, "description": "Whose rules to ask (default: you)."},
				"action":      {"type": "object", "properties": {"name": {"type": "string"}}, "description": "evaluation / explain: the verb, e.g. run, invoke, read."},
				"resource":    {"type": "object", "properties": {"type": {"type": "string"}, "id": {"type": "string"}, "owner": {"type": "string"}}, "description": "evaluation / explain: the resource, e.g. {type: tool, id: Bash}; omit id for any instance."},
				"context":     {"type": "object", "description": "Optional request context the rules' conditions read: transport, interactive, tier, user_tier, agent, depth."},
				"evaluations": {"type": "array", "items": {"type": "object"}, "description": "evaluations: the questions; each inherits action, resource and context from the top level when it omits them."},
				"options":     {"type": "object", "properties": {"evaluations_semantic": {"type": "string", "enum": ["execute_all", "deny_on_first_deny", "permit_on_first_permit"]}}},
				"acls":        {"type": "array", "items": {"type": "object"}, "description": "simulate: draft lists {name, rules}, replacing stored lists of the same name."},
				"bindings":    {"type": "array", "items": {"type": "object"}, "description": "simulate: draft bindings {acl, subject_kind, subject_id}."},
				"requests":    {"type": "array", "items": {"type": "object"}, "description": "simulate: the questions to answer now and with the draft."}`),
		},
	}
}

// aclToolSchema is a family's input schema: the op enum from the table, the
// admin's tenant focus, and the family's own fields.
func aclToolSchema(family, properties string) json.RawMessage {
	ops, _ := json.Marshal(connector.ACLManagementOps(family))
	return rawJSON(fmt.Sprintf(`{
			"type": "object",
			"properties": {
				"op":     {"type": "string", "enum": %s, "description": "Operation to perform."},
				"tenant": {"type": "string", "description": "ADMIN ONLY: the tenant to act in. Anyone else always acts in their own tenant, whatever this says."},%s
			},
			"required": ["op"]
		}`, ops, properties))
}

// wrapACL dispatches an access-control tool with ctx as the transport
// authenticated it. Unlike wrapBuiltin it stamps no identity and none of the
// operator-plane tool policies: the shared implementation authorizes from
// the principal on ctx alone (no principal, on the stdio launcher, is the
// operator, as on an open-mode HTTP server).
func wrapACL(toolName string, call func(connector.Connector, context.Context, json.RawMessage) (connector.ToolResult, error)) toolHandler {
	return func(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
		if env.connector == nil {
			return nil, fmt.Errorf("%s: no connector wired", toolName)
		}
		res, err := call(env.connector, ctx, args)
		if err != nil {
			return toolErrFrom(toolName, err), nil
		}
		return toolResultFromConnector(res), nil
	}
}
