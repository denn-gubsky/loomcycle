// Package http serves the HTTP+SSE API.
//
// One endpoint matters at v0.1: POST /v1/runs streams agent events as SSE.
// /healthz is the unauthenticated liveness probe.
package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/contextplugin"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/metrics"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/policy"
	"github.com/denn-gubsky/loomcycle/internal/webui"

	"go.opentelemetry.io/otel/trace"
)

// ProviderResolver returns a Provider by ID. The cmd/loomcycle main constructs one
// per provider on startup and passes the lookup in. Keeping this an interface
// keeps the api package free of concrete Anthropic/OpenAI/Ollama wiring.
type ProviderResolver interface {
	Get(id string) (providers.Provider, error)
}

// Server holds dependencies and serves HTTP requests.
type Server struct {
	cfg       *config.Config
	providers ProviderResolver
	tools     []tools.Tool
	sem       *concurrency.Semaphore
	store     store.Store // optional; nil means "don't persist"

	// sqlMem is the RFC AA SQL Memory manager (per-scope sqlite databases
	// backing the Memory tool's sql_query/sql_exec). Nil when the subsystem
	// is disabled. Wired from main.go via SetSqlMem; the server uses it ONLY
	// to drop a run-scope SQL database when the top-level run completes
	// (mirroring the ephemeral-volume purge).
	sqlMem *sqlmem.Manager

	// redactor masks secret-shaped substrings in tool I/O before it is
	// persisted to events.payload (F32). Built in New() from the secret-
	// classified env when cfg.Env.RedactSecrets; nil (a no-op) when disabled
	// via LOOMCYCLE_REDACT_SECRETS=0. Read-only after construction; the nil /
	// no-op cases are handled by *redact.Redactor's nil-safe methods.
	redactor *redact.Redactor

	// contextPlugins is the runtime-wide context-transform chain (RFC Z / F43),
	// built once from cfg.ContextPlugins at construction and shared read-only
	// across every run (the plugins are stateless/concurrent-safe). Passed into
	// each loop.RunOptions; the loop applies it on a COPY of the outbound
	// request and skips it for the code-js provider. nil = no chain.
	contextPlugins []contextplugin.Plugin

	// cancelReg holds the in-memory map of agent_id → cancelFn so the
	// cancel API can tear down a still-running loop from a different
	// HTTP request. Always non-nil after New(); empty on startup. See
	// internal/cancel/registry.go for the trust model.
	cancelReg *cancel.Registry
	// steerReg maps a live run_id → its operator steering queue (PR 2 /
	// interactive terminal). nil disables steering (POST /v1/runs/{id}/input
	// → 404). Set at boot via SetSteerRegistry.
	steerReg *steer.Registry

	// sessionLocks tracks per-session mutexes used by continuation
	// requests (handleMessages, or handleRuns with a non-empty
	// SessionID). A concurrent request to the same session fast-fails
	// with 409 (HTTP) / FailedPrecondition (gRPC).
	//
	// Lives in internal/runner so the gRPC wire surface — which
	// targets the same session_id space — coordinates against the
	// same lock map. main.go shares one instance between both
	// surfaces. See runner.SessionLockMap for the GC lifecycle.
	sessionLocks *runner.SessionLockMap

	// resolver picks (provider, model, effort) for tier-using agents
	// against an availability matrix. Optional — when nil, the
	// Server falls back to cfg.ResolveAgentModel (the explicit-pin
	// path) for every agent, preserving v0.6.x behaviour. cmd/
	// loomcycle/main.go calls SetResolver after construction so
	// tests that don't exercise tier resolution can omit the
	// dependency. See internal/resolve and the matrix RFC at
	// doc-internal/rfcs/model-resolution-matrix.md.
	resolver *resolve.Resolver

	// hookRegistry holds the runtime-registered tool-use hooks (the
	// /v1/hooks endpoints write into this), and hookDispatcher is the
	// loop-side adapter the agent loop calls into when dispatching
	// tools. Both are non-nil after New() — no consumer needs to nil-
	// check. An empty registry produces zero hook invocations on the
	// hot path (Match returns nil, dispatchOneTool fast-paths).
	//
	// Type is now hooks.RegistryInterface (v0.12.5 Phase 6) so cluster
	// mode can swap in *hooks.DBBackedRegistry. *hooks.Registry
	// implicitly satisfies the interface; existing tests need no
	// changes.
	hookRegistry   hooks.RegistryInterface
	hookDispatcher *hooks.Dispatcher

	// sessionLockPG is the v0.12.5 Phase 6 cluster-mode session lock.
	// When set, trySessionLock dispatches to it instead of sessionLocks
	// — providing cluster-wide per-session_id 409 ErrSessionBusy.
	// Single-replica deployments leave this nil and the in-process
	// SessionLockMap above stays the source of truth.
	sessionLockPG *runner.PgSessionLocker

	// mcpFallback is the optional Dispatcher fallback for lazy MCP
	// server registration. When set, an agent's call to a tool name
	// that's not in the per-run dispatcher's static map (typically
	// because the MCP server failed handshake at boot and was
	// "skipped" by main.go) gets a chance to recover via this fallback
	// before the dispatcher returns the standard "tool not found"
	// error. Wired in cmd/loomcycle/main.go via SetMCPFallback after
	// the MCP pool is built; nil-safe for tests + unit harnesses
	// that don't exercise MCP at all. See internal/tools/mcp/lazy.go.
	mcpFallback tools.FallbackFunc

	// dynamicTools, when set, returns the substrate-registered tools
	// (dynamic MCP servers' discovered tools + A2A peer skills) that were
	// NOT in the boot-time s.tools set. Folded into the per-run candidate
	// set BEFORE the allowed_tools filter, so a tool registered post-boot
	// is both ADVERTISED in the run's catalog and dispatchable — symmetric
	// with boot-registered tools, no restart needed. nil-safe (tests +
	// deployments without the substrate); wired in main.go.
	//
	// RFC N FIX 2-mcp: the tenant is an EXPLICIT argument, not ctx-derived.
	// candidateTools runs at the run-creation ENTRY sites BEFORE
	// WithRunIdentity is stamped on ctx, so for non-HTTP-principal spawn
	// surfaces (A2A/scheduler/webhook/MCP spawn_run/gRPC-legacy) the ctx
	// tenant is "" and the enumerator would advertise the wrong tenant's
	// MCP tool set. The caller passes the run's authoritative tenant.
	//
	// F33: wantServers is the set of dynamic MCP servers the run references
	// (from its allowed_tools). The enumerator advertises cached tools for all
	// servers, but only handshakes a referenced server whose cache is empty.
	dynamicTools func(ctx context.Context, tenantID string, wantServers map[string]bool) []tools.Tool

	// systemPublisher backs the v0.8.6 POST /v1/_channels/_system/...
	// admin endpoint. Nil = endpoint refuses every request with a
	// "system publisher not wired" 503. Set via SetSystemPublisher.
	systemPublisher channels.SystemPublisher

	// metricsSampler is the v0.8.x process-resource sampler.
	// Nil = the /v1/_metrics/* endpoints return 503. Set via
	// SetMetricsSampler from main.go after the sampler is
	// constructed.
	metricsSampler *metrics.Sampler

	// extraMux is the optional v1.x RFC G hook for registering
	// additional routes (the A2A binding mounts) on the shared mux.
	// Set via SetExtraMux; nil when the A2A surface is disabled.
	extraMux func(mux *http.ServeMux, adminAuth func(http.Handler) http.Handler)

	// webhookMux is the optional v1.x RFC H hook for registering the
	// inbound-webhook receiver route. Set via SetWebhookMux; nil when
	// LOOMCYCLE_WEBHOOKS_ENABLED is unset. Unlike extraMux it is NOT
	// handed the admin-auth wrapper: the receiver authenticates each
	// request against the resolved WebhookDef's own secret (HMAC /
	// bearer), so wrapping it in the global LOOMCYCLE_AUTH_TOKEN bearer
	// would defeat the per-webhook secret model. The hook receives a
	// MuxRegistrar (an adapter that applies recovery middleware) rather
	// than the bare mux, so a panicking webhook handler still becomes a
	// 500 instead of crashing the process. It ALSO receives the admin-auth
	// wrapper (recovery + bearer) so the WH-5b triage endpoints
	// (recent-deliveries / test) can sit behind LOOMCYCLE_AUTH_TOKEN while
	// the receiver POST stays unauthed.
	webhookMux func(reg MuxRegistrar, adminAuth func(http.Handler) http.Handler)

	// mcpPoolInspector returns the cached tools/list result for a
	// named MCP server as already-marshaled JSON. The "already JSON"
	// wire keeps this package free of internal/tools/mcp imports.
	// Returns nil when the Pool has no cached entry (init pending or
	// failed; server unknown). Used by the v0.9.x unified library
	// endpoints to surface static MCP servers' discovered_tools
	// alongside the substrate-side discovered_tools field. Wired in
	// cmd/loomcycle/main.go via SetMCPPoolInspector.
	mcpPoolInspector MCPPoolInspector

	// skillSet is the static skills.Set loaded at boot from
	// LOOMCYCLE_SKILLS_ROOT. Used by resolveSkillBodiesForRun (v0.8.22)
	// to fall back to the static body when a skill name has no
	// DB-active SkillDef row. Nil when LOOMCYCLE_SKILLS_ROOT is unset
	// or when the server is constructed without a SetSkillSet call —
	// the per-run resolver tolerates nil and emits no override in
	// that case.
	skillSet *skills.Set

	// pauseMgr is the v0.8.17 runtime pause/resume coordinator.
	// Nil = the /v1/_pause, /v1/_resume, /v1/_state endpoints return
	// 503. Set via SetPauseManager from main.go after the manager is
	// constructed (same wiring shape as metricsSampler).
	pauseMgr *pause.Manager

	// interruptionBus is the v0.8.16 in-process notification bus used
	// by the resolve handler to wake the blocked Interruption tool.
	// Same instance as the Channel tool's Bus — re-using one Bus per
	// process keeps wake-up paths uniform. Nil = resolve handler
	// still writes the row (the bus.Wait timer will fire once the
	// row's expires_at passes) but in-process wakeup is skipped. Set
	// via SetInterruptionBus from main.go.
	interruptionBus *channels.Bus

	// channelBus is the in-process notification bus the Channel tool
	// uses for long-poll wake-up. v0.9.x SubscribeChannel
	// (Connector + HTTP) consults the SAME instance so wire callers
	// wake on the same Notify() the in-band tool would have woken on.
	// Nil = subscribe falls back to polling (poll-read once + return,
	// no wait). Set via SetChannelBus from main.go.
	channelBus *channels.Bus

	// runStateBus is the v0.9.x n8n RFC Phase 0 in-process pub/sub
	// for run state transitions. Powers GET /v1/users/{user_id}/
	// agents/stream (SSE). Every finishRun* call site + the run-
	// creation moment publish here; the SSE handler subscribes.
	// Nil = the /agents/stream endpoint returns 503. Set via
	// SetRunStateBus from main.go.
	runStateBus *runstate.Bus

	// mcpServerDefTool is the v0.9.x MCPServerDef substrate tool.
	// NOT in s.tools (no per-agent dispatcher attachment — operator-
	// admin-only). Reached via Connector.MCPServerDef + the admin
	// endpoint + LoomCycle MCP meta-tool. Nil = the surface returns
	// "not configured" errors. Set via SetMCPServerDefTool.
	mcpServerDefTool tools.Tool

	// scheduleDefTool is the v1.x ScheduleDef substrate tool. Same
	// operator-admin-only posture as mcpServerDefTool — NOT in
	// s.tools, reached via Connector.ScheduleDef + the admin endpoint
	// + future LoomCycle MCP meta-tool. Nil = the surface returns
	// "not configured" errors. Set via SetScheduleDefTool.
	scheduleDefTool tools.Tool

	// a2aServerCardDefTool + a2aAgentDefTool are the v1.x RFC G A2A
	// substrate tools. Same operator-admin-only posture as
	// scheduleDefTool — NOT in s.tools, reached via Connector +
	// the admin endpoints + LoomCycle MCP meta-tools. Nil = the
	// surface returns "not configured" errors. Set via
	// SetA2AServerCardDefTool / SetA2AAgentDefTool.
	a2aServerCardDefTool tools.Tool
	a2aAgentDefTool      tools.Tool

	// webhookDefTool is the v1.x RFC H WebhookDef substrate tool. Same
	// operator-admin-only posture as a2aAgentDefTool — NOT in s.tools,
	// reached via Connector.WebhookDef + the admin endpoint + the
	// LoomCycle MCP meta-tool. Nil = the surface returns "not
	// configured" errors. Set via SetWebhookDefTool.
	webhookDefTool tools.Tool

	// memoryBackendDefTool is the RFC I MR-3a MemoryBackendDef substrate
	// tool. Same operator-admin-only posture as webhookDefTool — NOT in
	// s.tools, reached via Connector.MemoryBackendDef + the admin
	// endpoint + the LoomCycle MCP meta-tool. Nil = the surface returns
	// "not configured" errors. Set via SetMemoryBackendDefTool.
	memoryBackendDefTool tools.Tool

	// operatorTokenDefTool is the RFC L OperatorTokenDef substrate tool
	// (auth-token minting/rotation/retirement). Operator-admin-only — NOT
	// in s.tools; reached via Connector.OperatorTokenDef + the admin
	// endpoint + the gRPC RPC + the MCP meta-tool. Nil = the surface
	// returns "not configured". Set via SetOperatorTokenDefTool.
	operatorTokenDefTool tools.Tool

	// v0.12.0 multi-replica HA. backplane + replicaStore are nil in
	// single-replica deployments (LOOMCYCLE_REPLICA_ID unset); /healthz
	// then returns the same response shape as v0.11.x. When set,
	// /healthz includes a cluster view (replica_id + replicas[]).
	// Phase 1 wires these but the backplane has no live subscribers
	// outside the package's own tests — Phase 2+ build the consumers.
	//
	// replicaStore is typed as the local replicaLister interface (not
	// *coord.ReplicaStore) so the healthz test can stub the listing
	// without standing up a Postgres fixture. *coord.ReplicaStore
	// satisfies it structurally.
	backplane    coord.Backplane
	replicaStore replicaLister
	replicaID    string

	// tokenCache is the RFC L per-replica auth-token resolution cache.
	// Nil = disabled (direct lookup per request). Wired via
	// EnableTokenCache from main.go with LOOMCYCLE_AUTH_CACHE_TTL_SECONDS.
	tokenCache *tokenCache

	// mcpHTTPHandler is the v0.8.15.3 HTTP MCP transport (alternate
	// front-end to the stdio MCP server). Typed as http.Handler — NOT
	// *lcmcp.HTTPHandler — so this package does NOT import
	// internal/api/mcp. The coupling direction is one-way:
	// internal/api/mcp CONSUMES connector.Connector (implemented by
	// *Server here); internal/api/http stays unaware of MCP internals.
	// Nil = POST /v1/_mcp returns 503. Set via SetMCPHTTPHandler from
	// main.go after the handler is constructed.
	mcpHTTPHandler http.Handler

	// embedder is the v0.9.0 Vector Memory embedder. Same instance
	// the Memory tool holds. Nil = the /v1/_memory/reembed +
	// /v1/_memory/embed_stats endpoints return 503. Set via
	// SetEmbedder from main.go after the embedder is constructed.
	// Same wiring shape as the other late-bound deps above.
	embedder providers.Embedder

	// Build identifiers surfaced via /healthz so the Web UI topbar
	// can display the running binary's version instead of a stale
	// hard-coded string. Set via SetBuildInfo from cmd/loomcycle/main.go
	// after the buildVersion / buildCommit / buildTime vars have been
	// resolved (ldflags overrides → runtime/debug VCS stamp → "unknown").
	// Empty values render as "unknown" on /healthz.
	buildVersion string
	buildCommit  string
	buildTime    string
	// startedAt records process start so /healthz can report uptime
	// in seconds. Set in New().
	startedAt time.Time
}

// New constructs a Server. If st is non-nil, every run is recorded as a
// session+run+events tuple in the store; pass nil to keep v0.2 behaviour.
//
// The Agent built-in tool is registered automatically here (not in
// cmd/loomcycle/main.go) because its SubAgentRunner closes over the
// Server's own runSubAgent method — we'd otherwise have a chicken-and-
// egg between tool list and Server. Per-agent allow-list still gates
// access (an agent without "Agent" in `allowed_tools` won't see it).
//
// The cancel registry is also constructed here. It's always present
// (empty if no run has started) so handler code can call its methods
// unconditionally without nil-checking.
// defaultMaxRequestBytes is the run-ingest body cap used when the operator
// hasn't set one (and the fallback for tests that build a bare config). 16 MiB
// fits inline base64 image content (RFC AT); LOOMCYCLE_MAX_REQUEST_BYTES tunes
// the configured value.
const defaultMaxRequestBytes = 16 << 20

// maxRequestBytes returns the configured run-ingest body cap, falling back to
// defaultMaxRequestBytes when unset (<=0) so a bare-config Server never caps at
// zero bytes (which MaxBytesReader would treat as "reject every body").
func (s *Server) maxRequestBytes() int64 {
	if n := s.cfg.Env.MaxRequestBytes; n > 0 {
		return n
	}
	return defaultMaxRequestBytes
}

func New(cfg *config.Config, pr ProviderResolver, builtinTools []tools.Tool, sem *concurrency.Semaphore, st store.Store) *Server {
	// Hook registry is constructed with the operator-yaml host-widen
	// permit list (cfg.Hooks.PermitHostWiden.Owners). Without an entry
	// there, any Pre-hook's allow_hosts response is silently dropped
	// at dispatch time. The list is frozen at construction — the only
	// way to mutate the trust boundary is a restart with new yaml.
	hookReg := hooks.NewRegistryWithPermissions(cfg.Hooks.PermitHostWiden.Owners)
	s := &Server{
		cfg:            cfg,
		providers:      pr,
		tools:          builtinTools,
		sem:            sem,
		store:          st,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		startedAt:      time.Now(),
	}
	// F32: build the secret redactor from the process env (secret-classified
	// names only). Default-ON; LOOMCYCLE_REDACT_SECRETS=0 leaves s.redactor nil
	// (its methods are nil-safe, so makeRecordingEmit just skips redaction).
	if cfg.Env.RedactSecrets {
		s.redactor = redact.New(secretEnvValues(os.Environ()), true)
	}
	// RFC Z: build the runtime-wide context-transform chain once. The redact
	// plugin masks the same operator env-secret values + the Tier-B heuristic
	// patterns from the OUTBOUND request (vs F32's persisted-transcript path).
	// config.validate already rejected unknown names at load; a Build error
	// here is defensive — log it loudly rather than silently drop a transform.
	if cp, err := contextplugin.Build(cfg.ContextPlugins, secretEnvValues(os.Environ())); err != nil {
		log.Printf("context_plugins: build failed (chain disabled): %v", err)
	} else {
		s.contextPlugins = cp
	}
	s.tools = append(s.tools, &builtin.AgentTool{
		// Run drops the run_id (the common sequential/spawn path); RunDetailed
		// keeps it for the parallel_spawn ledger (RFC X Phase 3). Both drive
		// the same runSubAgent.
		Run: func(ctx context.Context, name, prompt, defID string) (string, error) {
			out, _, err := s.runSubAgent(ctx, name, prompt, defID)
			return out, err
		},
		RunDetailed: s.runSubAgent,
		SpawnLedger: cfg.Env.ResumeFanout, // RFC X Phase 3: record the spawn ledger (default off)
		// v0.11.8 — per-agent max_concurrent_children cap for
		// Agent.parallel_spawn. Walks the same resolver chain as
		// sub-run dispatch (yaml > dynamic_agents > AgentDef
		// substrate) so substrate-edited overrides apply on the
		// next call without restart. Returns 0 = no override; the
		// tool falls back to DefaultMaxConcurrentChildren.
		CapLookup: func(ctx context.Context, callingAgent string) int {
			// RFC N: resolve within the calling run's tenant (carried via
			// ctx RunIdentity for in-loop callers).
			def, ok := lookup.Agent(ctx, s.store, s.cfg, tenantFromCtx(ctx), callingAgent)
			if !ok {
				return 0
			}
			return def.MaxConcurrentChildren
		},
	})
	// F45: `Context op=tools` introspects the runtime-wide catalog via the
	// Context tool's Tools field. main.go can only set that to the PRE-server
	// list (builtinTools) — the AgentTool appended just above isn't in that
	// snapshot, so `Context op=tools` silently omitted Agent. The server owns
	// its tool set, so wire the catalog here, at serve time, AFTER the last
	// New()-time append: re-point any Context tool to the COMPLETE s.tools.
	for _, t := range s.tools {
		if ct, ok := t.(*builtin.Context); ok {
			ct.Tools = s.tools
		}
	}
	return s
}

// SetBuildInfo records the binary's identification triple. Called from
// cmd/loomcycle/main.go after the buildVersion/buildCommit/buildTime
// vars are resolved. The values flow through /healthz so the Web UI
// can render a real version label instead of a hard-coded string that
// drifts every release.
func (s *Server) SetBuildInfo(version, commit, builtAt string) {
	s.buildVersion = version
	s.buildCommit = commit
	s.buildTime = builtAt
}

// SetMCPFallback installs the optional MCP lazy-resolution fallback.
// Pass mcp.NewLazyResolver(...).Resolve here. Nil disables the
// fallback (per-run dispatchers behave exactly as before — unknown
// tool names return "tool not found" without any handshake retry).
//
// Mirrors the SetResolver pattern: tests that don't need lazy MCP
// recovery can omit this call entirely. Production wires it from
// cmd/loomcycle/main.go after the MCP pool is built.
func (s *Server) SetMCPFallback(fn tools.FallbackFunc) {
	s.mcpFallback = fn
}

// SetDynamicToolEnumerator installs the optional post-boot tool advertiser.
// fn returns the substrate-registered tools (dynamic MCP + A2A) currently
// available for the passed tenant; the run-creation path folds them into the
// candidate set before the allowed_tools filter, so post-boot registrations
// are advertised to the model without a restart. Nil-safe; wired in main.go
// after the registries + pool are built.
//
// RFC N FIX 2-mcp: fn takes the run's authoritative tenant EXPLICITLY rather
// than deriving it from ctx — see candidateTools.
//
// F33: fn also takes wantServers — the set of dynamic MCP server names this run
// references via its allowed_tools. The enumerator advertises every server's
// CACHED tools regardless, but only HANDSHAKES a referenced server whose cache
// is empty, so a dynamic-MCP-only agent sees its tools at run start (not just on
// a first call that, with nothing advertised, the model never makes).
func (s *Server) SetDynamicToolEnumerator(fn func(ctx context.Context, tenantID string, wantServers map[string]bool) []tools.Tool) {
	s.dynamicTools = fn
}

// candidateTools returns the boot-time tool set plus any post-boot
// substrate-registered tools, so the per-run allowed_tools filter (and thus
// the advertised catalog) sees dynamically-registered tools. The boot set is
// the floor; a dynamic tool that duplicates a boot-set name collapses in
// filterTools's by-name map (last writer wins; both wrap the same upstream).
//
// RFC N FIX 2-mcp: tenantID is the run's authoritative tenant, passed
// EXPLICITLY by the caller. The entry sites call this BEFORE WithRunIdentity
// is stamped on ctx, so a ctx-derived tenant would be "" for non-HTTP-principal
// spawn surfaces — and the run would advertise the wrong tenant's MCP tools.
//
// F33: agentAllowed is the agent's declared allowed_tools. We derive the set of
// dynamic MCP servers it references (referencedDynamicMCPServers) and hand it to
// the enumerator so a referenced server with no cached tools is handshaked and
// advertised at run start.
func (s *Server) candidateTools(ctx context.Context, tenantID string, agentAllowed []string) []tools.Tool {
	if s.dynamicTools == nil {
		return s.tools
	}
	dyn := s.dynamicTools(ctx, tenantID, referencedDynamicMCPServers(agentAllowed))
	if len(dyn) == 0 {
		return s.tools
	}
	out := make([]tools.Tool, 0, len(s.tools)+len(dyn))
	out = append(out, s.tools...)
	out = append(out, dyn...)
	return out
}

// referencedDynamicMCPServers extracts the set of MCP server names an agent's
// allowed_tools patterns reference, for the F33 run-start handshake. A pattern
// of the shape `mcp__<server>__<tool>` or the wildcard `mcp__<server>__*`
// contributes <server>; any other pattern (a native tool, a bare `*`, an
// `a2a__…` peer) contributes nothing — those don't gate on dynamic MCP
// discovery. The names are the SANITISED server segment as it appears in the
// advertised tool name, so they line up with the enumerator's comparison.
func referencedDynamicMCPServers(patterns []string) map[string]bool {
	var out map[string]bool
	for _, p := range patterns {
		rest, ok := strings.CutPrefix(p, "mcp__")
		if !ok {
			continue
		}
		// server is everything up to the first "__" separating it from the tool.
		server, _, ok := strings.Cut(rest, "__")
		if !ok || server == "" {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		out[server] = true
	}
	return out
}

// MCPPoolInspector returns the cached tools/list result for an MCP
// server name as already-JSON-marshaled bytes ([{name, description,
// input_schema}, …]) or nil when the Pool has no cached entry. The
// "already JSON" wire keeps this package free of internal/tools/mcp
// imports — the http layer doesn't need ToolDescriptor's Go shape,
// only the marshaled output for embedding into the library response.
type MCPPoolInspector func(name string) json.RawMessage

// SetMCPPoolInspector wires the live Pool's PeekTools accessor so the
// v0.9.x /v1/_library/mcp-servers endpoint can enumerate static
// servers' tool lists. Nil leaves the inspector unset — static MCP
// entries in the library response will omit `discovered_tools`.
func (s *Server) SetMCPPoolInspector(fn MCPPoolInspector) {
	s.mcpPoolInspector = fn
}

// SetMetricsSampler installs the v0.8.x metrics sampler so the
// /v1/_metrics/* endpoints have a backing object. Nil is the
// default — endpoints return 503 until this is called.
func (s *Server) SetMetricsSampler(m *metrics.Sampler) {
	s.metricsSampler = m
}

// SetSkillSet installs the static skills.Set so v0.8.22 SkillDef
// per-run resolution can fall back to the static body when a skill
// name has no DB-active SkillDef row. Nil is fine — the per-run
// resolver tolerates nil (no override).
func (s *Server) SetSkillSet(set *skills.Set) {
	s.skillSet = set
}

// SetPauseManager installs the v0.8.17 pause/resume coordinator so the
// /v1/_pause, /v1/_resume, /v1/_state endpoints have a backing manager.
// Nil is the default — endpoints return 503 until this is called.
// Same wiring shape as SetMetricsSampler.
func (s *Server) SetPauseManager(m *pause.Manager) {
	s.pauseMgr = m
}

// replicaLister is the minimum read surface of *coord.ReplicaStore
// the healthz handler needs. Declared here (not in coord) so tests
// can stub it without importing the live Postgres-backed type.
type replicaLister interface {
	ListReplicas(ctx context.Context) ([]coord.Replica, error)
}

// SetCoord installs the v0.12.0 multi-replica HA bus + replicas table
// reader. When called with non-nil arguments, /healthz starts including
// `replica_id` + `replicas[]` fields in its response. When unset (the
// default), /healthz returns the same response shape as v0.11.x and no
// cluster-mode code paths run. Phase 1 wires this; Phases 2-6 build
// publishers/subscribers on top of the backplane.
func (s *Server) SetCoord(bp coord.Backplane, rs replicaLister, replicaID string) {
	s.backplane = bp
	s.replicaStore = rs
	s.replicaID = replicaID
}

// PauseManager returns the wired pause manager (or nil). Read-only
// accessor for components outside this package (e.g. main.go's
// graceful shutdown path, MCP handlers that bridge into the same
// state). Don't expose mutation through this — use SetPauseManager.
func (s *Server) PauseManager() *pause.Manager {
	return s.pauseMgr
}

// SetEmbedder installs the v0.9.0 Vector Memory embedder so the
// /v1/_memory/reembed + /v1/_memory/embed_stats endpoints have a
// backing object. Nil is the default — those endpoints return 503
// until this is called. Same wiring shape as SetMetricsSampler.
func (s *Server) SetEmbedder(e providers.Embedder) {
	s.embedder = e
}

// SetMCPHTTPHandler installs the v0.8.15.3 HTTP MCP transport handler
// so the POST /v1/_mcp endpoint has a backing dispatcher. Nil is the
// default — the endpoint returns 503 until this is called.
//
// Typed as http.Handler (the standard library interface) rather than
// *lcmcp.HTTPHandler so this package gains no import of
// internal/api/mcp. main.go wires the concrete handler; this package
// never sees its type.
func (s *Server) SetMCPHTTPHandler(h http.Handler) {
	s.mcpHTTPHandler = h
}

// SetSystemPublisher installs the v0.8.6 system-channels publisher.
// Without this call, POST /v1/_channels/_system/{name}/publish
// refuses every request with 503. Wired from cmd/loomcycle/main.go
// after the Store is open + the Bus/Scheduler are constructed.
func (s *Server) SetSystemPublisher(p channels.SystemPublisher) {
	s.systemPublisher = p
}

// SetInterruptionBus wires the v0.8.16 in-process notification bus
// used by the Interruption-resolve HTTP handler to wake the blocked
// tool. Same Bus instance the Channel tool uses.
// SetSteerRegistry wires the operator-steering registry (PR 2). nil leaves
// steering disabled. Called once at boot.
func (s *Server) SetSteerRegistry(r *steer.Registry) {
	s.steerReg = r
}

func (s *Server) SetInterruptionBus(b *channels.Bus) {
	s.interruptionBus = b
}

// SetChannelBus wires the v0.9.x in-process notification bus used by
// SubscribeChannel (Connector + HTTP) to wake long-poll subscribers
// on publish. Same instance as the in-band Channel tool's Bus —
// reusing one bus per process keeps wake-up paths uniform and ensures
// agent publishes wake wire-side subscribers and vice versa.
func (s *Server) SetChannelBus(b *channels.Bus) {
	s.channelBus = b
}

// SetRunStateBus wires the v0.9.x run-state pub/sub bus that backs
// GET /v1/users/{user_id}/agents/stream. Without this call the SSE
// endpoint refuses with 503. Constructed once per process in main.go.
func (s *Server) SetRunStateBus(b *runstate.Bus) {
	s.runStateBus = b
}

// RunStateBus exposes the wired run-state bus for v0.12.3 Phase 4
// main.go wiring of the cluster-mode backplane fanout. Returns nil
// when the bus hasn't been set.
func (s *Server) RunStateBus() *runstate.Bus {
	return s.runStateBus
}

// Backplane exposes the wired v0.12.0 coord.Backplane for Phase 4's
// pause manager wiring (which runs AFTER SetCoord so it can't see
// the bp variable scoped to the cluster init block). Returns nil
// when coord wasn't wired (single-replica mode).
func (s *Server) Backplane() coord.Backplane {
	return s.backplane
}

// ChannelBus exposes the wired channel bus for the same Phase 4
// reason as RunStateBus. Returns nil when unwired.
func (s *Server) ChannelBus() *channels.Bus {
	return s.channelBus
}

// SetMCPServerDefTool wires the v0.9.x MCPServerDef substrate tool.
// Without this call, Connector.MCPServerDef + POST /v1/_mcpserverdef
// + the LoomCycle MCP meta-tool all refuse with "not configured".
// Set from main.go once the tool + dynamic registry + pool are built.
func (s *Server) SetMCPServerDefTool(t tools.Tool) {
	s.mcpServerDefTool = t
}

// SetScheduleDefTool wires the v1.x ScheduleDef substrate tool.
// Without this call, Connector.ScheduleDef + POST /v1/_scheduledef
// + the future LoomCycle MCP meta-tool all refuse with "not
// configured". Set from main.go once the tool is built. The tool
// only needs the store + cfg, so it can be constructed earlier
// than the MCP-dependent MCPServerDef tool.
func (s *Server) SetScheduleDefTool(t tools.Tool) {
	s.scheduleDefTool = t
}

// SetA2AServerCardDefTool wires the v1.x RFC G A2AServerCardDef substrate
// tool. Without this call, Connector.A2AServerCardDef +
// POST /v1/_a2aservercarddef + the LoomCycle MCP meta-tool all refuse with
// "not configured". The tool only needs the store + cfg, so it can be
// constructed alongside ScheduleDef in main.go.
func (s *Server) SetA2AServerCardDefTool(t tools.Tool) {
	s.a2aServerCardDefTool = t
}

// SetA2AAgentDefTool wires the v1.x RFC G A2AAgentDef substrate tool.
// Without this call, Connector.A2AAgentDef + POST /v1/_a2aagentdef + the
// LoomCycle MCP meta-tool all refuse with "not configured".
func (s *Server) SetA2AAgentDefTool(t tools.Tool) {
	s.a2aAgentDefTool = t
}

// SetWebhookDefTool wires the v1.x RFC H WebhookDef substrate tool.
// Without this call, Connector.WebhookDef + POST /v1/_webhookdef + the
// LoomCycle MCP meta-tool all refuse with "not configured". The tool
// only needs the store + cfg, so it can be constructed alongside the
// A2A substrate tools in main.go.
func (s *Server) SetWebhookDefTool(t tools.Tool) {
	s.webhookDefTool = t
}

// SetMemoryBackendDefTool wires the RFC I MR-3a MemoryBackendDef
// substrate tool. Without this call, Connector.MemoryBackendDef + POST
// /v1/_memorybackenddef + the LoomCycle MCP meta-tool all refuse with
// "not configured". The tool only needs the store + cfg, so it can be
// constructed alongside the other substrate tools in main.go.
func (s *Server) SetMemoryBackendDefTool(t tools.Tool) {
	s.memoryBackendDefTool = t
}

// SetOperatorTokenDefTool wires the RFC L OperatorTokenDef substrate
// tool. Without this call, Connector.OperatorTokenDef + POST
// /v1/_operatortokendef + the gRPC RPC + the MCP meta-tool all refuse
// with "not configured".
func (s *Server) SetOperatorTokenDefTool(t tools.Tool) {
	s.operatorTokenDefTool = t
}

// SetSqlMem wires the RFC AA SQL Memory manager so the server can drop a
// run-scope SQL database when the top-level run completes. Nil leaves the
// run-scope drop a no-op (the subsystem is disabled).
func (s *Server) SetSqlMem(m *sqlmem.Manager) {
	s.sqlMem = m
}

// Redactor returns the server's secret redactor (nil when redaction is
// disabled). Exposed so the Memory tool's SQL audit reuses the SAME redactor
// the server built from the secret-classified env — one source of truth for
// what counts as a secret. The returned *Redactor's methods are nil-safe.
func (s *Server) Redactor() *redact.Redactor { return s.redactor }

// newDispatcher centralises Dispatcher construction so the three call
// sites (handleRuns, handleMessages, runSubAgent) all pick up the
// fallback automatically. With s.mcpFallback nil this is identical
// to tools.NewDispatcher(allowedTools).
func (s *Server) newDispatcher(allowedTools []tools.Tool) *tools.Dispatcher {
	if s.mcpFallback == nil {
		return tools.NewDispatcher(allowedTools)
	}
	return tools.NewDispatcherWithFallback(allowedTools, s.mcpFallback)
}

// SessionLocks exposes the per-session lock map so the gRPC server
// (which shares the same Server instance via the runner.Runner
// interface) can use the same coordination point. Both wires
// targeting the same session_id must serialize on the same lock.
func (s *Server) SessionLocks() *runner.SessionLockMap { return s.sessionLocks }

// SetResolver wires the model-resolution matrix into the Server. Call
// from cmd/loomcycle/main.go after constructing both. Optional: when
// no resolver is set, every agent uses the explicit-pin path
// (cfg.ResolveAgentModel) — back-compat with v0.6.x.
func (s *Server) SetResolver(r *resolve.Resolver) { s.resolver = r }

// markStalledFn returns a closure suitable for loop.RunOptions.MarkStalled.
// The loop invokes the closure with the LIVE (provider, model) for the
// current iteration — which is meaningful when v0.8.2 fallback switched
// providers mid-run. The unused `provider`/`model` args are kept on the
// constructor signature for the wiring symmetry with markRateLimitedFn /
// clearStallFn at call sites; their values seed the initial resolution
// but the resolver receives the loop-supplied live pair.
//
// Returns nil when no resolver is wired (back-compat path) — RunOptions
// treats nil as "stall feedback disabled".
//
// PRE-2026-05-27 BUG: the closure used to ignore the loop's args and
// pin to the construction-time (provider, model). When fallback ran
// mid-run, MarkStalled poisoned the WRONG matrix entry — the original
// provider got blamed for the post-switch provider's failure. Fixed
// alongside the markRateLimitedFn addition (PR for #235 follow-up).
func (s *Server) markStalledFn(_, _ string) func(p, m, reason string) {
	if s.resolver == nil {
		return nil
	}
	return func(p, m, reason string) {
		s.resolver.MarkStalled(p, m, reason)
	}
}

// markRateLimitedFn returns a closure suitable for
// loop.RunOptions.MarkRateLimited. Sibling to markStalledFn but for
// the transient-429 case: the loop calls this — instead of
// markStalledFn — when the surfaced error is a rate-limit response
// that exhausted the driver's internal retry budget.
//
// Uses the LIVE (provider, model) from the loop's call. Critical for
// the fallback case: the post-fallback provider is the one that
// actually rate-limited, and that's the entry that should get the
// cooldown — NOT the pre-fallback original provider.
//
// tier is the run's user_tier name. When the operator yaml sets
// `user_tiers.<tier>.rate_limit_cooldown_ms` > 0, the closure
// substitutes that value for the loop's `retryAfter` ONLY when the
// loop passed 0 (the common "use default" case). A non-zero
// retryAfter from the loop (future work threading the real
// Retry-After header up from the driver) takes precedence — operators
// trust provider-supplied hints over their own static knob.
//
// Returns nil when no resolver is wired — RunOptions treats nil as
// "rate-limit feedback disabled".
func (s *Server) markRateLimitedFn(tier string) func(p, m string, retryAfter time.Duration) {
	if s.resolver == nil {
		return nil
	}
	operatorCooldown := s.rateLimitCooldownForTier(tier)
	return func(p, m string, retryAfter time.Duration) {
		if retryAfter <= 0 && operatorCooldown > 0 {
			retryAfter = operatorCooldown
		}
		s.resolver.MarkRateLimited(p, m, retryAfter)
	}
}

// clearStallFn returns a closure suitable for loop.RunOptions.ClearStall.
// Companion to markStalledFn: the loop calls it on a SUCCESSFUL
// iteration with the live (provider, model). After fallback the
// healthy provider is the one we want to credit — same fix as
// markStalledFn for the same reason.
func (s *Server) clearStallFn(_, _ string) func(p, m string) {
	if s.resolver == nil {
		return nil
	}
	return func(p, m string) {
		s.resolver.ClearStall(p, m)
	}
}

// resolveErrorToStatus maps a resolver error to the appropriate HTTP
// status code. Tier / pin unavailability returns 503 so caller-side
// retry-with-backoff hits the right path. Anything else (typo on
// agent name, missing pin/tier, validation failure) is 400.
func resolveErrorToStatus(err error) int {
	switch {
	case errors.Is(err, resolve.ErrTierUnavailable),
		errors.Is(err, resolve.ErrPinUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, resolve.ErrUnknownAgent),
		errors.Is(err, runner.ErrUnknownAgent):
		return http.StatusBadRequest
	default:
		// ErrInvalidArgument and unknown errors. 400 is the safer
		// default than 500 — most resolver errors are operator-
		// config issues that deserve to surface as bad-request.
		return http.StatusBadRequest
	}
}

// writeQuotaError maps a concurrency semaphore error to HTTP 429. Two
// distinct shapes share the status code but distinguish via the JSON
// body's `code` field so adapter consumers can branch retry strategies:
//
//   - `code: "per_user_quota_exhausted"` + `Retry-After: 5` header.
//     The user has hit their personal cap; they specifically need to
//     wait. Body carries `user_id` + `cap` so the adapter can surface
//     it in error messages or rate-limit telemetry.
//   - `code: "backpressure"`. The global queue is full (or timed out).
//     Operator-wide load signal; back off with longer jitter.
//
// Anything else falls through to 500 with the raw error string —
// shouldn't happen with the current AcquireForUser implementation but
// the safe-default keeps a panic from surfacing as an opaque 200.
func writeQuotaError(w http.ResponseWriter, err error) {
	var pue *concurrency.ErrPerUserQuotaExhausted
	if errors.As(err, &pue) {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"code":"per_user_quota_exhausted","error":%q,"user_id":%q,"cap":%d}`,
			pue.Error(), pue.UserID, pue.Cap)
		return
	}
	if concurrency.IsBackpressure(err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"code":"backpressure","error":%q}`, err.Error())
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// resolveAgent returns (provider, model, effort) for the named agent.
// Picks between two paths:
//
//   - Explicit pin (agent has Provider+Model): use cfg.ResolveAgentModel
//     directly. The resolver, when present, still gates the result via
//     the availability matrix so a stalled pinned model surfaces
//     ErrPinUnavailable instead of leaking the driver's 5xx.
//
//   - Tier (agent has Tier set): delegate to the resolver. Returns
//     ErrTierUnavailable if no candidate in the requested tier
//     resolves. The HTTP handler translates this to 503; gRPC to
//     codes.Unavailable.
//
// Returning runner.ErrInvalidArgument / runner.ErrUnknownAgent for
// pre-resolution errors keeps the wire-error vocabulary stable.
//
// userTier is the v0.8.2 user-facing-tier name from the request body.
// Empty falls through to cfg.UserTiers["default"] when an operator
// has a user_tiers block; otherwise the resolver operates without an
// overlay (v0.7.x behaviour). Unknown names are rejected upstream
// before this is called.
func (s *Server) resolveAgent(ctx context.Context, tenantID, agentName, userTier string) (providerID, model, effort string, err error) {
	// RFC N: resolve at the RUN's authoritative tenant (threaded by the
	// caller — effectiveTenantID / req.TenantID / sess.TenantID), NOT a
	// ctx-derived or empty tenant. This is the provider/model resolution +
	// existence gate; resolving at "" here made tenant-scoped DYNAMIC agents
	// unrunnable via /v1/runs ("unknown agent") even though the entry def
	// lookup (lookupAgent at the run sites) was already tenant-correct.
	// "" = shared/default tenant.
	def, ok := s.lookupAgent(ctx, tenantID, agentName)
	if !ok {
		return "", "", "", fmt.Errorf("%w: %s", runner.ErrUnknownAgent, agentName)
	}
	return s.resolveAgentDef(def, agentName, userTier)
}

// lookupAgent delegates to internal/lookup.Agent — the canonical
// agent-name resolver consolidating the static / dynamic_agents /
// substrate lookup chain + the normalizer chain. See the lookup
// package doc for the rationale + the "future substrate" contract.
//
// Centralizing the lookup at the package boundary is the antidote
// to the v0.8.15 regression where each entry point had its own
// inline lookup, AND the v0.9.x lookups that silently skipped the
// boot-time normalizer chain (PRs #184 + #186). Every code path
// that needs an agent name → def goes through here.
func (s *Server) lookupAgent(ctx context.Context, tenantID, name string) (config.AgentDef, bool) {
	// RFC N: tenant is an EXPLICIT argument, not ctx-derived. At the
	// run-creation entry sites the authoritative tenant is already
	// computed as effectiveTenantID (via applyPrincipal / the session)
	// BEFORE WithRunIdentity is stamped on ctx — so for non-HTTP-principal
	// spawn surfaces (A2A, scheduler, webhook, MCP spawn_run, gRPC-legacy)
	// tenantFromCtx(ctx) would return the WRONG tenant ("" or the caller's,
	// not the run's) and the entry agent would resolve at the wrong tenant
	// while memory + sub-agents use effectiveTenantID. Callers that already
	// run with RunIdentity on ctx (sub-agents, the Context-tool path, boot)
	// pass tenantFromCtx(ctx) explicitly to preserve today's behavior.
	// "" = shared/default/legacy tenant.
	// nil-store guard at the boundary so the lookup package can
	// type-assert an interface receiver. The lookup package treats
	// "no store" identically to "store didn't have the name" — both
	// fall through to (zero, false).
	if s.store == nil {
		return lookup.Agent(ctx, nil, s.cfg, tenantID, name)
	}
	return lookup.Agent(ctx, s.store, s.cfg, tenantID, name)
}

// resolveAgentDef mirrors resolveAgent but takes a caller-supplied
// AgentDef instead of looking it up in cfg.Agents. Used by the
// v0.8.5 sub-agent path when an overlay has already produced an
// effective def whose Provider/Model/Tier/Effort differ from the
// static yaml. Without this, a forked sub-agent runs against the
// static model — silently defeating the whole point of def_id
// pinning.
func (s *Server) resolveAgentDef(def config.AgentDef, agentName, userTier string) (providerID, model, effort string, err error) {
	hasPin := def.Provider != "" || def.Model != ""
	hasTier := def.Tier != ""

	// Tier path: agent declares tier (validation already rejected
	// pin+tier together), resolver does the work.
	if hasTier {
		if s.resolver == nil {
			// Tier requested but no resolver wired (test fixture or
			// degraded-startup edge case before SetResolver was
			// called). Fail explicitly rather than silently picking
			// some default.
			return "", "", "", fmt.Errorf("%w: agent %q uses tier %q but resolver is not configured",
				runner.ErrInvalidArgument, agentName, def.Tier)
		}
		req := resolve.AgentRequest{
			Name:      agentName,
			Tier:      def.Tier,
			Effort:    def.Effort,
			Providers: def.Providers,
			Models:    convertConfigCandidates(def.Models, s.cfg.Models),
			UserTier:  s.userTierOverlay(userTier),
		}
		dec, rerr := s.resolver.Resolve(req)
		if rerr != nil {
			return "", "", "", rerr
		}
		return dec.Provider, dec.Model, dec.Effort, nil
	}

	// Pin path (or fallback to defaults): use the v0.6.x logic against
	// the caller-supplied def, NOT the static cfg.Agents entry.
	if !hasPin && s.cfg.Defaults.Model == "" {
		return "", "", "", fmt.Errorf("%w: agent %q has no pin, no tier, and no defaults", runner.ErrInvalidArgument, agentName)
	}
	providerID, model, err = s.cfg.ResolveAgentDefModel(agentName, def)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: %v", runner.ErrInvalidArgument, err)
	}
	// Effort still flows through on the pin path — an explicit-pin
	// agent can declare effort and the driver will translate it
	// where supported. Empty when not declared.
	return providerID, model, def.Effort, nil
}

// userTierOverlay builds the resolver's UserTierOverlay from
// cfg.UserTiers[name]. Returns nil when:
//   - the operator has no user_tiers block configured (v0.7-era
//     deploys, unaffected by v0.8.2),
//   - the name is empty AND there's no "default" entry,
//   - the name doesn't resolve (handled upstream by HTTP-side
//     validation; returning nil here is a safety belt).
//
// When the operator HAS configured user_tiers, an empty name falls
// through to "default" — preserves v0.7.x clients that don't yet send
// user_tier in the request body.
func (s *Server) userTierOverlay(name string) *resolve.UserTierOverlay {
	if s.cfg == nil || len(s.cfg.UserTiers) == 0 {
		return nil
	}
	if name == "" {
		name = "default"
	}
	ut, ok := s.cfg.UserTiers[name]
	if !ok {
		return nil
	}
	return &resolve.UserTierOverlay{
		Name:                name,
		ProviderPriority:    ut.ProviderPriority,
		Tiers:               convertConfigCandidates(ut.Tiers, s.cfg.Models),
		FallbackOnError:     ut.FallbackOnError,
		MaxFallbackAttempts: ut.MaxFallbackAttempts,
		RetryAttempts:       ut.RetryAttempts,
		RateLimitCooldownMs: clampRateLimitCooldownMs(ut.RateLimitCooldownMs),
	}
}

// clampRateLimitCooldownMs enforces the operator-doc-promised
// [1_000, 600_000] bounds on the per-tier cooldown. 0 (unset)
// passes through so the resolver picks its default. The clamp
// happens here rather than at config-load so the bounds are a
// single source of truth (the docstring on UserTier.RateLimitCooldownMs
// names the same range).
func clampRateLimitCooldownMs(ms int) int {
	if ms <= 0 {
		return 0
	}
	const (
		minMs = 1_000   // 1 s — anything lower defeats the cooldown's purpose
		maxMs = 600_000 // 10 min — beyond this the periodic probe clears the matrix first
	)
	if ms < minMs {
		return minMs
	}
	if ms > maxMs {
		return maxMs
	}
	return ms
}

// retryAttemptsForTier returns the same-provider retry budget the
// loop should use for runs carrying this user_tier. Sourced from
// the user_tier yaml (UserTier.RetryAttempts); falls through to 0
// when no overlay exists or the field was omitted.
func (s *Server) retryAttemptsForTier(name string) int {
	overlay := s.userTierOverlay(name)
	if overlay == nil {
		return 0
	}
	return overlay.RetryAttempts
}

// rateLimitCooldownForTier returns the operator-tunable cooldown
// duration the resolver should apply on MarkRateLimited for runs
// carrying this user_tier. Zero (the default) means "use the
// resolver's hardcoded default" — the closure passes 0 through to
// resolver.MarkRateLimited, which substitutes its own default
// (30 s) when retryAfter <= 0.
//
// Reads s.cfg directly (not via userTierOverlay) — this is called
// once per RunOptions construction and we only need a single int
// field; rebuilding the full overlay (with candidate conversion)
// would be wasted work per run.
func (s *Server) rateLimitCooldownForTier(name string) time.Duration {
	if s.cfg == nil || name == "" {
		return 0
	}
	ut, ok := s.cfg.UserTiers[name]
	if !ok {
		return 0
	}
	ms := clampRateLimitCooldownMs(ut.RateLimitCooldownMs)
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// retryAttemptsForAgent resolves the same-provider retry budget for
// a specific (agent, user_tier) pair. Per-agent override (when set)
// wins; otherwise falls through to the user_tier value; 0 if neither.
//
// The agent override uses *int so "unset" (use tier) is
// distinguishable from "0" (explicitly disable retries even under a
// generous tier — high-stakes side-effectful agents force this
// regardless of operator tier policy).
func (s *Server) retryAttemptsForAgent(agentDef config.AgentDef, tier string) int {
	if agentDef.RetryAttempts != nil {
		return *agentDef.RetryAttempts
	}
	return s.retryAttemptsForTier(tier)
}

// substratePoliciesForAgent returns the v0.8.5 AgentDef +
// Evaluation policies for one agent. Mirrors channelPolicyForAgent's
// shape. selfName is the resolved agent name as seen by the ctx
// chain (== tools.AgentName at attach time); stamped onto the
// AgentDef policy so the tool's "self" scope check is robust to
// any future ctx-stack mutations.
func (s *Server) substratePoliciesForAgent(agentDef config.AgentDef, selfName string) (tools.AgentDefPolicyValue, tools.EvaluationPolicyValue) {
	return tools.AgentDefPolicyValue{
			Scopes:   agentDef.AgentDefScopes,
			SelfName: selfName,
		},
		tools.EvaluationPolicyValue{
			Scopes: agentDef.EvaluationScopes,
		}
}

// skillDefPolicyForAgent returns the v0.8.22 SkillDef policy for
// one agent. Default-deny: empty SkillDefScopes → no SkillDef ops.
func (s *Server) skillDefPolicyForAgent(agentDef config.AgentDef) tools.SkillDefPolicyValue {
	return tools.SkillDefPolicyValue{Scopes: agentDef.SkillDefScopes}
}

// volumeDefPolicyForAgent returns the RFC AH Phase 2a VolumeDef policy
// for one agent. Default-deny: empty VolumeDefScopes → no create/delete/
// purge ops.
func (s *Server) volumeDefPolicyForAgent(agentDef config.AgentDef) tools.VolumeDefPolicyValue {
	return tools.VolumeDefPolicyValue{Scopes: agentDef.VolumeDefScopes}
}

// runPromptProvenance captures the per-run metadata that fed into
// the resolved system prompt. Used as the payload sidecar on the
// v0.9.x `system_prompt` event so an operator inspecting the run
// can see WHICH AgentDef row + WHICH SkillDef rows produced the
// instructions the agent received — not just the merged text.
//
// SkillDefIDs maps the agent's declared skill names to the def_id
// of the DB-active SkillDef row that supplied the skill body for
// this specific run. Skills falling back to the static SKILL.md
// body (no DB-active row) are NOT in this map — only DB overrides.
// Empty map (or absent field on the wire) = pure static prompt.
type runPromptProvenance struct {
	SkillDefIDs map[string]string `json:"skill_def_ids,omitempty"`
}

// resolveSkillBodiesForRun rebuilds agentDef.SystemPrompt for a
// single run with v0.8.22 SkillDef per-run resolution. For each
// skill named in agentDef.Skills, the active SkillDef row's body
// (when present) overrides the static SKILL.md body baked at
// config-load.
//
// Fast path: if NO skill name has a DB-active row, the unmodified
// agentDef is returned (the config-load baked SystemPrompt is
// already correct). Slow path: rebuild from SystemPromptBase +
// per-skill (DB-or-static) bodies.
//
// Returns agentDef unchanged on any error (logged) — the run
// continues with the static baked prompt rather than fail. The
// agent loop is unchanged; only SystemPrompt may differ.
//
// Second return value is the per-run provenance (skillName → active
// SkillDef def_id) for callers emitting the v0.9.x `system_prompt`
// transcript event. Empty when no DB-active rows were used.
func (s *Server) resolveSkillBodiesForRun(ctx context.Context, tenantID string, agentDef config.AgentDef) (config.AgentDef, runPromptProvenance) {
	var prov runPromptProvenance
	if len(agentDef.Skills) == 0 || s.store == nil {
		return agentDef, prov
	}
	// Resolve each skill via the canonical lookup chain (substrate →
	// static). lookup.Skill returns Source="substrate" when a DB-active
	// row resolved + Source="static" when falling back to the boot
	// SkillsRoot bundle.
	resolutions := make(map[string]lookup.SkillResolution, len(agentDef.Skills))
	activeDefIDs := make(map[string]string, len(agentDef.Skills))
	anySubstrate := false
	// RFC N: tenant is an EXPLICIT argument, not ctx-derived — mirrors
	// lookupAgent (FIX 2). At the run-creation entry sites the run's
	// authoritative tenant (effectiveTenantID / sess.TenantID) is already
	// computed BEFORE WithRunIdentity is stamped on ctx, so deriving it
	// from tenantFromCtx(ctx) here would resolve skills at the WRONG tenant
	// for non-HTTP-principal spawn surfaces (A2A / scheduler / webhook /
	// MCP spawn_run / gRPC-legacy) while memory + sub-agents use the run's
	// tenant. The sub-agent call (runSubAgent) passes tenantFromCtx(ctx)
	// since the parent's RunIdentity already carries the tenant. A token
	// only sees its own tenant's promotions + the shared base. "" = shared.
	for _, skillName := range agentDef.Skills {
		sr, ok := lookup.Skill(ctx, s.store, s.skillSet, tenantID, skillName)
		if !ok {
			continue
		}
		resolutions[skillName] = sr
		if sr.Source == "substrate" {
			activeDefIDs[skillName] = sr.DefID
			anySubstrate = true
		}
	}
	// Fast path: when no substrate-active row contributed, the
	// boot-time bake of agentDef.SystemPrompt already reflects the
	// static skill bodies — return unchanged.
	if !anySubstrate {
		return agentDef, prov
	}
	prov.SkillDefIDs = activeDefIDs
	// Slow path: rebuild SystemPrompt from base + per-skill body so
	// substrate overrides land in place of the static bake.
	rebuilt := agentDef
	rebuilt.SystemPrompt = agentDef.SystemPromptBase
	for _, skillName := range agentDef.Skills {
		sr, ok := resolutions[skillName]
		if !ok || strings.TrimSpace(sr.Body) == "" {
			continue
		}
		if rebuilt.SystemPrompt != "" {
			rebuilt.SystemPrompt += "\n\n---\n\n"
		}
		rebuilt.SystemPrompt += sr.Body
	}
	return rebuilt, prov
}

// emitSystemPromptEvent persists the resolved system prompt + its
// provenance as a `system_prompt` transcript event so an operator
// inspecting the run can see what instructions the agent received —
// not just the model's subsequent output. Mirror of the existing
// `user_input` emission (which captures the caller's segments).
// Emitted ONCE per run, right after the user_input event so the two
// "what the agent saw" cards sort naturally at the top of the
// transcript.
//
// No-op when the store isn't configured, runID is empty, or the
// agent has no system prompt. Store errors are logged + swallowed
// (same posture as user_input); never blocks the run.
func (s *Server) emitSystemPromptEvent(
	ctx context.Context,
	runID string,
	systemPrompt string,
	agentDefID string,
	prov runPromptProvenance,
) {
	if s.store == nil || runID == "" || systemPrompt == "" {
		return
	}
	payload := map[string]any{"system_prompt": systemPrompt}
	if agentDefID != "" {
		payload["agent_def_id"] = agentDefID
	}
	if len(prov.SkillDefIDs) > 0 {
		payload["skill_def_ids"] = prov.SkillDefIDs
	}
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("store: marshal system_prompt event for run %s: %v", runID, err)
		return
	}
	if err := s.store.AppendEvent(ctx, runID, "system_prompt", b); err != nil {
		log.Printf("store: AppendEvent(system_prompt) failed for run %s: %v", runID, err)
	}
}

// historyPolicyForAgent returns the v0.8.7 Context.history scope policy
// for one agent. Default-deny shape: empty Scopes = no access.
func (s *Server) historyPolicyForAgent(agentDef config.AgentDef) tools.HistoryPolicyValue {
	return tools.HistoryPolicyValue{Scopes: agentDef.HistoryScope}
}

// interruptionPolicyForAgent maps the agent's yaml ACL into the
// runtime policy struct the Interruption tool reads via ctx.
// Default-deny: absent block in yaml → Enabled=false → tool returns
// is_error on every op.
func (s *Server) interruptionPolicyForAgent(agentDef config.AgentDef) tools.InterruptionPolicyValue {
	return tools.InterruptionPolicyValue{
		Enabled:    agentDef.Interruption.Enabled,
		Kinds:      agentDef.Interruption.Kinds,
		MaxPending: agentDef.Interruption.MaxPending,
	}
}

// volumePolicyForAgent resolves the agent's declared `volumes` binding
// (RFC AH Phase 1) against the operator's top-level `volumes:` config
// into the run-scoped VolumePolicy the file/exec tools read via ctx.
//
//   - An agent that declares NO volumes is implicitly bound to [default]
//     — a single synthesized binding from the `default` config volume.
//     When no `default` volume exists (no `volumes:` config), the policy is
//     INACTIVE so every file/exec tool refuses — sandbox-by-default, RFC AH
//     Phase 3 (the legacy jail is gone; declare a `default` volume to grant
//     disk access).
//   - An agent that declares volumes is confined to EXACTLY those (it
//     does NOT implicitly also get `default`). Each binding's Default
//     flag carries through from the config volume's `default: true`, so
//     an omitted `volume` arg resolves to that one (or the sole binding).
//
// Config-load validation guarantees every STATICALLY-declared name exists
// and every static volume path is an existing directory resolved absolute.
// A bound name NOT in cfg.Volumes is resolved as a dynamic VolumeDef
// (RFC AH Phase 2a) via lookup.VolumeDef within the run's authoritative
// tenant (from ctx's RunIdentity) — config-validation deliberately does NOT
// reject an unknown bound name when dynamic volumes are in play, so a miss
// here means "no static AND no dynamic row" → the binding is dropped (the
// agent simply isn't confined to a volume that doesn't exist).
func (s *Server) volumePolicyForAgent(ctx context.Context, agentDef config.AgentDef) tools.VolumePolicyValue {
	if len(agentDef.Volumes) == 0 {
		def, ok := s.cfg.Volumes["default"]
		if !ok {
			// No explicit `default` volume → inactive policy. RFC AH Phase 3:
			// the legacy jail is gone, so an inactive policy means DENY — every
			// file/exec tool refuses (sandbox-by-default). Declare a `default`
			// volume to grant the old single-jail behaviour.
			return tools.VolumePolicyValue{}
		}
		return tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{
			Name: "default", Root: def.Path, ReadOnly: def.ReadOnly(), Default: true,
		}}}
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	bindings := make([]tools.VolumeBinding, 0, len(agentDef.Volumes))
	for _, name := range agentDef.Volumes {
		if v, ok := s.cfg.Volumes[name]; ok {
			bindings = append(bindings, tools.VolumeBinding{
				Name: name, Root: v.Path, ReadOnly: v.ReadOnly(), Default: v.Default,
			})
			continue
		}
		// Not a static volume → resolve a dynamic VolumeDef. lookup.VolumeDef
		// puts static FIRST (the line above already covered it), then
		// tenant-dynamic, then shared-dynamic. Default flag stays false — a
		// dynamic volume is never the implicit default (the agent picks it by
		// name, or designates a static `default: true` as its default).
		if s.store != nil {
			if spec, ok := lookup.VolumeDef(ctx, s.cfg, s.store, tenantID, name); ok {
				bindings = append(bindings, tools.VolumeBinding{
					Name: name, Root: spec.Path, ReadOnly: spec.Mode == "ro", Default: false,
				})
			}
		}
		// A name that is neither static nor dynamic is dropped defensively.
	}
	return tools.VolumePolicyValue{Active: true, Bindings: bindings}
}

// narrowVolumes computes a sub-agent's volume binding set as the
// NARROW-ONLY intersection (RFC AH §4): child set = (child-declared) ∩
// (parent's active bindings). A child can NEVER gain a volume the parent
// lacks; where both hold a volume, ReadOnly resolves to the MORE
// restrictive of the two (ro wins). This mirrors NarrowHosts exactly —
// the parent's policy is the floor, the child can only shrink it.
//
// The child's Default flag is preserved from the child's own declaration
// (the parent might mark a different volume as its default), so an
// omitted `volume` in the child resolves against the child's own designated
// default among the surviving intersection.
//
// Called ONLY with an ACTIVE parent (see childVolumePolicy). The result is
// ALWAYS active: an empty intersection means the child shares none of the
// parent's volumes and is confined to NOTHING (every file-tool call refused),
// NOT dropped back to the legacy jail.
func narrowVolumes(parent, child tools.VolumePolicyValue) tools.VolumePolicyValue {
	parentByName := make(map[string]tools.VolumeBinding, len(parent.Bindings))
	for _, b := range parent.Bindings {
		parentByName[b.Name] = b
	}
	out := make([]tools.VolumeBinding, 0, len(child.Bindings))
	for _, cb := range child.Bindings {
		pb, ok := parentByName[cb.Name]
		if !ok {
			continue // child named a volume the parent lacks — drop it.
		}
		// More-restrictive-wins on the ro/rw axis: ro if EITHER side is ro.
		out = append(out, tools.VolumeBinding{
			Name:     cb.Name,
			Root:     pb.Root, // parent's resolved root is authoritative.
			ReadOnly: cb.ReadOnly || pb.ReadOnly,
			Default:  cb.Default,
		})
	}
	return tools.VolumePolicyValue{Active: true, Bindings: out}
}

// childVolumePolicy resolves a sub-agent's volume policy from the parent's
// run policy + the child's AgentDef (RFC AH §4). Three cases:
//
//   - Parent NOT confined by volumes (inactive — legacy/no `default`): the
//     child is resolved as if top-level (its own declared volumes, or the
//     operator `default`, or the inactive legacy fallback). There is no
//     parent volume scope to narrow against, and the child's volumes come
//     from its own operator-authored AgentDef, not from the parent.
//   - Unbound child of a confined parent: INHERIT the parent's policy
//     verbatim — the helper works within the parent's scope, exactly as a
//     sub-agent inherits the parent's host allowlist.
//   - Bound child of a confined parent: NARROW — child-declared ∩ parent
//     (narrow-only; a child can never reach a volume the parent lacks).
func (s *Server) childVolumePolicy(ctx context.Context, parentVol tools.VolumePolicyValue, def config.AgentDef) tools.VolumePolicyValue {
	if !parentVol.Active {
		return s.volumePolicyForAgent(ctx, def)
	}
	if len(def.Volumes) == 0 {
		return parentVol
	}
	return narrowVolumes(parentVol, s.volumePolicyForAgent(ctx, def))
}

// applyAgentDefOverlay overlays the v0.8.5 agent_defs.definition JSON
// onto a static cfg.Agents entry, producing the effective AgentDef
// for one sub-run. Mirrors the mutable-subset list maintained by the
// AgentDef tool's `mergedDef`: provider, model, tier, effort,
// max_tokens, max_iterations, system_prompt, allowed_tools, skills,
// providers, models, memory_scopes, memory_quota_bytes. Substrate policy fields
// (agent_def_scopes, evaluation_scopes) are NOT overlaid — they stay
// with the static yaml so the operator's substrate-capability gate
// can't be widened by a fork. AllowedTools narrowing is enforced at
// AgentDef.create/fork time (the tool refuses widening); here we
// simply trust the persisted row.
//
// Malformed JSON returns the base unchanged — the AgentDef tool's
// schema ensures rows are well-formed, so this is defensive only.
func applyAgentDefOverlay(base config.AgentDef, definition json.RawMessage) config.AgentDef {
	if len(definition) == 0 {
		return base
	}
	// Decode into a partial AgentDef-shaped struct. Using
	// config.AgentDef directly would pull substrate policy fields
	// into the merge by accident; an inline anonymous struct keeps
	// the overlay surface explicit.
	var ov struct {
		Provider            string                            `json:"provider,omitempty"`
		Model               string                            `json:"model,omitempty"`
		Tier                string                            `json:"tier,omitempty"`
		Effort              string                            `json:"effort,omitempty"`
		MaxTokens           int                               `json:"max_tokens,omitempty"`
		MaxIterations       int                               `json:"max_iterations,omitempty"`
		UnboundedIterations bool                              `json:"unbounded_iterations,omitempty"`
		SystemPrompt        string                            `json:"system_prompt,omitempty"`
		AllowedTools        []string                          `json:"allowed_tools,omitempty"`
		Skills              []string                          `json:"skills,omitempty"`
		Providers           []string                          `json:"providers,omitempty"`
		Models              map[string][]config.TierCandidate `json:"models,omitempty"`
		MemoryScopes        []string                          `json:"memory_scopes,omitempty"`
		MemoryQuotaBytes    int                               `json:"memory_quota_bytes,omitempty"`
		MemoryBackend       string                            `json:"memory_backend,omitempty"`
		// *int because 0 is a meaningful explicit value ("force no
		// retries"); non-pointer would collapse "not in overlay" and
		// "explicitly disable" into the same case and silently strip
		// the static yaml's high-stakes intent on a substrate sub-run.
		RetryAttempts *int `json:"retry_attempts,omitempty"`
	}
	if err := json.Unmarshal(definition, &ov); err != nil {
		log.Printf("agent_def overlay: malformed definition JSON, falling back to static: %v", err)
		return base
	}
	out := base
	if ov.Provider != "" {
		out.Provider = ov.Provider
	}
	// Pin XOR Tier defensive resolution. AgentDef.create/fork rejects
	// rows that set both, but a row written via direct SQL or migrated
	// from a future schema variant could carry both. When both are set
	// in the overlay, prefer Model (the more specific intent) and
	// drop Tier — matches what the resolver does when given a pin.
	switch {
	case ov.Model != "" && ov.Tier != "":
		out.Model = ov.Model
		out.Tier = ""
	case ov.Model != "":
		out.Model = ov.Model
		// Explicit model pin clears any static tier so the resolver
		// takes the pin path, not the tier path.
		out.Tier = ""
	case ov.Tier != "":
		out.Tier = ov.Tier
		// Mirror image: explicit tier clears any static model pin.
		out.Model = ""
	}
	if ov.Effort != "" {
		out.Effort = ov.Effort
	}
	if ov.MaxTokens != 0 {
		out.MaxTokens = ov.MaxTokens
	}
	if ov.MaxIterations != 0 {
		out.MaxIterations = ov.MaxIterations
	}
	if ov.UnboundedIterations {
		out.UnboundedIterations = true
	}
	if ov.SystemPrompt != "" {
		out.SystemPrompt = ov.SystemPrompt
	}
	if ov.AllowedTools != nil {
		out.AllowedTools = ov.AllowedTools
	}
	if ov.Skills != nil {
		out.Skills = ov.Skills
	}
	if ov.Providers != nil {
		out.Providers = ov.Providers
	}
	if ov.Models != nil {
		out.Models = ov.Models
	}
	if ov.MemoryScopes != nil {
		out.MemoryScopes = ov.MemoryScopes
	}
	if ov.MemoryQuotaBytes != 0 {
		out.MemoryQuotaBytes = ov.MemoryQuotaBytes
	}
	if ov.MemoryBackend != "" {
		out.MemoryBackend = ov.MemoryBackend
	}
	if ov.RetryAttempts != nil {
		// Pointer-set means the substrate row carries an explicit
		// override (including 0). Nil leaves the static yaml's
		// RetryAttempts in place — including the static yaml's own
		// "force 0" for high-stakes agents.
		out.RetryAttempts = ov.RetryAttempts
	}
	return out
}

// mergedChannelDefs returns the operator-declared channel defs by name —
// static yaml (cfg.Channels) first, then runtime-substrate rows from the
// store (yaml wins on a name collision, matching ListChannels' precedence).
// includeRuntime gates the store query: callers needing only the static set
// pass false to skip the read.
//
// F29: a channel declared at runtime (POST /v1/_channels, persisted in the
// `channels` table) must be usable for pub/sub exactly like a yaml channel,
// so the runtime store is merged in here.
//
// Single source of truth for a channel's declared scope: both
// channelPolicyForAgent (the per-agent Channel tool policy) and
// ResolveChannelScope (the scheduler on_complete publish path, F37) build on
// it so static + runtime channels resolve identically everywhere.
func (s *Server) mergedChannelDefs(ctx context.Context, includeRuntime bool) map[string]tools.ChannelDef {
	channels := make(map[string]tools.ChannelDef, len(s.cfg.Channels))
	for name, ch := range s.cfg.Channels {
		channels[name] = tools.ChannelDef{
			Name:        name,
			Scope:       ch.Scope,
			DefaultTTL:  ch.DefaultTTL,
			MaxMessages: ch.MaxMessages,
			Semantic:    ch.Semantic,
			Publisher:   ch.Publisher, // v0.8.6: agent publish refusal when "system"
		}
	}
	if includeRuntime && s.store != nil {
		if rows, err := s.store.ChannelsList(ctx); err == nil {
			for _, r := range rows {
				if _, exists := channels[r.Name]; exists {
					continue // yaml takes precedence on a name collision
				}
				channels[r.Name] = tools.ChannelDef{
					Name:        r.Name,
					Scope:       r.Scope,
					DefaultTTL:  r.DefaultTTL,
					MaxMessages: r.MaxMessages,
					Semantic:    r.Semantic,
					Publisher:   r.Publisher,
				}
			}
		}
	}
	return channels
}

// channelPolicyForAgent builds the v0.8.4 Channel-tool policy from the agent
// yaml + the top-level `channels:` block AND the runtime channel store.
// Returns a value suitable for tools.WithChannelPolicy. The Channels map is a
// copy of every declared channel — the tool layer needs the per-channel
// scope/TTL/max_messages even for channels NOT in this agent's allowlist
// (e.g. to phrase a useful refusal message). The runtime store read is
// skipped when the agent has no channel allowlist at all (it can't touch a
// channel either way), so the common no-channels run pays nothing.
func (s *Server) channelPolicyForAgent(ctx context.Context, agentDef config.AgentDef) tools.ChannelPolicyValue {
	includeRuntime := len(agentDef.Channels.Publish) > 0 || len(agentDef.Channels.Subscribe) > 0
	return tools.ChannelPolicyValue{
		Publish:   agentDef.Channels.Publish,
		Subscribe: agentDef.Channels.Subscribe,
		Channels:  s.mergedChannelDefs(ctx, includeRuntime),
	}
}

// ResolveChannelScope returns the declared scope ("global" | "user" |
// "agent") of a channel by name, consulting static yaml + runtime substrate
// (the same merge the Channel tool uses). ok=false when the channel is
// declared nowhere.
//
// Injected into the scheduler so its on_complete: channel.publish hook lands
// at the channel's DECLARED scope — a hook publishing to a scope:global
// channel writes under global, not under the run's user scope where a global
// reader (admin peek, Channel.await/subscribe resolving global) can't see it
// (F37 / RFC T).
func (s *Server) ResolveChannelScope(ctx context.Context, channel string) (string, bool) {
	def, ok := s.mergedChannelDefs(ctx, true)[channel]
	if !ok {
		return "", false
	}
	return def.Scope, true
}

// fallbackForRun builds the v0.8.2 PR-2 runtime-fallback policy +
// re-resolve closure for one run. Returns zero/nil when the
// operator has no user_tiers configured OR the resolved tier has
// fallback_on_error=false (free-tier cost-cap semantics).
//
// The closure captures agentName + userTier so the loop can call
// it on a retryable error mid-run. It walks the same path as the
// initial resolveAgent: mark the failed (provider, model) stalled
// in the matrix, ask the resolver for the next candidate, look up
// the corresponding driver.
//
// On no-more-candidates the closure returns an error — the loop
// then surfaces the ORIGINAL provider error to the caller (the
// fallback path is terminal for this run).
func (s *Server) fallbackForRun(tenantID, agentName, userTier string) (loop.FallbackPolicy, func(ctx context.Context, failedProvider, failedModel string, cause error) (providers.Provider, string, string, error)) {
	overlay := s.userTierOverlay(userTier)
	if overlay == nil || !overlay.FallbackOnError {
		return loop.FallbackPolicy{}, nil
	}
	policy := loop.FallbackPolicy{
		Enabled:         true,
		MaxAttempts:     overlay.MaxFallbackAttempts, // 0 = loop uses its own default (3)
		UserTierName:    overlay.Name,
		PinAfterSuccess: s.cfg.Env.FallbackPinAfterSuccess,
	}
	reResolve := func(ctx context.Context, failedProvider, failedModel string, cause error) (providers.Provider, string, string, error) {
		// Mark the failed pair stalled BEFORE re-resolving so the
		// resolver's matrix walks past it. SetReachable=false on
		// the model only, not the whole provider — other models on
		// the same provider may still be valid.
		if s.resolver != nil {
			s.resolver.MarkStalled(failedProvider, failedModel, cause.Error())
		}
		// Re-resolve with the same agent + user_tier at the run's tenant
		// (RFC N — captured tenantID, not a ctx-derived/empty tenant). The
		// resolver's stall flag we just set excludes the failed pair; the
		// next non-stalled candidate in the user_tier's priority is what
		// we get back.
		newProviderID, newModel, newEffort, err := s.resolveAgent(ctx, tenantID, agentName, userTier)
		if err != nil {
			return nil, "", "", err
		}
		newProvider, err := s.providers.Get(newProviderID)
		if err != nil {
			return nil, "", "", err
		}
		return newProvider, newModel, newEffort, nil
	}
	return policy, reResolve
}

// convertConfigCandidates translates the config-package representation
// of per-agent tier candidates into the resolver-package representation.
// Keeping the resolver package free of internal/config imports avoids
// circularity (resolver is consumed by the HTTP server, which already
// depends on config).
func convertConfigCandidates(in map[string][]config.TierCandidate, models map[string]config.ModelRef) map[string][]resolve.Candidate {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]resolve.Candidate, len(in))
	for tier, cands := range in {
		conv := make([]resolve.Candidate, 0, len(cands))
		for _, c := range cands {
			// Expand model aliases (top-level models:) the same way the
			// pin path does — so model: <alias> works in a tier candidate
			// too, not only as a pin. The resolver matches literal model
			// strings, so expansion must happen here at the boundary.
			prov, mdl := config.ExpandModelAlias(models, c.Provider, c.Model)
			conv = append(conv, resolve.Candidate{Provider: prov, Model: mdl})
		}
		out[tier] = conv
	}
	return out
}

// RunOnce is the wire-agnostic entry point for /v1/runs and
// /v1/sessions/{id}/messages. The HTTP handlers (handleRuns,
// handleMessages) and the gRPC handlers (Run, Continue) translate
// their own request shape into a runner.RunInput and call here.
//
// The function blocks until the loop terminates. Sentinel errors
// (runner.ErrFoo) come back so each wire surface can map to its own
// status codes (HTTP 4xx/5xx vs gRPC codes).
//
// Implementation note: this duplicates the body of handleRuns +
// handleMessages today (both still implement their own logic
// inline). PR-3-of-v0.5.5-followup will refactor those two handlers
// to call RunOnce. The duplication exists because folding it into
// the same change as gRPC's Run/Continue would touch ~500 LOC of
// HTTP code in a PR whose primary purpose is gRPC streaming;
// keeping them separate makes both PRs reviewable.
func (s *Server) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	// ---- Validation phase ----
	if in.UserID != "" && !validIdent(in.UserID) {
		return fmt.Errorf("%w: user_id must match [A-Za-z0-9_-]{1,128}", runner.ErrInvalidArgument)
	}
	if in.AgentID != "" && !validIdent(in.AgentID) {
		return fmt.Errorf("%w: agent_id must match [A-Za-z0-9_-]{1,128}", runner.ErrInvalidArgument)
	}

	// ---- Session resolution (continuation only) ----
	isContinuation := in.SessionID != ""
	effectiveAgentName := in.Agent
	effectiveTenantID := in.TenantID
	effectiveUserID := in.UserID
	// RFC L: on an authenticated entry (gRPC interceptor stamps the
	// principal), the principal is authoritative over the wire fields
	// (Decision 5). Fresh runs only — a continuation inherits the
	// session's identity, set authoritatively when the session was
	// created. Un-authed programmatic callers (scheduler / webhook /
	// A2A) carry no principal and keep their Def-supplied identity.
	if !isContinuation {
		effectiveTenantID, effectiveUserID = s.applyPrincipal(ctx, in.TenantID, in.UserID)
	}
	var priorMessages []providers.Message

	if isContinuation {
		if s.store == nil {
			return runner.ErrSessionRequired
		}
		sess, err := s.store.GetSession(ctx, in.SessionID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return fmt.Errorf("%w: %s", runner.ErrSessionNotFound, in.SessionID)
			}
			return fmt.Errorf("%w: %v", runner.ErrInternal, err)
		}
		// RFC L: cross-principal session-ownership guard (see
		// sessionOwnershipOK). Opaque not-found on mismatch, no oracle.
		if !sessionOwnershipOK(ctx, sess) {
			return fmt.Errorf("%w: %s", runner.ErrSessionNotFound, in.SessionID)
		}
		effectiveAgentName = sess.Agent
		effectiveTenantID = sess.TenantID
		effectiveUserID = sess.UserID

		releaseLock, ok := s.sessionLocks.TryLock(in.SessionID)
		if !ok {
			return fmt.Errorf("%w: another request is in flight on session %q", runner.ErrSessionBusy, in.SessionID)
		}
		defer releaseLock()
	}

	if effectiveAgentName == "" {
		return fmt.Errorf("%w: agent is required", runner.ErrInvalidArgument)
	}
	// RFC N: resolve the ENTRY agent at the run's authoritative tenant
	// (effectiveTenantID), NOT tenantFromCtx(ctx) — WithRunIdentity isn't
	// stamped until ~line 1500, so for non-HTTP-principal spawn surfaces
	// the ctx tenant is wrong here.
	agentDef, ok := s.lookupAgent(ctx, effectiveTenantID, effectiveAgentName)
	if !ok {
		return fmt.Errorf("%w: %s", runner.ErrUnknownAgent, effectiveAgentName)
	}
	// resolveAgent only consults s.cfg.Agents; dynamic agents loaded
	// from the v0.8.15 fallback above are NOT in that map. Pass the
	// already-resolved AgentDef through resolveAgentDef instead (same
	// path the v0.8.5 sub-agent overlay uses).
	providerID, model, effort, err := s.resolveAgentDef(agentDef, effectiveAgentName, in.UserTier)
	if err != nil {
		return err // already wrapped with runner.Err* sentinel
	}
	provider, err := s.providers.Get(providerID)
	if err != nil {
		return fmt.Errorf("%w: %v", runner.ErrUnknownProvider, err)
	}

	// ---- Transcript replay (continuation only) ----
	if isContinuation {
		transcript, err := s.store.GetTranscript(ctx, in.SessionID)
		if err != nil {
			return fmt.Errorf("%w: %v", runner.ErrInternal, err)
		}
		priorMessages = replayTranscript(transcript)
	}

	// Runtime-pause admission gate (RFC X / F41) — reject new runs while
	// pausing/paused. Covers the gRPC / webhook / A2A / scheduler surfaces
	// that drive runs through RunOnce. Checked before the concurrency slot.
	if err := s.pausedRunErr(); err != nil {
		return err
	}

	// ---- Concurrency slot ----
	acquireStart := time.Now()
	release, err := s.sem.AcquireForUser(ctx, effectiveUserID)
	queueWait := time.Since(acquireStart)
	if err != nil {
		if concurrency.IsPerUserQuotaExhausted(err) {
			return fmt.Errorf("%w: %v", runner.ErrPerUserQuotaExhausted, err)
		}
		if concurrency.IsBackpressure(err) {
			return fmt.Errorf("%w: %v", runner.ErrBackpressure, err)
		}
		return fmt.Errorf("%w: %v", runner.ErrInternal, err)
	}
	defer release()

	// ---- Tool filtering + host narrowing ----
	// RFC N FIX 2-mcp: advertise MCP tools at the run's authoritative
	// tenant (effectiveTenantID), not the ctx tenant — WithRunIdentity
	// isn't stamped yet, so for non-HTTP-principal spawn surfaces the ctx
	// tenant is "" and the run would not see its OWN dynamic MCP tools.
	allowedTools := filterTools(s.candidateTools(ctx, effectiveTenantID, agentDef.AllowedTools), agentDef.AllowedTools, in.AllowedTools)
	var hostPolicy tools.HostPolicyValue
	if in.AllowedHosts != nil || s.cfg.Env.HTTPCallerAuthoritative {
		var caller []string
		if in.AllowedHosts != nil {
			caller = *in.AllowedHosts
		}
		allowedTools = builtin.NarrowHosts(allowedTools, caller, in.WebSearchFilter, s.cfg.Env.HTTPCallerAuthoritative)
		hostPolicy = tools.HostPolicyValue{
			AllowedHosts:    caller,
			HasList:         in.AllowedHosts != nil,
			WebSearchFilter: in.WebSearchFilter,
		}
	}
	dispatcher := s.newDispatcher(allowedTools)

	// ---- Segments: prepend agent's system prompt ----
	segments := in.Segments
	// v0.8.22: rebuild SystemPrompt from per-run SkillDef bodies
	// when any of the agent's skills has a DB-active row. No-op
	// fast path when none do.
	// RFC N: resolve skills at the run's authoritative tenant
	// (effectiveTenantID), NOT tenantFromCtx(ctx) — WithRunIdentity isn't
	// stamped until below, so for non-HTTP-principal spawn surfaces the
	// ctx tenant is wrong here. Mirrors the lookupAgent fix above.
	agentDef, promptProv := s.resolveSkillBodiesForRun(ctx, effectiveTenantID, agentDef)
	if agentDef.SystemPrompt != "" {
		segments = append([]loop.PromptSegment{{
			Role: "system",
			Content: []loop.PromptContentBlock{{
				Type: "trusted-text", Text: agentDef.SystemPrompt, Cacheable: true,
			}},
		}}, segments...)
	}

	// ---- agent_id: caller-supplied or generated ----
	agentID := in.AgentID
	if agentID == "" {
		agentID = newAgentID()
	}

	// ---- Session+run creation ----
	identity := store.RunIdentity{AgentID: agentID, UserID: effectiveUserID, TenantID: effectiveTenantID, UserTier: in.UserTier, Model: model, ReplicaID: s.replicaID, ParentContext: in.ParentContext, IdempotencyKey: in.IdempotencyKey, Interactive: in.Interactive}
	sessionID, runID, sessErr := s.openOrCreateSessionAndRun(ctx, in.SessionID, effectiveAgentName, effectiveTenantID, effectiveUserID, identity)
	if sessErr != nil {
		var nf *store.ErrNotFound
		if errors.As(sessErr, &nf) {
			return fmt.Errorf("%w: %v", runner.ErrSessionNotFound, sessErr)
		}
		// RFC H Decision 10: a duplicate idempotency_key means an earlier
		// run already claimed this key. CRITICAL: return BEFORE the cancel
		// registry + agent loop so the run never double-executes. Wrap the
		// sentinel verbatim (no ErrInternal masking) so the webhook
		// receiver can errors.Is-detect it and resolve to the existing
		// run. (CreateSession ran before the failed CreateRun, leaving an
		// orphan session with no run — acceptable: it carries no events
		// and is never returned to a caller.)
		if errors.Is(sessErr, store.ErrDuplicateIdempotencyKey) {
			return sessErr
		}
		return fmt.Errorf("%w: %v", runner.ErrInternal, sessErr)
	}

	// ---- Cancel registry ----
	runCtx, cancelFn := context.WithCancelCause(ctx)
	defer cancelFn(nil)
	// v0.10.0 OTEL: top-level loomcycle.run span covers the entire
	// run. Loop iterations + provider calls + tool dispatch nest
	// under it via context propagation. Span name + attribute set
	// stable across all 4 run-creation sites (RunOnce, handleRuns,
	// handleMessages, runSubAgent).
	runCtx, runSpan := lcotel.RecordRunStart(runCtx, lcotel.RunStartAttrs{
		RunID:     runID,
		AgentID:   agentID,
		AgentName: effectiveAgentName,
		UserID:    effectiveUserID,
	})
	defer runSpan.End()
	// v0.10.1: surface the semaphore queue wait on the run span. 0
	// means immediate acquire; a sustained non-zero distribution per
	// user_id is the operator's signal that fairness is engaging.
	lcotel.RecordQueueWait(runSpan, queueWait)
	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:   agentID,
		RunID:     runID,
		SessionID: sessionID,
		UserID:    effectiveUserID,
		StartedAt: time.Now(),
	}, cancelFn)
	// v0.9.x n8n RFC Phase 0: identity bundle for runstate.Bus
	// publishes. Constructed once; passed to every finishRun* below
	// AND used right now for the "running" transition.
	meta := runStateMeta{
		RunID:         runID,
		AgentID:       agentID,
		Agent:         effectiveAgentName,
		UserID:        effectiveUserID,
		TenantID:      effectiveTenantID,
		ParentContext: in.ParentContext,
		// RFC AH Phase 2b: this is a TOP-LEVEL run — it owns the ephemeral
		// volume tree purge at completion. RootRunID is its own id.
		IsTopLevel: true,
		RootRunID:  runID,
	}
	// Stash the run span on the meta so finishRun* can close it with
	// final attrs (usage totals + stop_reason + error status).
	meta.otelSpan = runSpan
	if errors.Is(regErr, cancel.ErrInUse) {
		s.finishRunFailedReason(runID, "agent_id collision; run never started", meta)
		return fmt.Errorf("%w: agent_id %q is already mapped to an active run", runner.ErrAgentIDInUse, agentID)
	}
	if regErr != nil {
		s.finishRunFailedReason(runID, "registry register failed: "+regErr.Error(), meta)
		return fmt.Errorf("%w: %v", runner.ErrInternal, regErr)
	}
	defer s.cancelReg.Deregister(agentID)
	s.publishRunState(meta, "running", "", "")

	// ---- Persist input segments ----
	if s.store != nil && runID != "" {
		if inputJSON, err := json.Marshal(in.Segments); err == nil {
			if err := s.store.AppendEvent(ctx, runID, "user_input", inputJSON); err != nil {
				log.Printf("store: AppendEvent(user_input) failed: %v", err)
			}
		}
	}
	// v0.9.x: persist the resolved system prompt + provenance so the
	// transcript carries WHAT the agent received, not just WHAT the
	// model emitted. The companion Web UI rendering surfaces this as
	// the first card on /ui run views. agent_def_id is empty here —
	// RunInput doesn't pin a def, so the field stays unset (operators
	// inspecting can look up the run row's AgentDefID column directly).
	s.emitSystemPromptEvent(ctx, runID, agentDef.SystemPrompt, "", promptProv)

	// ---- Caller registration callback ----
	if cb.OnRegistered != nil {
		cb.OnRegistered(agentID, runID, sessionID, "")
	}

	// ---- Build emit chain (record + forward to caller) ----
	emit := s.makeRecordingEmit(ctx, runID, func(ev providers.Event) {
		if cb.OnEvent != nil {
			cb.OnEvent(ev)
		}
	})

	// PR 2: operator steering queue for this run (in-flight input injection).
	steerQ, onSteer, deregSteer := s.makeSteer(ctx, runID, agentID, sessionID, effectiveUserID, emit)
	defer deregSteer()

	loopCtx := tools.WithAgentTools(runCtx, toolNames(allowedTools))
	loopCtx = tools.WithRunIdentity(loopCtx, tools.RunIdentityValue{
		UserID:          effectiveUserID,
		TenantID:        effectiveTenantID, // RFC L: authoritative tenant (memory tenancy key)
		AgentID:         agentID,
		RootRunID:       runID, // RFC AH Phase 2b: top-level run roots its own spawn tree
		UserTier:        in.UserTier,
		UserBearer:      in.UserBearer,      // v0.8.x: per-run MCP bearer
		UserCredentials: in.UserCredentials, // v1.x RFC F: per-tool named credentials
		ParentContext:   in.ParentContext,   // v0.12.x: opaque tracking lineage, inherited by sub-agents
	})
	loopCtx = tools.WithHostPolicy(loopCtx, hostPolicy)
	// Memory tool policy: agent name + per-agent scope allowlist +
	// per-agent quota override. The Memory tool reads these from ctx
	// at call time to refuse out-of-policy operations and resolve
	// scope_id (yaml agent name for `agent` scope, user_id for
	// `user` scope).
	loopCtx = tools.WithAgentName(loopCtx, effectiveAgentName)
	loopCtx = tools.WithMemoryPolicy(loopCtx, tools.MemoryPolicyValue{
		AllowedScopes: agentDef.MemoryScopes,
		QuotaBytes:    agentDef.MemoryQuotaBytes,
		Backend:       agentDef.MemoryBackend,
	})
	// RFC AA: the agent's SQL Memory ACL. Empty sql_scopes → default-deny.
	loopCtx = tools.WithSqlMemPolicy(loopCtx, tools.SqlMemPolicyValue{
		AllowedScopes: agentDef.SqlScopes,
		QuotaBytes:    agentDef.SqlQuotaBytes,
	})
	// Resolved compaction settings flow down the spawn tree via ctx: a sub-agent
	// inherits the parent's effective policy (its def fills any gaps the parent
	// left unset), overridable per-spawn by the Agent tool.
	loopCtx = tools.WithCompactionPolicy(loopCtx, config.MergeCompaction(agentDef.Compaction, in.Compaction))
	// RFC AH: the run's filesystem-volume bindings. Unbound agents get an
	// empty policy (the file tools fall back to the legacy jail Root);
	// sub-agents inherit + narrow this via runSubAgent.
	loopCtx = tools.WithVolumePolicy(loopCtx, s.volumePolicyForAgent(loopCtx, agentDef))
	// RFC AH Phase 2b: a FRESH run-scoped ephemeral volume set, attached ONCE
	// at this top-level start. Sub-agents inherit this SAME pointer via the
	// ctx chain (runSubAgent must not re-attach it) — never shared across
	// top-level runs (the load-bearing isolation property).
	loopCtx = tools.WithEphemeralVolumes(loopCtx, tools.NewEphemeralVolumeSet())
	loopCtx = tools.WithChannelPolicy(loopCtx, s.channelPolicyForAgent(loopCtx, agentDef))
	loopCtx = tools.WithEventEmitter(loopCtx, emit)
	adPolicy, evPolicy := s.substratePoliciesForAgent(agentDef, effectiveAgentName)
	loopCtx = tools.WithAgentDefPolicy(loopCtx, adPolicy)
	loopCtx = tools.WithSkillDefPolicy(loopCtx, s.skillDefPolicyForAgent(agentDef))
	loopCtx = tools.WithVolumeDefPolicy(loopCtx, s.volumeDefPolicyForAgent(agentDef))
	loopCtx = tools.WithEvaluationPolicy(loopCtx, evPolicy)
	loopCtx = tools.WithHistoryPolicy(loopCtx, s.historyPolicyForAgent(agentDef))
	loopCtx = tools.WithInterruptionPolicy(loopCtx, s.interruptionPolicyForAgent(agentDef))
	loopCtx = tools.WithRunID(loopCtx, runID)
	loopCtx = tools.WithDispatcher(loopCtx, dispatcher)

	heartbeat := s.makeHeartbeat(runID)

	// Cooperative pause quiesce (RFC X / F41): park at an iteration boundary
	// while paused. RunOnce is synchronous, so a plain defer-deregister works.
	gate, deregGate := s.newPauseGate(runID)
	defer deregGate()
	// RFC X Phase 3: expose the gate to the Agent tool so a parallel_spawn
	// fan-out parent can park mid-tool-call (no-op unless LOOMCYCLE_RESUME_FANOUT).
	loopCtx = tools.WithPauseGate(loopCtx, gate)

	fbPolicy, fbReResolve := s.fallbackForRun(effectiveTenantID, effectiveAgentName, in.UserTier)
	res, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:               provider,
		Model:                  model,
		Tools:                  allowedTools,
		Dispatcher:             dispatcher,
		Segments:               injectMetadataSegments(segments, provider.Capabilities().MetadataViaInput, in.Metadata, in.PayloadMetadata),
		PriorMessages:          priorMessages,
		PauseGate:              gate,
		OnEvent:                emit,
		OnHeartbeat:            heartbeat,
		MaxTokens:              agentDef.MaxTokens,
		MaxIterations:          agentDef.MaxIterations, // 0 → loop default (16)
		UnboundedIterations:    agentDef.UnboundedIterations,
		SteerQueue:             steerQ,
		OnSteer:                onSteer,
		Effort:                 effort,
		MarkStalled:            s.markStalledFn(providerID, model),
		MarkRateLimited:        s.markRateLimitedFn(in.UserTier),
		ClearStall:             s.clearStallFn(providerID, model),
		ToolParallelism:        s.cfg.Env.ToolParallelism,
		AgentName:              effectiveAgentName,
		CodeBody:               agentDef.Code, // inline code-js body (RFC J); "" → FS fallback
		Metadata:               in.Metadata,
		PayloadMetadata:        in.PayloadMetadata,
		RunTimeoutSeconds:      pickRunTimeout(in.RunTimeoutSeconds, agentDef.RunTimeoutSeconds),
		Interactive:            in.Interactive,
		Sampling:               config.MergeSampling(agentDef.Sampling, in.Sampling),       // per-run wins per field
		Compaction:             config.MergeCompaction(agentDef.Compaction, in.Compaction), // per-run wins per field
		ContextPlugins:         s.contextPlugins,                                           // RFC Z runtime-wide chain (code-js exempt in the loop)
		UserTier:               in.UserTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForAgent(agentDef, in.UserTier),
	})
	s.finishRunWithCancel(ctx, runCtx, runID, res, runErr, meta)
	return nil
}

// Compile-time guard: *Server must satisfy runner.Runner so the
// gRPC wire surface (which depends on the interface) can be wired
// without a separate adapter type.
var _ runner.Runner = (*Server)(nil)

// CancelRegistry exposes the in-memory registry so a parallel API
// surface (the gRPC server in internal/api/grpc) can answer cancel /
// status queries against the same state. Both surfaces are
// constructed in cmd/loomcycle/main.go from the same dependencies;
// they share the registry rather than maintaining parallel ones,
// which would let a cancel issued via gRPC silently miss runs that
// originated on the HTTP path (or vice versa).
func (s *Server) CancelRegistry() *cancel.Registry { return s.cancelReg }

// SteerRegistry exposes the operator-steering registry so main.go can wire the
// cross-replica SteerCoordinator (SetClusterSteerer + the subscriber
// goroutines). nil when steering isn't enabled.
func (s *Server) SteerRegistry() *steer.Registry { return s.steerReg }

// trySessionLock try-locks the session-scoped mutex for id. Returns
// (release, true) on success and (nil, false) if another caller already
// holds it — in which case the caller should respond 409 / session_busy.
// id must be non-empty; an empty id is a programmer error and panics.
//
// Callers MUST validate the session exists in the store before calling
// this — sessionLocks entries are GC'd only when both refcount=0 AND
// idle ≥ maxIdle, but unknown-ID entries would still hang around for
// at least one GC cycle and leak slowly. The DoS guard remains a
// caller obligation.
func (s *Server) trySessionLock(id string) (release func(), ok bool) {
	if id == "" {
		panic("trySessionLock: empty session id")
	}
	// v0.12.5 Phase 6: cluster-mode dispatch. When sessionLockPG is
	// wired (cluster mode), use the Postgres advisory lock so
	// concurrent continuations on the same session_id ACROSS REPLICAS
	// get the same 409 ErrSessionBusy semantics. Single-replica mode:
	// sessionLockPG is nil, the in-process SessionLockMap path runs.
	if s.sessionLockPG != nil {
		return s.sessionLockPG.TryLock(context.Background(), id)
	}
	return s.sessionLocks.TryLock(id)
}

// SetHookRegistry installs the v0.12.5 Phase 6 DB-backed hook
// registry. Called from main.go inside the cluster-mode init block.
// Replaces both the in-process hooks.Registry AND the Dispatcher's
// reference to it so the loop sees the new registry on the next
// tool dispatch.
//
// Invariant: call during boot wiring, BEFORE the server starts serving.
// hookRegistry / hookDispatcher are read unlocked on the request path, so
// safety relies on the happens-before edge from this call to the request
// goroutines ListenAndServe later spawns. It is NOT safe to call concurrently
// with request handling (a hot-reload path would need a guard added here).
func (s *Server) SetHookRegistry(r hooks.RegistryInterface) {
	s.hookRegistry = r
	s.hookDispatcher = hooks.NewDispatcher(r, nil)
}

// SetPgSessionLocker installs the v0.12.5 Phase 6 cluster-wide
// session lock. When non-nil, trySessionLock dispatches to it
// instead of the in-process SessionLockMap.
//
// Invariant: call during boot wiring, BEFORE the server starts serving —
// sessionLockPG is read unlocked in trySessionLock on the request path. Same
// set-before-Serve contract as SetHookRegistry; not safe to call concurrently
// with request handling.
func (s *Server) SetPgSessionLocker(l *runner.PgSessionLocker) {
	s.sessionLockPG = l
}

// lockedSessionCount returns the number of entries in sessionLocks.
// Test-only: used to assert (a) the DoS fix (unknown IDs must not grow
// the table) and (b) the GC reclaims idle entries.
func (s *Server) lockedSessionCount() int {
	return s.sessionLocks.Size()
}

// RunSessionLockGC periodically prunes session-lock entries whose
// refcount is zero AND whose lastAccessed is older than maxIdle. Run
// it on a goroutine that owns the lifecycle (typically alongside the
// HTTP / gRPC servers in cmd/loomcycle/main.go).
//
// interval and maxIdle are operator-configurable; the recommended
// ratio is maxIdle ≥ 2 × interval so a session that's just woken up
// after a quiet period doesn't get its lock yanked mid-acquisition.
func (s *Server) RunSessionLockGC(ctx context.Context, interval, maxIdle time.Duration) {
	if interval <= 0 || maxIdle <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.sessionLocks.GC(maxIdle)
		}
	}
}

// Mux returns the http.Handler ready to be served.
//
// /v1 routes are wrapped with recovery middleware so a panic in the agent
// loop, a tool, or a provider driver returns a 500 to the caller instead
// of taking down the process. /healthz stays bare — it should never panic
// and a panic there is a programmer error worth crashing on.
func (s *Server) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.Handle("POST /v1/runs", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRuns))))
	// RFC Y external fan-out: one call spawns N runs concurrently (mode "join").
	// Literal sibling of /v1/runs — distinct from /v1/runs/{run_id}/* (no slash
	// after "runs"), so it cannot collide with the per-run routes.
	mux.Handle("POST /v1/runs:batch", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRunsBatch))))
	mux.Handle("GET /v1/sessions/{id}/transcript", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleTranscript))))
	mux.Handle("POST /v1/sessions/{id}/messages", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMessages))))
	// v0.4 tracking + cancel API.
	mux.Handle("GET /v1/agents/{agent_id}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleGetAgent))))
	mux.Handle("POST /v1/agents/{agent_id}/cancel", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleCancelAgent))))
	mux.Handle("GET /v1/users/{user_id}/agents", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListUserAgents))))
	// v0.9.x n8n RFC Phase 0: SSE stream of run state transitions
	// scoped to one user_id. Filters via ?status=...&agent=...
	// Bearer-authed. Returns 503 when the runStateBus isn't wired
	// (operator-constructed Server without SetRunStateBus).
	mux.Handle("GET /v1/users/{user_id}/agents/stream", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleStreamUserAgents))))
	// v0.7.x tool-use hook registration API.
	mux.Handle("POST /v1/hooks", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRegisterHook))))
	mux.Handle("GET /v1/hooks", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListHooks))))
	mux.Handle("DELETE /v1/hooks/{id}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleDeleteHook))))
	// v0.7.x resolver introspection — operator-only debug surface.
	mux.Handle("GET /v1/_resolver", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleResolverSnapshot))))
	// Routing view: per user_tier × tier, the resolved provider/model cascade
	// (top → fallbacks). Tenant-readable (config cascade only); admin also gets
	// live availability + the active-providers header. Scope-gated in
	// requiredScopeFor.
	mux.Handle("GET /v1/_routing", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRouting))))
	// Operator-triggered immediate re-probe (issue #88) — unsticks the
	// availability matrix after a transient outage without a restart,
	// instead of waiting up to a full probe interval for the next tick.
	mux.Handle("POST /v1/_resolve/probe", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleResolveProbe))))
	// v0.12.x live provider introspection. Complements /v1/_resolver
	// (cached matrix) by doing a fresh ListModels round-trip — useful
	// after adding a model to the upstream console without waiting
	// 15 min for the next periodic probe.
	mux.Handle("GET /v1/_providers/{id}/models", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleProviderModels))))
	// v0.7.3+ user picker — admin-style endpoint surfacing distinct
	// user_ids that have runs in the store. Bearer-authed; drives
	// the Web UI's run-list user dropdown.
	mux.Handle("GET /v1/_users", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListUsers))))
	// v0.8.6 system channels admin endpoint. Bearer-authed publish to
	// _system/* channels. Used by external monitoring (push alerts in
	// via webhook), ops dashboards (operator-issued alarms), and
	// manual debugging (operator publishes from CLI/curl).
	//
	// {name...} is the multi-segment wildcard (Go's http.ServeMux
	// requires `...` only at the END of the pattern). Handler
	// validates the `_system/` prefix and rejects anything else with
	// 403, so non-system-channel admin publishes are not enabled
	// here. Future verbs (GET to peek, etc.) can use the same path
	// with different methods.
	mux.Handle("POST /v1/_channels/{name...}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSystemChannelPublish))))
	// v0.9.x Channel CRUD — admin (scope=global) + per-user (scope=user)
	// surfaces. Bearer-authed. The trailing /publish|/subscribe|/peek|
	// /ack segment makes these patterns strictly more specific than the
	// system-publish route above so Go 1.22+ mux picks them when both
	// would match. {name} is single-segment; channel names containing
	// slashes (e.g. `findings/alpha`) must URL-encode the slash.
	mux.Handle("POST /v1/_channels/{name}/publish", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleAdminChannelPublish))))
	mux.Handle("POST /v1/_channels/{name}/subscribe", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleAdminChannelSubscribe))))
	mux.Handle("GET /v1/_channels/{name}/peek", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleAdminChannelPeek))))
	mux.Handle("POST /v1/_channels/{name}/ack", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleAdminChannelAck))))
	mux.Handle("POST /v1/_channels/{name}/purge", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleChannelPurge))))
	// RFC S client twins — multi-channel fan-in / fan-out. The reserved
	// `_await` / `_broadcast` literals are strictly more specific than the
	// `{name...}` system-publish catch-all, so Go 1.22+ mux routes them here.
	mux.Handle("POST /v1/_channels/_await", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleAdminChannelAwait))))
	mux.Handle("POST /v1/_channels/_broadcast", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleAdminChannelBroadcast))))
	mux.Handle("POST /v1/users/{user_id}/channels/{name}/publish", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleUserChannelPublish))))
	mux.Handle("POST /v1/users/{user_id}/channels/{name}/subscribe", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleUserChannelSubscribe))))
	mux.Handle("GET /v1/users/{user_id}/channels/{name}/peek", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleUserChannelPeek))))
	mux.Handle("POST /v1/users/{user_id}/channels/{name}/ack", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleUserChannelAck))))
	// v0.9.x n8n RFC Phase 0: list declared channels + aggregate
	// stats (count, oldest/newest visible_at). Used by n8n's
	// credential-picker for the channel-name dropdown; also useful
	// for operator dashboards. Bearer-authed.
	mux.Handle("GET /v1/_channels", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListChannels))))
	// v0.11.5 channel CRUD on the runtime substrate. yaml-declared
	// channels refuse mutations with 409 channel_yaml_immutable. POST
	// /v1/_channels has no name path segment so it doesn't collide
	// with the multi-segment system-publish route above; PATCH +
	// DELETE differ by method from the existing POST /v1/_channels/{name...}.
	mux.Handle("POST /v1/_channels", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleCreateChannel))))
	mux.Handle("PATCH /v1/_channels/{name}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleUpdateChannel))))
	mux.Handle("DELETE /v1/_channels/{name}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleDeleteChannel))))
	// v0.8.x process-resource metrics sampler endpoints. All
	// bearer-authed. Return 503 when metricsSampler is nil
	// (LOOMCYCLE_METRICS_ENABLED=0 deployment).
	mux.Handle("GET /v1/_metrics/samples", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMetricsSamples))))
	mux.Handle("GET /v1/_metrics/runs/{run_id}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMetricsRunSummary))))
	mux.Handle("GET /v1/_metrics/summary", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMetricsSummary))))
	// v1.x Prometheus text-format scrape endpoint. Live-read of runtime
	// + concurrency state — no DB hop, no instrumentation surface.
	// Bearer-authed identically to /v1/_metrics/*. See
	// rfcs/observability-profiles.md Decisions 1 + 3.
	mux.Handle("GET /metrics", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMetricsProm))))
	// v0.10.1 — per-tenant fairness inspection. Bearer-authed snapshot
	// of the global semaphore + per-user counts. Sister of the
	// /v1/_metrics/* family; same auth posture.
	mux.Handle("GET /v1/_concurrency/stats", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleConcurrencyStats))))
	// v0.8.21 audit view — paginated cross-session event log with
	// optional type + date-range filter. Drives the Web UI's
	// /ui/audit page. Bearer-authed admin surface.
	mux.Handle("GET /v1/_events", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListEvents))))
	// v0.8.22 substrate admin endpoints. Bearer-authed; accept the
	// same op-discriminated JSON body as the in-process tool +
	// dispatch through the Connector with operator-trust ctx.
	// Mirrors the MCP `agentdef` / `skilldef` meta-tool dispatch —
	// same connector path, different wire surface.
	mux.Handle("POST /v1/_agentdef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateAgentDef))))
	mux.Handle("POST /v1/_skilldef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateSkillDef))))
	// v0.9.x dynamic MCP server registration. Bearer-authed; operator-
	// admin-only (no per-agent surface). Same dispatch shape as the
	// other two substrate admin endpoints.
	mux.Handle("POST /v1/_mcpserverdef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateMCPServerDef))))
	// RFC AH Phase 2a dynamic-volume substrate. Bearer-authed; tenant-
	// confined (ScopeTenant via isTenantConfinedDefPath) — the tool stamps
	// the caller's authoritative tenant + opaque-404s cross-tenant reads.
	mux.Handle("POST /v1/_volumedef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateVolumeDef))))
	// RFC AL Path VFS + RFC AK Document — the two scope-aware primitives lifted
	// to the wire (Plan 3 / RFC AK Phase 2). Bearer-authed; tenant-confined
	// (ScopeTenant via isTenantConfinedDefPath) — the tools resolve scope from
	// the operator-trust ctx + stamp the caller's authoritative tenant, never
	// the wire. Same dispatch shape as the other substrate admin endpoints.
	mux.Handle("POST /v1/_path", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstratePath))))
	mux.Handle("POST /v1/_document", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateDocument))))
	// RFC AH Phase 4 (Web UI) — two ADDITIVE read-only views of the volume
	// universe. Tenant-confined (ScopeTenant via isTenantConfinedDefPath):
	// statics are shown to all (the shared bind floor), dynamic + ephemeral
	// rows are filtered to the caller's authoritative tenant. No new runtime
	// primitive — CRUD stays on POST /v1/_volumedef.
	mux.Handle("GET /v1/_volumes", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListVolumes))))
	mux.Handle("GET /v1/_volumes/ephemeral", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListEphemeralVolumes))))
	// v1.x scheduled-runs substrate. Same operator-admin-only dispatch
	// shape; the tool is reachable only via this endpoint + the MCP
	// meta-tool + the future gRPC RPC (no per-agent dispatcher slot).
	mux.Handle("POST /v1/_scheduledef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateScheduleDef))))
	// v1.x RFC G A2A substrate. Same operator-admin-only dispatch shape
	// as the other substrate admin endpoints; the tools are reachable
	// only via these endpoints + the MCP meta-tools + the gRPC RPCs (no
	// per-agent dispatcher slot).
	mux.Handle("POST /v1/_a2aservercarddef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateA2AServerCardDef))))
	mux.Handle("POST /v1/_a2aagentdef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateA2AAgentDef))))
	// v1.x RFC H Input Webhooks substrate. Same operator-admin-only
	// dispatch shape as the other substrate admin endpoints.
	mux.Handle("POST /v1/_webhookdef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateWebhookDef))))
	// RFC I MR-3a MemoryBackendDef substrate. Same operator-admin-only
	// dispatch shape as the other substrate admin endpoints.
	mux.Handle("POST /v1/_memorybackenddef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateMemoryBackendDef))))
	// RFC L OSS multi-tenant authorization — OperatorTokenDef admin.
	mux.Handle("POST /v1/_operatortokendef", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSubstrateOperatorTokenDef))))
	// Whoami — the Web UI's role source (any authenticated principal).
	mux.Handle("GET /v1/_me", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleWhoami))))
	// v0.11.0 LLM Gateway — direct provider routing without the agent
	// loop. Bearer-authed admin scope. Both stream:true (SSE) and
	// stream:false (single-shot JSON) selected by the request body.
	// See internal/api/http/llm_gateway.go for the dispatch shape.
	mux.Handle("POST /v1/_llm/chat", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleLLMChat))))
	// v0.11.3 OpenAI Chat Completions compatibility shim. Same
	// bearer-authed admin scope as /v1/_llm/chat; translates the
	// OpenAI wire shape onto loomcycle's native llmChatRequest and
	// shares the prepareGatewayDispatch path (security policy,
	// per-user quota, audit logging all in one place). The path lacks
	// the underscore prefix because OpenAI SDKs hardcode
	// /v1/chat/completions — the whole point is consumers change
	// only the base URL.
	mux.Handle("POST /v1/chat/completions", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleOpenAICompatChat))))
	// v0.11.4 OpenAI Embeddings compatibility shim. Same bearer-
	// authed admin scope as /v1/chat/completions; thin translator
	// over the single configured providers.Embedder (the same
	// instance Memory tool uses internally for embed:true). No
	// resolver path, no streaming — embeddings are synchronous and
	// loomcycle has one configured embedder per instance per the
	// v0.9.0 RFC. See internal/api/http/embeddings_compat.go.
	mux.Handle("POST /v1/embeddings", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleEmbeddings))))
	// v0.9.x Introspection — bearer-authed read-only enumeration of
	// declared names per substrate. The companion to the op-dispatched
	// substrate write endpoints; drives the Web UI's /ui/library tab.
	mux.Handle("GET /v1/_agentdef/names", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListAgentDefNames))))
	mux.Handle("GET /v1/_skilldef/names", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListSkillDefNames))))
	mux.Handle("GET /v1/_mcpserverdef/names", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListMCPServerDefNames))))
	mux.Handle("GET /v1/_scheduledef/names", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListScheduleDefNames))))
	mux.Handle("GET /v1/_a2aservercarddef/names", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListA2AServerCardDefNames))))
	mux.Handle("GET /v1/_a2aagentdef/names", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListA2AAgentDefNames))))
	mux.Handle("GET /v1/_webhookdef/names", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListWebhookDefNames))))
	mux.Handle("GET /v1/_memorybackenddef/names", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListMemoryBackendDefNames))))
	mux.Handle("GET /v1/_operatortokendef/names", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListOperatorTokenDefNames))))
	// RFC AQ — read-only embedded preset/bundle + env-template introspection
	// (the `loomcycle presets` / `env-template` CLI, web-reachable for the
	// Settings hub). Admin-gated via the /v1/_* default in requiredScopeFor.
	mux.Handle("GET /v1/_presets", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListPresets))))
	mux.Handle("GET /v1/_presets/{name}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleShowPreset))))
	mux.Handle("GET /v1/_env_template", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleEnvTemplate))))
	// v0.9.x Library v2 — unified enumeration that merges static cfg
	// + substrate views into one envelope per entry. The names/* sister
	// endpoints above stay as-is for backwards compat with external
	// adapter consumers; these new endpoints back the /ui/library v2
	// tab which shows STATIC + DYNAMIC entries with source chips.
	mux.Handle("GET /v1/_library/agents", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListLibraryAgents))))
	mux.Handle("GET /v1/_library/skills", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListLibrarySkills))))
	mux.Handle("GET /v1/_library/mcp-servers", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListLibraryMcpServers))))
	// v1.x RFC E /ui/schedules — list (merged static + substrate),
	// per-def runtime state view, + admin mutations (run-now / pause
	// / resume). The ScheduleDef CRUD itself lives on the existing
	// POST /v1/_scheduledef endpoint; this surface just exposes the
	// list + state queries the UI needs.
	mux.Handle("GET /v1/_schedules/list-all", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListSchedules))))
	mux.Handle("GET /v1/_schedules/{def_id}/state", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleGetScheduleState))))
	mux.Handle("POST /v1/_schedules/{def_id}/run-now", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleScheduleRunNow))))
	mux.Handle("POST /v1/_schedules/{def_id}/pause", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleSchedulePause))))
	mux.Handle("POST /v1/_schedules/{def_id}/resume", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleScheduleResume))))
	// v0.9.x Introspection — channels an agent has cursored on
	// (scope=agent, scope_id={agent_name}). Drives the Web UI's
	// per-agent "channels this agent is subscribed to" sub-tab.
	mux.Handle("GET /v1/agents/{agent_name}/channels", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleAgentChannels))))
	// v0.8.15.3 HTTP MCP transport — Streamable HTTP endpoint that
	// dispatches the same 20 MCP tools as the stdio MCP server.
	// POST is the JSON-RPC frame transport; DELETE terminates a
	// session by Mcp-Session-Id. Returns 503 when no HTTP MCP
	// handler is wired (operator didn't construct one in main.go).
	mux.Handle("POST /v1/_mcp", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMCPHTTP))))
	mux.Handle("DELETE /v1/_mcp", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMCPHTTP))))
	// v0.8.0 Memory admin — read-only browsing of stored Memory rows.
	// Drives the Web UI's Memory page. Bearer-authed; same admin
	// posture as /v1/_users / /v1/_resolver.
	mux.Handle("GET /v1/_memory/scopes", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListMemoryScopes))))
	mux.Handle("GET /v1/_memory/scopes/{scope}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListMemoryScopeIDs))))
	mux.Handle("GET /v1/_memory/scopes/{scope}/{scope_id}/keys", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListMemoryEntries))))
	// {key...} catches multi-segment keys — Memory keys frequently use
	// `/`-prefixed paths (e.g. `events/2026-05-09T10:00`) and a
	// single-segment {key} would 404 on those.
	mux.Handle("GET /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleGetMemoryEntry))))
	// v0.11.5: idempotent memory-entry upsert + delete. Mirrors the
	// in-band Memory tool's set/delete ops at the wire boundary for
	// n8n integration-test fixtures + Web UI CRUD. {key...} uses the
	// multi-segment wildcard because key strings can contain slashes
	// (e.g. "company/policy/v1") same as the GET path above.
	mux.Handle("PUT /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handlePutMemoryEntry))))
	mux.Handle("DELETE /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleDeleteMemoryEntry))))
	// v0.9.0 Vector Memory admin — per-scope embedding stats +
	// operator-driven re-embedding for model migrations. Bearer-
	// authed; refuse with 503 when the backend has no vector
	// support OR no embedder is configured.
	mux.Handle("GET /v1/_memory/embed_stats", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMemoryEmbedStats))))
	mux.Handle("POST /v1/_memory/reembed", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMemoryReembed))))
	// v0.8.17 Snapshot capture (PR 2). Bearer-authed; same posture
	// as /v1/_resolver. The full runtime-state JSON envelope; see
	// internal/snapshot/snapshot.go for the wire shape.
	mux.Handle("POST /v1/_snapshots", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleCreateSnapshot))))
	mux.Handle("GET /v1/_snapshots", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListSnapshots))))
	mux.Handle("GET /v1/_snapshots/{id}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleGetSnapshot))))
	mux.Handle("DELETE /v1/_snapshots/{id}", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleDeleteSnapshot))))
	// v0.8.17 PR 3: restore + export. /restore consumes the
	// envelope (looked up by {id} or supplied inline); /export
	// returns the canonical JSON with a Content-Disposition header
	// for `curl -O`.
	mux.Handle("POST /v1/_snapshots/{id}/restore", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRestoreSnapshot))))
	mux.Handle("GET /v1/_snapshots/{id}/export", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleExportSnapshot))))
	// v0.8.17 PR 4: pause / resume / state. Each returns 503 until
	// main.go calls SetPauseManager. Operators drive the runtime-wide
	// quiesce + snapshot cycle via these three; the CLI subcommands
	// (loomcycle pause / resume / state) wrap them for ergonomics.
	mux.Handle("POST /v1/_pause", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handlePauseRuntime))))
	mux.Handle("POST /v1/_resume", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleResumeRuntime))))
	mux.Handle("GET /v1/_state", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRuntimeState))))
	// v0.8.16 Interruption tool. resolve is the human-side answer
	// submit; the two list endpoints drive the Web UI (run-scoped
	// audit + user-scoped inbox).
	mux.Handle("POST /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleResolveInterrupt))))
	mux.Handle("GET /v1/runs/{run_id}/interrupts", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListRunInterrupts))))
	mux.Handle("GET /v1/users/{user_id}/interrupts", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListUserInterrupts))))
	// PR 2 / interactive terminal: inject an operator "steering" instruction
	// into an in-flight run (appended to the live conversation mid-turn).
	mux.Handle("POST /v1/runs/{run_id}/input", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRunInput))))
	mux.Handle("POST /v1/runs/{run_id}/compact", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleCompactRun))))
	// Re-attach to a running (or finished) run's event stream — the operator
	// leaves the interactive /run terminal and returns to the same live run.
	mux.Handle("GET /v1/runs/{run_id}/stream", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRunStream))))
	// v0.7.3 Web UI — embedded React SPA. The cookie-set landing
	// page (/ui with a ?token= query) is intentionally NOT
	// auth-middleware-wrapped; it sets the cookie that the
	// authMiddleware will then accept on subsequent /v1 calls.
	// Static asset requests (/ui/assets/*) don't need auth either
	// — the SPA shell is public; it pulls protected data from
	// /v1/* which DOES go through authMiddleware. Standard SPA-on-
	// API split.
	uiHandler := webui.Handler("/ui", false)
	mux.Handle("GET /ui", recoveryMiddleware(uiHandler))
	mux.Handle("GET /ui/", recoveryMiddleware(uiHandler))
	// v1.x RFC G A2A — additive routes (well-known AgentCard + REST /
	// JSON-RPC binding mounts) registered by an external hook so the
	// A2A package stays decoupled from this one. The hook also receives
	// the recovery+auth middleware chain so it can gate its admin-only
	// surfaces (e.g. the extended card) with the same posture as /v1/_*.
	// Nil when the A2A surface is disabled (the default).
	if s.extraMux != nil {
		s.extraMux(mux, func(next http.Handler) http.Handler {
			return recoveryMiddleware(s.authMiddleware(next))
		})
	}
	// v1.x RFC H inbound-webhook receiver. Mounted WITHOUT the bearer
	// authMiddleware — the receiver does its own per-WebhookDef auth.
	// Wrapped in recoveryMiddleware only, so a panic in a handler still
	// becomes a 500 rather than tearing down the process. Nil when the
	// receiver is disabled (the default).
	if s.webhookMux != nil {
		s.webhookMux(&webhookMuxAdapter{mux: mux}, func(next http.Handler) http.Handler {
			return recoveryMiddleware(s.authMiddleware(next))
		})
	}
	return mux
}

// MuxRegistrar is the minimal mux surface the webhook receiver's Mount
// needs. Declared here (not as a concrete *http.ServeMux) so the recovery
// wrapper can be interposed transparently.
type MuxRegistrar interface {
	Handle(pattern string, handler http.Handler)
}

// webhookMuxAdapter wraps each webhook handler in recoveryMiddleware
// (recovery only — no auth; the receiver authenticates per-WebhookDef).
type webhookMuxAdapter struct{ mux *http.ServeMux }

func (a *webhookMuxAdapter) Handle(pattern string, h http.Handler) {
	a.mux.Handle(pattern, recoveryMiddleware(h))
}

// SetWebhookMux installs the v1.x RFC H webhook-receiver mount hook,
// called at the end of Mux(). Nil-safe. The hook receives a MuxRegistrar
// that applies recovery middleware (for the unauthed receiver POST, which
// does its own per-WebhookDef auth) AND an admin-auth wrapper (recovery +
// bearer) for the WH-5b triage endpoints, which ARE operator-only.
func (s *Server) SetWebhookMux(fn func(reg MuxRegistrar, adminAuth func(http.Handler) http.Handler)) {
	s.webhookMux = fn
}

// SetExtraMux installs a hook called at the end of Mux() to register
// additional routes on the shared mux. The hook receives the mux and an
// admin-auth middleware wrapper (recovery + bearer authMiddleware) for
// gating admin-only routes. Used by the A2A server surface; nil-safe.
func (s *Server) SetExtraMux(fn func(mux *http.ServeMux, adminAuth func(http.Handler) http.Handler)) {
	s.extraMux = fn
}

// recoveryMiddleware turns a panicking handler into a 500. If headers have
// already been sent (the SSE path opens the stream before running anything
// that could panic), we can't write a status — we log and let the connection
// terminate.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic recovered in %s %s: %v", r.Method, r.URL.Path, rec)
				// Best-effort 500. If headers are already sent (SSE has
				// started writing) the WriteHeader call is a no-op and the
				// client sees the connection close, which is the cleanest
				// signal we can give at that point.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":"internal server error"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- handlers ---

// healthzResponse mirrors the gRPC HealthResponse shape so adapters
// switching between the two transports decode the same JSON keys.
//
// uptime_seconds is computed at request time (not cached) so a long-
// running process surfaces its real uptime, not the moment New() ran.
//
// v0.8.21 also adds `metrics_enabled` so the Web UI's Activity
// Monitor can render its "metrics are off, set
// LOOMCYCLE_METRICS_ENABLED=1" empty state without first probing
// /v1/_metrics and getting a 503. metricsSampler is constructed in
// main.go only when cfg.Env.MetricsEnabled is true, so its nil-ness
// is the single source of truth — no separate field on Server.
type healthzResponse struct {
	OK             bool   `json:"ok"`
	Version        string `json:"version,omitempty"`
	Commit         string `json:"commit,omitempty"`
	Built          string `json:"built,omitempty"`
	UptimeSeconds  int64  `json:"uptime_seconds,omitempty"`
	MetricsEnabled bool   `json:"metrics_enabled"`
	// v0.12.0 multi-replica cluster view. Populated only when
	// LOOMCYCLE_REPLICA_ID is set (SetCoord wires the backing
	// store). The omitempty rule keeps single-replica /healthz
	// responses byte-identical to v0.11.x.
	ReplicaID string          `json:"replica_id,omitempty"`
	Replicas  []coord.Replica `json:"replicas,omitempty"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Compute uptime relative to startedAt. The fallback to time.Time{}
	// case is for *Server values constructed via struct literal in
	// tests — uptime would otherwise be ~negative-epoch.
	var uptime int64
	if !s.startedAt.IsZero() {
		uptime = int64(time.Since(s.startedAt).Seconds())
	}
	resp := healthzResponse{
		OK:             true,
		Version:        s.buildVersion,
		Commit:         s.buildCommit,
		Built:          s.buildTime,
		UptimeSeconds:  uptime,
		MetricsEnabled: s.metricsSampler != nil,
	}
	// v0.12.0 cluster view. replica_id is the in-memory identity of
	// THIS replica and is always populated when coord is wired —
	// monitoring/LB systems that key off it must not see intermittent
	// identity loss when Postgres hiccups (review finding #4). Only
	// the replicas[] list depends on a successful ListReplicas; on
	// error we log + omit it but still return ok:true. Liveness probe
	// semantics trump cluster-view completeness.
	if s.replicaStore != nil {
		resp.ReplicaID = s.replicaID
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		replicas, err := s.replicaStore.ListReplicas(ctx)
		if err != nil {
			log.Printf("healthz: list replicas: %v", err)
		} else {
			resp.Replicas = replicas
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// systemChannelPublishRequest is the body shape for the v0.8.6
// admin endpoint. payload is required; deliver_at is optional
// (RFC3339; in-past treated as immediate, same as the Channel tool).
type systemChannelPublishRequest struct {
	Payload   json.RawMessage `json:"payload"`
	DeliverAt string          `json:"deliver_at,omitempty"`
}

// handleSystemChannelPublish accepts bearer-authed publishes to
// `_system/*` channels. Use cases per the system-channels RFC:
//
//   - External monitoring → push alerts via webhook
//   - Ops dashboards → operator-issued alarms
//   - Manual debugging (curl) → operator-triggered publishes
//
// The endpoint enforces:
//   - Channel name MUST start with `_system/` (prefix is the wire
//     contract; without this guard the admin endpoint would also
//     be a bypass for regular agent channels' ACLs).
//   - Channel MUST be declared in operator yaml (no auto-creation).
//   - Bearer auth via the existing middleware (already enforced
//     before this handler runs).
//
// published_by_user_id is set to the bearer's resolved user_id if
// one is in context; otherwise falls back to "_admin" (operator
// used a raw bearer with no user mapping).
// handleMCPHTTP routes POST/DELETE /v1/_mcp requests through the
// installed HTTP MCP transport handler. Returns 503 with a JSON-RPC
// error envelope when no handler is wired — distinct from the
// transport-level "session not found or expired" 404 which only
// happens once the handler is dispatching real requests.
//
// All the protocol logic (session management, SSE vs JSON, frame
// dispatch through (*lcmcp.Server).handleFrame) lives in
// internal/api/mcp/http_transport.go. This function is a thin guard
// + delegate so internal/api/http stays free of MCP-internal types.
func (s *Server) handleMCPHTTP(w http.ResponseWriter, r *http.Request) {
	if s.mcpHTTPHandler == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"error":{"code":-32603,"message":"HTTP MCP transport not enabled"}}`))
		return
	}
	s.mcpHTTPHandler.ServeHTTP(w, r)
}

func (s *Server) handleSystemChannelPublish(w http.ResponseWriter, r *http.Request) {
	if s.systemPublisher == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "system_publisher_unwired", "system publisher not wired (operator misconfiguration)")
		return
	}

	name := r.PathValue("name")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_name", "missing channel name")
		return
	}
	// Validate `_system/` prefix — the admin endpoint is only for
	// system channels (operator-authored, bearer-authed). Agent
	// channels are published via the Channel tool, not here.
	if !strings.HasPrefix(name, "_system/") || strings.Contains(name, "..") {
		writeJSONError(w, http.StatusForbidden, "non_system_channel", fmt.Sprintf("admin publish is only valid for `_system/*` channels; got %q", name))
		return
	}
	fullName := name

	def, ok := s.cfg.Channels[fullName]
	if !ok {
		writeJSONError(w, http.StatusNotFound, "channel_not_declared", fmt.Sprintf("channel %q not declared in operator yaml", fullName))
		return
	}

	var body systemChannelPublishRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", fmt.Sprintf("invalid request body: %s", err))
		return
	}
	// `len(body.Payload) == 0` catches the missing-key case, but a
	// caller sending `{"payload": null}` produces a 4-byte literal
	// `[]byte("null")` — len > 0, json.Valid returns true — which
	// would silently store a null payload. Explicitly reject the
	// JSON-null literal so the contract "payload required" holds.
	if len(body.Payload) == 0 || string(body.Payload) == "null" {
		writeJSONError(w, http.StatusBadRequest, "missing_payload", "missing required field: payload")
		return
	}
	if !json.Valid(body.Payload) {
		writeJSONError(w, http.StatusBadRequest, "invalid_payload", "payload is not valid JSON")
		return
	}
	if cap := s.cfg.Env.ChannelsMaxValueBytes; cap > 0 && len(body.Payload) > cap {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", fmt.Sprintf("payload (%d bytes) exceeds max %d", len(body.Payload), cap))
		return
	}

	var deliverAt time.Time
	if body.DeliverAt != "" {
		parsed, err := time.Parse(time.RFC3339, body.DeliverAt)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_deliver_at", fmt.Sprintf("invalid deliver_at %q: %s", body.DeliverAt, err))
			return
		}
		deliverAt = parsed
	}

	// Resolve scope_id from the channel's declared scope. System
	// channels typically use scope=global (one shared cursor) but
	// scope=user is valid for per-user system notifications. agent
	// scope is rejected — agents aren't the audience for system
	// channels and the scope wouldn't be meaningfully resolvable
	// from an admin context anyway.
	var scope store.MemoryScope
	var scopeID string
	switch def.Scope {
	case "global":
		scope, scopeID = store.MemoryScopeGlobal, ""
	case "user":
		writeJSONError(w, http.StatusNotImplemented, "scope_user_unsupported", "scope=user system channel admin publish is not yet supported (use the agent-tool publish path or scope=global)")
		return
	case "agent":
		writeJSONError(w, http.StatusBadRequest, "scope_agent_invalid", "scope=agent is not valid for system channels (use global or user)")
		return
	default:
		writeJSONError(w, http.StatusInternalServerError, "scope_unknown", fmt.Sprintf("channel %q has unknown scope %q", fullName, def.Scope))
		return
	}

	// Audit attribution. tools.RunIdentity isn't on r.Context() (this
	// isn't a run-bound request), so we fall back to "_admin".
	publishedBy := "_admin"

	msg, err := s.systemPublisher.Publish(r.Context(), fullName, scope, scopeID,
		body.Payload, deliverAt, publishedBy, def.MaxMessages, def.DefaultTTL)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "publish_failed", fmt.Sprintf("publish failed: %s", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	resp := map[string]any{
		"msg_id":     msg.ID,
		"channel":    fullName,
		"created_at": msg.PublishedAt.UTC().Format(time.RFC3339Nano),
	}
	if !msg.VisibleAt.IsZero() && !msg.VisibleAt.Equal(msg.PublishedAt) {
		resp["visible_at"] = msg.VisibleAt.UTC().Format(time.RFC3339Nano)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// runRequest is the JSON body shape for POST /v1/runs.
type runRequest struct {
	Agent string `json:"agent"`
	// Segments is the explicit, typed input form (role + content blocks).
	Segments []loop.PromptSegment `json:"segments"`
	// Prompt is convenience sugar (F47): a bare top-level user prompt. When
	// set and Segments is empty, it expands to a single trusted-text user
	// segment. Segments wins when both are present. It exists because callers
	// naturally send {"prompt":"..."}; without the field Go silently dropped
	// it, leaving an empty messages array (Anthropic 400; DeepSeek silent).
	Prompt       string   `json:"prompt,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// AllowedHosts narrows the HTTP/WebFetch/WebSearch host allowlist
	// for THIS run only. nil/omitted = no narrowing (operator's static
	// list applies). Empty array `[]` = deny all (every network call
	// refuses). Non-empty = intersection with the operator list (caller
	// can shrink, never widen). The pointer-to-slice shape lets us
	// distinguish nil from empty in JSON.
	AllowedHosts *[]string `json:"allowed_hosts,omitempty"`
	// WebSearchFilter selects what happens to Brave search results
	// whose URL host isn't in the intersected AllowedHosts list:
	//   - "drop" (default when AllowedHosts is non-nil) omits non-
	//     matching results entirely; the model only sees URLs it can
	//     follow up on with WebFetch.
	//   - "keep" returns Brave's full result set; the caller filters
	//     downstream. Useful when the caller wants visibility into
	//     what Brave found before narrowing.
	// Ignored when AllowedHosts is nil.
	WebSearchFilter string `json:"web_search_filter,omitempty"`
	// SessionID is optional. When set, the new run is appended to that
	// session (the prior transcript is NOT replayed by /v1/runs — use
	// /v1/sessions/{id}/messages for continuation). When empty, a fresh
	// session is created. The new session ID is announced as the first
	// SSE event so the caller can address subsequent calls to it.
	SessionID string `json:"session_id,omitempty"`
	TenantID  string `json:"tenant_id,omitempty"`
	// UserID binds the run to a user (v0.4+). Optional. Charset:
	// [A-Za-z0-9_-]{1,128}. Empty leaves the run unbound (legacy v0.3
	// behaviour). Sub-agent runs spawned from this run inherit it.
	UserID string `json:"user_id,omitempty"`
	// AgentID is a caller-supplied tracking handle (v0.4+). Optional;
	// when omitted, loomcycle generates one and announces it in the
	// `event: agent` SSE frame. Charset: [A-Za-z0-9_-]{1,128}. Distinct
	// from SessionID — agent_id addresses a single run for status/cancel,
	// session_id addresses the conversation thread for transcript
	// continuation.
	AgentID string `json:"agent_id,omitempty"`
	// UserTier names the user-facing-tier policy the resolver should
	// overlay for this run (v0.8.2+). When set, MUST exist in
	// cfg.UserTiers (unknown → 400). When omitted, the resolver uses
	// cfg.UserTiers["default"] if the operator has a user_tiers
	// block, otherwise falls through to the v0.7-era library + per-
	// agent overrides. See docs/PLAN.md → v0.8.2 for the full
	// resolver overlay precedence chain.
	UserTier string `json:"user_tier,omitempty"`
	// UserBearer is the v0.8.x per-run MCP bearer token. Substituted
	// into MCP HTTP header values containing ${run.user_bearer} at
	// outbound request-build time. Charset:
	// [A-Za-z0-9._\-+/=]{16,512} when present → 400 otherwise. Empty
	// is backwards compat (static-bearer setups unaffected). Sub-
	// agents inherit identically. Never persisted; never logged in
	// full.
	UserBearer string `json:"user_bearer,omitempty"`
	// UserCredentials is the v1.x RFC F named-credentials map —
	// per-tool/per-MCP-server bearers keyed by operator-chosen name.
	// Substituted into MCP HTTP header values containing
	// ${run.credentials.<name>} at outbound request-build time.
	// Keys validated as [a-zA-Z0-9_-]{1,64}; values arbitrary
	// strings; empty map is valid (no per-tool auth needed).
	// Empty + UserBearer set → at WithRunIdentity time the default
	// key is auto-populated from UserBearer for back-compat with
	// v0.8.x single-bearer flows. Never persisted; never logged.
	// See `Context.help per-run-credentials` for the operator-facing
	// reference; rfcs/per-run-credentials.md for the design lock.
	UserCredentials map[string]string `json:"user_credentials,omitempty"`
	// ParentContext is opaque caller-tracking lineage (v0.12.x). The
	// runtime carries it verbatim, inherits it onto every sub-agent the
	// Agent tool spawns, persists it on each run row, and echoes it on
	// the per-agent report surfaces (agents stream, agent status, SSE
	// "agent" frame) — so an external consumer can attribute a child
	// sub-agent's usage back to the user-initiated request. Field
	// lengths bounded at wire entry; an all-empty struct is treated as
	// absent. Not a secret (safe to persist/log/emit). Omitted = no
	// tracking context (back-compat).
	ParentContext *store.ParentContext `json:"parent_context,omitempty"`
	// Metadata is the optional NON-SECRET structured blob passed to the
	// agent (repo name, review policy, preferred skills, …) — symmetric with
	// the WebHook/Schedule trigger paths. A first-party /v1/runs caller is
	// bearer-authed, so this is TRUSTED: delivered to a code-js agent as
	// input.metadata, and to an LLM agent as a trusted-text prompt segment.
	// Not a secret (safe to log); credentials use user_credentials. Per-call,
	// not session state — a continuation must re-send it (see RunInput.Metadata).
	Metadata map[string]any `json:"metadata,omitempty"`
	// RunTimeoutSeconds is an optional ad-hoc per-run wall-clock budget for a
	// code-js agent, overriding the agent's run_timeout_seconds and the global
	// LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS (precedence: per-run >
	// per-agent > global). 0 = inherit. Ignored by LLM agents.
	RunTimeoutSeconds int `json:"run_timeout_seconds,omitempty"`
	// Interactive starts a PERSISTENT run that parks for operator steering at
	// end_turn instead of terminating (interactive terminal). Drive it via
	// POST /v1/runs/{run_id}/input; pair with an unbounded_iterations agent
	// for a true always-on terminal. Cancel ends it.
	Interactive bool `json:"interactive,omitempty"`

	// Sampling is an optional per-RUN LLM sampling override (temperature,
	// top_p, …) — merged PER FIELD over the agent's own sampling (this wins;
	// unset fields inherit the agent's). nil = inherit the agent's entirely.
	Sampling *config.Sampling `json:"sampling,omitempty"`

	// Compaction is an optional per-RUN context-compaction override — merged PER
	// FIELD over the agent's own compaction block (this wins; unset fields
	// inherit). nil = inherit the agent's entirely.
	Compaction *config.Compaction `json:"compaction,omitempty"`
}

// pickRunTimeout resolves the effective code-js wall-clock budget override:
// per-run (the request field) wins over per-agent (AgentDef), else 0 (the
// provider's global default). A code-js orchestrator that blocks in
// Agent.parallel_spawn awaiting LLM children needs a longer envelope than the
// CPU-oriented global default; this lets a single run or a single agent raise
// it without bumping the global for every code agent.
func pickRunTimeout(perRun, perAgent int) int {
	if perRun > 0 {
		return perRun
	}
	return perAgent
}

// injectMetadataSegments inserts the run's NON-SECRET metadata into the prompt
// segments for an LLM agent: the trusted `metadata` as a system-role
// trusted-text block, and the untrusted `payloadMetadata` (external-trigger
// projection) as a user-role untrusted-block fenced under kind "run_metadata".
// Both go AFTER a leading system segment (the agent's system prompt stays
// first) and before the user content.
//
// No-op when metadataViaInput is true — the provider receives metadata
// structurally via RunMeta → input.metadata / input.payload_metadata (a
// user-role block here would also shadow the latest-user-text it reads as
// input.prompt), and when both maps are empty. metadataViaInput is the
// provider's Capabilities flag (set by code-js), not a hardcoded id, so a
// future structured-input provider is handled without a special case.
func injectMetadataSegments(segs []loop.PromptSegment, metadataViaInput bool, metadata, payloadMetadata map[string]any) []loop.PromptSegment {
	if metadataViaInput {
		return segs
	}
	var inject []loop.PromptSegment
	if len(metadata) > 0 {
		if b, err := json.MarshalIndent(metadata, "", "  "); err == nil {
			inject = append(inject, loop.PromptSegment{
				Role: "system",
				Content: []loop.PromptContentBlock{{
					Type: "trusted-text",
					Text: "Run metadata (operator-authored, trusted):\n" + string(b),
				}},
			})
		}
	}
	if len(payloadMetadata) > 0 {
		if b, err := json.MarshalIndent(payloadMetadata, "", "  "); err == nil {
			inject = append(inject, loop.PromptSegment{
				Role: "user",
				Content: []loop.PromptContentBlock{{
					Type: "untrusted-block", Kind: "run_metadata", Text: string(b),
				}},
			})
		}
	}
	if len(inject) == 0 {
		return segs
	}
	idx := 0
	if len(segs) > 0 && segs[0].Role == "system" {
		idx = 1
	}
	out := make([]loop.PromptSegment, 0, len(segs)+len(inject))
	out = append(out, segs[:idx]...)
	out = append(out, inject...)
	out = append(out, segs[idx:]...)
	return out
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	// Cap the body so a malicious caller can't exhaust memory by streaming a
	// huge body (ReadHeaderTimeout doesn't cover the body). Default 16 MiB to
	// fit inline base64 image content (RFC AT); LOOMCYCLE_MAX_REQUEST_BYTES.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBytes())
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, fmt.Sprintf("request body exceeds the %d-byte limit", maxErr.Limit), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Agent == "" {
		http.Error(w, `agent is required`, http.StatusBadRequest)
		return
	}

	// F47: expand a top-level `prompt` string into a single trusted-text user
	// segment when no explicit `segments` were given. The explicit form wins
	// when both are present.
	if len(req.Segments) == 0 && req.Prompt != "" {
		req.Segments = []loop.PromptSegment{{
			Role:    "user",
			Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: req.Prompt}},
		}}
	}
	// F47: refuse a run with no input rather than dispatching an empty messages
	// array to the provider (Anthropic 400s; DeepSeek silently accepts) — a
	// clear 400 here beats a confusing provider-side error downstream.
	if len(req.Segments) == 0 {
		http.Error(w, `no input: provide "segments" (or a top-level "prompt" string)`, http.StatusBadRequest)
		return
	}

	// Validate the v0.4 tracking fields. user_id is optional; an empty
	// agent_id triggers server-side generation. Both, when supplied,
	// must satisfy the [A-Za-z0-9_-]{1,128} charset so they're safe to
	// use as URL path segments and registry keys.
	if req.UserID != "" && !validIdent(req.UserID) {
		http.Error(w, `user_id must match [A-Za-z0-9_-]{1,128}`, http.StatusBadRequest)
		return
	}
	if req.AgentID != "" && !validIdent(req.AgentID) {
		http.Error(w, `agent_id must match [A-Za-z0-9_-]{1,128}`, http.StatusBadRequest)
		return
	}

	// RFC L/N: resolve the authoritative identity FIRST (principal over the
	// wire on authed routes; the wire value on open/un-authed paths), so the
	// existence-check + ALL downstream resolution use req.TenantID and agree
	// in BOTH modes. A pre-applyPrincipal check at tenantFromCtx returned ""
	// in open mode while the run used the wire tenant — rejecting a
	// tenant-scoped dynamic agent as "unknown agent" (RFC N runtime QA BUG-1).
	req.TenantID, req.UserID = s.applyPrincipal(r.Context(), req.TenantID, req.UserID)

	// Existence-check the agent at the run's authoritative tenant.
	agentDef, ok := s.lookupAgent(r.Context(), req.TenantID, req.Agent)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown agent %q", req.Agent), http.StatusBadRequest)
		return
	}

	// v0.8.2: validate user_tier early so an unknown name surfaces as
	// 400 before the resolver runs. Empty user_tier is fine — the
	// resolver falls through to cfg.UserTiers["default"] (or to v0.7-
	// era behaviour when no user_tiers block is configured).
	if req.UserTier != "" && len(s.cfg.UserTiers) > 0 {
		if _, ok := s.cfg.UserTiers[req.UserTier]; !ok {
			http.Error(w, fmt.Sprintf("unknown user_tier %q", req.UserTier), http.StatusBadRequest)
			return
		}
	}

	if req.UserBearer != "" && !validUserBearer(req.UserBearer) {
		http.Error(w, `user_bearer must match [A-Za-z0-9._\-+/=]{16,512}`, http.StatusBadRequest)
		return
	}
	if errMsg, ok := connector.ValidateUserCredentialsMap(req.UserCredentials); !ok {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	// Bound the opaque tracking fields so a consumer can't push unbounded
	// strings into the run table / event stream. An all-empty struct is
	// normalised to nil so back-compat decode paths see "no context".
	if errMsg, ok := validateParentContext(req.ParentContext); !ok {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	if req.ParentContext.IsZero() {
		req.ParentContext = nil
	}

	// (req.TenantID / req.UserID were made authoritative above via
	// applyPrincipal — fairness key, session tenant, run-row attribution,
	// and threaded RunIdentity all derive from them.)
	providerID, model, effort, err := s.resolveAgent(r.Context(), req.TenantID, req.Agent, req.UserTier)
	if err != nil {
		http.Error(w, err.Error(), resolveErrorToStatus(err))
		return
	}
	provider, err := s.providers.Get(providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Per-session continuation lock: when the caller is resuming an
	// existing session, serialize at the session level so two concurrent
	// POSTs can't replay overlapping transcripts. Fresh runs (empty
	// SessionID) skip this — they have no prior history to corrupt.
	//
	// CRITICAL: validate the session exists BEFORE taking the lock.
	// Otherwise an attacker can spam unknown IDs and each LoadOrStore
	// grows sessionLocks permanently (entries are never GC'd at v0.3.2).
	if req.SessionID != "" {
		if s.store == nil {
			http.Error(w, "session_id requires persistence (Store not configured)", http.StatusBadRequest)
			return
		}
		if _, err := s.store.GetSession(r.Context(), req.SessionID); err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		releaseSess, ok := s.trySessionLock(req.SessionID)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `{"code":"session_busy","error":"another request is in flight on session %q"}`, req.SessionID)
			return
		}
		defer releaseSess()
	}

	// Runtime-pause admission gate (RFC X / F41): reject new runs with 503
	// while pausing/paused, BEFORE acquiring a slot so a rejected run doesn't
	// burn a quota slot. The help doc already documents this 503.
	if s.rejectIfPausedHTTP(w) {
		return
	}

	// Acquire concurrency slot first so backpressure is reported as 429
	// before we open the SSE stream.
	acquireStart := time.Now()
	release, err := s.sem.AcquireForUser(r.Context(), req.UserID)
	queueWait := time.Since(acquireStart)
	if err != nil {
		writeQuotaError(w, err)
		return
	}
	defer release()

	// Filter tools by agent allowlist + caller request.
	// RFC N: advertise at the run's authoritative tenant. Use req.TenantID
	// (made authoritative by applyPrincipal above), NOT tenantFromCtx — the
	// two agree on authed routes but diverge in open mode (tenantFromCtx=""
	// vs the wire tenant), which would advertise the wrong tenant's MCP tools
	// for an open-mode run. Consistent with the agent existence-check +
	// resolveAgent on this path.
	allowedTools := filterTools(s.candidateTools(r.Context(), req.TenantID, agentDef.AllowedTools), agentDef.AllowedTools, req.AllowedTools)
	// Per-run host narrowing for HTTP/WebFetch/WebSearch. Behaviour
	// depends on LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE — see NarrowHosts
	// doc comment. In caller-authoritative mode we ALWAYS call so the
	// nil-fallback-to-operator path works; in default mode we only
	// call when the caller actually supplied a list.
	//
	// hostPolicy captures the inputs in a form sub-agents can re-apply
	// via tools.HostPolicy(ctx) — without this propagation, sub-agents
	// silently fall back to the operator's static allowlist and lose
	// reachability to caller-supplied hosts (commonly localhost).
	var hostPolicy tools.HostPolicyValue
	if req.AllowedHosts != nil || s.cfg.Env.HTTPCallerAuthoritative {
		var caller []string
		if req.AllowedHosts != nil {
			caller = *req.AllowedHosts
		}
		allowedTools = builtin.NarrowHosts(allowedTools, caller, req.WebSearchFilter, s.cfg.Env.HTTPCallerAuthoritative)
		hostPolicy = tools.HostPolicyValue{
			AllowedHosts:    caller,
			HasList:         req.AllowedHosts != nil,
			WebSearchFilter: req.WebSearchFilter,
		}
	}
	dispatcher := s.newDispatcher(allowedTools)

	// v0.8.22: rebuild SystemPrompt from per-run SkillDef bodies.
	// RFC N: resolve skills at the run's authoritative tenant — req.TenantID
	// (authed: == principal; open: == wire), NOT tenantFromCtx, so an
	// open-mode run resolves its tenant-scoped skills instead of "". Mirrors
	// the agent existence-check + resolveAgent on this path.
	agentDef, promptProv := s.resolveSkillBodiesForRun(r.Context(), req.TenantID, agentDef)
	// Optional system prompt from agent def.
	if agentDef.SystemPrompt != "" {
		req.Segments = append([]loop.PromptSegment{{
			Role: "system",
			Content: []loop.PromptContentBlock{{
				Type:      "trusted-text",
				Text:      agentDef.SystemPrompt,
				Cacheable: true,
			}},
		}}, req.Segments...)
	}

	// Resolve the run's agent_id: the caller's value when supplied,
	// otherwise a fresh server-generated one. We need this BEFORE
	// session/run creation so we can write it to the row in one shot,
	// AND BEFORE registering the cancel entry so a cancel arriving
	// between Register and the loop's first ctx-check finds the entry.
	agentID := req.AgentID
	if agentID == "" {
		agentID = newAgentID()
	}

	// Persistence: resolve or create a session, create a run, route every
	// emitted event through the store before forwarding to SSE. With
	// s.store == nil the recording becomes a no-op so v0.2 callers see no
	// behaviour change.
	identity := store.RunIdentity{AgentID: agentID, UserID: req.UserID, TenantID: req.TenantID, UserTier: req.UserTier, Model: model, ReplicaID: s.replicaID, ParentContext: req.ParentContext, Interactive: req.Interactive}
	sessionID, runID, sessErr := s.openOrCreateSessionAndRun(r.Context(), req.SessionID, req.Agent, req.TenantID, req.UserID, identity)
	if sessErr != nil {
		var nf *store.ErrNotFound
		if errors.As(sessErr, &nf) {
			http.Error(w, sessErr.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, sessErr.Error(), http.StatusInternalServerError)
		return
	}

	// Derive the loop ctx with a cancel-cause function. For a normal run the
	// HTTP request ctx is the parent so client-disconnect tears the run down.
	// For an INTERACTIVE run we detach with context.WithoutCancel: the run
	// keeps the request's ctx VALUES (auth principal, tenant) but is NOT
	// cancelled when the client navigates away — it parks and the operator
	// re-attaches via GET /v1/runs/{run_id}/stream. cancelFn (registered under
	// agent_id) is the only thing that stops it.
	runParent := r.Context()
	if req.Interactive {
		runParent = context.WithoutCancel(r.Context())
	}
	runCtx, cancelFn := context.WithCancelCause(runParent)
	// handOff flips true once an interactive run's background goroutine takes
	// ownership of teardown (cancelFn / span / steer / cancel registry /
	// finishRun). Until then — and always for non-interactive runs — the
	// handler's defers own teardown. This prevents the handler returning on
	// client-disconnect from tearing down a still-running detached run.
	handOff := false
	defer func() {
		if !handOff {
			cancelFn(nil) // ensure ctx leaks don't survive the handler
		}
	}()
	// v0.10.0 OTEL: top-level loomcycle.run span covers the whole run.
	runCtx, runSpan := lcotel.RecordRunStart(runCtx, lcotel.RunStartAttrs{
		RunID:     runID,
		AgentID:   agentID,
		AgentName: req.Agent,
		UserID:    req.UserID,
	})
	defer func() {
		if !handOff {
			runSpan.End()
		}
	}()
	lcotel.RecordQueueWait(runSpan, queueWait)

	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:   agentID,
		RunID:     runID,
		SessionID: sessionID,
		UserID:    req.UserID,
		StartedAt: time.Now(),
	}, cancelFn)
	// v0.9.x: identity bundle threaded into finishRun* + the
	// "running" transition publish below.
	meta := runStateMeta{
		RunID:         runID,
		AgentID:       agentID,
		Agent:         req.Agent,
		UserID:        req.UserID,
		TenantID:      req.TenantID,
		otelSpan:      runSpan,
		ParentContext: req.ParentContext,
		// RFC AH Phase 2b: top-level run — owns its ephemeral tree purge.
		IsTopLevel: true,
		RootRunID:  runID,
	}
	if errors.Is(regErr, cancel.ErrInUse) {
		// We've already created the session+run row in the store
		// (session creation is unavoidable to satisfy the FK on
		// runs). Leaving the run at status=running would orphan it
		// permanently — the heartbeat sweeper (when it lands) would
		// eventually catch it, but in the meantime it pollutes
		// ListActiveRunsByUser. Mark it failed with a clear reason
		// so the row is terminal from this exit path.
		s.finishRunFailedReason(runID, "agent_id collision; run never started", meta)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"code":"agent_id_in_use","error":"agent_id %q is already mapped to an active run"}`, agentID)
		return
	}
	if regErr != nil {
		s.finishRunFailedReason(runID, "registry register failed: "+regErr.Error(), meta)
		http.Error(w, regErr.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		if !handOff {
			s.cancelReg.Deregister(agentID)
		}
	}()
	s.publishRunState(meta, "running", "", "")

	// If we're persisting, record the caller's input segments as the first
	// event in the run. The loop never emits the caller's input itself, so
	// without this the transcript would start with the assistant's first
	// turn — and replay couldn't reconstruct the user prompt. Persist under
	// runCtx (not r.Context()) so an interactive run's prompt survives a
	// client disconnect; for a normal run runCtx tracks the request anyway.
	if s.store != nil && runID != "" {
		if inputJSON, err := json.Marshal(req.Segments); err == nil {
			if err := s.store.AppendEvent(runCtx, runID, "user_input", inputJSON); err != nil {
				log.Printf("store: AppendEvent(user_input) failed: %v", err)
			}
		}
	}
	// v0.9.x: persist the resolved system prompt + provenance so the
	// Web UI surfaces it as a card on the run timeline. Mirrors the
	// emission in RunOnce + handleMessages + runSubAgent.
	s.emitSystemPromptEvent(runCtx, runID, agentDef.SystemPrompt, "", promptProv)

	stream, ok := newSSE(w)
	if !ok {
		// ResponseWriter doesn't implement http.Flusher — every frame would
		// be buffered until handler return, defeating SSE. Refuse cleanly so
		// the caller gets a useful error instead of silent buffering.
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()
	// Keep the underlying TCP/HTTP path warm during long quiet stretches
	// (parent agent waiting on a fan-out of sub-agents, single sub-agent
	// mid-WebFetch, etc.). Goroutine exits when r.Context() fires
	// (handler return / client disconnect). No-op if interval == 0.
	stream.startKeepalive(r.Context(), s.cfg.Env.SSEKeepaliveInterval)

	// Announce the (possibly newly-created) session/run IDs so the caller
	// can address continuation requests at the same session.
	if sessionID != "" {
		stream.send(providers.Event{
			Type: "session", // not part of providers.EventType — just a side-channel
			Text: sessionID,
		})
	}
	// New v0.4 side-channel: announce the agent_id (and run_id) so the
	// caller can address cancel/status without knowing the loomcycle-
	// internal session/run IDs. parent_agent_id is null for top-level
	// runs; sub-agents emit it via runSubAgent's own side-channel work.
	stream.sendRaw("agent", map[string]any{
		"agent_id":        agentID,
		"run_id":          runID,
		"session_id":      sessionID,
		"parent_agent_id": nil,
		"parent_context":  req.ParentContext, // v0.12.x: opaque tracking lineage (nil when absent)
	})

	// For an interactive (detached) run the loop runs in a background
	// goroutine that OUTLIVES this handler — and net/http forbids writing to
	// the ResponseWriter after the handler returns. So the loop must NOT push
	// to the live stream; it only persists. The handler (and any re-attach)
	// streams by tailing the store. For a normal run, forward live as before.
	streamFwd := stream.send
	if req.Interactive {
		streamFwd = func(providers.Event) {}
	}
	// Persist under runCtx so events survive a client disconnect on an
	// interactive run (runCtx tracks the request for a normal run).
	emit := s.makeRecordingEmit(runCtx, runID, streamFwd)

	// PR 2: operator steering queue for this run (in-flight input injection).
	steerQ, onSteer, deregSteer := s.makeSteer(runCtx, runID, agentID, sessionID, req.UserID, emit)
	deferDeregSteer := func() {
		if !handOff {
			deregSteer()
		}
	}
	defer deferDeregSteer()

	// Pass the agent's effective tool names to the dispatcher so tools
	// that need a runtime view of "what this agent can use" (e.g. the
	// Skill tool's subset check on each call) read it via ctx instead
	// of being constructed per-run.
	loopCtx := tools.WithAgentTools(runCtx, toolNames(allowedTools))
	// Stash the run's identity so the Agent built-in tool's
	// SubAgentRunner can inherit user_id and set parent_agent_id on
	// any sub-runs it spawns.
	loopCtx = tools.WithRunIdentity(loopCtx, tools.RunIdentityValue{
		UserID:          req.UserID,
		TenantID:        req.TenantID, // RFC L: authoritative tenant (memory tenancy key)
		AgentID:         agentID,
		RootRunID:       runID, // RFC AH Phase 2b: top-level run roots its own spawn tree
		UserTier:        req.UserTier,
		UserBearer:      req.UserBearer,      // v0.8.x: per-run MCP bearer
		UserCredentials: req.UserCredentials, // v1.x RFC F: per-tool named credentials
		ParentContext:   req.ParentContext,   // v0.12.x: opaque tracking lineage, inherited by sub-agents
	})
	// Stash the caller's host policy so any sub-agents spawned by the
	// Agent tool inherit the same allowed_hosts / WebSearchFilter
	// narrowing the parent received.
	loopCtx = tools.WithHostPolicy(loopCtx, hostPolicy)
	loopCtx = tools.WithAgentName(loopCtx, req.Agent)
	loopCtx = tools.WithMemoryPolicy(loopCtx, tools.MemoryPolicyValue{
		AllowedScopes: agentDef.MemoryScopes,
		QuotaBytes:    agentDef.MemoryQuotaBytes,
		Backend:       agentDef.MemoryBackend,
	})
	// RFC AA: the agent's SQL Memory ACL. Empty sql_scopes → default-deny.
	loopCtx = tools.WithSqlMemPolicy(loopCtx, tools.SqlMemPolicyValue{
		AllowedScopes: agentDef.SqlScopes,
		QuotaBytes:    agentDef.SqlQuotaBytes,
	})
	loopCtx = tools.WithCompactionPolicy(loopCtx, config.MergeCompaction(agentDef.Compaction, req.Compaction))
	// RFC AH: the run's filesystem-volume bindings. Unbound agents get an
	// empty policy (the file tools fall back to the legacy jail Root);
	// sub-agents inherit + narrow this via runSubAgent.
	loopCtx = tools.WithVolumePolicy(loopCtx, s.volumePolicyForAgent(loopCtx, agentDef))
	// RFC AH Phase 2b: fresh run-scoped ephemeral volume set (inherited by
	// sub-agents via ctx; never shared across top-level runs).
	loopCtx = tools.WithEphemeralVolumes(loopCtx, tools.NewEphemeralVolumeSet())
	loopCtx = tools.WithChannelPolicy(loopCtx, s.channelPolicyForAgent(loopCtx, agentDef))
	loopCtx = tools.WithEventEmitter(loopCtx, emit)
	adPolicy, evPolicy := s.substratePoliciesForAgent(agentDef, req.Agent)
	loopCtx = tools.WithAgentDefPolicy(loopCtx, adPolicy)
	loopCtx = tools.WithSkillDefPolicy(loopCtx, s.skillDefPolicyForAgent(agentDef))
	loopCtx = tools.WithVolumeDefPolicy(loopCtx, s.volumeDefPolicyForAgent(agentDef))
	loopCtx = tools.WithEvaluationPolicy(loopCtx, evPolicy)
	loopCtx = tools.WithHistoryPolicy(loopCtx, s.historyPolicyForAgent(agentDef))
	loopCtx = tools.WithInterruptionPolicy(loopCtx, s.interruptionPolicyForAgent(agentDef))
	loopCtx = tools.WithRunID(loopCtx, runID)
	loopCtx = tools.WithDispatcher(loopCtx, dispatcher)

	// Heartbeat hook: each loop iteration updates last_heartbeat_at so a
	// future sweeper can detect crashed processes (no heartbeat for > N
	// minutes → presumed dead). Cheap (~10–100 calls per run).
	heartbeat := s.makeHeartbeat(runID)

	fbPolicy, fbReResolve := s.fallbackForRun(req.TenantID, req.Agent, req.UserTier)
	runOpts := loop.RunOptions{
		Provider:               provider,
		Model:                  model,
		Tools:                  allowedTools,
		Dispatcher:             dispatcher,
		Segments:               injectMetadataSegments(req.Segments, provider.Capabilities().MetadataViaInput, req.Metadata, nil),
		OnEvent:                emit,
		OnHeartbeat:            heartbeat,
		MaxTokens:              agentDef.MaxTokens,     // 0 → driver default
		MaxIterations:          agentDef.MaxIterations, // 0 → loop default (16)
		UnboundedIterations:    agentDef.UnboundedIterations,
		SteerQueue:             steerQ,
		OnSteer:                onSteer,
		Effort:                 effort,
		MarkStalled:            s.markStalledFn(providerID, model),
		MarkRateLimited:        s.markRateLimitedFn(req.UserTier),
		ClearStall:             s.clearStallFn(providerID, model),
		ToolParallelism:        s.cfg.Env.ToolParallelism,
		AgentName:              req.Agent,
		CodeBody:               agentDef.Code, // inline code-js body (RFC J); "" → FS fallback
		Metadata:               req.Metadata,  // direct /v1/runs caller is first-party → trusted; no payload_metadata
		RunTimeoutSeconds:      pickRunTimeout(req.RunTimeoutSeconds, agentDef.RunTimeoutSeconds),
		Interactive:            req.Interactive,
		Sampling:               config.MergeSampling(agentDef.Sampling, req.Sampling),       // per-run wins per field
		Compaction:             config.MergeCompaction(agentDef.Compaction, req.Compaction), // per-run wins per field
		ContextPlugins:         s.contextPlugins,                                            // RFC Z runtime-wide chain (code-js exempt in the loop)
		UserTier:               req.UserTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForAgent(agentDef, req.UserTier),
	}

	// Cooperative pause quiesce (RFC X / F41): the loop parks at an iteration
	// boundary while paused. Registers the run with the pause barrier; the
	// matching deregister runs after the loop (inline) or in the detached
	// goroutine (interactive) — never a handler-level defer (handOff).
	gate, deregGate := s.newPauseGate(runID)
	runOpts.PauseGate = gate
	// RFC X Phase 3: expose the gate to the Agent tool so a parallel_spawn
	// fan-out parent can park mid-tool-call (no-op unless LOOMCYCLE_RESUME_FANOUT).
	loopCtx = tools.WithPauseGate(loopCtx, gate)

	if req.Interactive && s.store != nil {
		// Detached run: execute the loop in a background goroutine that
		// OUTLIVES this handler, so navigating away (client disconnect) no
		// longer kills the run — it parks and the operator re-attaches via
		// GET /v1/runs/{run_id}/stream. The goroutine owns teardown (handOff
		// neutralises the handler's defers); the handler streams by tailing
		// the store (it must NOT use the loop's emit→stream path, because
		// net/http forbids writing to the ResponseWriter after the handler
		// returns). Returning here does NOT cancel the run.
		handOff = true
		go func() {
			// Teardown is DEFERRED + panic-guarded: a panic in loop.Run must
			// not (a) crash the process (this goroutine has no other recover —
			// the recoveryMiddleware only wraps the synchronous handler) nor
			// (b) skip deregistration, which would leak the run in the cancel /
			// steer / pause-barrier registries — a leaked pause entry never
			// parks, so every future Pause would time out waiting for a ghost.
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("interactive run %s panicked: %v", runID, rec)
					s.finishRunFailedReason(runID, fmt.Sprintf("panic: %v", rec), meta)
				}
				deregSteer()
				deregGate()
				s.cancelReg.Deregister(agentID)
				runSpan.End()
				cancelFn(nil)
			}()
			loopRes, runErr := loop.Run(loopCtx, runOpts)
			if runErr != nil {
				// Persist (not stream) the failure so a tailing client sees it.
				emit(providers.Event{Type: providers.EventError, Error: runErr.Error()})
			}
			// WithoutCancel: the store write must not ride a runCtx that an
			// API-cancel already cancelled (the cause is still read from runCtx).
			s.finishRunWithCancel(context.WithoutCancel(runCtx), runCtx, runID, loopRes, runErr, meta)
		}()
		// Tail the store to this client until they disconnect (r.Context()) or
		// the run terminates. from_seq=0 → stream the whole run live. The SSE
		// writer never errors, so the visitor always returns nil.
		_ = s.streamRunEvents(r.Context(), runID, 0, func(pe providers.Event) error {
			stream.send(pe)
			return nil
		})
		return
	}

	loopRes, runErr := loop.Run(loopCtx, runOpts)
	if runErr != nil {
		stream.send(providers.Event{Type: providers.EventError, Error: runErr.Error()})
	}

	s.finishRunWithCancel(r.Context(), runCtx, runID, loopRes, runErr, meta)
	deregGate()
}

// messagesRequest is the JSON body for POST /v1/sessions/{id}/messages. It
// only accepts new segments — agent / model / tools come from the session's
// existing config (looked up by session.Agent → cfg.Agents).
type messagesRequest struct {
	Segments     []loop.PromptSegment `json:"segments"`
	AllowedTools []string             `json:"allowed_tools,omitempty"`
	// AllowedHosts and WebSearchFilter mirror runRequest — see there
	// for the full semantics. Per-call: continuations re-supply the
	// list each time rather than inheriting from the seed call. This
	// keeps "what hosts can this run reach?" answerable from the
	// request alone, no session state to chase.
	AllowedHosts    *[]string `json:"allowed_hosts,omitempty"`
	WebSearchFilter string    `json:"web_search_filter,omitempty"`
	// AgentID is a fresh tracking handle for the new run created by
	// this continuation (v0.4+). Same charset rules as runRequest.
	// UserID is NOT accepted here — continuation runs inherit the
	// session's user_id (set at original creation); allowing a
	// different user_id mid-session would be confusing.
	AgentID string `json:"agent_id,omitempty"`
	// UserTier is the v0.8.2 user-facing-tier policy name for THIS
	// continuation. Unlike UserID (session-bound), user_tier is
	// per-request so a user upgrading mid-session sees the new tier
	// applied immediately. Empty falls through to
	// cfg.UserTiers["default"] when user_tiers is configured.
	UserTier string `json:"user_tier,omitempty"`
	// UserBearer follows runRequest semantics. Per-request (not
	// session-bound) so different continuations in the same session
	// may carry different end-user tokens — natural for a future
	// flow where each continuation gets a fresh short-lived bearer.
	UserBearer string `json:"user_bearer,omitempty"`
	// UserCredentials follows runRequest semantics — same wire shape,
	// same validation, same back-compat sugar (legacy UserBearer
	// promoted to UserCredentials["default"] at WithRunIdentity time).
	UserCredentials map[string]string `json:"user_credentials,omitempty"`
	// ParentContext follows runRequest semantics — a continuation can
	// (re)set the opaque tracking lineage for the new run it creates.
	// Sub-agents spawned from this continuation inherit it identically.
	ParentContext *store.ParentContext `json:"parent_context,omitempty"`
	// Metadata mirrors runRequest.Metadata for the continuation path —
	// optional trusted non-secret blob for the new run this message spawns.
	// It is NOT inherited from the original run (metadata is a per-call input,
	// not session state — see RunInput.Metadata): to carry the original run's
	// metadata into the continuation, re-send it here.
	Metadata map[string]any `json:"metadata,omitempty"`
	// RunTimeoutSeconds mirrors runRequest.RunTimeoutSeconds for the
	// continuation's new run (per-run > per-agent > global). 0 = inherit.
	RunTimeoutSeconds int `json:"run_timeout_seconds,omitempty"`
	// Interactive makes this continuation a PERSISTENT run that parks for
	// operator steering at end_turn (interactive terminal). Same semantics as
	// runRequest.Interactive.
	Interactive bool `json:"interactive,omitempty"`

	// Sampling: per-RUN LLM sampling override for this continuation turn,
	// merged per field over the agent's. Same semantics as runRequest.Sampling.
	Sampling *config.Sampling `json:"sampling,omitempty"`

	// Compaction: per-RUN context-compaction override for this continuation,
	// merged per field over the agent's. Same semantics as runRequest.Compaction.
	Compaction *config.Compaction `json:"compaction,omitempty"`
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "session continuation requires persistence (Store not configured)", http.StatusNotFound)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	// Default 16 MiB to fit inline base64 image content on a continuation turn
	// (RFC AT); LOOMCYCLE_MAX_REQUEST_BYTES. MaxBytesReader still hard-stops.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBytes())

	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id is required", http.StatusBadRequest)
		return
	}

	var body messagesRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, fmt.Sprintf("request body exceeds the %d-byte limit", maxErr.Limit), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the session exists BEFORE taking the per-session lock.
	// Otherwise an attacker can spam unknown IDs and each LoadOrStore
	// grows sessionLocks permanently (entries are never GC'd at v0.3.2).
	//
	// RFC L: a continuation runs under the session's stored tenant+subject, so
	// the caller must OWN the session (the tenant boundary). The tenant-scoped
	// accessor folds a cross-tenant session into the same *store.ErrNotFound a
	// missing one returns → identical opaque 404, no existence oracle.
	sess, err := s.tenantStore(r.Context()).GetSession(r.Context(), id)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Per-session continuation lock: take the lock before transcript
	// replay so two concurrent POSTs to the same session can't read
	// half-written history. Fast-fail with 409 since the alternative —
	// blocking on an SSE handler — would hold an HTTP connection open
	// for the full length of the in-flight run.
	releaseSess, ok := s.trySessionLock(id)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"code":"session_busy","error":"another request is in flight on session %q"}`, id)
		return
	}
	defer releaseSess()

	// Resolve provider+model from the session's stored agent so the
	// continuation runs against the same model as the original session.
	// RFC N: resolve at the SESSION's authoritative tenant (the same value
	// RunOnce uses for a continuation), not tenantFromCtx — the ownership
	// gate above already proved the caller is entitled to this session.
	agentDef, ok := s.lookupAgent(r.Context(), sess.TenantID, sess.Agent)
	if !ok {
		http.Error(w, fmt.Sprintf("session refers to unknown agent %q", sess.Agent), http.StatusBadRequest)
		return
	}
	// v0.8.2: validate user_tier early (same shape as handleRuns).
	if body.UserTier != "" && len(s.cfg.UserTiers) > 0 {
		if _, ok := s.cfg.UserTiers[body.UserTier]; !ok {
			http.Error(w, fmt.Sprintf("unknown user_tier %q", body.UserTier), http.StatusBadRequest)
			return
		}
	}
	if body.UserBearer != "" && !validUserBearer(body.UserBearer) {
		http.Error(w, `user_bearer must match [A-Za-z0-9._\-+/=]{16,512}`, http.StatusBadRequest)
		return
	}
	if errMsg, ok := connector.ValidateUserCredentialsMap(body.UserCredentials); !ok {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	if errMsg, ok := validateParentContext(body.ParentContext); !ok {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	if body.ParentContext.IsZero() {
		body.ParentContext = nil
	}
	providerID, model, effort, err := s.resolveAgent(r.Context(), sess.TenantID, sess.Agent, body.UserTier)
	if err != nil {
		http.Error(w, err.Error(), resolveErrorToStatus(err))
		return
	}
	provider, err := s.providers.Get(providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Replay prior conversation history from the transcript.
	transcript, err := s.store.GetTranscript(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	priorMessages := replayTranscript(transcript)

	// Runtime-pause admission gate (RFC X / F41): a continuation starts a new
	// turn/run, so reject it with 503 while pausing/paused (before the slot).
	if s.rejectIfPausedHTTP(w) {
		return
	}

	// Acquire concurrency slot before opening the SSE stream so backpressure
	// is reported as 429. user_id comes from the session (set at original
	// creation); continuations don't accept a new user_id.
	acquireStart := time.Now()
	release, err := s.sem.AcquireForUser(r.Context(), sess.UserID)
	queueWait := time.Since(acquireStart)
	if err != nil {
		writeQuotaError(w, err)
		return
	}
	defer release()

	// RFC N FIX 2-mcp: a continuation advertises at the SESSION's
	// authoritative tenant (the same value the continuation run uses), not
	// the ctx tenant — the ownership gate above already proved the caller
	// is entitled to this session.
	allowedTools := filterTools(s.candidateTools(r.Context(), sess.TenantID, agentDef.AllowedTools), agentDef.AllowedTools, body.AllowedTools)
	var hostPolicy tools.HostPolicyValue
	if body.AllowedHosts != nil || s.cfg.Env.HTTPCallerAuthoritative {
		var caller []string
		if body.AllowedHosts != nil {
			caller = *body.AllowedHosts
		}
		allowedTools = builtin.NarrowHosts(allowedTools, caller, body.WebSearchFilter, s.cfg.Env.HTTPCallerAuthoritative)
		hostPolicy = tools.HostPolicyValue{
			AllowedHosts:    caller,
			HasList:         body.AllowedHosts != nil,
			WebSearchFilter: body.WebSearchFilter,
		}
	}
	dispatcher := s.newDispatcher(allowedTools)

	// Re-prepend the agent's system prompt — it isn't in the transcript
	// (it's per-call configuration, not conversation content).
	segments := body.Segments
	// v0.8.22: rebuild SystemPrompt from per-run SkillDef bodies
	// when any of the agent's skills has a DB-active row. No-op
	// fast path when none do.
	// RFC N: resolve at the SESSION's authoritative tenant (the same value
	// RunOnce uses for a continuation), not tenantFromCtx — the ownership
	// gate above already proved the caller is entitled to this session.
	agentDef, promptProv := s.resolveSkillBodiesForRun(r.Context(), sess.TenantID, agentDef)
	if agentDef.SystemPrompt != "" {
		segments = append([]loop.PromptSegment{{
			Role: "system",
			Content: []loop.PromptContentBlock{{
				Type: "trusted-text", Text: agentDef.SystemPrompt, Cacheable: true,
			}},
		}}, segments...)
	}

	// Validate any caller-supplied agent_id and reserve one for the new
	// run (sub-fresh per continuation; never inherited from the prior
	// run since "the run" is what agent_id addresses).
	if body.AgentID != "" && !validIdent(body.AgentID) {
		http.Error(w, `agent_id must match [A-Za-z0-9_-]{1,128}`, http.StatusBadRequest)
		return
	}
	agentID := body.AgentID
	if agentID == "" {
		agentID = newAgentID()
	}

	// Create a new run inside the existing session. user_id is
	// inherited from the session (set at original creation). user_tier
	// is per-request (v0.8.2) — a user upgrading mid-session sees
	// the new tier applied immediately on this continuation.
	run, err := s.store.CreateRun(r.Context(), id, store.RunIdentity{
		AgentID:       agentID,
		UserID:        sess.UserID,
		TenantID:      sess.TenantID, // RFC L: continuation inherits the session's authoritative tenant
		UserTier:      body.UserTier,
		Model:         model,
		ReplicaID:     s.replicaID,
		ParentContext: body.ParentContext, // v0.12.x: tracking lineage for this continuation + its sub-agents
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Derive a runCtx with cancel-cause and register in the cancel
	// registry. Same shape as handleRuns.
	runCtx, cancelFn := context.WithCancelCause(r.Context())
	defer cancelFn(nil)
	// v0.10.0 OTEL: top-level loomcycle.run span for session
	// continuations. Each /v1/messages turn = one span.
	runCtx, runSpan := lcotel.RecordRunStart(runCtx, lcotel.RunStartAttrs{
		RunID:     run.ID,
		AgentID:   agentID,
		AgentName: sess.Agent,
		UserID:    sess.UserID,
	})
	defer runSpan.End()
	lcotel.RecordQueueWait(runSpan, queueWait)

	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:   agentID,
		RunID:     run.ID,
		SessionID: id,
		UserID:    sess.UserID,
		StartedAt: time.Now(),
	}, cancelFn)
	// v0.9.x: per-run meta for runstate.Bus.
	meta := runStateMeta{
		RunID:         run.ID,
		AgentID:       agentID,
		Agent:         sess.Agent,
		UserID:        sess.UserID,
		TenantID:      sess.TenantID,
		otelSpan:      runSpan,
		ParentContext: body.ParentContext,
		// RFC AH Phase 2b: a continuation is a top-level run; it owns the
		// ephemeral tree purge for THIS run id.
		IsTopLevel: true,
		RootRunID:  run.ID,
	}
	if errors.Is(regErr, cancel.ErrInUse) {
		// Same orphan-row mitigation as handleRuns — the run was
		// already inserted at status=running.
		s.finishRunFailedReason(run.ID, "agent_id collision; run never started", meta)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"code":"agent_id_in_use","error":"agent_id %q is already mapped to an active run"}`, agentID)
		return
	}
	if regErr != nil {
		s.finishRunFailedReason(run.ID, "registry register failed: "+regErr.Error(), meta)
		http.Error(w, regErr.Error(), http.StatusInternalServerError)
		return
	}
	defer s.cancelReg.Deregister(agentID)
	s.publishRunState(meta, "running", "", "")

	// Persist the new user input segments so a future replay sees them.
	if inputJSON, err := json.Marshal(body.Segments); err == nil {
		if err := s.store.AppendEvent(r.Context(), run.ID, "user_input", inputJSON); err != nil {
			log.Printf("store: AppendEvent(user_input) failed: %v", err)
		}
	}
	// v0.9.x: also persist the resolved system prompt + provenance.
	// On a continuation run, AgentDef could have been promoted to a
	// new version between turns — emit on EVERY run so each cycle
	// records the instructions the agent saw at THAT moment.
	s.emitSystemPromptEvent(r.Context(), run.ID, agentDef.SystemPrompt, "", promptProv)

	stream, ok := newSSE(w)
	if !ok {
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()
	// See handleRuns for the keepalive rationale.
	stream.startKeepalive(r.Context(), s.cfg.Env.SSEKeepaliveInterval)
	stream.send(providers.Event{Type: "session", Text: id})
	stream.sendRaw("agent", map[string]any{
		"agent_id":        agentID,
		"run_id":          run.ID,
		"session_id":      id,
		"parent_agent_id": nil,
		"parent_context":  body.ParentContext, // v0.12.x: opaque tracking lineage (nil when absent)
	})

	emit := s.makeRecordingEmit(r.Context(), run.ID, stream.send)

	// PR 2: operator steering queue for this continuation run.
	steerQ, onSteer, deregSteer := s.makeSteer(r.Context(), run.ID, agentID, id, sess.UserID, emit)
	defer deregSteer()
	heartbeat := s.makeHeartbeat(run.ID)

	loopCtx := tools.WithAgentTools(runCtx, toolNames(allowedTools))
	loopCtx = tools.WithRunIdentity(loopCtx, tools.RunIdentityValue{
		UserID:          sess.UserID,
		TenantID:        sess.TenantID, // RFC L: tenant from the session (authoritative at creation)
		AgentID:         agentID,
		RootRunID:       run.ID, // RFC AH Phase 2b: continuation roots its own spawn tree
		UserTier:        body.UserTier,
		UserBearer:      body.UserBearer,      // v0.8.x: per-run MCP bearer
		UserCredentials: body.UserCredentials, // v1.x RFC F: per-tool named credentials
		ParentContext:   body.ParentContext,   // v0.12.x: opaque tracking lineage, inherited by sub-agents
	})
	// Sub-agents spawned by this continuation must inherit the
	// caller-authoritative host narrowing, same as runRequest +
	// handleRuns. Without this, a sub-agent runs against the
	// operator's static allowlist instead of the caller's narrowed
	// list — the production failure mode 9677b85 fixed for top-level
	// runs and this continuation path was missing the same fix.
	loopCtx = tools.WithHostPolicy(loopCtx, hostPolicy)
	loopCtx = tools.WithAgentName(loopCtx, sess.Agent)
	loopCtx = tools.WithMemoryPolicy(loopCtx, tools.MemoryPolicyValue{
		AllowedScopes: agentDef.MemoryScopes,
		QuotaBytes:    agentDef.MemoryQuotaBytes,
		Backend:       agentDef.MemoryBackend,
	})
	// RFC AA: the agent's SQL Memory ACL. Empty sql_scopes → default-deny.
	loopCtx = tools.WithSqlMemPolicy(loopCtx, tools.SqlMemPolicyValue{
		AllowedScopes: agentDef.SqlScopes,
		QuotaBytes:    agentDef.SqlQuotaBytes,
	})
	loopCtx = tools.WithCompactionPolicy(loopCtx, config.MergeCompaction(agentDef.Compaction, body.Compaction))
	// RFC AH: the run's filesystem-volume bindings. Unbound agents get an
	// empty policy (the file tools fall back to the legacy jail Root);
	// sub-agents inherit + narrow this via runSubAgent.
	loopCtx = tools.WithVolumePolicy(loopCtx, s.volumePolicyForAgent(loopCtx, agentDef))
	// RFC AH Phase 2b: fresh run-scoped ephemeral volume set (inherited by
	// sub-agents via ctx; never shared across top-level runs).
	loopCtx = tools.WithEphemeralVolumes(loopCtx, tools.NewEphemeralVolumeSet())
	loopCtx = tools.WithChannelPolicy(loopCtx, s.channelPolicyForAgent(loopCtx, agentDef))
	loopCtx = tools.WithEventEmitter(loopCtx, emit)
	adPolicy, evPolicy := s.substratePoliciesForAgent(agentDef, sess.Agent)
	loopCtx = tools.WithAgentDefPolicy(loopCtx, adPolicy)
	loopCtx = tools.WithSkillDefPolicy(loopCtx, s.skillDefPolicyForAgent(agentDef))
	loopCtx = tools.WithVolumeDefPolicy(loopCtx, s.volumeDefPolicyForAgent(agentDef))
	loopCtx = tools.WithEvaluationPolicy(loopCtx, evPolicy)
	loopCtx = tools.WithHistoryPolicy(loopCtx, s.historyPolicyForAgent(agentDef))
	loopCtx = tools.WithInterruptionPolicy(loopCtx, s.interruptionPolicyForAgent(agentDef))
	loopCtx = tools.WithRunID(loopCtx, run.ID)
	loopCtx = tools.WithDispatcher(loopCtx, dispatcher)
	// Cooperative pause quiesce (RFC X / F41) — synchronous handler, defer-deregister.
	gate, deregGate := s.newPauseGate(run.ID)
	defer deregGate()
	// RFC X Phase 3: expose the gate to the Agent tool (parallel_spawn parent park).
	loopCtx = tools.WithPauseGate(loopCtx, gate)
	fbPolicy, fbReResolve := s.fallbackForRun(sess.TenantID, sess.Agent, body.UserTier)
	loopRes, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:               provider,
		Model:                  model,
		Tools:                  allowedTools,
		Dispatcher:             dispatcher,
		Segments:               injectMetadataSegments(segments, provider.Capabilities().MetadataViaInput, body.Metadata, nil),
		PriorMessages:          priorMessages,
		PauseGate:              gate,
		OnEvent:                emit,
		OnHeartbeat:            heartbeat,
		MaxTokens:              agentDef.MaxTokens,     // 0 → driver default
		MaxIterations:          agentDef.MaxIterations, // 0 → loop default (16)
		UnboundedIterations:    agentDef.UnboundedIterations,
		SteerQueue:             steerQ,
		OnSteer:                onSteer,
		Effort:                 effort,
		MarkStalled:            s.markStalledFn(providerID, model),
		MarkRateLimited:        s.markRateLimitedFn(body.UserTier),
		ClearStall:             s.clearStallFn(providerID, model),
		ToolParallelism:        s.cfg.Env.ToolParallelism,
		AgentName:              sess.Agent,
		CodeBody:               agentDef.Code, // inline code-js body (RFC J); "" → FS fallback
		Metadata:               body.Metadata,
		RunTimeoutSeconds:      pickRunTimeout(body.RunTimeoutSeconds, agentDef.RunTimeoutSeconds),
		Interactive:            body.Interactive,
		Sampling:               config.MergeSampling(agentDef.Sampling, body.Sampling),       // per-run wins per field
		Compaction:             config.MergeCompaction(agentDef.Compaction, body.Compaction), // per-run wins per field
		ContextPlugins:         s.contextPlugins,                                             // RFC Z runtime-wide chain (code-js exempt in the loop)
		UserTier:               body.UserTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForAgent(agentDef, body.UserTier),
	})
	if runErr != nil {
		stream.send(providers.Event{Type: providers.EventError, Error: runErr.Error()})
	}

	s.finishRunWithCancel(r.Context(), runCtx, run.ID, loopRes, runErr, meta)
}

// replayTranscript walks the persisted events of a session and reconstructs
// the conversation history as []providers.Message, ready to feed into
// loop.Run via PriorMessages.
//
// The structure of a run in the event log:
//   - user_input        — segments the caller posted (one per run start)
//   - text              — assistant text deltas
//   - tool_call         — assistant requested a tool
//   - tool_result       — loop reports tool output (next user turn)
//   - usage / done      — loop bookkeeping; ignored for replay
//
// Each run boundary (new user_input event) marks the end of the previous
// assistant/user-tool-result turn pair.
func replayTranscript(events []store.Event) []providers.Message {
	var messages []providers.Message
	var asstText strings.Builder
	var asstTools []providers.ContentBlock
	var pendingToolResults []providers.ContentBlock
	// asstReasoning carries reasoning_content captured from the
	// iteration's "done" event so the rebuilt assistant Message can
	// echo it back to the API on continuation. Required by DeepSeek
	// V4 Pro / deepseek-reasoner — without it, the next request 400s
	// with "reasoning_content in the thinking mode must be passed
	// back". Empty for non-thinking models.
	var asstReasoning string

	flushAssistant := func() {
		if asstText.Len() == 0 && len(asstTools) == 0 {
			return
		}
		var content []providers.ContentBlock
		if asstText.Len() > 0 {
			content = append(content, providers.ContentBlock{Type: "text", Text: asstText.String()})
		}
		content = append(content, asstTools...)
		messages = append(messages, providers.Message{
			Role:      "assistant",
			Content:   content,
			Reasoning: asstReasoning,
		})
		asstText.Reset()
		asstTools = nil
		asstReasoning = ""
	}
	flushPendingTools := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		messages = append(messages, providers.Message{Role: "user", Content: pendingToolResults})
		pendingToolResults = nil
	}

	for _, ev := range events {
		switch ev.Type {
		case "user_input":
			// New user turn: flush any in-progress assistant + tool_result accumulation.
			flushAssistant()
			flushPendingTools()
			var segs []loop.PromptSegment
			if err := json.Unmarshal(ev.Payload, &segs); err != nil {
				continue
			}
			var userBlocks []providers.ContentBlock
			for _, seg := range segs {
				if seg.Role != "user" {
					continue
				}
				for _, c := range seg.Content {
					userBlocks = append(userBlocks, loop.FlattenContent(c))
				}
			}
			if len(userBlocks) > 0 {
				messages = append(messages, providers.Message{Role: "user", Content: userBlocks})
			}
		case "text":
			// New assistant turn starting → close any prior user(tool_result)
			// turn that's still pending. We can't use "usage" as the boundary
			// because the loop emits usage BEFORE tool_result within an
			// iteration (see loop.go:163 vs loop.go:178), so usage-as-flush
			// would close the user turn before the tool_results land in it.
			flushPendingTools()
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil {
				asstText.WriteString(pe.Text)
			}
		case "tool_call":
			// Same reasoning as "text": this is a new assistant turn signal.
			flushPendingTools()
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil && pe.ToolUse != nil {
				asstTools = append(asstTools, providers.ContentBlock{
					Type:      "tool_use",
					ToolUseID: pe.ToolUse.ID,
					ToolName:  pe.ToolUse.Name,
					ToolInput: pe.ToolUse.Input,
				})
			}
		case "tool_result":
			// The assistant turn that emitted tool_use is now complete; flush it.
			flushAssistant()
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil && pe.ToolUse != nil {
				pendingToolResults = append(pendingToolResults, providers.ContentBlock{
					Type:      "tool_result",
					ToolUseID: pe.ToolUse.ID,
					Text:      pe.Text,
					IsError:   pe.IsError,
				})
			}
			// Don't flush pendingToolResults yet — multiple tools at the
			// same boundary belong to one user message, and the next text
			// or tool_call event will close this user turn.
		case "done":
			// Capture reasoning_content (if present) BEFORE the flush
			// so the rebuilt assistant Message carries it. Mid-
			// conversation, this done event also marks the end of
			// the iteration's assistant turn — done arrives in the
			// stream BEFORE tool_result events, so the flush here
			// commits the assistant Message with reasoning attached.
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil {
				asstReasoning = pe.Reasoning
			}
			// End-of-run boundary — used both mid-conversation (the
			// per-iteration assistant turn closes here) and at the
			// very end (final iteration with purely textual output,
			// no tool_results to carry over).
			flushAssistant()
			flushPendingTools()
		case "context_compaction":
			// Interactive context compaction: everything before this marker
			// collapses to the summary. RESET all accumulated state and seed the
			// same user+assistant summary pair the live loop swapped in, so a
			// rebuild (crash recovery / resume / /sessions/messages) reconstructs
			// the compacted conversation, not the full history. Any events AFTER
			// the marker (turns since the compaction) replay normally on top.
			flushAssistant()
			flushPendingTools()
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil && pe.ContextCompaction != nil {
				cc := pe.ContextCompaction
				// Keep the same last-N of the accumulated history the live loop
				// kept, and pin the first message when keep_first — the marker
				// records both, so the rebuild is byte-identical to what the loop
				// produced (counts align: system prompt is excluded from both).
				keepN := cc.KeepN
				if keepN < 0 {
					keepN = 0
				}
				if keepN > len(messages) {
					keepN = len(messages)
				}
				tail := append([]providers.Message(nil), messages[len(messages)-keepN:]...)
				pinned := ""
				if cc.KeepFirst && len(messages) > 0 {
					for _, c := range messages[0].Content {
						if c.Type == "text" {
							pinned += c.Text
						}
					}
				}
				messages = loop.CompactionMessages(pinned, cc.Summary, tail)
			}
			asstText.Reset()
			asstTools = nil
			pendingToolResults = nil
			asstReasoning = ""
		}
	}
	flushAssistant()
	flushPendingTools()
	return messages
}

// transcriptResponse is the JSON shape of GET /v1/sessions/{id}/transcript.
type transcriptResponse struct {
	Session store.Session     `json:"session"`
	Events  []transcriptEvent `json:"events"`
}

// transcriptEvent is one event row, with payload re-decoded into a typed
// providers.Event so the caller doesn't have to round-trip through
// json.RawMessage. ts is unix-nanos so it round-trips losslessly.
//
// v0.9.x: for event types whose payload doesn't map cleanly onto
// providers.Event (user_input carries []loop.PromptSegment;
// system_prompt carries {system_prompt, agent_def_id, skill_def_ids}),
// the raw JSON is surfaced via the Payload sidecar so adapters /
// Web UI parse it without inflating providers.Event with
// transcript-only fields. Empty for the streaming event types.
type transcriptEvent struct {
	Seq     int64           `json:"seq"`
	RunID   string          `json:"run_id"`
	TsNs    int64           `json:"ts_ns"`
	Type    string          `json:"type"`
	Event   providers.Event `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "transcript persistence is not configured", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id is required", http.StatusBadRequest)
		return
	}
	// RFC L: the transcript exposes the session's full history — gate it on the
	// same tenant ownership as continuation. The accessor folds a cross-tenant
	// session into the same opaque *store.ErrNotFound a missing one returns.
	sess, err := s.tenantStore(r.Context()).GetSession(r.Context(), id)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	transcript, err := s.store.GetTranscript(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := transcriptResponse{Session: sess, Events: make([]transcriptEvent, 0, len(transcript))}
	for _, ev := range transcript {
		te := transcriptEvent{
			Seq:   ev.Seq,
			RunID: ev.RunID,
			TsNs:  ev.Timestamp.UnixNano(),
			Type:  ev.Type,
		}
		// v0.9.x: payload-only event types (no fields on providers.Event)
		// — surface the raw JSON via the Payload sidecar so the Web UI
		// and adapter consumers can parse it directly. The Event field
		// stays minimal (just the Type) for these.
		switch ev.Type {
		case "user_input", "system_prompt":
			te.Event = providers.Event{Type: providers.EventType(ev.Type)}
			te.Payload = append(json.RawMessage(nil), ev.Payload...)
		default:
			// Decode payload back to a typed Event. If it fails (corrupt
			// row), surface a minimal record so the rest of the
			// transcript still ships.
			if err := json.Unmarshal(ev.Payload, &te.Event); err != nil {
				te.Event = providers.Event{Type: providers.EventType(ev.Type)}
			}
		}
		resp.Events = append(resp.Events, te)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("transcript: encode failed: %v", err)
	}
}

// openOrCreateSessionAndRun resolves the session (creating one if the caller
// didn't pass an ID), then creates a run inside it. Returns ("", "", nil) when
// no store is configured — the caller treats both empty IDs as "skip persistence".
//
// userID is forwarded into the new session row (empty when the caller didn't
// supply one). identity carries the v0.4 tracking fields for the new run; for
// continuation requests on an existing session, the run inherits the session's
// user_id automatically via the denormalised UserID on RunIdentity (caller is
// responsible for setting that when continuing — typically copied from
// GetSession).
func (s *Server) openOrCreateSessionAndRun(ctx context.Context, requestedSessionID, agent, tenantID, userID string, identity store.RunIdentity) (string, string, error) {
	if s.store == nil {
		return "", "", nil
	}
	var sess store.Session
	var err error
	if requestedSessionID != "" {
		sess, err = s.store.GetSession(ctx, requestedSessionID)
		if err != nil {
			return "", "", err
		}
		// RFC L: continuing an existing session via POST /v1/runs runs under
		// that session's stored identity — enforce cross-principal ownership
		// (opaque not-found on mismatch).
		if !sessionOwnershipOK(ctx, sess) {
			return "", "", &store.ErrNotFound{Kind: "session", ID: requestedSessionID}
		}
	} else {
		sess, err = s.store.CreateSession(ctx, tenantID, agent, userID)
		if err != nil {
			return "", "", err
		}
	}
	// Denormalise session.UserID onto the new run if the caller didn't
	// supply one (cv-rewriter etc. always have one path; continuation
	// inherits via the session lookup).
	if identity.UserID == "" {
		identity.UserID = sess.UserID
	}
	run, err := s.store.CreateRun(ctx, sess.ID, identity)
	if err != nil {
		return "", "", err
	}
	return sess.ID, run.ID, nil
}

// makeRecordingEmit returns an OnEvent callback that records each event into
// the store before forwarding to the SSE stream. Persistence failures are
// logged but never block the stream — the caller has already received the
// event and should not be punished for our IO problems.
//
// Concurrency: returns a closure with a private mutex so the (AppendEvent,
// fwd) pair is atomic per event. Without the mutex, two concurrent emit
// callers (e.g., parallel tool goroutines from v0.7.0 ToolParallelism)
// could interleave their AppendEvent + fwd pairs so the store's event
// order disagrees with the SSE wire order. v0.7.x's pattern was "only the
// loop goroutine calls emit," but v0.8.4's Channel-tool typed-audit-events
// (EventChannelPublish / EventChannelDelivery) emit directly from inside
// tool.Execute() — which runs in a tool goroutine, not the loop. The
// mutex makes that safe for any concurrent caller.
// makeSteer wires a run's operator steering queue (PR 2 / interactive
// terminal). It registers the run in the steer registry and returns the
// loop's SteerQueue, the OnSteer callback, and a deregister func (defer it so
// even a panic cleans up). OnSteer persists each drained instruction as a
// "user_input" transcript event (so a continuation replay rebuilds the
// conversation) and emits the live EventSteer via the run's emit. Returns
// (nil, nil, noop) when steering isn't wired (registry nil / no run_id) so
// callers pass nil into RunOptions and the loop's steering path stays off.
func (s *Server) makeSteer(ctx context.Context, runID, agentID, sessionID, userID string, emit func(providers.Event)) (<-chan steer.Message, func(steer.Message), func()) {
	if s.steerReg == nil || runID == "" {
		return nil, nil, func() {}
	}
	q, dereg := s.steerReg.Register(steer.Entry{
		RunID:     runID,
		AgentID:   agentID,
		SessionID: sessionID,
		UserID:    userID,
	})
	onSteer := func(m steer.Message) {
		if s.store != nil {
			seg := []loop.PromptSegment{{
				Role:    "user",
				Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: m.Text}},
			}}
			if b, err := json.Marshal(seg); err == nil {
				if err := s.store.AppendEvent(ctx, runID, "user_input", b); err != nil {
					log.Printf("steer: persist user_input failed (run=%s): %v", runID, err)
				}
			}
		}
		emit(providers.Event{Type: providers.EventSteer, UserInput: &providers.UserInputEventInfo{
			Text: m.Text, Source: m.Source, SeenAt: time.Now().UTC().Format(time.RFC3339Nano),
		}})
	}
	return q, onSteer, dereg
}

func (s *Server) makeRecordingEmit(ctx context.Context, runID string, fwd func(providers.Event)) func(providers.Event) {
	if s.store == nil || runID == "" {
		// Even on the store-less path, multiple concurrent callers
		// could write to fwd in parallel. fwd itself (stream.send)
		// is already mutex-protected for the SSE path, so the bare
		// fwd is safe; just return it.
		return fwd
	}
	var mu sync.Mutex
	return func(ev providers.Event) {
		// Track parked (awaiting_input) state for the compaction boundary gate
		// (handleCompactRun's IsParked check). The run is parked when it emits
		// awaiting_input and resumes work on the next steer/text/tool_call. No-op
		// for non-interactive runs (they never emit awaiting_input) and for
		// run_ids not in the steer registry. SetParked takes its own lock.
		if s.steerReg != nil {
			switch ev.Type {
			case providers.EventAwaitingInput:
				s.steerReg.SetParked(runID, true)
			case providers.EventSteer, providers.EventText, providers.EventToolCall:
				s.steerReg.SetParked(runID, false)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		// EventSteer (operator steering, PR 2) is forwarded LIVE only — the
		// runner's OnSteer persists the operator instruction as a separate
		// "user_input" transcript row (the shape replayTranscript rebuilds the
		// conversation from). Persisting the steer Event too would double-write
		// and feed replayTranscript an Event where it expects []PromptSegment.
		if ev.Type == providers.EventSteer {
			fwd(ev)
			return
		}
		// RFC X Phase 3: the spawn ledger (spawn_child_started / spawn_child_result)
		// is a STORE-side durability mechanism for reconstructing a parked fan-out
		// parent on resume — the mirror image of EventSteer above. It must NOT reach
		// live SSE/gRPC consumers: it's not a client-facing event, and eventToProto
		// carries no SpawnChild payload (a gRPC client would get a typed-but-empty
		// frame). Persist, don't forward.
		if ev.Type == providers.EventSpawnChildStarted || ev.Type == providers.EventSpawnChildResult {
			payload, err := json.Marshal(ev)
			if err == nil {
				if err := s.store.AppendEvent(ctx, runID, string(ev.Type), payload); err != nil {
					log.Printf("store: AppendEvent failed (run=%s type=%s): %v", runID, ev.Type, err)
				}
			}
			return
		}
		// F32: redact secrets out of the PERSISTED copy only — the tool_call
		// input and tool_result text are the surfaces where a token inlined on a
		// Bash cmdline / echoed by a tool would otherwise hit the events BLOB
		// (and, downstream, snapshots + the /v1/_events audit API). fwd() below
		// still forwards the ORIGINAL event: the live SSE caller is the trust
		// boundary that already holds the secret, so we don't mangle its stream.
		toStore := ev
		if s.redactor.Enabled() && (ev.Type == providers.EventToolCall || ev.Type == providers.EventToolResult) {
			if ev.ToolUse != nil {
				tu := *ev.ToolUse // copy so we don't mutate the event fwd() sends
				tu.Input = s.redactor.Bytes(tu.Input)
				toStore.ToolUse = &tu
			}
			toStore.Text = s.redactor.String(ev.Text)
		}
		payload, err := json.Marshal(toStore)
		if err == nil {
			if err := s.store.AppendEvent(ctx, runID, string(ev.Type), payload); err != nil {
				log.Printf("store: AppendEvent failed (run=%s type=%s): %v", runID, ev.Type, err)
			}
		}
		fwd(ev)
	}
}

// secretEnvValues extracts the VALUES of secret-classified env vars (by name)
// from an environ slice ("NAME=value" entries), for the F32 redactor's exact-
// match tier. Empty values are skipped (nothing to mask).
func secretEnvValues(environ []string) map[string]string {
	out := make(map[string]string)
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name, val := kv[:i], kv[i+1:]
		if val == "" || !config.IsSecretEnvName(name) {
			continue
		}
		out[name] = val
	}
	return out
}

// runSubAgent is the SubAgentRunner closure injected into the Agent
// built-in tool. It resolves the named agent through lookup.Agent
// (cfg.Agents → dynamic_agents → agent_def_active), builds a fresh
// session+run for the sub-execution, drives loop.Run with the
// sub-agent's full declared tool set, and returns the FinalText.
//
// Trust model: the sub-agent's `allowed_tools` is the SOLE authority on
// its tool surface. Parent and child are both operator-vetted YAML
// definitions; neither widens nor narrows the other. The Agent tool's
// availability to the parent is the gate (a parent without "Agent" in
// its allowed_tools cannot call this in the first place).
//
// Sub-runs are persisted as their OWN sessions for replayability. The
// parent's transcript records only the tool_call (with input) and
// tool_result (with the sub's final text); the sub's intermediate
// events are reachable via GET /v1/sessions/{sub-session-id}/transcript.
//
// Concurrency: sub-runs DO NOT acquire a fresh semaphore slot. They run
// inside the parent's slot — the entire run tree counts as one against
// MAX_CONCURRENT_RUNS. This avoids deadlocks at low concurrency caps
// and matches the cost model (a parent's compute budget already covers
// the work it delegates).
//
// Errors propagate as Go errors back to the Agent tool, which surfaces
// them as IsError tool_results to the parent's model rather than
// tearing down the parent run.
// runSubAgent runs a named sub-agent to completion. The second return is the
// child's run row id (RFC X Phase 3) — "" when the run was never created (an
// early resolution/provider error), otherwise the sub-run's id even on failure
// so the parent's spawn ledger can re-find it. (Wired to AgentTool.Run.)
func (s *Server) runSubAgent(ctx context.Context, name string, prompt string, defID string) (string, string, error) {
	// RFC N: a parent in tenant T resolves the sub-agent name within T's
	// view (parent tenant flows via ctx RunIdentity, inherited by every
	// sub-agent). Confirms RFC N's open-question on cross-boundary spawn:
	// the lookup is tenant-scoped, so a parent cannot spawn another
	// tenant's private agent by name.
	def, ok := lookup.Agent(ctx, s.store, s.cfg, tenantFromCtx(ctx), name)
	if !ok {
		return "", "", fmt.Errorf("unknown sub-agent %q (not in cfg.Agents, dynamic_agents, or agent_def_active)", name)
	}

	// v0.8.5 substrate: when defID is set, overlay the named def's
	// mutable fields (system_prompt, allowed_tools, model, tier,
	// effort, max_tokens, memory_scopes, etc.) over the static
	// cfg.Agents entry for this single sub-run. Name mismatch is a
	// hard refuse — pinning across names would let a parent bypass
	// the operator's static agent boundary.
	if defID != "" {
		if s.store == nil {
			return "", "", fmt.Errorf("Agent tool: def_id pinning requires a configured store backend")
		}
		row, err := s.store.AgentDefGet(ctx, defID)
		if err != nil {
			return "", "", fmt.Errorf("Agent tool: def_id %q lookup failed: %w", defID, err)
		}
		if row.Name != name {
			return "", "", fmt.Errorf("Agent tool: def_id %q is for agent %q, not %q (cross-name pinning refused)", defID, row.Name, name)
		}
		if row.Retired {
			return "", "", fmt.Errorf("Agent tool: def_id %q is retired", defID)
		}
		def = applyAgentDefOverlay(def, row.Definition)
	}

	// v0.8.2: inherit parent's user_tier via ctx. The Agent built-in
	// tool can't pass it explicitly (the tool's input schema is
	// caller-authoritative). Reading from ctx keeps the same tier
	// policy + fallback posture applied to the whole sub-run tree.
	parentTier := tools.RunIdentity(ctx).UserTier

	// Route through resolveAgentDef so an overlay's Model/Tier/Provider/
	// Effort actually take effect. resolveAgent re-reads cfg.Agents and
	// would silently discard the fork's values.
	providerID, model, effort, err := s.resolveAgentDef(def, name, parentTier)
	if err != nil {
		return "", "", fmt.Errorf("resolve sub-agent %q model: %w", name, err)
	}
	provider, err := s.providers.Get(providerID)
	if err != nil {
		return "", "", fmt.Errorf("provider for sub-agent %q: %w", name, err)
	}

	// Read parent's identity from ctx to inherit user_id and pin
	// parent_agent_id on the sub-run. tools.RunIdentity returns zero
	// value if the parent didn't set it — sub-agents spawned from
	// callers that didn't supply user_id naturally inherit empty.
	parentIdentity := tools.RunIdentity(ctx)

	// Generate a fresh agent_id for the sub-run. Always generated;
	// callers can't override (the sub is loomcycle-controlled).
	subAgentID := newAgentID()

	// Sub-run gets its OWN session, under the PARENT's tenant (RFC L). The
	// session row's tenant_id must match the run's (subIdentity.TenantID below) —
	// they're created together in openOrCreateSessionAndRun — or the tenant-gated
	// reads (transcript / continuation via s.tenantStore) 404 the sub-agent
	// session for its own tenant operator while the run is still visible.
	subIdentity := store.RunIdentity{
		AgentID:       subAgentID,
		ParentAgentID: parentIdentity.AgentID,
		ReplicaID:     s.replicaID,
		// ParentRunID is left empty here — we don't have the parent's
		// run.ID handy without an extra registry lookup. Cascade
		// works via parent_agent_id alone; ParentRunID is informational
		// for transcript stitching and can be filled in by a future
		// refactor that threads parent run.ID through ctx.
		UserID:     parentIdentity.UserID,
		TenantID:   parentIdentity.TenantID, // RFC L: sub-runs inherit the parent's authoritative tenant
		UserTier:   parentIdentity.UserTier, // v0.8.2: same user_tier across the sub-run tree
		AgentDefID: defID,                   // v0.8.5: pin defID on the sub-run for evaluation denormalisation
		Model:      model,                   // resolved model — written at create so the UI sees it during the run
		// v0.12.x: the root's opaque tracking lineage flows UNCHANGED to
		// every descendant (Clone so child + parent don't alias). This is
		// the propagation seam — remove it and child run rows lose their
		// link back to the user-initiated request.
		ParentContext: parentIdentity.ParentContext.Clone(),
	}
	subSessionID, subRunID, err := s.openOrCreateSessionAndRun(ctx, "", name, parentIdentity.TenantID, parentIdentity.UserID, subIdentity)
	if err != nil {
		return "", "", fmt.Errorf("create sub-session for %q: %w", name, err)
	}

	// RFC X Phase 3 spawn ledger: when this sub-run is a parallel_spawn child
	// (the parent threaded a spawn index + the parent's tool_use id is on ctx)
	// and the feature is on, record a spawn_child_started row on the PARENT's
	// transcript NOW — while the child run id is known but the child may park
	// before it completes. The emitter on ctx is still the parent's (the
	// sub-run's own emitter is swapped in below). Gives a restored fan-out
	// parent the (index → run_id) mapping to await + re-collect this child.
	if s.cfg.Env.ResumeFanout {
		if idx, ok := tools.SpawnIndex(ctx); ok {
			if tuID := tools.ToolUseID(ctx); tuID != "" {
				if emit := tools.EventEmitter(ctx); emit != nil {
					emit(providers.Event{
						Type: providers.EventSpawnChildStarted,
						SpawnChild: &providers.SpawnChildEventInfo{
							ToolUseID: tuID,
							Index:     idx,
							RunID:     subRunID,
							Agent:     name,
						},
					})
				}
			}
		}
	}

	// Register the sub-run in the cancel registry so a parent-cancel
	// can cascade through. Sub uses parent's runCtx (passed in via
	// ctx) — a parent ctx-cancel already tears down the sub-loop, but
	// the registry entry lets the cascade walk in Cancel() find this
	// sub explicitly (belt-and-braces against grandchild races).
	subRunCtx, subCancelFn := context.WithCancelCause(ctx)
	defer subCancelFn(nil)
	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:       subAgentID,
		RunID:         subRunID,
		SessionID:     subSessionID,
		UserID:        parentIdentity.UserID,
		ParentAgentID: parentIdentity.AgentID,
		StartedAt:     time.Now(),
	}, subCancelFn)
	if regErr != nil {
		// A duplicate-active collision is essentially impossible here
		// (subAgentID is freshly generated); log and continue without
		// registering rather than fail the run for a registry hiccup.
		log.Printf("cancel registry: sub-agent register failed (%s): %v", subAgentID, regErr)
	} else {
		defer s.cancelReg.Deregister(subAgentID)
	}
	// v0.10.0 OTEL: sub-runs open their own loomcycle.run span. The
	// span is automatically a child of the parent's
	// loomcycle.iteration span via ctx propagation (the Agent built-in
	// tool's Execute runs inside an iteration span; spawning a sub-run
	// passes that ctx through to runSubAgent → here). Operators see
	// the full multi-level run tree in Jaeger.
	subRunCtx, subRunSpan := lcotel.RecordRunStart(subRunCtx, lcotel.RunStartAttrs{
		RunID:         subRunID,
		AgentID:       subAgentID,
		AgentName:     name,
		UserID:        parentIdentity.UserID,
		ParentAgentID: parentIdentity.AgentID,
	})
	defer subRunSpan.End()

	// v0.9.x: sub-agent meta for runstate.Bus. ParentAgentID lets
	// SSE subscribers see the lineage edge.
	subMeta := runStateMeta{
		RunID:         subRunID,
		AgentID:       subAgentID,
		Agent:         name,
		UserID:        parentIdentity.UserID,
		TenantID:      parentIdentity.TenantID, // sub-agent inherits the parent run's tenant
		ParentAgentID: parentIdentity.AgentID,
		otelSpan:      subRunSpan,
		ParentContext: parentIdentity.ParentContext, // v0.12.x: sub-agent's run-state events carry the root's lineage
	}
	s.publishRunState(subMeta, "running", "", "")

	// v0.8.22: rebuild SystemPrompt from per-run SkillDef bodies
	// when any of the sub-agent's skills has a DB-active row. Same
	// call as the three top-level run-creation sites — without it,
	// sub-agents would silently keep the static baked body and
	// SkillDef promotions never take effect for agents only spawned
	// as sub-agents.
	// RFC N: a sub-agent runs with the parent's RunIdentity already on
	// ctx, so tenantFromCtx(ctx) returns the parent's (== the run's)
	// authoritative tenant — resolve the sub-agent's skills there.
	def, promptProv := s.resolveSkillBodiesForRun(ctx, tenantFromCtx(ctx), def)
	// Build segments: agent's system_prompt (with cache_control) + the
	// caller-supplied prompt as the first user message. Mirrors the
	// shape of /v1/runs.
	var segs []loop.PromptSegment
	if def.SystemPrompt != "" {
		segs = append(segs, loop.PromptSegment{
			Role: "system",
			Content: []loop.PromptContentBlock{{
				Type:      "trusted-text",
				Text:      def.SystemPrompt,
				Cacheable: true,
			}},
		})
	}
	segs = append(segs, loop.PromptSegment{
		Role: "user",
		Content: []loop.PromptContentBlock{{
			Type: "trusted-text",
			Text: prompt,
		}},
	})

	// RFC N FIX 2-mcp: sub-agents run with the parent's RunIdentity already
	// on ctx, so tenantFromCtx returns the run's authoritative tenant —
	// the sub-agent advertises the same tenant's MCP tools as its parent.
	subTools := filterTools(s.candidateTools(ctx, tenantFromCtx(ctx), def.AllowedTools), def.AllowedTools, nil)
	// Inherit the parent's caller-authoritative host policy. Without
	// this, sub-agents fall back to the operator's static
	// HTTPHostAllowlist — which typically doesn't include localhost
	// callbacks — and a parent that worked against ["localhost"]
	// silently spawns children that can't reach the caller's API.
	// Production case: cv-batch-adapter (parent has localhost via
	// caller-authoritative) → cv-adapter children that need to PATCH
	// /api/applications/<id> back to jobs-search-web, hit
	// "host \"localhost\" not in allowlist", waste iterations
	// guessing hostnames, get capped by max_iterations, never write
	// the documents (2026-05-06).
	parentHostPolicy := tools.HostPolicy(ctx)
	if parentHostPolicy.HasList || s.cfg.Env.HTTPCallerAuthoritative {
		subTools = builtin.NarrowHosts(subTools, parentHostPolicy.AllowedHosts, parentHostPolicy.WebSearchFilter, s.cfg.Env.HTTPCallerAuthoritative)
	}
	subDispatcher := s.newDispatcher(subTools)

	// Persist the input segments as the first event so transcript
	// replay reconstructs the user prompt the same way fresh runs do.
	if s.store != nil && subRunID != "" {
		if inputJSON, err := json.Marshal(segs); err == nil {
			if err := s.store.AppendEvent(ctx, subRunID, "user_input", inputJSON); err != nil {
				log.Printf("store: AppendEvent(user_input) failed for sub-run %s: %v", subRunID, err)
			}
		}
	}
	// v0.9.x: also persist the resolved system prompt + provenance.
	// On sub-runs we have the explicit def_id (parameter), so the
	// event payload includes agent_def_id — unique to this path.
	s.emitSystemPromptEvent(ctx, subRunID, def.SystemPrompt, defID, promptProv)

	// Sub-emit records to the sub's transcript only — the parent's SSE
	// stream is fwd=no-op so sub events don't bleed into the parent's
	// event stream. The parent observes only the wrapping
	// tool_call/tool_result on its own stream.
	subEmit := s.makeRecordingEmit(ctx, subRunID, func(providers.Event) {})

	// Sub-run gets ITS OWN agent tools attached to ctx — the parent's
	// tool list does not leak to the child (and vice versa). This
	// matches the trust model: each agent's allowed_tools is its own
	// authority for any subset checks done inside its run.
	//
	// The sub's run identity is also threaded through ctx so a
	// recursive Agent tool call from this sub picks up the right
	// parent_agent_id (= subAgentID).
	subCtx := tools.WithAgentTools(subRunCtx, toolNames(subTools))
	subCtx = tools.WithRunIdentity(subCtx, tools.RunIdentityValue{
		UserID:          parentIdentity.UserID,
		TenantID:        parentIdentity.TenantID, // RFC L: sub-agents inherit the parent's authoritative tenant (same isolation boundary)
		AgentID:         subAgentID,
		RootRunID:       parentIdentity.RootRunID,             // RFC AH Phase 2b: INHERIT the tree's root id (do NOT overwrite)
		UserTier:        parentIdentity.UserTier,              // v0.8.2: sub-agents inherit parent's user_tier
		AgentDefID:      defID,                                // v0.8.7: surface pinned def_id via Context.self
		UserBearer:      parentIdentity.UserBearer,            // v0.8.x: bearer inherited identically (same end-user)
		UserCredentials: parentIdentity.UserCredentials,       // v1.x RFC F: credentials map inherited identically
		ParentContext:   parentIdentity.ParentContext.Clone(), // v0.12.x: tracking lineage flows to grandchildren too
	})
	subCtx = tools.WithAgentName(subCtx, name)
	// Sub-agents get THEIR OWN Memory policy from yaml — the parent's
	// memory_scopes do NOT cascade. This matches the existing
	// `allowed_tools` model: a child's surface is its own yaml's
	// authority. Cross-agent state-sharing is what the `user` scope
	// is for; sub-agents that share state with their parent simply
	// both list `user` (or `agent` keyed by a shared name) in their
	// memory_scopes.
	subCtx = tools.WithMemoryPolicy(subCtx, tools.MemoryPolicyValue{
		AllowedScopes: def.MemoryScopes,
		QuotaBytes:    def.MemoryQuotaBytes,
		Backend:       def.MemoryBackend,
	})
	// RFC AA: the sub-agent's SQL Memory ACL is its OWN def's sql_scopes
	// (like memory/channels, not inherited down the tree). Empty →
	// default-deny. The run-scope DB keys off RootRunID (inherited unchanged
	// above), so a child granted `run` reads/writes the SAME ephemeral DB as
	// the rest of the tree — dropped once at the top-level run's completion.
	subCtx = tools.WithSqlMemPolicy(subCtx, tools.SqlMemPolicyValue{
		AllowedScopes: def.SqlScopes,
		QuotaBytes:    def.SqlQuotaBytes,
	})
	// Compaction flows DOWN the spawn tree (unlike memory/channels/sampling,
	// which are the child's own). The parent's effective policy (on ctx) wins per
	// field; the child def fills any field the PARENT left unset; a per-spawn
	// Agent-tool override (also on ctx) wins over both. The result is stamped on
	// subCtx so the child's OWN children inherit it (recursive), and passed to
	// the sub-loop's RunOptions for its auto-compaction.
	subCompaction := config.MergeCompaction(def.Compaction, tools.CompactionPolicy(ctx))
	subCompaction = config.MergeCompaction(subCompaction, tools.CompactionOverride(ctx))
	subCtx = tools.WithCompactionPolicy(subCtx, subCompaction)
	// RFC AH §4 — the load-bearing spawn invariant: a child's volume set is
	// the NARROW-ONLY intersection of (child-declared) ∩ (parent's active
	// bindings). A sub-agent can never gain a volume its parent lacks, and
	// where both hold one the ro/rw axis resolves to the more restrictive.
	// Mirrors the host-allowlist narrowing read from ctx just above (4376).
	subCtx = tools.WithVolumePolicy(subCtx, s.childVolumePolicy(subCtx, tools.VolumePolicy(ctx), def))
	// RFC AH Phase 2b: the run-tree's ephemeral volume SET is deliberately NOT
	// re-attached here — subCtx derives from the parent's ctx (via subRunCtx ←
	// WithCancelCause(ctx)), so the SAME *EphemeralVolumeSet pointer flows down
	// unchanged. A sub-agent creating an ephemeral volume thus registers it in
	// the shared set the whole tree resolves through, and it is purged with the
	// tree at the top-level run's completion (sub-agents never purge).
	// Sub-agent's Channel policy follows the same per-yaml shape as
	// MemoryPolicy above. The Channels map (operator-declared
	// channels) IS shared with the parent — those are operator
	// state, not agent state. The ALLOWLISTS (publish / subscribe)
	// come from the child's yaml.
	subCtx = tools.WithChannelPolicy(subCtx, s.channelPolicyForAgent(subCtx, def))
	// Sub-agent event emitter writes to the SUB's transcript (per
	// subEmit above). Channel-tool publishes from inside the sub
	// surface on the sub's SSE stream, not the parent's.
	subCtx = tools.WithEventEmitter(subCtx, subEmit)
	// Sub-agent's substrate policies come from ITS OWN yaml — same
	// shape as Memory/Channel. selfName is the sub's name so the
	// "self" scope resolves to the sub-agent's identity, not the
	// parent's.
	subADPolicy, subEvPolicy := s.substratePoliciesForAgent(def, name)
	subCtx = tools.WithAgentDefPolicy(subCtx, subADPolicy)
	subCtx = tools.WithSkillDefPolicy(subCtx, s.skillDefPolicyForAgent(def))
	subCtx = tools.WithVolumeDefPolicy(subCtx, s.volumeDefPolicyForAgent(def))
	subCtx = tools.WithEvaluationPolicy(subCtx, subEvPolicy)
	subCtx = tools.WithHistoryPolicy(subCtx, s.historyPolicyForAgent(def))
	subCtx = tools.WithInterruptionPolicy(subCtx, s.interruptionPolicyForAgent(def))
	subCtx = tools.WithRunID(subCtx, subRunID)
	subCtx = tools.WithDispatcher(subCtx, subDispatcher)

	subHeartbeat := s.makeHeartbeat(subRunID)

	// Cooperative pause quiesce (RFC X / F41): a sub-agent parks at its own
	// boundary while paused (so a fan-out tree quiesces). NOT admission-gated —
	// it's a child of an already-admitted run, not new top-level work.
	subGate, deregSubGate := s.newPauseGate(subRunID)
	defer deregSubGate()
	// RFC X Phase 3: expose the gate so a NESTED fan-out parent (a sub-agent
	// that itself parallel_spawns) can park mid-tool-call too.
	subCtx = tools.WithPauseGate(subCtx, subGate)

	fbPolicy, fbReResolve := s.fallbackForRun(tenantFromCtx(ctx), name, parentTier)
	res, runErr := loop.Run(subCtx, loop.RunOptions{
		Provider:            provider,
		Model:               model,
		Tools:               subTools,
		Dispatcher:          subDispatcher,
		Segments:            segs,
		OnEvent:             subEmit,
		OnHeartbeat:         subHeartbeat,
		PauseGate:           subGate,
		MaxTokens:           def.MaxTokens,     // 0 → driver default
		MaxIterations:       def.MaxIterations, // 0 → loop default (16)
		UnboundedIterations: def.UnboundedIterations,
		Effort:              effort,
		MarkStalled:         s.markStalledFn(providerID, model),
		MarkRateLimited:     s.markRateLimitedFn(parentTier),
		ClearStall:          s.clearStallFn(providerID, model),
		ToolParallelism:     s.cfg.Env.ToolParallelism,
		AgentName:           name,
		CodeBody:            def.Code, // inline code-js body (RFC J); "" → FS fallback
		// A code-js sub-agent's wall-clock budget is its OWN per-agent
		// run_timeout_seconds — a spawn has no ad-hoc per-run knob, so
		// per-agent is the sole source (== pickRunTimeout(0, def...)). Without
		// this, the 4th run-creation site falls back to the global default,
		// dropping the budget for the fan-out orchestrator case the per-agent
		// override exists to serve.
		RunTimeoutSeconds: def.RunTimeoutSeconds,
		// Sub-agents use their OWN def's sampling (no per-spawn override yet —
		// a breeder varies temperature by FORKING a def, then spawning it).
		Sampling:               def.Sampling,
		Compaction:             subCompaction,
		ContextPlugins:         s.contextPlugins, // RFC Z runtime-wide chain (sub-agents included; code-js exempt in the loop)
		UserTier:               parentTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForAgent(def, parentTier),
	})
	s.finishRunWithCancel(ctx, subRunCtx, subRunID, res, runErr, subMeta)

	if runErr != nil {
		// Wrap with session/run IDs so a developer reading parent logs
		// can locate the sub's transcript directly. The parent agent's
		// model sees the unwrapped error message. agent_id is the v0.4
		// addressable handle, so include it too — the easiest hint for
		// "GET /v1/agents/<this>" debugging.
		return "", subRunID, fmt.Errorf("sub-agent %q failed (agent=%s session=%s run=%s): %w",
			name, subAgentID, subSessionID, subRunID, runErr)
	}
	// Surface the sub agent_id to the parent agent's transcript by
	// prefixing the tool_result text. Parent caller's model sees this
	// and can echo it to the UI. Cheap; unblocks future "cancel only
	// the sub" UX.
	return formatSubAgentOutput(subAgentID, res.FinalText), subRunID, nil
}

// agentResponse is the JSON shape returned by GET /v1/agents/{id} and
// each entry in GET /v1/users/{user_id}/agents. Mirrors store.Run plus
// a flag distinguishing live (still in the cancel registry) from
// terminated.
//
// The response intentionally avoids exposing internal fields like
// loomcycle's session_id when the caller didn't already know it; we
// include it because the same caller almost always has the session_id
// from the original SSE stream and surfacing it here keeps things
// debug-friendly.
type agentResponse struct {
	AgentID   string `json:"agent_id"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	// Agent is the YAML-declared agent name (e.g. "qa-agent",
	// "company-researcher"). Surfaced from the parent session via
	// SQL JOIN so consumers don't have to fetch the session row
	// separately. Empty when the session row is missing (manual
	// pruning / very-old data).
	Agent           string             `json:"agent,omitempty"`
	UserID          string             `json:"user_id,omitempty"`
	ParentAgentID   string             `json:"parent_agent_id,omitempty"`
	Status          store.RunStatus    `json:"status"`
	StartedAt       time.Time          `json:"started_at"`
	CompletedAt     *time.Time         `json:"completed_at,omitempty"`
	StopReason      string             `json:"stop_reason,omitempty"`
	Error           string             `json:"error,omitempty"`
	Usage           agentResponseUsage `json:"usage"`
	LastHeartbeatAt *time.Time         `json:"last_heartbeat_at,omitempty"`
	Live            bool               `json:"live"`
	// Interactive marks a persistent interactive run (started with
	// interactive:true; parks at end_turn for operator steering). Surfaced
	// from runs.interactive so the Web UI can tag interactive runs and list
	// re-attachable interactive sessions. Omitted for ordinary runs.
	Interactive bool `json:"interactive,omitempty"`
	// v0.8.21 awaited-state surface — what the running agent is
	// currently blocked on. Empty for non-running rows AND for
	// running rows where the agent is making progress (no
	// unresolved Channel.subscribe / Interruption.ask). Two-field
	// shape avoids encoding a parser into the wire:
	//   AwaitedState = "" | "channel" | "interrupted"
	//   AwaitedOn    = channel name (when state=channel) or
	//                  interruption kind (when state=interrupted)
	AwaitedState string `json:"awaited_state,omitempty"`
	AwaitedOn    string `json:"awaited_on,omitempty"`
	// v0.12.x cluster-mode surface — which replica owns the run's live
	// cancel handle. Empty (and omitted from JSON) in single-replica
	// deployments so the UI stays uncluttered for the common case.
	ReplicaID string `json:"replica_id,omitempty"`
	// v0.12.x parent_context — the opaque caller-tracking lineage this
	// run carries (inherited from its root for sub-agents). Echoed here
	// alongside Usage so a consumer can attribute a child sub-agent's
	// cost to the user-initiated request in a single fetch. Omitted when
	// the run carried no context.
	ParentContext *store.ParentContext `json:"parent_context,omitempty"`
}

type agentResponseUsage struct {
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheCreationTokens int    `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int    `json:"cache_read_tokens,omitempty"`
	Model               string `json:"model,omitempty"`
	// Provider is the provider id that actually served the final
	// successful iteration of the run (e.g. "anthropic", "deepseek").
	// Distinct from Model so post-run analysis can tell
	// primary-provider runs from runtime-fallback routed runs (the
	// v0.8.2 tryProviderFallback path mutates opts.Provider in place
	// when a 429 / 5xx triggers a switch). Empty for pre-migration
	// rows and for runs that never reached a provider call.
	Provider string `json:"provider,omitempty"`
}

// runToAgentResponse converts a store.Run into the API response shape.
// `live` indicates whether the cancel registry still has an entry —
// distinguishes "running and cancellable" from "running per the row but
// the registry doesn't know about it" (which can happen after a process
// restart). The HTTP layer surfaces this flag so the UI can decide
// whether to offer a Cancel button.
func runToAgentResponse(r store.Run, live bool) agentResponse {
	resp := agentResponse{
		AgentID:       r.AgentID,
		RunID:         r.ID,
		SessionID:     r.SessionID,
		Agent:         r.Agent,
		UserID:        r.UserID,
		ParentAgentID: r.ParentAgentID,
		Status:        r.Status,
		StartedAt:     r.StartedAt,
		StopReason:    r.StopReason,
		Error:         r.ErrorMsg,
		Usage: agentResponseUsage{
			InputTokens:         r.InputTokens,
			OutputTokens:        r.OutputTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			CacheReadTokens:     r.CacheReadTokens,
			Model:               r.Model,
			Provider:            r.Provider,
		},
		Live:          live,
		Interactive:   r.Interactive,
		ReplicaID:     r.ReplicaID,
		ParentContext: r.ParentContext, // v0.12.x: echo tracking lineage alongside usage
	}
	if !r.CompletedAt.IsZero() {
		t := r.CompletedAt
		resp.CompletedAt = &t
	}
	if !r.LastHeartbeatAt.IsZero() {
		t := r.LastHeartbeatAt
		resp.LastHeartbeatAt = &t
	}
	return resp
}

// handleGetAgent serves GET /v1/agents/{agent_id}. Returns the most
// recent run carrying the agent_id, with its current status. 404 when
// neither the registry nor the store knows the id.
func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent_id")
	if !validIdent(agentID) {
		http.Error(w, `agent_id must match [A-Za-z0-9_-]{1,128}`, http.StatusBadRequest)
		return
	}
	if s.store == nil {
		// Without persistence we can only answer "live in registry" —
		// no historical runs to query. ONE Get call so a concurrent
		// Deregister between a check and a read doesn't return a
		// half-populated entry.
		entry, ok := s.cancelReg.Get(agentID)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"code":"unknown_agent_id","error":"no live run for %q (no store configured)"}`, agentID)
			return
		}
		writeJSON(w, http.StatusOK, agentResponse{
			AgentID:   agentID,
			RunID:     entry.RunID,
			SessionID: entry.SessionID,
			UserID:    entry.UserID,
			Status:    store.RunRunning,
			StartedAt: entry.StartedAt,
			Live:      true,
			// The registry only holds local entries, so a hit here is
			// always owned by the responding replica. Empty in
			// single-replica mode (omitted from JSON via omitempty).
			ReplicaID: s.replicaID,
		})
		return
	}
	// Multi-tenant authz: the accessor folds a cross-tenant run into the same
	// *store.ErrNotFound a missing run returns, so the not-found branch below
	// covers both — a cross-tenant probe gets the identical opaque 404 (no
	// existence oracle). Super-admin / legacy / open mode see all.
	run, err := s.tenantStore(r.Context()).GetRunByAgentID(r.Context(), agentID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"code":"unknown_agent_id","error":"no run found for agent_id %q"}`, agentID)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, live := s.cancelReg.Get(agentID)
	resp := runToAgentResponse(run, live)
	if resp.Status == store.RunRunning {
		single := []agentResponse{resp}
		fillAwaitedStateForRunning(r.Context(), s.store, single)
		resp = single[0]
	}
	writeJSON(w, http.StatusOK, resp)
}

// cancelRequest is the (optional) JSON body for POST /v1/agents/{id}/cancel.
type cancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

// cancelResponse is the JSON shape returned by the cancel endpoint.
type cancelResponse struct {
	Cancelled bool     `json:"cancelled"`
	AgentID   string   `json:"agent_id"`
	Cascaded  []string `json:"cascaded,omitempty"`
	Status    string   `json:"status,omitempty"` // present on idempotent re-cancel of a terminated run
	Reason    string   `json:"reason,omitempty"`
}

// handleCancelAgent serves POST /v1/agents/{agent_id}/cancel. Cancels
// the in-flight run (and cascading children); idempotent — a second
// cancel of a terminated run returns 200 with the prior status rather
// than 404.
func (s *Server) handleCancelAgent(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent_id")
	if !validIdent(agentID) {
		http.Error(w, `agent_id must match [A-Za-z0-9_-]{1,128}`, http.StatusBadRequest)
		return
	}

	var body cancelRequest
	if r.ContentLength > 0 {
		// Best-effort decode — empty body is fine.
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body)
	}

	res, ok := s.cancelReg.Cancel(agentID, body.Reason)
	if ok {
		// v0.12.2: Cancel may now return ok=true with res.Cancelled=false
		// when the cluster canceller found the run but couldn't cancel
		// it (owner_replica_unreachable, owner_dead_marked_failed,
		// already-terminal). Surface res.Cancelled directly instead of
		// hardcoding true.
		writeJSON(w, http.StatusOK, cancelResponse{
			Cancelled: res.Cancelled,
			AgentID:   agentID,
			Cascaded:  res.Cascaded,
			Reason:    res.Reason,
		})
		return
	}

	// Not in registry — either already terminated, or never existed.
	// Distinguish via the store: a row exists → terminated (idempotent
	// 200); no row → 404.
	if s.store == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"code":"unknown_agent_id","error":"no live or terminated run for %q (no store configured)"}`, agentID)
		return
	}
	run, err := s.store.GetRunByAgentID(r.Context(), agentID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"code":"unknown_agent_id","error":"no run found for agent_id %q"}`, agentID)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Idempotent: surface the existing terminal status. cancelled=false
	// signals "this call did not initiate the cancel" but is still 200.
	writeJSON(w, http.StatusOK, cancelResponse{
		Cancelled: false,
		AgentID:   agentID,
		Status:    string(run.Status),
	})
}

// handleListUserAgents serves GET /v1/users/{user_id}/agents?status=running.
// Returns at most 100 runs ordered by started_at DESC.
func (s *Server) handleListUserAgents(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if !validIdent(userID) {
		http.Error(w, `user_id must match [A-Za-z0-9_-]{1,128}`, http.StatusBadRequest)
		return
	}
	if s.store == nil {
		http.Error(w, "list-by-user requires persistence (Store not configured)", http.StatusNotFound)
		return
	}
	statusFilter := store.RunStatus(r.URL.Query().Get("status"))
	// Default to running — the most useful view for "what's in flight
	// for me?". Pass status=all to override.
	if statusFilter == "" {
		statusFilter = store.RunRunning
	}
	if statusFilter == "all" {
		statusFilter = ""
	}
	runs, err := s.store.ListActiveRunsByUser(r.Context(), userID, statusFilter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Multi-tenant authz: a tenant principal sees only runs in its own
	// tenant (requesting another tenant's user_id yields an empty list);
	// super-admin sees all, or focuses one tenant via ?tenant= (the UI's
	// tenant switcher). allTenants=true → no tenant filter.
	scopeTenant, allTenants := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	out := make([]agentResponse, 0, len(runs))
	for _, run := range runs {
		if !allTenants && run.TenantID != scopeTenant {
			continue
		}
		_, live := s.cancelReg.Get(run.AgentID)
		out = append(out, runToAgentResponse(run, live))
	}
	fillAwaitedStateForRunning(r.Context(), s.store, out)
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

// ---- Interruption (v0.8.16) ---------------------------------------

// resolveInterruptRequest is the JSON body for the resolve endpoint.
// kind-discriminated: v0.8.16 supports only kind="question"; future
// kinds (pause/wait_until/approval) parse different fields.
type resolveInterruptRequest struct {
	Kind       string `json:"kind"`
	Answer     string `json:"answer"`
	ResolvedBy string `json:"resolved_by,omitempty"`
}

// handleResolveInterrupt accepts a human's answer to a pending
// interruption + wakes the blocked tool. The path captures
// {run_id, interrupt_id}; the body carries the kind-discriminated
// payload. 422 on invalid answer, 409 on already-terminal, 410 on
// expired-but-not-yet-swept.
func (s *Server) handleResolveInterrupt(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "interrupts require persistence (Store not configured)", http.StatusServiceUnavailable)
		return
	}
	runID := r.PathValue("run_id")
	interruptID := r.PathValue("interrupt_id")
	if !validIdent(runID) || !validIdent(interruptID) {
		http.Error(w, "run_id / interrupt_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}

	var req resolveInterruptRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
		return
	}
	if req.Kind == "" {
		req.Kind = store.InterruptKindQuestion
	}
	if req.Kind != store.InterruptKindQuestion {
		// v0.8.16 supports only "question". Future kinds add their
		// own validators here; the closed-enum contract means the
		// resolve handler is the gate, not the model or the
		// caller.
		http.Error(w, fmt.Sprintf("unsupported kind %q (v0.8.16 supports: question)", req.Kind), http.StatusUnprocessableEntity)
		return
	}
	if req.ResolvedBy == "" {
		// Authoritative attribution: cookie session (Web UI) writes
		// "webui"; bearer-only API calls write "api". The Web UI
		// resolve flow runs through the same handler.
		if hasSessionCookie(r) {
			req.ResolvedBy = store.InterruptResolvedByWebUI
		} else {
			req.ResolvedBy = store.InterruptResolvedByAPI
		}
	}

	// Validate the answer against the stored row's options + expiry.
	row, err := s.store.InterruptGet(r.Context(), interruptID)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		http.Error(w, "interrupt not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if row.RunID != runID {
		// Defensive: the URL path's run_id must match the stored
		// row's run_id. Prevents one user's resolve from being
		// retargeted at another run's interrupt by URL manipulation.
		http.Error(w, "interrupt does not belong to that run", http.StatusNotFound)
		return
	}
	if row.Status != store.InterruptStatusPending {
		if row.Status == store.InterruptStatusTimedOut && !row.ExpiresAt.IsZero() && row.ExpiresAt.Before(time.Now()) {
			http.Error(w, "interrupt expired", http.StatusGone)
			return
		}
		http.Error(w, fmt.Sprintf("interrupt already %s", row.Status), http.StatusConflict)
		return
	}
	if !row.ExpiresAt.IsZero() && row.ExpiresAt.Before(time.Now()) {
		http.Error(w, "interrupt expired", http.StatusGone)
		return
	}

	// Option-list validation: when the original ask declared
	// options, the answer must be one of them. Free-text answers
	// (no options) accept any non-empty string.
	if len(row.Options) > 0 {
		var opts []string
		if err := json.Unmarshal(row.Options, &opts); err == nil && len(opts) > 0 {
			ok := false
			for _, o := range opts {
				if o == req.Answer {
					ok = true
					break
				}
			}
			if !ok {
				http.Error(w, fmt.Sprintf("answer %q is not one of the declared options: %v", req.Answer, opts), http.StatusUnprocessableEntity)
				return
			}
		}
	} else if req.Answer == "" {
		http.Error(w, "answer is required for free-text interrupts", http.StatusUnprocessableEntity)
		return
	}

	if err := s.store.InterruptResolve(r.Context(), interruptID, req.Answer, req.ResolvedBy, nil); err != nil {
		if errors.Is(err, store.ErrInterruptAlreadyTerminal) {
			http.Error(w, "interrupt already resolved, timed out, or cancelled", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Wake the blocked tool. Bus.Notify is best-effort — if no
	// waiter, it's a no-op. If we crash before Notify fires, the
	// next bus.Wait cycle exits via timeout and the storage row
	// is the source of truth.
	if s.interruptionBus != nil {
		s.interruptionBus.Notify("intr:" + interruptID)
	}

	// Publish the external `_system/interrupts/resolved` signal so
	// non-run Channel subscribers (dashboards, Slack bots) see the
	// terminal state. Best-effort.
	if s.systemPublisher != nil && row.UserID != "" {
		payload, _ := json.Marshal(map[string]any{
			"interrupt_id": interruptID,
			"run_id":       runID,
			"kind":         row.Kind,
			"status":       store.InterruptStatusResolved,
			"answer":       req.Answer,
			"resolved_by":  req.ResolvedBy,
		})
		_, _ = s.systemPublisher.PublishNow(
			r.Context(),
			"_system/interrupts/resolved",
			store.MemoryScopeUser, row.UserID,
			payload, channels.SystemPublisherUserID, 0, 0,
		)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"interrupt_id": interruptID,
		"status":       store.InterruptStatusResolved,
		"resolved_at":  time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// runInputRequest is the JSON body for POST /v1/runs/{run_id}/input.
type runInputRequest struct {
	Text string `json:"text"`
}

// handleRunInput serves POST /v1/runs/{run_id}/input — inject an operator
// "steering" instruction into an in-flight run (PR 2 / interactive terminal).
// The text is appended to the running conversation as a user turn at the top
// of the loop's next iteration. 404 if no run is live for run_id (start a new
// turn via POST /v1/sessions/{id}/messages instead); 429 if the run's input
// buffer is full; 422 on empty text. Cross-tenant steers get an opaque 404.
func (s *Server) handleRunInput(w http.ResponseWriter, r *http.Request) {
	if s.steerReg == nil {
		http.Error(w, "steering is not enabled on this server", http.StatusServiceUnavailable)
		return
	}
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	var req runInputRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "text is required", http.StatusUnprocessableEntity)
		return
	}

	// Resolve the authoritative source at the HTTP auth boundary (cookie →
	// webui, else api), then dispatch through the shared connector method
	// (tenant-ownership gate + steerReg.Push, incl. cross-replica routing).
	// The tenant gate folds a cross-tenant steer into an opaque ErrRunNotInFlight
	// 404 — run_ids are not secrets (returned to callers + shown in the UI), so
	// the gate must not become an existence oracle.
	source := store.InterruptResolvedByAPI
	if hasSessionCookie(r) {
		source = store.InterruptResolvedByWebUI
	}
	delivered, err := s.SteerRun(r.Context(), runID, text, source)
	switch {
	case errors.Is(err, connector.ErrSteerQueueFull):
		w.Header().Set("Retry-After", "1")
		http.Error(w, "run input queue full; retry shortly", http.StatusTooManyRequests)
		return
	case errors.Is(err, connector.ErrRunNotInFlight):
		http.Error(w, "no in-flight run for that run_id", http.StatusNotFound)
		return
	case errors.Is(err, connector.ErrSteeringUnavailable):
		http.Error(w, "steering is not enabled on this server", http.StatusServiceUnavailable)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "delivered": delivered})
}

// minCompactMessages: a conversation shorter than this isn't worth a model call.
const minCompactMessages = 4

// compactResponse is the JSON body of POST /v1/runs/{run_id}/compact — an alias
// for the canonical connector.CompactResult so the HTTP wire and the connector
// surface (the MCP compact_run tool, gRPC) share exactly one shape.
type compactResponse = connector.CompactResult

// compactErr carries the HTTP status + machine code + retry hint a CompactRun
// failure should surface, so the operation can be shared between the HTTP
// handler (which maps these onto the response) and the connector tools (MCP /
// gRPC), which surface only the message as a tool error. A zero status is
// treated as 500.
type compactErr struct {
	status     int
	code       string // non-empty → writeJSONError(status, code, msg); else plain http.Error
	msg        string
	retryAfter int // seconds; >0 → set Retry-After (queue-full back-pressure)
}

func (e *compactErr) Error() string { return e.msg }

// HTTPStatus exposes the carried status so a non-HTTP transport (the gRPC
// CompactRun handler) maps this error to the matching code by asserting the
// interface{ HTTPStatus() int } — without importing this unexported type.
func (e *compactErr) HTTPStatus() int { return e.status }

// handleCompactRun serves POST /v1/runs/{run_id}/compact — summarize the run's
// conversation to free context and continue from the summary (interactive
// context compaction). Gated to a safe boundary: a live run must be PARKED
// (awaiting_input); mid-turn returns 409. Computes the summary with one model
// call (the run's resolved provider/model), then either pushes a compact
// control to the live loop (it swaps its in-memory history + emits the persisted
// marker) or, for a terminal run with no live loop, persists the marker directly
// so the next continuation rebuilds compacted. Cross-tenant gets an opaque 404.
func (s *Server) handleCompactRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	// Preserve the webui-vs-api source attribution on the HTTP path (a cookie'd
	// /run terminal vs a programmatic caller); the connector CompactRun uses API.
	res, err := s.compactRunWithSource(r.Context(), runID, compactSource(r))
	if err != nil {
		var ce *compactErr
		if errors.As(err, &ce) {
			status := ce.status
			if status == 0 {
				status = http.StatusInternalServerError
			}
			if ce.retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(ce.retryAfter))
			}
			if ce.code != "" {
				writeJSONError(w, status, ce.code, ce.msg)
			} else {
				http.Error(w, ce.msg, status)
			}
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// CompactRun is the connector-surface compaction op (the MCP compact_run tool,
// gRPC, future CLI) — the same operation as POST /v1/runs/{run_id}/compact,
// attributed to the API source. A run-state mutation, so transports gate it on
// runs:create. Run lookup, tenant-ownership, the parked-boundary gate, and both
// apply paths (live steer push vs terminal marker) are shared with the HTTP
// handler via compactRunWithSource.
func (s *Server) CompactRun(ctx context.Context, runID string) (connector.CompactResult, error) {
	return s.compactRunWithSource(ctx, runID, store.InterruptResolvedByAPI)
}

// compactRunWithSource is the shared compaction core. It returns a *compactErr
// on every failure path (carrying the status/code the HTTP handler maps) and a
// CompactResult on success — including the Compacted:false "noop" outcome for a
// conversation too short to summarize. `source` attributes the steer-push (api
// vs webui) for audit.
func (s *Server) compactRunWithSource(ctx context.Context, runID, source string) (connector.CompactResult, error) {
	if s.store == nil {
		return connector.CompactResult{}, &compactErr{status: http.StatusServiceUnavailable, msg: "compaction requires persistence"}
	}
	// Tenant-ownership via the tenant-scoped accessor: a cross-tenant (or
	// missing) run both fold into the same opaque 404 (run_ids aren't secrets,
	// so the gate must not become an existence oracle). Shared by the HTTP
	// handler and the gRPC/MCP CompactRun — both carry the principal in ctx.
	run, err := s.tenantStore(ctx).GetRun(ctx, runID)
	if err != nil {
		return connector.CompactResult{}, &compactErr{status: http.StatusNotFound, msg: "no run for that run_id"}
	}

	terminal := isTerminalRunStatus(run.Status)
	live := s.steerReg != nil && func() bool { _, ok := s.steerReg.Get(runID); return ok }()
	// Boundary gate (user-chosen: safe boundary only). A LOCAL live run must be
	// parked; refuse mid-turn so compaction applies at the boundary the agent is
	// already at rather than being deferred into the current turn.
	if live && !terminal && !s.steerReg.IsParked(runID) {
		return connector.CompactResult{}, &compactErr{status: http.StatusConflict, code: "run_busy",
			msg: "the agent is mid-turn; compact when it's parked waiting for your input"}
	}

	// Rebuild the conversation from this run's transcript (== the parked loop's
	// in-memory history — a parked run has persisted everything up to the park).
	events, terr := s.store.GetTranscript(ctx, run.SessionID)
	if terr != nil {
		return connector.CompactResult{}, &compactErr{status: http.StatusInternalServerError, msg: "read transcript: " + terr.Error()}
	}
	runEvents := make([]store.Event, 0, len(events))
	for _, e := range events {
		if e.RunID == runID {
			runEvents = append(runEvents, e)
		}
	}
	msgs := replayTranscript(runEvents)
	before := estimateMessageTokens(msgs)
	if len(msgs) < minCompactMessages {
		return connector.CompactResult{RunID: runID, Compacted: false, BeforeTokens: before, AfterTokens: before, Applied: "noop"}, nil
	}

	// Resolve provider/model + summarize (one model call) — same chain resume
	// uses; no per-run secrets needed (the operator's provider key serves it).
	agentDef, ok := s.lookupAgent(ctx, run.TenantID, run.Agent)
	if !ok {
		return connector.CompactResult{}, &compactErr{status: http.StatusConflict, code: "agent_gone", msg: "the run's agent no longer exists"}
	}
	providerID, model, _, rerr := s.resolveAgentDef(agentDef, run.Agent, run.UserTier)
	if rerr != nil {
		return connector.CompactResult{}, &compactErr{status: http.StatusInternalServerError, msg: "resolve provider/model: " + rerr.Error()}
	}
	provider, perr := s.providers.Get(providerID)
	if perr != nil {
		return connector.CompactResult{}, &compactErr{status: http.StatusServiceUnavailable, msg: "provider unavailable: " + perr.Error()}
	}

	// Resolve the run's compaction keep-N / keep-first / target / summary-model
	// (agent-def settings; defaults applied) so a manual compact keeps recent
	// turns + pins the task exactly like auto-compaction does.
	keepLastN, keepFirst, targetPct := config.CompactionDefaultKeepLastN, config.CompactionDefaultKeepFirst, config.CompactionDefaultTargetPct
	summaryModel := model
	if c := agentDef.Compaction; c != nil {
		if c.KeepLastN != nil {
			keepLastN = *c.KeepLastN
		}
		if c.KeepFirst != nil {
			keepFirst = *c.KeepFirst
		}
		if c.TargetPercentage != nil {
			targetPct = *c.TargetPercentage
		}
		if c.Model != nil && *c.Model != "" {
			summaryModel = *c.Model
		}
	}
	// CompactionSplit (shared with the loop) picks what to summarize vs keep.
	firstIdx, cut, splitOK := loop.CompactionSplit(msgs, keepLastN, keepFirst)
	if !splitOK {
		return connector.CompactResult{RunID: runID, Compacted: false, BeforeTokens: before, AfterTokens: before, Applied: "noop"}, nil
	}
	summary, serr := loop.Summarize(ctx, provider, summaryModel, msgs[firstIdx:cut], targetPct)
	if serr != nil {
		return connector.CompactResult{}, &compactErr{status: http.StatusBadGateway, msg: "summarize: " + serr.Error()}
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return connector.CompactResult{}, &compactErr{status: http.StatusBadGateway, msg: "summarization produced no text"}
	}
	keepN := len(msgs) - cut
	pinned := ""
	if firstIdx > 0 {
		for _, c := range msgs[0].Content {
			if c.Type == "text" {
				pinned += c.Text
			}
		}
	}
	after := estimateMessageTokens(loop.CompactionMessages(pinned, summary, msgs[cut:]))

	// Apply. Live (local or cross-replica, routed by steerReg.Push): push the
	// compact control (summary + keep-N/keep-first) — the loop swaps its history
	// + emits the persisted marker. Terminal: no loop, so persist the marker here
	// for the next continuation.
	applied := "marker"
	if !terminal && s.steerReg != nil {
		_, pushErr := s.steerReg.Push(ctx, runID, steer.Message{
			Kind: steer.KindCompact, Text: summary, KeepN: keepN, KeepFirst: firstIdx > 0,
			Source: source, EnqueuedAt: time.Now(),
		})
		switch {
		case errors.Is(pushErr, steer.ErrQueueFull):
			return connector.CompactResult{}, &compactErr{status: http.StatusTooManyRequests, msg: "run input queue full; retry shortly", retryAfter: 1}
		case errors.Is(pushErr, steer.ErrRunNotFound):
			// Raced to terminal between GetRun and Push → fall through to marker.
		case pushErr != nil:
			return connector.CompactResult{}, &compactErr{status: http.StatusInternalServerError, msg: pushErr.Error()}
		default:
			applied = "live"
		}
	}
	if applied == "marker" {
		payload, merr := json.Marshal(providers.Event{
			Type: providers.EventContextCompaction,
			ContextCompaction: &providers.ContextCompactionEventInfo{
				Summary: summary, KeepN: keepN, KeepFirst: firstIdx > 0,
				BeforeTokens: before, AfterTokens: after, Trigger: "manual",
			},
		})
		if merr == nil {
			if aerr := s.store.AppendEvent(ctx, runID, string(providers.EventContextCompaction), payload); aerr != nil {
				return connector.CompactResult{}, &compactErr{status: http.StatusInternalServerError, msg: "persist compaction marker: " + aerr.Error()}
			}
		}
	}
	return connector.CompactResult{RunID: runID, Compacted: true, BeforeTokens: before, AfterTokens: after, Applied: applied}, nil
}

// compactSource mirrors handleRunInput's api-vs-webui source resolution.
func compactSource(r *http.Request) string {
	if hasSessionCookie(r) {
		return store.InterruptResolvedByWebUI
	}
	return store.InterruptResolvedByAPI
}

// estimateMessageTokens is a cheap chars/4 heuristic (no tokenizer) over message
// text + tool I/O — enough for the operator-facing "context compacted (N→M)"
// readout, not for billing.
func estimateMessageTokens(msgs []providers.Message) int {
	chars := 0
	for _, m := range msgs {
		for _, c := range m.Content {
			chars += len(c.Text) + len(c.ToolInput)
		}
	}
	return chars / 4
}

// handleListRunInterrupts serves GET /v1/runs/{run_id}/interrupts.
// Capped at 200 rows by the store; ordering is created_at DESC.
func (s *Server) handleListRunInterrupts(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "interrupts require persistence", http.StatusServiceUnavailable)
		return
	}
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "all" {
		statusFilter = ""
	}
	// Tenant-ownership via the accessor: it gates on the OWNING run's tenant
	// (interrupts have no tenant column — they inherit the run's). A
	// cross-tenant or unknown run folds into *store.ErrNotFound, which we map
	// to an empty list — indistinguishable from a real run with zero
	// interrupts, so the listing can't be a cross-tenant existence oracle.
	rows, err := s.tenantStore(r.Context()).InterruptListByRun(r.Context(), runID, statusFilter)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSON(w, http.StatusOK, map[string]any{"interrupts": []store.InterruptRow{}, "total": 0})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interrupts": rows, "total": len(rows)})
}

// handleListUserInterrupts serves GET /v1/users/{user_id}/interrupts.
// Drives the Web UI inbox view. Same status filter as the run-scoped
// variant; defaults to pending (most useful view for "what do I need
// to answer?").
func (s *Server) handleListUserInterrupts(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "interrupts require persistence", http.StatusServiceUnavailable)
		return
	}
	userID := r.PathValue("user_id")
	if !validIdent(userID) {
		http.Error(w, "user_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		statusFilter = store.InterruptStatusPending
	}
	if statusFilter == "all" {
		statusFilter = ""
	}
	// Tenant-scope the inbox: a tenant principal sees only interrupts on its
	// own tenant's runs (the accessor passes the principal's tenant down to the
	// store JOIN); super-admin / legacy / open mode see all. Without this a
	// token could read another tenant's pending questions by guessing a
	// user_id (user_ids are not secret). Mirrors handleListUsers' scoping.
	rows, err := s.tenantStore(r.Context()).InterruptListByUser(r.Context(), userID, statusFilter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interrupts": rows, "total": len(rows)})
}

// hasSessionCookie returns true when the request carries the
// Web UI session cookie — drives the resolved_by attribution.
func hasSessionCookie(r *http.Request) bool {
	c, err := r.Cookie("loomcycle_session")
	return err == nil && c != nil && c.Value != ""
}

// writeJSON is a small helper for the new endpoints that avoids
// repeating the Content-Type + WriteHeader + Encode dance.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("writeJSON encode failed: %v", err)
	}
}

// runStateMeta is the per-run identity bundle the finishRun* helpers
// need to publish a meaningful event on the runstate.Bus. Every call
// site (handleRuns / handleMessages / runSubAgent / connector
// spawn_run + their collision-fail / register-fail subpaths) has the
// fields in scope at the moment they call finishRun*; assembling the
// struct is one line at each site.
//
// Zero-valued meta is safe (every finishRun* helper nil-guards on
// s.runStateBus and publishes the event verbatim — the SSE handler's
// filter shape tolerates empty agent / user_id).
type runStateMeta struct {
	RunID         string
	AgentID       string
	Agent         string
	UserID        string
	TenantID      string // run's authoritative tenant — gates the user-agents stream
	ParentAgentID string
	// IsTopLevel marks a TOP-LEVEL run (handleRuns / handleMessages / resume)
	// vs a sub-agent run (RFC AH Phase 2b). finishRun purges the run tree's
	// ephemeral volumes ONLY for a top-level run — a sub-agent completing must
	// NOT tear down the tree its siblings + parent still use. Defaults false
	// (a zero meta from an early-failure path never purges).
	IsTopLevel bool
	// RootRunID is the top-level run id at the root of this spawn tree, used
	// by finishRun to derive the ephemeral subtree to purge (RFC AH Phase 2b).
	// Set to the run's own id at every top-level site.
	RootRunID string
	// ParentContext is the run's opaque tracking lineage, echoed on the
	// published RunStateEvent (v0.12.x).
	ParentContext *store.ParentContext
	// otelSpan is the top-level loomcycle.run span the four run-creation
	// sites open before kicking off the loop. finishRun* attaches final
	// attributes (usage totals, stop_reason, error status) to it via
	// lcotel.SetRunDone right before End() — End() itself is deferred at
	// the open site. nil-safe (zero meta from tests + early-failure
	// paths produces nil; SetRunDone is a no-op then).
	otelSpan trace.Span
}

// publishRunState fans out one event to the v0.9.x runstate.Bus.
// No-op when the bus isn't wired (tests / minimal embeddings).
func (s *Server) publishRunState(m runStateMeta, status, stopReason, errMsg string) {
	if s.runStateBus == nil {
		return
	}
	s.runStateBus.Publish(runstate.RunStateEvent{
		RunID:         m.RunID,
		AgentID:       m.AgentID,
		Agent:         m.Agent,
		UserID:        m.UserID,
		TenantID:      m.TenantID,
		ParentAgentID: m.ParentAgentID,
		Status:        status,
		StopReason:    stopReason,
		Error:         errMsg,
		ParentContext: m.ParentContext,
	})
}

// makeHeartbeat returns a callback the loop fires at each iteration.
// It updates runs.last_heartbeat_at via a fire-and-forget background
// context (the loop's ctx may be cancelled mid-write; the heartbeat
// shouldn't gate on it).
//
// nil store or runID makes this a no-op so v0.2 callers stay
// hands-off.
//
// The per-call timeout is sized for pool-saturation tolerance, not for
// heartbeat freshness. At the launch crest of the v0.12.9 x5000 load
// test (15 K agent runs, ~140 concurrent), 10 heartbeat UPDATEs timed
// out within a 1-second window because the pgxpool was briefly burst-
// saturated by concurrent Memory.set / Channel.publish / AppendEvent
// calls. The pool acquire blocked past the prior 1-second deadline,
// the inherited ctx fired, and the heartbeat logged "context deadline
// exceeded". The runs themselves were unaffected (heartbeats are
// advisory; the sweeper is the authority on stale-run detection), but
// the operator-visible noise + the theoretical risk of the sweeper
// misfiring on a slightly older last_heartbeat_at warranted a fix.
//
// 5 seconds is chosen because:
//   - Heartbeats fire once per loop iteration (~100-200ms cadence
//     with a mock provider; ~1-3s with a real provider). A 5s budget
//     means at most 1-2 missed beats under sustained pool saturation
//     before the next iteration arrives — and "1-2 missed beats" is
//     well within the sweeper's stale-detection window (60s+ by
//     default per `heartbeat.SweeperConfig`).
//   - The substrate-level retryOnTransientConn helper added in PR
//     #246 doesn't apply here because the error is ctx.DeadlineExceeded,
//     not SQLSTATE 53300. The fix is to give the heartbeat enough
//     budget to ride out the pool's natural acquire jitter.
func (s *Server) makeHeartbeat(runID string) func() {
	if s.store == nil || runID == "" {
		return nil
	}
	return func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.UpdateHeartbeat(bg, runID); err != nil {
			// Log only — never fail the run on a heartbeat hiccup.
			log.Printf("store: UpdateHeartbeat(%s) failed: %v", runID, err)
		}
	}
}

// finishRunWithCancel is the cause-aware version of finishRun. It
// inspects context.Cause(runCtx) to discriminate API-cancel
// (cancel.ErrCancelledByAPI sentinel attached) from client-disconnect
// (plain ctx.Canceled, no cause) and other errors. API-cancel writes
// status=cancelled with the reason; everything else falls through to
// finishRun's existing failed/completed logic.
//
// runCtx is the per-run ctx derived from the HTTP request ctx via
// context.WithCancelCause. ctx (the first arg) is the outer ctx used
// only by finishRun for its store write — passing both keeps the
// background-write fallback in finishRun reusable for both code paths.
func (s *Server) finishRunWithCancel(ctx context.Context, runCtx context.Context, runID string, res loop.RunResult, runErr error, meta runStateMeta) {
	// RFC AH Phase 2b: tear down this run tree's ephemeral volumes at
	// terminal — but ONLY for a TOP-LEVEL run (a sub-agent completing must
	// not purge the tree its parent + siblings still use). Deferred so it
	// fires after either terminal-write branch below. Re-derives the subtree;
	// best-effort (the sweeper backstops a fault).
	if meta.IsTopLevel {
		defer s.purgeEphemeralVolumesForRun(meta.RootRunID)
	}
	if cause := context.Cause(runCtx); errors.Is(cause, cancel.ErrCancelledByAPI) {
		// API-cancel terminal write. Reason text comes from the
		// optional wrapper; falls back to the sentinel string.
		reason := cancel.ReasonFromCause(cause)
		if reason == "" {
			reason = "cancelled by api"
		}
		s.finishRunCancelled(ctx, runID, res, reason, meta)
		return
	}
	// A run whose context was cancelled WITHOUT an API-cancel cause ended
	// because the caller went away — most commonly a CLIENT DISCONNECT on a
	// non-interactive run (its runCtx derives from the request ctx, so a
	// dropped connection cancels it), or an upstream/parent cascade. Record it
	// as a clean `cancelled`, not `failed: "context canceled"`: the run didn't
	// fail, the caller left. This is the difference between a status poll
	// seeing a tidy `cancelled` vs a half-written `failed` row (the symptom
	// JobEmber hit — its disconnected batch runs showed up as provider-ish
	// failures). finishRunCancelled writes the terminal row under a fresh
	// background ctx, so it persists even though runCtx is cancelled.
	//
	// A run TIMEOUT surfaces as context.DeadlineExceeded (not Canceled), so it
	// still falls through to finishRun's failed path — a timeout IS a failure.
	if runCtx.Err() != nil && errors.Is(runErr, context.Canceled) {
		reason := "client disconnected"
		if !meta.IsTopLevel {
			reason = "parent run cancelled"
		}
		s.finishRunCancelled(ctx, runID, res, reason, meta)
		return
	}
	s.finishRun(ctx, runID, res, runErr, meta)
}

// finishRunFailedReason marks a run terminal with status=failed and
// the supplied error string, no usage. Used by the BLOCKING-fix paths
// where we created a run row but bailed before the loop ran (e.g.
// agent_id collision in the registry). Without this, those rows
// orphan at status=running and pollute ListActiveRunsByUser.
//
// Mirrors finishRun's structure: fresh background ctx with 5s timeout
// so the write isn't lost when the request ctx is already torn down.
func (s *Server) finishRunFailedReason(runID, reason string, meta runStateMeta) {
	// v0.10.0 OTEL: stamp final attrs on the run span even when we
	// can't write to the store (s.store == nil path). Span.End() is
	// deferred at the open site, so failing to record here means the
	// span closes without stop_reason — survivable, but skipping the
	// store guard here gives operators consistent telemetry.
	lcotel.SetRunDone(meta.otelSpan, lcotel.RunDoneAttrs{
		StopReason: "failed",
		Err:        errors.New(reason),
	})
	if s.store == nil || runID == "" {
		return
	}
	bg, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFn()
	if err := s.store.FinishRun(bg, runID, store.RunFailed, "", store.Usage{}, reason); err != nil {
		log.Printf("store: FinishRun(failed reason=%q) failed (run=%s): %v", reason, runID, err)
	}
	s.publishRunState(meta, "failed", "", reason)
	// RFC AH Phase 2b: a top-level run that fails (incl. mid-flight panic in
	// the background-goroutine paths) must still purge its ephemeral subtree.
	// A no-op for the pre-loop collision/register-fail bails (no volumes were
	// created yet → the subtree doesn't exist).
	if meta.IsTopLevel {
		s.purgeEphemeralVolumesForRun(meta.RootRunID)
	}
}

// finishRunCancelled writes the terminal cancelled status with the
// supplied reason. Mirrors finishRun's structure (background ctx with
// 5s timeout) so the store write isn't lost when runCtx is cancelled.
//
// Note: the ctx parameter is intentionally ignored — the function
// always uses a fresh background ctx because at the call site BOTH
// the request ctx and the runCtx are typically already cancelled.
// The parameter is kept for signature parity with finishRun so the
// two are interchangeable at the call site (finishRunWithCancel
// dispatches to one or the other), but it's a no-op input. If you
// add real ctx propagation here, audit every caller.
func (s *Server) finishRunCancelled(_ context.Context, runID string, res loop.RunResult, reason string, meta runStateMeta) {
	// v0.10.0 OTEL: final attrs on the run span.
	lcotel.SetRunDone(meta.otelSpan, lcotel.RunDoneAttrs{
		InputTokens:     res.Usage.InputTokens,
		OutputTokens:    res.Usage.OutputTokens,
		CacheReadTokens: res.Usage.CacheReadTokens,
		StopReason:      "cancelled",
	})
	if s.store == nil || runID == "" {
		return
	}
	bg, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFn()
	usage := store.Usage{
		InputTokens:         res.Usage.InputTokens,
		OutputTokens:        res.Usage.OutputTokens,
		CacheCreationTokens: res.Usage.CacheCreationTokens,
		CacheReadTokens:     res.Usage.CacheReadTokens,
		Model:               res.Usage.Model,
		Provider:            res.Usage.Provider,
	}
	if err := s.store.FinishRun(bg, runID, store.RunCancelled, reason, usage, ""); err != nil {
		log.Printf("store: FinishRun(cancelled) failed (run=%s): %v", runID, err)
	}
	s.publishRunState(meta, "cancelled", reason, "")
}

// purgeEphemeralVolumesForRun tears down a TOP-LEVEL run tree's ephemeral
// volumes at completion (RFC AH Phase 2b): a FENCED os.RemoveAll of
// <dynamic_root>/_ephemeral/<root_run_id>/ + the ephemeral_volume_defs rows.
// Best-effort + logged — a purge fault must never fail the run (the sweeper
// backstops it). No-op when no dynamic_root is configured (no ephemeral
// volumes could exist) or rootRunID is empty.
//
// Gated to top-level runs by the caller (meta.IsTopLevel): a sub-agent
// completing must NOT purge the tree its parent + siblings still use.
func (s *Server) purgeEphemeralVolumesForRun(rootRunID string) {
	if rootRunID == "" {
		return
	}
	// RFC AA SQL Memory: roll back any explicit transactions this run tree left
	// open (Phase 3a) — releasing their pinned scope connections — BEFORE
	// dropping the run-scope database (an open txn on the run scope would hold a
	// connection the drop needs). Then drop the run-scope SQL database for this
	// top-level run. Independent of dynamic volumes (a deployment may run SQL
	// Memory without any dynamic_root), so it happens BEFORE the volume
	// early-return. Best-effort + logged — a fault must never fail the run.
	if s.sqlMem != nil {
		s.sqlMem.RollbackRunTxns(rootRunID)
		if _, err := s.sqlMem.DropRunScope(rootRunID); err != nil {
			log.Printf("sqlmem run-scope drop (run=%s): %v", rootRunID, err)
		}
	}
	if s.store == nil {
		return
	}
	dynRoot, ok := builtin.DynamicVolumeRoot(s.cfg)
	if !ok {
		return // no dynamic root → no ephemeral volumes possible
	}
	// FENCED RemoveAll of the per-run ephemeral subtree (re-derived; never a
	// stored path). removed=false when it was already gone (nothing to do).
	if _, err := builtin.PurgeEphemeralRunTree(dynRoot, rootRunID, "ephemeral inline purge"); err != nil {
		log.Printf("ephemeral inline purge (run=%s): %v", rootRunID, err)
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.store.EphemeralVolumeDeleteByRun(bg, rootRunID); err != nil {
		log.Printf("ephemeral inline purge: delete rows (run=%s): %v", rootRunID, err)
	}
}

// rehydrateEphemeralVolumes builds a run-scoped EphemeralVolumeSet from the
// ephemeral_volume_defs rows a run created before it was paused/snapshotted
// (RFC AH Phase 2b). Used by the resume path so a resumed paused run keeps
// in-memory resolution of its own ephemeral volumes (the rows + on-disk dirs
// survive because the sweeper skips paused runs). Best-effort: a store fault
// or a malformed row body is logged + skipped, returning whatever resolved
// (an empty set in the worst case — the agent can re-create).
func (s *Server) rehydrateEphemeralVolumes(ctx context.Context, rootRunID string) *tools.EphemeralVolumeSet {
	set := tools.NewEphemeralVolumeSet()
	if s.store == nil || rootRunID == "" {
		return set
	}
	rows, err := s.store.EphemeralVolumeListByRun(ctx, rootRunID)
	if err != nil {
		log.Printf("ephemeral rehydrate (run=%s): %v", rootRunID, err)
		return set
	}
	for _, r := range rows {
		var body struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(r.Definition, &body); err != nil || body.Path == "" {
			continue
		}
		set.Add(r.Name, tools.EphemeralVolumeRef{Root: body.Path, ReadOnly: body.Mode == "ro"})
	}
	return set
}

// finishRun marks the run terminal in the store. status is derived from
// runErr: nil → completed, non-nil → failed. ctx may already be cancelled
// (the client disconnected); we use a fresh background context with a short
// timeout so the FinishRun write isn't lost.
func (s *Server) finishRun(_ context.Context, runID string, res loop.RunResult, runErr error, meta runStateMeta) {
	// v0.10.0 OTEL: final attrs on the run span. Runs through this
	// path on both completion + non-cancel failure. runErr maps to
	// Error span status; the rest are scalar attribute writes.
	lcotel.SetRunDone(meta.otelSpan, lcotel.RunDoneAttrs{
		InputTokens:     res.Usage.InputTokens,
		OutputTokens:    res.Usage.OutputTokens,
		CacheReadTokens: res.Usage.CacheReadTokens,
		StopReason:      res.StopReason,
		Err:             runErr,
	})
	if s.store == nil || runID == "" {
		return
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status := store.RunCompleted
	errMsg := ""
	if runErr != nil {
		status = store.RunFailed
		errMsg = runErr.Error()
	}
	usage := store.Usage{
		InputTokens:         res.Usage.InputTokens,
		OutputTokens:        res.Usage.OutputTokens,
		CacheCreationTokens: res.Usage.CacheCreationTokens,
		CacheReadTokens:     res.Usage.CacheReadTokens,
		Model:               res.Usage.Model,
		Provider:            res.Usage.Provider,
	}
	if err := s.store.FinishRun(bg, runID, status, res.StopReason, usage, errMsg); err != nil {
		log.Printf("store: FinishRun failed (run=%s): %v", runID, err)
	}
	publishStatus := "completed"
	if runErr != nil {
		publishStatus = "failed"
	}
	s.publishRunState(meta, publishStatus, res.StopReason, errMsg)
}

// authMiddleware moved to auth_principal.go (RFC L): it now resolves the
// bearer to an auth.Principal (tenant + subject + scopes), stamps it into
// ctx, and enforces the route's required scope. The legacy
// LOOMCYCLE_AUTH_TOKEN keeps working via the principal-resolution
// fallback until an admin-scoped OperatorTokenDef exists.

// validIdent reports whether s is a valid user_id or agent_id. Charset
// matches the spec: [A-Za-z0-9_-] with length 1..128. Used to refuse
// malformed input at the HTTP boundary so SQL queries and registry
// keys stay sane (no embedded slashes that could confuse URL routing,
// no whitespace that could land in a stop_reason field).
func validIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			continue
		default:
			return false
		}
	}
	return true
}

// validateParentContext delegates to connector.ValidateParentContext so
// every wire surface (HTTP, MCP, gRPC) bounds the opaque tracking fields
// identically — one source of truth, mirroring ValidateUserCredentialsMap.
func validateParentContext(pc *store.ParentContext) (string, bool) {
	return connector.ValidateParentContext(pc)
}

// validUserBearer reports whether s is a valid per-run MCP bearer
// token. Charset: [A-Za-z0-9._\-+/=], length 16..512. Matches §5.1 of
// per-run-mcp-bearer-plan: covers JWT shape (base64url + dots + maybe
// padding) and opaque tokens. Empty is rejected here — callers that
// want no bearer omit the field entirely (handled by an empty-check
// before invoking this validator).
func validUserBearer(s string) bool {
	if len(s) < 16 || len(s) > 512 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '+', r == '/', r == '=':
			continue
		default:
			return false
		}
	}
	return true
}

// newAgentID produces a fresh agent_id when the caller didn't supply
// one (or when sub-agents need their own). Same shape as session/run
// IDs: short prefix + 16 hex chars (8 random bytes = 64 bits of entropy).
func newAgentID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "a_" + hex.EncodeToString(b[:])
}

// toolNames returns the names of a slice of tools — used to populate
// the per-run AgentTools context value for tools that need a runtime
// view of "what this agent can use" (e.g. the Skill tool's subset
// check on each call).
func toolNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

// filterTools applies the agent + caller allowlists to the registered builtins.
// Glob suffixes ("mcp__brave-search__*") work via internal/tools/policy.
func filterTools(all []tools.Tool, agentAllowed, callerAllowed []string) []tools.Tool {
	if len(agentAllowed) == 0 {
		return nil
	}
	available := make([]string, 0, len(all))
	byName := make(map[string]tools.Tool, len(all))
	for _, t := range all {
		available = append(available, t.Name())
		byName[t.Name()] = t
	}
	allowed := policy.Apply(available, agentAllowed, callerAllowed)
	out := make([]tools.Tool, 0, len(allowed))
	for _, name := range allowed {
		if t, ok := byName[name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// Logger is the package-level logger; cmd/loomcycle may swap it out.
var Logger = log.Default()
