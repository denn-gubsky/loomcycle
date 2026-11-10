package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// mcpErrForbidden is the JSON-RPC error code returned when a principal calls a
// meta-tool its scopes don't permit. It sits in the implementation-defined
// server-error range (-32000..-32099) so a client can distinguish "forbidden"
// from -32601 "unknown tool" and -32603 "internal error".
const mcpErrForbidden = -32001

// tenantConfinableTools is the explicit allowlist of /v1/_mcp meta-tools a
// non-admin (substrate:tenant) principal may list + call. Membership means the
// underlying tool keys on RunIdentity.TenantID — so the mcpPrincipalCtx tenant
// stamp plus the tool's own tenant filter confine it — NOT that this map grants
// any capability itself.
//
// A tool ABSENT from this set requires substrate:admin: deny-by-default, so a
// newly added meta-tool is admin-only until it is explicitly classified
// tenant-safe here (RFC AG §2 / §5). This mirrors requiredScopeFor's
// default-deny arm on the HTTP plane — the enforcement-correctness lives in this
// map, not the router, so an unclassified tool must fail closed.
//
// The /v1/_mcp route gate is substrate:tenant (RFC AG Phase 2), so a tenant
// token can open a session — and this map is the load-bearing per-operation
// authz that keeps it confined: a tenant session lists + may call only these
// tools; the admin-only ones are filtered out + 403'd by principalMayCallTool.
var tenantConfinableTools = map[string]bool{
	// identity: the read-only directory of the caller's OWN tenant (the
	// Identity tool). Tenant-confinable because the tenant comes from the
	// PRINCIPAL and there is no wire field for it, so a session cannot look up
	// another tenant's subject; what it may see inside the tenant is the
	// tool's own locks (read / manage principal). Its op=tenants sub-op is
	// separately refused for a non-admin inside the handler — that one op IS
	// cross-tenant, and the tool is listed here rather than in adminOnlyTools
	// so the useful ops stay reachable.
	"identity": true,
	// erasure: a data-subject report/erasure confined to the caller's own tenant.
	// Tenant-confinable rather than admin-only because the handler takes the
	// tenant from the PRINCIPAL and offers no wire field for it, so a tenant
	// session physically cannot reach another tenant's subject — the same posture
	// as credentialdef. It also needs substrate:tenant by name
	// (toolRequiredScope): POST /v1/_erasure is carved out of the member path,
	// and neither the handler nor the connector asks who is calling (RFC FY).
	"erasure": true,
	// retention_override: the connector takes the tenant from the PRINCIPAL
	// for anyone but an admin, so a tenant operator's session can only reach
	// its own tenant's agents. It also needs substrate:tenant by name
	// (toolRequiredScope): a member must not switch storage back on.
	"retention_override": true,
	// ontology: merge_subjects folds one subject into another in the
	// connector, which takes the tenant from the PRINCIPAL for anyone but an
	// admin and checks write on both documents. It also needs substrate:tenant
	// by name (toolRequiredScope): which subject survives is the operator's
	// call, not a member's.
	"ontology": true,
	// review: the statement review queue and bulk rulings. The connector takes
	// the tenant from the PRINCIPAL for anyone but an admin and asks each
	// statement whether the caller may rule on it (an operator's write, or a
	// member's access-control `review` grant). No scope by name: a member
	// holding `review` on a path must reach it, as it reaches the HTTP routes.
	"review": true,
	// Run lifecycle — tenant flows via the run identity (applyPrincipal on
	// the wire identity lands in RFC AG Phase 1).
	"spawn_run":   true,
	"spawn_runs":  true,
	"cancel_run":  true,
	"get_run":     true,
	"compact_run": true,
	"retune_run":  true,
	// review_run: the connector gates the verdict on the run's tenant and
	// session owner, like steering.
	"review_run": true,
	// configured_run: create / edit / start / discard a draft; every op goes
	// through the connector's tenant gate and keeps the draft's identity.
	"configured_run": true,
	"list_runs":      true,
	// delete_run_tree: the connector requires read access to the root through
	// the tenant gate and refuses a tree that leaves the root's tenant. It
	// additionally needs runs:delete (toolRequiredScope), which a member holds
	// only by explicit grant.
	"delete_run_tree": true,

	// Agent management.
	"register_agent":   true,
	"unregister_agent": true,
	"list_agents":      true,

	// Def authoring — each stamps the row's tenant from ctx and opaque-404s
	// cross-tenant reads.
	"agentdef": true,
	"skilldef": true,
	"teamdef":  true, // RFC AP — tenant-confined team-workflow substrate
	// A team's own channels, read by team and local name. The connector
	// confines the caller to teams its tenant holds (another tenant's is not
	// found) and to its own user's keyspace of a user-scoped channel.
	"list_team_channels": true,
	"peek_team_channel":  true,
	// The operator's half of a hold on a team's own channel: delivers what
	// the team wrote and its definition held. Same confinement as the reads.
	"release_team_channel": true,
	"hookdef":              true, // tenant-confined reusable hook definitions
	"mcpserverdef":         true,
	"scheduledef":          true,
	"a2aservercarddef":     true,
	"a2aagentdef":          true,
	"webhookdef":           true,
	"memorybackenddef":     true,
	"documentsourcedef":    true, // RFC CE — tenant-confined remote-document-source substrate
	"volumedef":            true,
	"credentialdef":        true, // RFC AR — tenant/user-confined secure credential store
	// crypto (RFC EP P1b): the connector takes the tenant and the subject
	// whose user keys it may use from the PRINCIPAL; the only wire field that
	// names a tenant is the admin-only top-level "tenant", which it refuses
	// for anyone else. It also needs substrate:tenant by name
	// (toolRequiredScope): it uses the tenant's keys.
	"crypto": true,
	// file (RFC FF D16): a run's inbox, read through get_run's tenant and
	// owner gate; the writing ops need the run's owner or an admin (a tenant
	// operator is one for its own tenant's runs), and keep reaches only the
	// caller's own tenant's volumes. It needs runs:read by
	// name (toolRequiredScope), as get_run does.
	"file": true,

	// RFC EZ access-control management. The shared implementation stamps the
	// tenant from the principal (an admin alone may focus another) and holds
	// every gate itself; groupdef/acldef/aclbinding also need substrate:tenant
	// by name (toolRequiredScope), as their HTTP routes keep members out.
	"groupdef":   true,
	"acldef":     true,
	"aclbinding": true,
	// authz: the questions a principal asks about itself; naming another
	// subject needs a tenant operator, which the implementation checks.
	"authz": true,
	// clientdef: the implementation stamps the tenant from the principal (an
	// admin alone names another) and opaque-404s another tenant's client. It
	// also needs substrate:tenant by name (toolRequiredScope), as its HTTP
	// route keeps members out.
	"clientdef": true,

	// Per-(scope, scope_id, tenant) data tools.
	"memory":     true,
	"channel":    true,
	"channeldef": true,
	"path":       true,
	"document":   true,
	// RFC BE History — the ctx tenant stamp + the tool's own scope fold confine
	// a tenant session to its own tenant; the cross-tenant `global` scope is
	// refused by the admin-gated history policy (grantOperatorPolicies).
	"history": true,
	// decision: it holds no tenant's data (it reads only the state passed in the
	// call), and the connector resolves the provider key and the bill for the
	// caller's own principal. It spends tokens, so it additionally needs the
	// scope that creates a run (toolRequiredScope).
	"decision": true,

	// Per-run / per-user — tenant inherited; the underlying tool applies its
	// own own-subject / cross-tenant-404 gate.
	"evaluation":             true,
	"context":                true,
	"interruption_resolve":   true,
	"publish_channel":        true,
	"subscribe_channel":      true,
	"peek_channel":           true,
	"ack_channel":            true,
	"stream_user_run_states": true,
	// operatortokendef: a tenant operator's own tenant's metrics scrape
	// tokens and nothing else — the tool's grant comes from the principal
	// (tools.OperatorTokenDefPolicyFor), which confines it to those. It also
	// needs substrate:tenant by name (toolRequiredScope), as the route's
	// member carve-out keeps a member from minting.
	"operatortokendef": true,
}

// adminOnlyTools enumerates the runtime-global / operator-plane meta-tools that
// have NO tenant dimension and so cannot be confined — they stay admin-only
// (RFC AG §2). The gate does NOT consult this set (it relies on
// tenantConfinableTools + deny-by-default); it exists so the drift test can
// assert every dispatchable tool is *consciously* classified as one or the
// other, and that the two sets are disjoint. Adding a meta-tool without
// classifying it here turns the drift test red.
var adminOnlyTools = map[string]bool{
	// Runtime-global control + introspection.
	"pause_runtime":     true,
	"resume_runtime":    true,
	"get_runtime_state": true,
	"resolve_probe":     true,

	// Snapshots capture / restore cross-tenant state.
	"create_snapshot":  true,
	"list_snapshots":   true,
	"get_snapshot":     true,
	"export_snapshot":  true,
	"restore_snapshot": true,
	"delete_snapshot":  true,

	// Operator aggregate over every scope.
	"list_channels": true,
}

// userSelfServiceTools is the RFC CN allowlist for an ISOLATED substrate:user
// session: the ONLY meta-tools an isolated user may list + call over /v1/_mcp. An
// isolated user is admitted to open a session solely to self-serve its OWN
// scope=user credentials from the `loomcycle mcp` thin client; the credentialdef
// dispatch applies the scope=user constraint (credential.ConstrainToUserScope), so
// this session grants nothing beyond the user's own tokens. Every entry MUST also
// be in tenantConfinableTools (an isolated user is a strict subset of a tenant
// session's surface); the drift test asserts that.
var userSelfServiceTools = map[string]bool{
	"credentialdef": true, // RFC CN — a user's own scope=user credential store.
	// authz: an isolated user may ask about itself on HTTP and gRPC, so here
	// too; it is confined to its own subject by the implementation.
	"authz": true,
	// evaluation: reachable by an isolated user on HTTP and gRPC, so here too.
	// The grant, not this list, holds it to submit_self
	// (tools.OffRunEvaluationPolicy), and the tool to its own runs.
	"evaluation": true,
}

// toolRequiredScope is a scope a principal must hold to list + call a tool, ON
// TOP of its class's allowlist above. The allowlists say which tools a class of
// principal can be confined on; they do not look at what the token was granted.
// A non-isolated member token reaches /v1/_mcp whatever its scopes are (the
// route's member path), so without this a token minted with only runs:read
// could start, steer and cancel runs here, or spend tokens on a decision, while
// POST /v1/runs and the Run RPC refuse it.
//
// Each entry is the scope the tool's gRPC twin needs (grpcConsumerScopes), which
// mirrors the HTTP route's. A tool with no entry is governed by its allowlist
// alone: the def-authoring and data tools, whose HTTP routes a member also
// reaches on the member path.
var toolRequiredScope = map[string]string{
	// An operator's control, not member data: a member who could set one could
	// read what its colleagues' no-trace runs said.
	"retention_override": auth.ScopeTenant,
	// The tenant's keys, which POST /v1/_crypto keeps from a tenant's
	// members: a member who could decrypt with them could read whatever the
	// tenant sealed.
	"crypto": auth.ScopeTenant,
	// Folding one ontology subject into another, which POST
	// /v1/_ontology/subjects/merge keeps from a tenant's members.
	"ontology": auth.ScopeTenant,
	// Reporting on or erasing a subject, which /v1/_erasure keeps from a
	// tenant's members: a member could otherwise erase a colleague.
	"erasure": auth.ScopeTenant,
	// Starting a run, and every write on a run's state.
	"spawn_run":            auth.ScopeRunsCreate,
	"spawn_runs":           auth.ScopeRunsCreate,
	"cancel_run":           auth.ScopeRunsCreate,
	"compact_run":          auth.ScopeRunsCreate,
	"retune_run":           auth.ScopeRunsCreate,
	"review_run":           auth.ScopeRunsCreate,
	"configured_run":       auth.ScopeRunsCreate,
	"interruption_resolve": auth.ScopeRunsCreate,
	// Deleting a run tree removes stored records: a scope of its own, not
	// implied for a member.
	"delete_run_tree": auth.ScopeRunsDelete,
	// Reading runs.
	"get_run":                auth.ScopeRunsRead,
	"file":                   auth.ScopeRunsRead,
	"list_runs":              auth.ScopeRunsRead,
	"stream_user_run_states": auth.ScopeRunsRead,
	// The per-user channel surface.
	"publish_channel":   auth.ScopeChannelPublish,
	"ack_channel":       auth.ScopeChannelPublish,
	"subscribe_channel": auth.ScopeChannelRead,
	"peek_channel":      auth.ScopeChannelRead,
	// A team's own channels are channels: reading one is a channel read, and
	// releasing what one holds is a channel write. The HTTP routes ask the
	// same of a member; the RPCs ask substrate:tenant, which implies both.
	"list_team_channels":   auth.ScopeChannelRead,
	"peek_team_channel":    auth.ScopeChannelRead,
	"release_team_channel": auth.ScopeChannelPublish,
	// Asking a decision model outside a run spends tokens: the scope
	// POST /v1/_decide and the Decide RPC require.
	"decision": auth.ScopeRunsCreate,
	// Evaluation: the floor POST /v1/_evaluation and the Evaluation RPC gate on.
	"evaluation": auth.ScopeRunsRead,
	// Groups, lists and bindings are the tenant operator's: the scope their
	// HTTP routes and RPCs require, which keeps a member out here too.
	"groupdef":   auth.ScopeTenant,
	"acldef":     auth.ScopeTenant,
	"aclbinding": auth.ScopeTenant,
	// An app client decides where people's sign-in codes go: the operator's
	// to register, as POST /v1/_clientdef requires.
	"clientdef": auth.ScopeTenant,
	// Handing out a credential, even a tenant's own scrape token, is the
	// operator's: POST /v1/_operatortokendef is carved out of the member path.
	"operatortokendef": auth.ScopeTenant,
}

// principalMayCallTool reports whether the principal on ctx may list/invoke
// toolName over the /v1/_mcp transport (RFC AG §3.3):
//
//   - No principal (stdio / open mode): process-local operator-trust → every tool.
//   - Admin principal (substrate:admin, incl. the legacy token): every tool.
//   - Isolated substrate:user principal (RFC CN): only the user self-service
//     allowlist — everything else, including the tenant-confinable tools, stays
//     closed.
//   - Non-admin, non-isolated (substrate:tenant / RFC CB member) principal: the
//     tenant-confinable allowlist; everything else — including an unclassified new
//     tool — is admin-only (deny-by-default).
//   - Any non-admin principal additionally needs a tool's toolRequiredScope,
//     when it has one.
//
// runPlaneTools is the run plane on this transport: the whole of what a
// credential with an agent allowlist may call (auth.Restrictions.RunPlaneOnly).
// `decision` is deliberately absent — a model call with no agent — and so are
// teamdef, every other definition tool, channels, memory and documents.
var runPlaneTools = map[string]bool{
	"spawn_run": true, "spawn_runs": true, "configured_run": true,
	"cancel_run": true, "compact_run": true, "retune_run": true, "review_run": true,
	"interruption_resolve": true,
	"get_run":              true, "list_runs": true, "stream_user_run_states": true,
}

// mcpToolQuestions is every ACL question the per-tool gate asks of toolName,
// whatever the op: `invoke mcp-tool:<name>`, and the question each op's HTTP
// twin route asks (RFC FY Part E), read through authz.HTTPSurface so the two
// cannot differ — before, a `deny cancel run:*` refused POST
// /v1/runs/{id}/cancel and let cancel_run through. An op the twin route
// answers per call is asked by the shared operation behind both (the
// connector). The enforcement census
// (TestACLEnforcement_MetaToolsAskWhatTheTableSays) reads this.
func mcpToolQuestions(toolName string) []authz.Surface {
	out := []authz.Surface{authz.MCPToolSurface(toolName)}
	tw, ok := mcpToolTwins[toolName]
	if !ok {
		return out
	}
	if tw.Ops == nil {
		if q, ok := authz.HTTPSurface(tw.Method, tw.Path); ok {
			out = append(out, q)
		}
		return out
	}
	for _, ot := range tw.Ops {
		if q, ok := authz.HTTPSurface(ot.Method, ot.Path); ok {
			out = append(out, q)
		}
	}
	return out
}

// errUnmappedOp is a call of a per-op meta-tool naming an op its twin table
// does not map: refused before the handler, never let through on another
// op's question.
type errUnmappedOp struct{ tool, op string }

func (e errUnmappedOp) Error() string {
	return fmt.Sprintf("%s: op %q is not one this server asks access-control questions for", e.tool, e.op)
}

// errAmbiguousArgs is a call of a per-op meta-tool whose arguments name the op
// or the name argument more than once — a duplicate key or a case variant.
// The handlers decode into structs, which match keys case-insensitively and
// keep the last, so the gate would have to guess which one runs: it refuses.
type errAmbiguousArgs struct{ tool, key string }

func (e errAmbiguousArgs) Error() string {
	return fmt.Sprintf("%s: the arguments name %q more than once (a repeated key or another spelling of it); name it once", e.tool, e.key)
}

// opTwinHead is how a per-op tool's handler reads the op and the channel name:
// encoding/json struct decoding, so the gate reads them the same way.
type opTwinHead struct {
	Op   string `json:"op"`
	Name string `json:"name"`
}

// refuseAmbiguousKeys reports the first of keys that args names more than
// once, comparing as encoding/json matches a struct field (case folding).
func refuseAmbiguousKeys(args json.RawMessage, keys ...string) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(args))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return "", false // not an object: the handler refuses it
	}
	seen := map[string]int{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return "", false
		}
		k, _ := t.(string)
		for _, want := range keys {
			if strings.EqualFold(k, want) {
				seen[want]++
				if seen[want] > 1 {
					return want, true
				}
			}
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", false
		}
	}
	return "", false
}

// mcpTwinSurface is the question toolName's HTTP twin route asks for a call
// with args, and false when it has none. A per-op tool answers each op with
// its own route, the instance taken from the call's `name` when the op's route
// names one; an op it does not map is an errUnmappedOp, and arguments naming
// the op or the name twice an errAmbiguousArgs.
func mcpTwinSurface(toolName string, args json.RawMessage) (authz.Surface, bool, error) {
	tw, ok := mcpToolTwins[toolName]
	if !ok {
		return authz.Surface{}, false, nil
	}
	if tw.Ops == nil {
		q, governed := authz.HTTPSurface(tw.Method, tw.Path)
		return q, governed, nil
	}
	if key, dup := refuseAmbiguousKeys(args, "op", "name"); dup {
		return authz.Surface{}, false, errAmbiguousArgs{tool: toolName, key: key}
	}
	var head opTwinHead
	_ = json.Unmarshal(args, &head)
	op := head.Op
	if op == "" {
		op = tw.DefaultOp
	}
	ot, mapped := tw.Ops[op]
	if !mapped {
		return authz.Surface{}, false, errUnmappedOp{tool: toolName, op: op}
	}
	q, governed := authz.HTTPSurface(ot.Method, ot.Path)
	if governed && ot.ByName {
		q.Name = head.Name
		if q.Name == "" {
			q.Name = authz.AnyInstance
		}
	}
	return q, governed, nil
}

// mcpTwinRefusal asks the principal on ctx the question toolName's twin route
// asks for this call, after the per-tool gate admitted it. It returns the
// refusal the twin route would write — acl_denied, or acl_unavailable when
// enforce mode has no set — as the tool's error result, or nil when the call
// may proceed. An op a per-op tool does not map is refused (unknown_op)
// whoever calls. No principal (stdio, open mode): nothing else to ask.
func mcpTwinRefusal(ctx context.Context, toolName string, args json.RawMessage) *loommcp.CallToolResult {
	q, twinned, err := mcpTwinSurface(toolName, args)
	if err != nil {
		code := "unknown_op"
		if _, ambiguous := err.(errAmbiguousArgs); ambiguous {
			code = "ambiguous_arguments"
		}
		body, _ := json.Marshal(map[string]any{"code": code, "error": err.Error()})
		return toolErr(string(body))
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || !twinned {
		return nil
	}
	res := authz.EvaluateSurface(ctx, p.Effective, q, true)
	if res.Diverged {
		slog.Warn("authz: acl_shadow_diverged", "tenant", p.TenantID, "subject", p.Subject, "action", q.Action,
			"resource", q.Type+":"+q.Name, "outcome", res.Decision.Outcome.String(), "transport", authz.TransportFrom(ctx))
	}
	code, msg := "", ""
	switch {
	case res.Unavailable:
		code, msg = "acl_unavailable", "access control could not be evaluated; retry"
	case !res.Allowed:
		code, msg = "acl_denied", fmt.Sprintf("%s %s:%s is denied", q.Action, q.Type, q.Name)
		slog.Warn("authz: acl_denied", "tenant", p.TenantID, "subject", p.Subject, "action", q.Action,
			"resource", q.Type+":"+q.Name, "outcome", res.Decision.Outcome.String(), "transport", authz.TransportFrom(ctx))
	default:
		return nil
	}
	body, _ := json.Marshal(map[string]any{"code": code, "error": msg, "action": q.Action, "resource": q.Type + ":" + q.Name, "reasons": res.Decision.Reasons})
	return toolErr(string(body))
}

// mcpToolTwin is the HTTP route a meta-tool mirrors. A tool whose ops are one
// route each (a channel's create, update, delete and purge) maps every op in
// Ops instead, and an op not there is refused; DefaultOp is the op an omitted
// `op` means. A tool whose ops all share one route with the op in the body
// (a def family, document, path) names that route: the shared operation asks
// each op its own question. Placeholder segments are `*` (or a name the op's
// ByName argument replaces) so an instance route asks about every instance.
type mcpToolTwin struct {
	Method, Path string
	Ops          map[string]mcpOpTwin
	DefaultOp    string
}

// mcpOpTwin is one op's route. ByName: the route's name segment stands for the
// call's `name` argument, the instance the gate asks about.
type mcpOpTwin struct {
	Method, Path string
	ByName       bool
}

// mcpToolTwins maps each meta-tool to its HTTP twin. A meta-tool with none is
// in mcpToolsWithoutTwin with the reason; TestMCPToolTwins_EveryToolIsListed
// holds every tool to one of the two.
var mcpToolTwins = map[string]mcpToolTwin{
	"spawn_run":              {Method: "POST", Path: "/v1/runs"},
	"spawn_runs":             {Method: "POST", Path: "/v1/runs:batch"},
	"configured_run":         {Method: "POST", Path: "/v1/runs"},
	"cancel_run":             {Method: "POST", Path: "/v1/runs/r/cancel"},
	"get_run":                {Method: "GET", Path: "/v1/runs/r"},
	"list_runs":              {Method: "GET", Path: "/v1/runs"},
	"stream_user_run_states": {Method: "GET", Path: "/v1/users/u/agents/stream"},
	"compact_run":            {Method: "POST", Path: "/v1/runs/r/compact"},
	"retune_run":             {Method: "POST", Path: "/v1/runs/r/retune"},
	"review_run":             {Method: "POST", Path: "/v1/runs/r/review"},
	"delete_run_tree":        {Method: "DELETE", Path: "/v1/runs/r/tree"},
	"interruption_resolve":   {Method: "POST", Path: "/v1/runs/r/interrupts/i/resolve"},
	"file":                   {Method: "GET", Path: "/v1/runs/r/file"},
	"retention_override": {DefaultOp: "list", Ops: map[string]mcpOpTwin{
		"list":  {Method: "GET", Path: "/v1/_retention/overrides"},
		"set":   {Method: "PUT", Path: "/v1/_retention/overrides"},
		"clear": {Method: "DELETE", Path: "/v1/_retention/overrides"},
	}},
	"erasure": {DefaultOp: "report", Ops: map[string]mcpOpTwin{
		"report":  {Method: "GET", Path: "/v1/_erasure"},
		"execute": {Method: "POST", Path: "/v1/_erasure"},
	}},
	"ontology": {Ops: map[string]mcpOpTwin{
		"merge_subjects": {Method: "POST", Path: "/v1/_ontology/subjects/merge"},
		"bind_principal": {Method: "POST", Path: "/v1/_ontology/subjects/principal"},
	}},
	"review": {Ops: map[string]mcpOpTwin{
		"queue":        {Method: "GET", Path: "/v1/_statements/review"},
		"rule":         {Method: "POST", Path: "/v1/_statements/rule"},
		"revert":       {Method: "POST", Path: "/v1/_statements/revert"},
		"rulings":      {Method: "GET", Path: "/v1/_statements/rulings"},
		"apply_policy": {Method: "POST", Path: "/v1/_statements/apply_policy"},
	}},
	"list_agents":       {Method: "GET", Path: "/v1/_runnable-agents"},
	"register_agent":    {Method: "POST", Path: "/v1/_agentdef"},
	"unregister_agent":  {Method: "POST", Path: "/v1/_agentdef"},
	"agentdef":          {Method: "POST", Path: "/v1/_agentdef"},
	"skilldef":          {Method: "POST", Path: "/v1/_skilldef"},
	"teamdef":           {Method: "POST", Path: "/v1/_teamdef"},
	"hookdef":           {Method: "POST", Path: "/v1/_hookdef"},
	"mcpserverdef":      {Method: "POST", Path: "/v1/_mcpserverdef"},
	"scheduledef":       {Method: "POST", Path: "/v1/_scheduledef"},
	"a2aservercarddef":  {Method: "POST", Path: "/v1/_a2aservercarddef"},
	"a2aagentdef":       {Method: "POST", Path: "/v1/_a2aagentdef"},
	"webhookdef":        {Method: "POST", Path: "/v1/_webhookdef"},
	"memorybackenddef":  {Method: "POST", Path: "/v1/_memorybackenddef"},
	"documentsourcedef": {Method: "POST", Path: "/v1/_documentsourcedef"},
	"volumedef":         {Method: "POST", Path: "/v1/_volumedef"},
	"clientdef":         {Method: "POST", Path: "/v1/_clientdef"},
	"operatortokendef":  {Method: "POST", Path: "/v1/_operatortokendef"},
	"credentialdef":     {Method: "POST", Path: "/v1/_credentialdef"},
	"crypto":            {Method: "POST", Path: "/v1/_crypto"},
	"groupdef":          {Method: "GET", Path: "/v1/_groups"},
	"acldef":            {Method: "POST", Path: "/v1/_acldef"},
	"aclbinding":        {Method: "GET", Path: "/v1/_aclbinding"},
	"evaluation":        {Method: "POST", Path: "/v1/_evaluation"},
	"path":              {Method: "POST", Path: "/v1/_path"},
	"document":          {Method: "POST", Path: "/v1/_document"},
	"history":           {Method: "POST", Path: "/v1/_history"},
	"decision":          {Method: "POST", Path: "/v1/_decide"},
	"memory":            {Method: "GET", Path: "/v1/_memory/scopes/user/u/keys/k"},
	// create asks manage of the name it is about to get, as the shared
	// operation behind POST /v1/_channels does (askChannelManage).
	"channeldef": {Ops: map[string]mcpOpTwin{
		"create": {Method: "POST", Path: "/v1/_channels", ByName: true},
		"update": {Method: "PATCH", Path: "/v1/_channels/c", ByName: true},
		"delete": {Method: "DELETE", Path: "/v1/_channels/c", ByName: true},
		"purge":  {Method: "POST", Path: "/v1/_channels/c/purge", ByName: true},
	}},
	"channel":              {Method: "GET", Path: "/v1/_channels"},
	"list_channels":        {Method: "GET", Path: "/v1/_channels"},
	"publish_channel":      {Method: "POST", Path: "/v1/_channels/*/publish"},
	"subscribe_channel":    {Method: "POST", Path: "/v1/_channels/*/subscribe"},
	"peek_channel":         {Method: "GET", Path: "/v1/_channels/*/peek"},
	"ack_channel":          {Method: "POST", Path: "/v1/_channels/*/ack"},
	"list_team_channels":   {Method: "GET", Path: "/v1/_teamdef/t/channels"},
	"peek_team_channel":    {Method: "GET", Path: "/v1/_teamdef/t/channels/c/peek"},
	"release_team_channel": {Method: "POST", Path: "/v1/_teamdef/t/channels/c/release"},
	"pause_runtime":        {Method: "POST", Path: "/v1/_pause"},
	"resume_runtime":       {Method: "POST", Path: "/v1/_resume"},
	"get_runtime_state":    {Method: "GET", Path: "/v1/_state"},
	"resolve_probe":        {Method: "POST", Path: "/v1/_resolve/probe"},
	"create_snapshot":      {Method: "POST", Path: "/v1/_snapshots"},
	"list_snapshots":       {Method: "GET", Path: "/v1/_snapshots"},
	"get_snapshot":         {Method: "GET", Path: "/v1/_snapshots/x"},
	"export_snapshot":      {Method: "GET", Path: "/v1/_snapshots/x/export"},
	"restore_snapshot":     {Method: "POST", Path: "/v1/_snapshots/x/restore"},
	"delete_snapshot":      {Method: "DELETE", Path: "/v1/_snapshots/x"},
}

// mcpOpsInBody are the twinned meta-tools that take several ops through ONE
// question: the ops share a route with the op in its body, or every op's route
// asks the same question, and the operation behind them asks each op its own
// instance question on every transport. TestMCPToolTwins_EveryOpIsClassified
// holds every meta-tool with an `op` to this list, to a per-op twin (Ops), or
// to mcpToolsWithoutTwin, so a new op on a per-op tool, or a new multi-op
// tool, cannot land asking another op's question.
var mcpOpsInBody = map[string]string{
	"configured_run":    "create, update, start and delete each ask run agent:*, as their routes do; the run intake asks the agent",
	"memory":            "a write op asks write memory:* (sqlmem for SQL) in the shared operation (memoryOpQuestion)",
	"channel":           "each op asks its own route's question of the channel it names in the shared operation (channelOpQuestions)",
	"file":              "one route, GET|POST /v1/runs/{id}/file; the File twin asks the run's owner for its writing ops",
	"path":              "one route; the Path tool asks each op's verb of the name it touches",
	"document":          "one route; the Document tool asks each op's verb of the document it resolves",
	"history":           "one route; the History tool reads only the caller's own scopes",
	"evaluation":        "one route; the Evaluation tool holds each op to the token's evaluation scopes",
	"crypto":            "one route; the Crypto tool asks use of each key it resolves",
	"credentialdef":     "one route; CredentialDef asks use or manage of the credential each op names",
	"operatortokendef":  "one route; the token tool asks mint, manage or read of the tokens each op acts on",
	"groupdef":          "the family's weakest verb; the shared operation asks manage of a write",
	"aclbinding":        "the family's weakest verb; the shared operation asks author of a write",
	"acldef":            "one route; the shared operation asks author, retire or read of the named list",
	"agentdef":          defFamilyOps,
	"skilldef":          defFamilyOps,
	"teamdef":           defFamilyOps,
	"hookdef":           defFamilyOps,
	"mcpserverdef":      defFamilyOps,
	"scheduledef":       defFamilyOps,
	"a2aservercarddef":  defFamilyOps,
	"a2aagentdef":       defFamilyOps,
	"webhookdef":        defFamilyOps,
	"memorybackenddef":  defFamilyOps,
	"documentsourcedef": defFamilyOps,
	"volumedef":         defFamilyOps,
	"clientdef":         defFamilyOps,
}

const defFamilyOps = "one route; the definition operation asks author, retire or read of the definition each op names"

// mcpToolsWithoutTwin are the meta-tools no HTTP route mirrors.
var mcpToolsWithoutTwin = map[string]string{
	"authz":    "the access questions are ungoverned on every transport (POST /v1/_authz/*)",
	"context":  "the caller's own identity and reach; no route",
	"identity": "the directory questions ask read or manage principal per call",
}

func principalMayCallTool(ctx context.Context, toolName string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return true
	}
	// Before the admin shortcut: a credential limited to certain agents is
	// held to the run plane however wide its scopes.
	if p.Restrictions.RunPlaneOnly() && !runPlaneTools[toolName] {
		return false
	}
	// A metrics scraper's credential calls no tool. The route gate keeps it
	// off /v1/_mcp already; this holds should a session reach here another way.
	if auth.IsMetricsReader(p) {
		return false
	}
	// RFC EY: an explicit ACL deny on the meta-tool holds for every class,
	// admin included (deny overrides); in enforce mode an unmatched tool is
	// refused and an unavailable set refuses everything.
	res := authz.EvaluateSurface(ctx, p.Effective, authz.MCPToolSurface(toolName), true)
	if res.Unavailable || !res.Allowed {
		return false
	}
	if auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return true
	}
	if required, gated := toolRequiredScope[toolName]; gated && !auth.HasScope(p.Scopes, required) {
		return false
	}
	if auth.IsIsolated(p, ok) {
		return userSelfServiceTools[toolName]
	}
	return tenantConfinableTools[toolName]
}

// scopeNeededFor names the scope a refused principal lacks for toolName, for
// the refusal's text: the tool's own required scope when that is what failed,
// else substrate:admin (the tool is outside the principal's allowlist).
func scopeNeededFor(ctx context.Context, toolName string) string {
	if required, gated := toolRequiredScope[toolName]; gated {
		if p, ok := auth.PrincipalFromContext(ctx); ok && !auth.HasScope(p.Scopes, required) {
			return required
		}
	}
	return auth.ScopeAdmin
}

// toolOpMissingScope reports the scope the principal on ctx lacks for the
// OPERATION a call asks of a tool that passed principalMayCallTool, and that
// operation; scope is "" when the call may proceed.
//
// toolRequiredScope gates a whole tool, which fits the run and channel tools:
// every operation of each needs the same scope. teamdef does not fit: most of
// its ops author definitions, which a member may do whatever its token holds,
// so the tool has no entry there and stays listed — but op=run starts a walk
// that spawns runs and spends tokens. Without this a token granted only
// runs:read, refused spawn_run, could start a team's agents through teamdef.
func toolOpMissingScope(ctx context.Context, toolName string, args json.RawMessage) (op, scope string) {
	if toolName != "teamdef" {
		return "", ""
	}
	return builtin.TeamDefMissingScope(ctx, args)
}
