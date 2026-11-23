package authz

// RPC is everything the gRPC gates ask of one RPC of loomcycle.v1.Loomcycle
// (RFC EZ P6): the token scope a caller must hold, whether the RPC is on the
// run plane (the whole reach of a credential limited to certain agents), and
// the ACL question. Each mirrors the RPC's HTTP twin route, so a principal
// gets one answer on both transports; a substrate:tenant RPC is also the
// member path, which admits a non-isolated principal without the scope when
// its twin is on the HTTP member path (internal/api/memberpath).
type RPC struct {
	Scope    string
	RunPlane bool
	ask      *Surface
}

// Question is the RPC's ACL question, and false for an RPC no ACL governs.
func (r RPC) Question() (Surface, bool) {
	if r.ask == nil {
		return Surface{}, false
	}
	return *r.ask, true
}

func rpc(scope string, runPlane bool, q Surface) RPC {
	return RPC{Scope: scope, RunPlane: runPlane, ask: &q}
}

// rpcOpen is an RPC no ACL governs: Config's report narrows itself to the
// caller, Authz answers about the caller (or, for an operator, a subject of
// its tenant), the caller's own app sign-ins are whoami's kind of question,
// and Health runs before authentication.
func rpcOpen(scope string, runPlane bool) RPC { return RPC{Scope: scope, RunPlane: runPlane} }

// GRPCRoute returns the row of an RPC (its name, without the service
// prefix). An RPC with no row is held to substrate:admin, off the run plane,
// asking nothing: a new RPC is protected until someone gives it a row.
func GRPCRoute(name string) (RPC, bool) {
	r, ok := grpcRPCs[name]
	if !ok {
		return RPC{Scope: ScopeAdmin}, false
	}
	return r, true
}

// GRPCSurface returns an RPC's ACL question. The second result is false for
// an RPC no ACL governs or with no row.
func GRPCSurface(name string) (Surface, bool) {
	r, _ := GRPCRoute(name)
	return r.Question()
}

// GRPCRoutes returns every row, for the census tests.
func GRPCRoutes() map[string]RPC {
	out := make(map[string]RPC, len(grpcRPCs))
	for k, v := range grpcRPCs {
		out[k] = v
	}
	return out
}

var grpcRPCs = map[string]RPC{
	// Starting and controlling runs: the scope that creates a run, as POST
	// /v1/runs and the per-run write routes ask; steering injects
	// instructions, a turn cancel, a retune and a review verdict change the
	// run, a configured run's create/edit/start/discard is a run write.
	"Run":                 rpc(ScopeRunsCreate, true, typeLevel("run", "agent")),
	"Continue":            rpc(ScopeRunsCreate, true, typeLevel("run", "agent")),
	"SpawnRunBatch":       rpc(ScopeRunsCreate, true, typeLevel("run", "agent")),
	"ReplaySession":       rpc(ScopeRunsCreate, true, typeLevel("run", "agent")),
	"CreateConfiguredRun": rpc(ScopeRunsCreate, true, typeLevel("run", "agent")),
	"UpdateConfiguredRun": rpc(ScopeRunsCreate, true, typeLevel("run", "agent")),
	"StartConfiguredRun":  rpc(ScopeRunsCreate, true, typeLevel("run", "agent")),
	"DeleteConfiguredRun": rpc(ScopeRunsCreate, true, typeLevel("run", "agent")),
	"RunInput":            rpc(ScopeRunsCreate, true, typeLevel("steer", "run")),
	"ResolveInterrupt":    rpc(ScopeRunsCreate, true, typeLevel("steer", "run")),
	"RetuneRun":           rpc(ScopeRunsCreate, true, typeLevel("retune", "run")),
	"CancelTurn":          rpc(ScopeRunsCreate, true, typeLevel("cancel", "run")),
	"CancelAgent":         rpc(ScopeRunsCreate, true, typeLevel("cancel", "run")),
	"ReviewRun":           rpc(ScopeRunsCreate, true, typeLevel("review", "run")),
	"CompactRun":          rpc(ScopeRunsCreate, true, typeLevel("compact", "run")),
	// Deleting a run tree removes stored records: a scope of its own, as on
	// HTTP, and off the run plane; the connector also requires read access
	// to the root.
	"DeleteRunTree": rpc(ScopeRunsDelete, false, typeLevel("delete", "run")),

	// Reading runs.
	"StreamRun":           rpc(ScopeRunsRead, true, typeLevel("read", "run")),
	"GetTranscript":       rpc(ScopeRunsRead, true, typeLevel("read", "run")),
	"GetAgent":            rpc(ScopeRunsRead, true, typeLevel("read", "run")),
	"GetRun":              rpc(ScopeRunsRead, true, typeLevel("read", "run")),
	"ListUserAgents":      rpc(ScopeRunsRead, true, typeLevel("read", "run")),
	"ListWalkRuns":        rpc(ScopeRunsRead, true, typeLevel("read", "run")),
	"ListChildRuns":       rpc(ScopeRunsRead, true, typeLevel("read", "run")),
	"StreamUserRunStates": rpc(ScopeRunsRead, true, typeLevel("read", "run")),
	// Instance configuration: any authenticated principal, as GET /v1/config;
	// the report emits only what the caller's own scopes allow.
	"Config": rpcOpen("", true),

	// A decision asked outside a run, and the models it may name: the scope
	// that creates a run, as POST /v1/_decide. Off the run plane: it reaches
	// a model with no agent, so no allowlist can admit it.
	"Decide":             rpc(ScopeRunsCreate, false, q("use", "gateway", "decide")),
	"ListDecisionModels": rpc(ScopeRunsCreate, false, q("use", "gateway", "decide")),

	// The Calc tool outside a run, as POST /v1/_calc: any principal, off the
	// run plane, no question (a pure function that reaches no stored data).
	"Calc": rpcOpen("", false),

	// The per-user channel surface, as /v1/users/{user_id}/channels/*.
	"PublishChannel":   rpc(ScopeChannelPublish, false, typeLevel("publish", "channel")),
	"AckChannel":       rpc(ScopeChannelPublish, false, typeLevel("ack", "channel")),
	"SubscribeChannel": rpc(ScopeChannelRead, false, typeLevel("subscribe", "channel")),
	"PeekChannel":      rpc(ScopeChannelRead, false, typeLevel("subscribe", "channel")),
	"ListChannels":     rpc(ScopeChannelRead, false, typeLevel("subscribe", "channel")),
	// The fan-in and fan-out twins of POST /v1/_channels/_await and
	// _broadcast. They have no per-user twin, so an isolated user is refused
	// them on HTTP; a channel scope here would let it through on gRPC.
	"AwaitChannels":     rpc(ScopeTenant, false, typeLevel("await", "channel")),
	"BroadcastChannels": rpc(ScopeTenant, false, typeLevel("broadcast", "channel")),
	// A team's own channels, as /v1/_teamdef/{team}/channels.
	"ListTeamChannels":   rpc(ScopeTenant, false, typeLevel("subscribe", "channel")),
	"PeekTeamChannel":    rpc(ScopeTenant, false, typeLevel("subscribe", "channel")),
	"ReleaseTeamChannel": rpc(ScopeTenant, false, typeLevel("manage", "channel")),

	// The tenant operator's reports and controls, each tenant-confined by its
	// connector as its HTTP route is. An RPC that carries list|set|delete
	// (TokenLimit, RetentionOverride, TenantTelemetry) asks the family's
	// weakest verb; the gate asks each op its HTTP twin's question.
	"UsageReport":       rpc(ScopeTenant, false, q("read", "runtime", "usage")),
	"TokenLimit":        rpc(ScopeTenant, false, q("read", "runtime", "limits")),
	"RetentionOverride": rpc(ScopeTenant, false, q("read", "runtime", "retention")),
	"TenantTelemetry":   rpc(ScopeTenant, false, q("read", "runtime", "telemetry")),
	// Erasure: answering and executing a data-subject request are the tenant
	// operator's job; dry_run defaults true and confirm must equal the
	// subject.
	"ErasureReport":  rpc(ScopeTenant, false, q("read", "runtime", "erasure")),
	"ErasureExecute": rpc(ScopeTenant, false, q("manage", "runtime", "erasure")),
	// Directory reads take the tenant from the caller. The tenant list is
	// cross-tenant information: admin, re-checked in the handler.
	"DirectoryUsers":   rpc(ScopeTenant, false, q("read", "principal", "user:*")),
	"DirectoryInspect": rpc(ScopeTenant, false, q("read", "principal", "user:*")),
	"DirectoryTenants": rpc(ScopeAdmin, false, q("read", "runtime", "tenants")),
	// A user's app sign-ins, as GET/DELETE /v1/_users/{subject}/app-sign-ins;
	// the shared operation asks the named user's own instance.
	"ListUserAppSignIns": rpc(ScopeTenant, false, q("read", "principal", "user:*")),
	"EndUserAppSignIn":   rpc(ScopeTenant, false, q("read", "principal", "user:*")),
	// The caller's own, as /v1/_me/app-sign-ins.
	"ListMyAppSignIns": rpcOpen("", false),
	"EndMyAppSignIn":   rpcOpen("", false),

	// The definition families, as their /v1/_<family>def routes: the op is in
	// the body, so the RPC asks the family's weakest verb and the tool asks
	// each op's own question of the definition it names.
	"AgentDef":          rpc(ScopeTenant, false, q("read", "def", "agent/*")),
	"SkillDef":          rpc(ScopeTenant, false, q("read", "def", "skill/*")),
	"MCPServerDef":      rpc(ScopeTenant, false, q("read", "def", "mcpserver/*")),
	"ScheduleDef":       rpc(ScopeTenant, false, q("read", "def", "schedule/*")),
	"A2AServerCardDef":  rpc(ScopeTenant, false, q("read", "def", "a2aserver/*")),
	"A2AAgentDef":       rpc(ScopeTenant, false, q("read", "def", "a2a/*")),
	"WebhookDef":        rpc(ScopeTenant, false, q("read", "def", "webhook/*")),
	"MemoryBackendDef":  rpc(ScopeTenant, false, q("read", "def", "memorybackend/*")),
	"DocumentSourceDef": rpc(ScopeTenant, false, q("read", "def", "documentsource/*")),
	"ClientDef":         rpc(ScopeTenant, false, q("read", "def", "client/*")),
	"VolumeDef":         rpc(ScopeTenant, false, q("read", "def", "volume/*")),
	"EgressRouteDef":    rpc(ScopeTenant, false, q("read", "def", "egressroute/*")),
	"TeamDef":           rpc(ScopeTenant, false, q("read", "def", "team/*")),
	"HookDef":           rpc(ScopeTenant, false, q("read", "def", "hook/*")),
	// Token minting, as POST /v1/_operatortokendef: a tenant operator only
	// for its own tenant's metrics scrape tokens (the tool's grant).
	"OperatorTokenDef": rpc(ScopeTenant, false, q("read", "principal", "token:*")),

	// The data tools outside a run, tenant-confined as their routes are.
	"Path":     rpc(ScopeTenant, false, typeLevel("read", "document")),
	"Document": rpc(ScopeTenant, false, typeLevel("read", "document")),
	"Memory":   rpc(ScopeTenant, false, typeLevel("read", "memory")),
	// The credential store; an isolated user is admitted for scope=user by
	// the gate's self-service carve-out.
	"CredentialDef": rpc(ScopeTenant, false, typeLevel("use", "credential")),
	// The tenant's keys, as POST /v1/_crypto: not in the self-service
	// carve-out, nor on the run plane.
	"Crypto": rpc(ScopeTenant, false, typeLevel("use", "crypto")),
	// Chat browse; a member is capped to its own chats. Unlike /v1/_history
	// it does not admit an isolated user.
	"History": rpc(ScopeTenant, false, typeLevel("read", "history")),
	// Evaluation outside a run: runs:read, as POST /v1/_evaluation; which ops
	// a caller may use is tools.OffRunEvaluationPolicy.
	"Evaluation": rpc(ScopeRunsRead, false, typeLevel("read", "evaluation")),

	// The ontology, as /v1/_ontology/*: a merge and a principal binding write
	// the subjects' documents; a plan writes nothing. The review queue, a bulk
	// ruling, its undo and the review policy ask read, as /v1/_statements/*;
	// the connector asks review or write of each statement.
	"MergeSubjects":        rpc(ScopeTenant, false, typeLevel("write", "document")),
	"BindSubjectPrincipal": rpc(ScopeTenant, false, typeLevel("write", "document")),
	"PlanOntology":         rpc(ScopeTenant, false, typeLevel("read", "document")),
	"ListReviewQueue":      rpc(ScopeTenant, false, typeLevel("read", "document")),
	"RuleStatements":       rpc(ScopeTenant, false, typeLevel("read", "document")),
	"RevertRuling":         rpc(ScopeTenant, false, typeLevel("read", "document")),
	"ListRulings":          rpc(ScopeTenant, false, typeLevel("read", "document")),
	"ApplyReviewPolicy":    rpc(ScopeTenant, false, typeLevel("read", "document")),

	// Access-control management (RFC EZ §3), as its routes: the family's
	// weakest verb, each op asking its own. Authz is ungoverned, as POST
	// /v1/_authz/* is.
	"GroupDef":   rpc(ScopeTenant, false, q("read", "principal", "group:*")),
	"AclDef":     rpc(ScopeTenant, false, q("read", "def", "acl/*")),
	"AclBinding": rpc(ScopeTenant, false, q("read", "def", "acl/*")),
	"Authz":      rpcOpen("", false),

	// Runtime administration: the operator's.
	"PauseRuntime":    rpc(ScopeAdmin, false, q("manage", "runtime", "pause")),
	"ResumeRuntime":   rpc(ScopeAdmin, false, q("manage", "runtime", "pause")),
	"GetRuntimeState": rpc(ScopeAdmin, false, q("read", "runtime", "pause")),
	"ResolveProbe":    rpc(ScopeAdmin, false, q("manage", "runtime", "config")),
	"CreateSnapshot":  rpc(ScopeAdmin, false, q("manage", "runtime", "snapshot")),
	"RestoreSnapshot": rpc(ScopeAdmin, false, q("manage", "runtime", "snapshot")),
	"DeleteSnapshot":  rpc(ScopeAdmin, false, q("manage", "runtime", "snapshot")),
	"ListSnapshots":   rpc(ScopeAdmin, false, q("read", "runtime", "snapshot")),
	"GetSnapshot":     rpc(ScopeAdmin, false, q("read", "runtime", "snapshot")),
	"ExportSnapshot":  rpc(ScopeAdmin, false, q("read", "runtime", "snapshot")),
	// Health is answered before authentication.
	"Health": rpcOpen(ScopeAdmin, false),
}

// MCPTool is what the /v1/_mcp per-tool gate asks of one meta-tool beyond its
// class allowlist (RFC EZ P6): the scope a non-admin principal must also hold
// ("" = the allowlist alone governs it, as for the definition and data tools,
// whose HTTP routes a member reaches on the member path), and whether the
// tool is on the run plane. Each scope is the one the tool's gRPC twin needs.
type MCPTool struct {
	Scope    string
	RunPlane bool
}

// MCPToolRoute returns a meta-tool's row. A tool with no row needs no extra
// scope and is off the run plane.
func MCPToolRoute(name string) (MCPTool, bool) {
	t, ok := mcpToolRows[name]
	return t, ok
}

// MCPToolRoutes returns every row, for the census tests.
func MCPToolRoutes() map[string]MCPTool {
	out := make(map[string]MCPTool, len(mcpToolRows))
	for k, v := range mcpToolRows {
		out[k] = v
	}
	return out
}

var mcpToolRows = map[string]MCPTool{
	// Starting a run, and every write on a run's state.
	"spawn_run":            {ScopeRunsCreate, true},
	"spawn_runs":           {ScopeRunsCreate, true},
	"configured_run":       {ScopeRunsCreate, true},
	"cancel_run":           {ScopeRunsCreate, true},
	"compact_run":          {ScopeRunsCreate, true},
	"retune_run":           {ScopeRunsCreate, true},
	"review_run":           {ScopeRunsCreate, true},
	"interruption_resolve": {ScopeRunsCreate, true},
	// Deleting a run tree removes stored records: a scope of its own.
	"delete_run_tree": {ScopeRunsDelete, false},
	// Reading runs.
	"get_run":                {ScopeRunsRead, true},
	"list_runs":              {ScopeRunsRead, true},
	"stream_user_run_states": {ScopeRunsRead, true},
	"file":                   {ScopeRunsRead, false},
	// The per-user channel surface, and a team's own channels (a channel read,
	// or a channel write for a release).
	"publish_channel":      {ScopeChannelPublish, false},
	"ack_channel":          {ScopeChannelPublish, false},
	"subscribe_channel":    {ScopeChannelRead, false},
	"peek_channel":         {ScopeChannelRead, false},
	"list_team_channels":   {ScopeChannelRead, false},
	"peek_team_channel":    {ScopeChannelRead, false},
	"release_team_channel": {ScopeChannelPublish, false},
	// A decision outside a run spends tokens: the scope POST /v1/_decide
	// requires. Off the run plane: a model call with no agent.
	"decision": {ScopeRunsCreate, false},
	// A workspace outside a run runs commands on a worker (RFC GW D35): the
	// scope that creates a run. Off the run plane: no agent.
	"workspace": {ScopeRunsCreate, false},
	// Evaluation: the floor POST /v1/_evaluation gates on.
	"evaluation": {ScopeRunsRead, false},
	// The operator's controls, kept from a tenant's members as their routes
	// keep them: a no-trace override, the tenant's trace export, its keys, a
	// subject merge, erasure, groups, lists and bindings, app clients, and
	// handing out credentials.
	"retention_override": {ScopeTenant, false},
	"telemetry":          {ScopeTenant, false},
	"crypto":             {ScopeTenant, false},
	"ontology":           {ScopeTenant, false},
	"erasure":            {ScopeTenant, false},
	"groupdef":           {ScopeTenant, false},
	"acldef":             {ScopeTenant, false},
	"aclbinding":         {ScopeTenant, false},
	"clientdef":          {ScopeTenant, false},
	"operatortokendef":   {ScopeTenant, false},
	// Where a tenant's workspaces may send data (RFC GW §6.3): its
	// operators', never a member's.
	"egressroutedef": {ScopeTenant, false},
}
