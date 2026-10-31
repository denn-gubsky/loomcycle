package memberpath

import "net/http"

// Twin is the HTTP route a gRPC RPC mirrors. Path is a concrete request path
// the mux would route (ids filled in), so the HTTP gate functions can be asked
// about it exactly as they are asked about a live request.
type Twin struct {
	// Op is the RPC's own `op` request field this route answers ("" when the
	// route answers every call of the RPC). An RPC whose twins disagree on
	// the member path must name them by op, so the gate can tell the calls
	// apart (OpTwin).
	Op     string
	Method string
	Path   string
}

// The RPCs of loomcycle.v1.Loomcycle and the HTTP route(s) each mirrors. It is
// read at runtime by the gRPC member path (RPC, OpTwin) and by the transport
// parity test, which asks the HTTP gate of every twin listed here the same
// question it asks the gRPC gate of the RPC. A new RPC has to be added here
// (or to NoTwin) or the parity test fails.
var twins = map[string][]Twin{
	// Runs: starting, continuing, controlling.
	"Run":                 {{Method: http.MethodPost, Path: "/v1/runs"}},
	"Continue":            {{Method: http.MethodPost, Path: "/v1/sessions/s/messages"}},
	"SpawnRunBatch":       {{Method: http.MethodPost, Path: "/v1/runs:batch"}},
	"CompactRun":          {{Method: http.MethodPost, Path: "/v1/runs/r/compact"}},
	"DeleteRunTree":       {{Method: http.MethodDelete, Path: "/v1/runs/r/tree"}},
	"CreateConfiguredRun": {{Method: http.MethodPost, Path: "/v1/runs"}},
	"UpdateConfiguredRun": {{Method: http.MethodPatch, Path: "/v1/runs/r"}},
	"StartConfiguredRun":  {{Method: http.MethodPost, Path: "/v1/runs/r/start"}},
	"DeleteConfiguredRun": {{Method: http.MethodDelete, Path: "/v1/runs/r"}},
	"ReplaySession":       {{Method: http.MethodPost, Path: "/v1/sessions/s/replay"}},
	"RunInput":            {{Method: http.MethodPost, Path: "/v1/runs/r/input"}},
	"RetuneRun":           {{Method: http.MethodPost, Path: "/v1/runs/r/retune"}},
	"CancelTurn":          {{Method: http.MethodPost, Path: "/v1/runs/r/cancel"}},
	"ReviewRun":           {{Method: http.MethodPost, Path: "/v1/runs/r/review"}},
	"ResolveInterrupt":    {{Method: http.MethodPost, Path: "/v1/runs/r/interrupts/i/resolve"}},
	"CancelAgent":         {{Method: http.MethodPost, Path: "/v1/agents/a/cancel"}},
	// Runs: reading.
	"StreamRun":           {{Method: http.MethodGet, Path: "/v1/runs/r/stream"}},
	"GetTranscript":       {{Method: http.MethodGet, Path: "/v1/sessions/s/transcript"}},
	"GetAgent":            {{Method: http.MethodGet, Path: "/v1/agents/a"}},
	"GetRun":              {{Method: http.MethodGet, Path: "/v1/runs/r"}},
	"ListUserAgents":      {{Method: http.MethodGet, Path: "/v1/users/u/agents"}},
	"ListWalkRuns":        {{Method: http.MethodGet, Path: "/v1/runs"}},
	"ListChildRuns":       {{Method: http.MethodGet, Path: "/v1/runs"}},
	"StreamUserRunStates": {{Method: http.MethodGet, Path: "/v1/users/u/agents/stream"}},
	// Decisions and the instance's configuration.
	"Decide":             {{Method: http.MethodPost, Path: "/v1/_decide"}},
	"ListDecisionModels": {{Method: http.MethodGet, Path: "/v1/_decide/models"}},
	"Config":             {{Method: http.MethodGet, Path: "/v1/config"}},
	// Usage, budgets, retention, the directory, erasure.
	"UsageReport": {{Method: http.MethodGet, Path: "/v1/_usage"}},
	"TokenLimit": {
		{Op: "list", Method: http.MethodGet, Path: "/v1/_limits"},
		{Op: "set", Method: http.MethodPut, Path: "/v1/_limits"},
		{Op: "delete", Method: http.MethodDelete, Path: "/v1/_limits"},
	},
	"RetentionOverride": {
		{Op: "list", Method: http.MethodGet, Path: "/v1/_retention/overrides"},
		{Op: "set", Method: http.MethodPut, Path: "/v1/_retention/overrides"},
		{Op: "clear", Method: http.MethodDelete, Path: "/v1/_retention/overrides"},
	},
	"DirectoryUsers":   {{Method: http.MethodGet, Path: "/v1/_users"}},
	"DirectoryInspect": {{Method: http.MethodGet, Path: "/v1/_users/u"}},
	"DirectoryTenants": {{Method: http.MethodGet, Path: "/v1/_tenants"}},
	"ErasureReport":    {{Method: http.MethodGet, Path: "/v1/_erasure"}},
	"ErasureExecute":   {{Method: http.MethodPost, Path: "/v1/_erasure"}},
	// The tenant ontology.
	"MergeSubjects":        {{Method: http.MethodPost, Path: "/v1/_ontology/subjects/merge"}},
	"BindSubjectPrincipal": {{Method: http.MethodPost, Path: "/v1/_ontology/subjects/principal"}},
	// Statement review: the queue, bulk rulings and their undo.
	"ListReviewQueue": {{Method: http.MethodGet, Path: "/v1/_statements/review"}},
	"RuleStatements":  {{Method: http.MethodPost, Path: "/v1/_statements/rule"}},
	"RevertRuling":    {{Method: http.MethodPost, Path: "/v1/_statements/revert"}},
	// Runtime admin.
	"PauseRuntime":    {{Method: http.MethodPost, Path: "/v1/_pause"}},
	"ResumeRuntime":   {{Method: http.MethodPost, Path: "/v1/_resume"}},
	"GetRuntimeState": {{Method: http.MethodGet, Path: "/v1/_state"}},
	"ResolveProbe":    {{Method: http.MethodPost, Path: "/v1/_resolve/probe"}},
	"CreateSnapshot":  {{Method: http.MethodPost, Path: "/v1/_snapshots"}},
	"ListSnapshots":   {{Method: http.MethodGet, Path: "/v1/_snapshots"}},
	"GetSnapshot":     {{Method: http.MethodGet, Path: "/v1/_snapshots/x"}},
	"ExportSnapshot":  {{Method: http.MethodGet, Path: "/v1/_snapshots/x/export"}},
	"RestoreSnapshot": {{Method: http.MethodPost, Path: "/v1/_snapshots/x/restore"}},
	"DeleteSnapshot":  {{Method: http.MethodDelete, Path: "/v1/_snapshots/x"}},
	// The definition families: one POST route each, every op in the body.
	"AgentDef":          {{Method: http.MethodPost, Path: "/v1/_agentdef"}},
	"SkillDef":          {{Method: http.MethodPost, Path: "/v1/_skilldef"}},
	"MCPServerDef":      {{Method: http.MethodPost, Path: "/v1/_mcpserverdef"}},
	"ScheduleDef":       {{Method: http.MethodPost, Path: "/v1/_scheduledef"}},
	"A2AServerCardDef":  {{Method: http.MethodPost, Path: "/v1/_a2aservercarddef"}},
	"A2AAgentDef":       {{Method: http.MethodPost, Path: "/v1/_a2aagentdef"}},
	"WebhookDef":        {{Method: http.MethodPost, Path: "/v1/_webhookdef"}},
	"MemoryBackendDef":  {{Method: http.MethodPost, Path: "/v1/_memorybackenddef"}},
	"DocumentSourceDef": {{Method: http.MethodPost, Path: "/v1/_documentsourcedef"}},
	"ClientDef":         {{Method: http.MethodPost, Path: "/v1/_clientdef"}},
	"OperatorTokenDef":  {{Method: http.MethodPost, Path: "/v1/_operatortokendef"}},
	"VolumeDef":         {{Method: http.MethodPost, Path: "/v1/_volumedef"}},
	"TeamDef":           {{Method: http.MethodPost, Path: "/v1/_teamdef"}},
	"HookDef":           {{Method: http.MethodPost, Path: "/v1/_hookdef"}},
	// The scope-aware tools outside a run.
	"Path":          {{Method: http.MethodPost, Path: "/v1/_path"}},
	"Document":      {{Method: http.MethodPost, Path: "/v1/_document"}},
	"CredentialDef": {{Method: http.MethodPost, Path: "/v1/_credentialdef"}},
	"Crypto":        {{Method: http.MethodPost, Path: "/v1/_crypto"}},
	"History":       {{Method: http.MethodPost, Path: "/v1/_history"}},
	"Evaluation":    {{Method: http.MethodPost, Path: "/v1/_evaluation"}},
	// Memory has no single tool route on HTTP: it is the key/value routes and
	// search under /v1/_memory/, which a member reaches as one family.
	"Memory": {
		{Method: http.MethodGet, Path: "/v1/_memory/scopes/user/u/keys/k"},
		{Method: http.MethodPut, Path: "/v1/_memory/scopes/user/u/keys/k"},
		{Method: http.MethodDelete, Path: "/v1/_memory/scopes/user/u/keys/k"},
		{Method: http.MethodPost, Path: "/v1/_memory/search"},
	},
	// Access-control management: the op travels inside input_json, so these
	// name every route of the family without an op (they agree: all
	// operator-only for a member).
	"GroupDef": {
		{Method: http.MethodGet, Path: "/v1/_groups"},
		{Method: http.MethodPost, Path: "/v1/_groups"},
		{Method: http.MethodGet, Path: "/v1/_groups/g"},
		{Method: http.MethodDelete, Path: "/v1/_groups/g"},
		{Method: http.MethodGet, Path: "/v1/_groups/g/effective"},
		{Method: http.MethodPost, Path: "/v1/_groups/g/members"},
		{Method: http.MethodDelete, Path: "/v1/_groups/g/members/user/u"},
	},
	"AclDef": {{Method: http.MethodPost, Path: "/v1/_acldef"}},
	"AclBinding": {
		{Method: http.MethodGet, Path: "/v1/_aclbinding"},
		{Method: http.MethodPost, Path: "/v1/_aclbinding"},
		{Method: http.MethodDelete, Path: "/v1/_aclbinding/b"},
	},
	"Authz": {
		{Method: http.MethodPost, Path: "/v1/_authz/evaluation"},
		{Method: http.MethodPost, Path: "/v1/_authz/evaluations"},
		{Method: http.MethodPost, Path: "/v1/_authz/explain"},
		{Method: http.MethodPost, Path: "/v1/_authz/simulate"},
	},
	// Channels.
	"ListChannels":       {{Method: http.MethodGet, Path: "/v1/_channels"}},
	"PublishChannel":     {{Method: http.MethodPost, Path: "/v1/_channels/c/publish"}},
	"SubscribeChannel":   {{Method: http.MethodPost, Path: "/v1/_channels/c/subscribe"}},
	"PeekChannel":        {{Method: http.MethodGet, Path: "/v1/_channels/c/peek"}},
	"AckChannel":         {{Method: http.MethodPost, Path: "/v1/_channels/c/ack"}},
	"AwaitChannels":      {{Method: http.MethodPost, Path: "/v1/_channels/_await"}},
	"BroadcastChannels":  {{Method: http.MethodPost, Path: "/v1/_channels/_broadcast"}},
	"ListTeamChannels":   {{Method: http.MethodGet, Path: "/v1/_teamdef/t/channels"}},
	"PeekTeamChannel":    {{Method: http.MethodGet, Path: "/v1/_teamdef/t/channels/c/peek"}},
	"ReleaseTeamChannel": {{Method: http.MethodPost, Path: "/v1/_teamdef/t/channels/c/release"}},
}

// noTwin is every RPC with no HTTP twin to compare against, and why.
var noTwin = map[string]string{
	"Health": "unauthenticated on both transports: the gRPC interceptors skip it, and /healthz is not behind the HTTP auth middleware",
}

// Twins returns the HTTP route(s) rpc mirrors (nil for an RPC not listed).
func Twins(rpc string) []Twin { return append([]Twin(nil), twins[rpc]...) }

// TwinnedRPCs returns every RPC that has twins.
func TwinnedRPCs() []string {
	out := make([]string, 0, len(twins))
	for rpc := range twins {
		out = append(out, rpc)
	}
	return out
}

// NoTwin returns the RPCs without an HTTP twin, each with its reason.
func NoTwin() map[string]string {
	out := make(map[string]string, len(noTwin))
	for k, v := range noTwin {
		out[k] = v
	}
	return out
}

// RPC reports whether the member path admits a call of rpc, whose own required
// scope is required: when it opens any of the RPC's twin routes. For an RPC
// whose twins disagree that is the widest answer, and the gate then holds each
// call to its own operation's twin (OpTwin).
func RPC(rpc, required string) bool {
	for _, t := range twins[rpc] {
		if Route(required, t.Method, t.Path) {
			return true
		}
	}
	return false
}

// ByOp reports whether rpc's twins are keyed by its `op` field — the RPCs whose
// calls the gate must tell apart.
func ByOp(rpc string) bool {
	ts := twins[rpc]
	return len(ts) > 0 && ts[0].Op != ""
}

// OpTwin returns the twin route of one operation of an op-keyed RPC; false
// when the RPC is not op-keyed or op is not one of its operations.
func OpTwin(rpc, op string) (Twin, bool) {
	if !ByOp(rpc) {
		return Twin{}, false
	}
	for _, t := range twins[rpc] {
		if t.Op == op {
			return t, true
		}
	}
	return Twin{}, false
}
