package authz

import (
	"net/http"
	"strings"
)

// AnyInstance is the resource name of a TYPE-LEVEL question: "may the
// principal do this action on any instance of the type". A selector matches
// it only when it covers every instance (`agent:*`, `memory:*`,
// `document:*`), so a rule about some instances (`deny run agent:admin/*`)
// is neither an allow nor a deny at the type level — the instance check at
// the handler decides. The transport gates ask the type-level question
// when the request has not been read yet (RFC EZ §1.1).
const AnyInstance = "*"

// Surface is what one transport entry point asks the engine: the action
// and the resource, type-level (Name == AnyInstance) or the instance the
// path or method names.
type Surface struct {
	Action string
	Type   string
	Name   string
}

func typeLevel(action, typ string) Surface {
	return Surface{Action: action, Type: typ, Name: AnyInstance}
}

func instance(action, typ, name string) Surface {
	if name == "" {
		name = AnyInstance
	}
	return Surface{Action: action, Type: typ, Name: name}
}

// Resource returns the surface as a Resource in tenant.
func (s Surface) Resource(tenant string) Resource {
	return Resource{Type: s.Type, Name: s.Name, TenantID: tenant}
}

// HTTPSurface maps an HTTP route to the question its gate asks. The second
// result is false for a route no ACL governs (health, docs, whoami, the UI
// shell). The completeness test in the HTTP package holds that every
// registered route is either mapped here or in its ungoverned list, so a
// new route cannot land ungoverned by accident (RFC EZ §2).
//
// The gate asks the WEAKEST verb a route's handler may need when the
// request body carries the op (a `_<family>def` POST dispatches
// create/fork/get/list/retire on `op`): `read def:<fam>/*` here, the
// per-op instance question in the handler.
func HTTPSurface(method, path string) (Surface, bool) {
	if path == "/metrics" {
		return instance("read", "runtime", "metrics"), true
	}
	seg := strings.Split(strings.Trim(path, "/"), "/")
	// seg[0] is "v1" for every other governed route.
	if len(seg) < 2 || seg[0] != "v1" {
		return Surface{}, false
	}
	rest := seg[1:]
	get := method == http.MethodGet
	switch {
	// ---- runs and sessions ----
	case path == "/v1/runs":
		if get {
			return typeLevel("read", "run"), true
		}
		return typeLevel("run", "agent"), true
	case path == "/v1/runs:batch":
		return typeLevel("run", "agent"), true
	case rest[0] == "runs" && len(rest) >= 2:
		if len(rest) == 2 {
			if get {
				return typeLevel("read", "run"), true
			}
			return typeLevel("run", "agent"), true // PATCH / DELETE a configured draft
		}
		switch rest[2] {
		case "stream", "config", "prompt", "effective-config", "interrupts":
			if len(rest) == 5 && rest[4] == "resolve" {
				return typeLevel("steer", "run"), true
			}
			return typeLevel("read", "run"), true
		case "breakpoints":
			if get {
				return typeLevel("read", "run"), true
			}
			return typeLevel("steer", "run"), true
		case "input":
			return typeLevel("steer", "run"), true
		case "cancel":
			return typeLevel("cancel", "run"), true
		case "compact":
			return typeLevel("compact", "run"), true
		case "retune":
			return typeLevel("retune", "run"), true
		case "review":
			return typeLevel("review", "run"), true
		case "start":
			return typeLevel("run", "agent"), true
		case "tree":
			return typeLevel("delete", "run"), true
		}
	case rest[0] == "sessions" && len(rest) >= 2:
		if len(rest) == 2 {
			return typeLevel("delete", "run"), true
		}
		switch rest[2] {
		case "transcript":
			return typeLevel("read", "run"), true
		case "messages", "replay":
			return typeLevel("run", "agent"), true
		}
	case rest[0] == "agents" && len(rest) >= 2:
		switch {
		case len(rest) == 2:
			return typeLevel("read", "run"), true
		case rest[2] == "cancel":
			return typeLevel("cancel", "run"), true
		case rest[2] == "channels":
			return typeLevel("subscribe", "channel"), true
		}
	case rest[0] == "users" && len(rest) >= 3:
		switch rest[2] {
		case "agents", "interrupts":
			return typeLevel("read", "run"), true
		case "channels":
			if len(rest) >= 5 {
				return instance(channelVerb(rest[4]), "channel", rest[3]), true
			}
		}
	case path == "/v1/client-tools":
		return typeLevel("run", "agent"), true
	case path == "/v1/config":
		return Surface{}, false
	case path == "/v1/chat/completions", path == "/v1/_llm/chat":
		return instance("use", "gateway", "chat"), true
	case path == "/v1/embeddings":
		return instance("use", "gateway", "embeddings"), true
	}
	if !strings.HasPrefix(rest[0], "_") {
		return Surface{}, false
	}
	name := rest[0][1:]
	switch {
	case name == "me", name == "openapi.json", name == "openapi.yaml", name == "docs":
		return Surface{}, false
	case name == "resident":
		if get {
			return typeLevel("read", "run"), true
		}
		return typeLevel("cancel", "run"), true
	case name == "runnable-agents":
		return typeLevel("read", "agent"), true
	case name == "library":
		if len(rest) >= 2 {
			switch rest[1] {
			case "agents":
				return typeLevel("read", "agent"), true
			case "skills":
				return typeLevel("read", "skill"), true
			case "mcp-servers":
				return instance("read", "def", "mcpserver/*"), true
			}
		}
	case name == "acldef":
		// Every op of POST /v1/_acldef — get and list as well as create and
		// retire — is a POST with the op in the body, so the route asks the
		// weakest verb the family has. The handler asks author or retire of
		// the named list per op (aclListOpSurface). This arm comes before the
		// generic _<x>def one, which would otherwise take the name.
		return instance("read", "def", "acl/*"), true
	case strings.HasSuffix(name, "def") && name != "operatortokendef":
		fam := defFamily(strings.TrimSuffix(name, "def"))
		if len(rest) >= 3 && rest[2] == "overridable" {
			return typeLevel("read", "agent"), true
		}
		if len(rest) >= 3 && rest[2] == "channels" || len(rest) >= 4 && (rest[3] == "peek" || rest[3] == "release") {
			return typeLevel("subscribe", "channel"), true
		}
		return instance("read", "def", fam+"/*"), true
	case name == "operatortokendef":
		return instance("mint", "principal", "token:*"), true
	case name == "history":
		return typeLevel("read", "history"), true
	case name == "evaluation":
		return typeLevel("read", "evaluation"), true
	case name == "schedules":
		if get {
			return instance("read", "def", "schedule/*"), true
		}
		return instance("manage", "def", "schedule/*"), true
	case name == "channels":
		if len(rest) == 1 {
			if get {
				return typeLevel("subscribe", "channel"), true
			}
			return typeLevel("manage", "channel"), true
		}
		if rest[1] == "_await" {
			return typeLevel("await", "channel"), true
		}
		if rest[1] == "_broadcast" {
			return typeLevel("broadcast", "channel"), true
		}
		if len(rest) == 2 {
			if get {
				return instance("subscribe", "channel", rest[1]), true
			}
			return instance(map[bool]string{true: "manage", false: "publish"}[method == http.MethodPatch || method == http.MethodDelete], "channel", rest[1]), true
		}
		return instance(channelVerb(rest[2]), "channel", rest[1]), true
	case name == "memory":
		if len(rest) >= 2 && rest[1] == "repair-tenant" {
			return instance("manage", "runtime", "tenants"), true
		}
		if get {
			return typeLevel("read", "memory"), true
		}
		return typeLevel("write", "memory"), true
	case name == "path", name == "document", name == "ontology":
		if len(rest) >= 2 && (rest[1] == "describe_images" || rest[1] == "reindex" || rest[1] == "derive_units" || rest[1] == "adopt" || rest[1] == "proposals") {
			return typeLevel("write", "document"), true
		}
		return typeLevel("read", "document"), true
	case name == "volumes":
		return typeLevel("read", "volume"), true
	case name == "credentialdef":
		return typeLevel("use", "credential"), true
	case name == "crypto":
		return typeLevel("use", "crypto"), true
	case name == "usage":
		return instance("read", "runtime", "usage"), true
	case name == "limits":
		return instance(readOrManage(method), "runtime", "limits"), true
	case name == "retention":
		return instance(readOrManage(method), "runtime", "retention"), true
	case name == "erasure":
		return instance(readOrManage(method), "runtime", "erasure"), true
	case name == "routing", name == "models":
		return instance("read", "runtime", "routing"), true
	case name == "resolver", name == "resolve", name == "providers", name == "config", name == "presets", name == "env_template":
		if get {
			return instance("read", "runtime", "config"), true
		}
		return instance("manage", "runtime", "config"), true
	case name == "decide":
		return instance("use", "gateway", "decide"), true
	case name == "users":
		switch {
		case len(rest) == 1 && get:
			return instance("read", "principal", "user:*"), true
		case len(rest) == 1:
			return instance("manage", "principal", "user:*"), true
		case len(rest) >= 3 && rest[2] == "tokens":
			return instance("mint", "principal", "user:"+rest[1]), true
		case get:
			return instance("read", "principal", "user:"+rest[1]), true
		default:
			return instance("manage", "principal", "user:"+rest[1]), true
		}
	case name == "tenants":
		return instance("read", "runtime", "tenants"), true
	case name == "snapshots":
		return instance(readOrManage(method), "runtime", "snapshot"), true
	case name == "pause", name == "resume":
		return instance("manage", "runtime", "pause"), true
	case name == "state":
		return instance("read", "runtime", "pause"), true
	case name == "metrics", name == "concurrency":
		return instance("read", "runtime", "metrics"), true
	case name == "events":
		return instance("read", "runtime", "events"), true
	case name == "mcp":
		return instance("use", "transport", "mcp"), true
	case name == "webhooks":
		return instance("manage", "def", "webhook/*"), true
	case name == "teams":
		return typeLevel("run", "team"), true
	case name == "groups":
		if get {
			return instance("read", "principal", "group:*"), true
		}
		return instance("manage", "principal", "group:*"), true
	case name == "aclbinding":
		if get {
			return instance("read", "def", "acl/*"), true
		}
		return instance("author", "def", "acl/*"), true
	case name == "authz":
		return Surface{}, false
	}
	return Surface{}, false
}

func readOrManage(method string) string {
	if method == http.MethodGet {
		return "read"
	}
	return "manage"
}

func channelVerb(op string) string {
	switch op {
	case "publish":
		return "publish"
	case "subscribe", "peek":
		return "subscribe"
	case "ack":
		return "ack"
	case "await":
		return "await"
	case "broadcast":
		return "broadcast"
	}
	return "manage"
}

// defFamily maps a `_<x>def` route name to its catalog family.
func defFamily(route string) string {
	switch route {
	case "a2aservercard":
		return "a2aserver"
	case "a2aagent":
		return "a2a"
	}
	return route
}

// GRPCSurface maps a gRPC RPC name (without the service prefix) to its
// question. The second result is false for Health and Config.
func GRPCSurface(rpc string) (Surface, bool) {
	switch rpc {
	case "Run", "Continue", "SpawnRunBatch", "CreateConfiguredRun", "UpdateConfiguredRun", "StartConfiguredRun", "DeleteConfiguredRun", "ReplaySession":
		return typeLevel("run", "agent"), true
	case "StreamRun", "GetTranscript", "GetAgent", "GetRun", "ListUserAgents", "ListWalkRuns", "ListChildRuns", "StreamUserRunStates":
		return typeLevel("read", "run"), true
	case "RunInput", "ResolveInterrupt":
		return typeLevel("steer", "run"), true
	case "RetuneRun":
		return typeLevel("retune", "run"), true
	case "CancelTurn", "CancelAgent":
		return typeLevel("cancel", "run"), true
	case "ReviewRun":
		return typeLevel("review", "run"), true
	case "CompactRun":
		return typeLevel("compact", "run"), true
	case "DeleteRunTree":
		return typeLevel("delete", "run"), true
	case "Decide", "ListDecisionModels":
		return instance("use", "gateway", "decide"), true
	case "PublishChannel":
		return typeLevel("publish", "channel"), true
	case "AckChannel":
		return typeLevel("ack", "channel"), true
	case "SubscribeChannel", "PeekChannel", "ListChannels", "ListTeamChannel", "PeekTeamChannel", "ListTeamChannels":
		return typeLevel("subscribe", "channel"), true
	case "ReleaseTeamChannel":
		return typeLevel("manage", "channel"), true
	case "AwaitChannels":
		return typeLevel("await", "channel"), true
	case "BroadcastChannels":
		return typeLevel("broadcast", "channel"), true
	case "UsageReport":
		return instance("read", "runtime", "usage"), true
	case "TokenLimit":
		return instance("manage", "runtime", "limits"), true
	case "RetentionOverride":
		return instance("manage", "runtime", "retention"), true
	case "DirectoryUsers", "DirectoryInspect":
		return instance("read", "principal", "user:*"), true
	case "DirectoryTenants":
		return instance("read", "runtime", "tenants"), true
	case "ErasureReport":
		return instance("read", "runtime", "erasure"), true
	case "ErasureExecute":
		return instance("manage", "runtime", "erasure"), true
	case "AgentDef":
		return instance("read", "def", "agent/*"), true
	case "SkillDef":
		return instance("read", "def", "skill/*"), true
	case "MCPServerDef":
		return instance("read", "def", "mcpserver/*"), true
	case "ScheduleDef":
		return instance("read", "def", "schedule/*"), true
	case "A2AServerCardDef":
		return instance("read", "def", "a2aserver/*"), true
	case "A2AAgentDef":
		return instance("read", "def", "a2a/*"), true
	case "WebhookDef":
		return instance("read", "def", "webhook/*"), true
	case "MemoryBackendDef":
		return instance("read", "def", "memorybackend/*"), true
	case "DocumentSourceDef":
		return instance("read", "def", "documentsource/*"), true
	case "VolumeDef":
		return instance("read", "def", "volume/*"), true
	case "TeamDef":
		return instance("read", "def", "team/*"), true
	case "HookDef":
		return instance("read", "def", "hook/*"), true
	case "Path", "Document":
		return typeLevel("read", "document"), true
	case "Memory":
		return typeLevel("read", "memory"), true
	case "CredentialDef":
		return typeLevel("use", "credential"), true
	case "Crypto":
		return typeLevel("use", "crypto"), true
	case "History":
		return typeLevel("read", "history"), true
	case "Evaluation":
		return typeLevel("read", "evaluation"), true
	case "OperatorTokenDef":
		return instance("mint", "principal", "token:*"), true
	case "PauseRuntime", "ResumeRuntime":
		return instance("manage", "runtime", "pause"), true
	case "GetRuntimeState":
		return instance("read", "runtime", "pause"), true
	case "ResolveProbe":
		return instance("manage", "runtime", "config"), true
	case "CreateSnapshot", "RestoreSnapshot", "DeleteSnapshot":
		return instance("manage", "runtime", "snapshot"), true
	case "ListSnapshots", "GetSnapshot", "ExportSnapshot":
		return instance("read", "runtime", "snapshot"), true
	}
	return Surface{}, false
}

// MCPToolSurface is the question the loomcycle-as-MCP-server per-tool gate
// asks for one of its meta-tools.
func MCPToolSurface(tool string) Surface { return instance("invoke", "mcp-tool", tool) }
