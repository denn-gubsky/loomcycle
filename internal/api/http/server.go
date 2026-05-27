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
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/metrics"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/skills"
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

	// cancelReg holds the in-memory map of agent_id → cancelFn so the
	// cancel API can tear down a still-running loop from a different
	// HTTP request. Always non-nil after New(); empty on startup. See
	// internal/cancel/registry.go for the trust model.
	cancelReg *cancel.Registry

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

	// systemPublisher backs the v0.8.6 POST /v1/_channels/_system/...
	// admin endpoint. Nil = endpoint refuses every request with a
	// "system publisher not wired" 503. Set via SetSystemPublisher.
	systemPublisher channels.SystemPublisher

	// metricsSampler is the v0.8.x process-resource sampler.
	// Nil = the /v1/_metrics/* endpoints return 503. Set via
	// SetMetricsSampler from main.go after the sampler is
	// constructed.
	metricsSampler *metrics.Sampler

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
	s.tools = append(s.tools, &builtin.AgentTool{
		Run: s.runSubAgent,
		// v0.11.8 — per-agent max_concurrent_children cap for
		// Agent.parallel_spawn. Walks the same resolver chain as
		// sub-run dispatch (yaml > dynamic_agents > AgentDef
		// substrate) so substrate-edited overrides apply on the
		// next call without restart. Returns 0 = no override; the
		// tool falls back to DefaultMaxConcurrentChildren.
		CapLookup: func(ctx context.Context, callingAgent string) int {
			def, ok := lookup.Agent(ctx, s.store, s.cfg, callingAgent)
			if !ok {
				return 0
			}
			return def.MaxConcurrentChildren
		},
	})
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
func (s *Server) resolveAgent(agentName, userTier string) (providerID, model, effort string, err error) {
	def, ok := s.lookupAgent(context.Background(), agentName)
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
func (s *Server) lookupAgent(ctx context.Context, name string) (config.AgentDef, bool) {
	// nil-store guard at the boundary so the lookup package can
	// type-assert an interface receiver. The lookup package treats
	// "no store" identically to "store didn't have the name" — both
	// fall through to (zero, false).
	if s.store == nil {
		return lookup.Agent(ctx, nil, s.cfg, name)
	}
	return lookup.Agent(ctx, s.store, s.cfg, name)
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
			Models:    convertConfigCandidates(def.Models),
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
	if len(s.cfg.UserTiers) == 0 {
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
		Tiers:               convertConfigCandidates(ut.Tiers),
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
func (s *Server) resolveSkillBodiesForRun(ctx context.Context, agentDef config.AgentDef) (config.AgentDef, runPromptProvenance) {
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
	for _, skillName := range agentDef.Skills {
		sr, ok := lookup.Skill(ctx, s.store, s.skillSet, skillName)
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
		Provider         string                            `json:"provider,omitempty"`
		Model            string                            `json:"model,omitempty"`
		Tier             string                            `json:"tier,omitempty"`
		Effort           string                            `json:"effort,omitempty"`
		MaxTokens        int                               `json:"max_tokens,omitempty"`
		MaxIterations    int                               `json:"max_iterations,omitempty"`
		SystemPrompt     string                            `json:"system_prompt,omitempty"`
		AllowedTools     []string                          `json:"allowed_tools,omitempty"`
		Skills           []string                          `json:"skills,omitempty"`
		Providers        []string                          `json:"providers,omitempty"`
		Models           map[string][]config.TierCandidate `json:"models,omitempty"`
		MemoryScopes     []string                          `json:"memory_scopes,omitempty"`
		MemoryQuotaBytes int                               `json:"memory_quota_bytes,omitempty"`
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
	return out
}

// channelPolicyForAgent builds the v0.8.4 Channel-tool policy from
// the agent yaml + the top-level `channels:` block. Returns a value
// suitable for tools.WithChannelPolicy. The Channels map is a copy
// of every operator-declared channel — the tool layer needs the
// per-channel scope/TTL/max_messages even for channels NOT in this
// agent's allowlist (e.g. to phrase a useful refusal message).
func (s *Server) channelPolicyForAgent(agentDef config.AgentDef) tools.ChannelPolicyValue {
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
	return tools.ChannelPolicyValue{
		Publish:   agentDef.Channels.Publish,
		Subscribe: agentDef.Channels.Subscribe,
		Channels:  channels,
	}
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
func (s *Server) fallbackForRun(agentName, userTier string) (loop.FallbackPolicy, func(ctx context.Context, failedProvider, failedModel string, cause error) (providers.Provider, string, string, error)) {
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
		// Re-resolve with the same agent + user_tier. The resolver's
		// stall flag we just set excludes the failed pair; the next
		// non-stalled candidate in the user_tier's priority is what
		// we get back.
		newProviderID, newModel, newEffort, err := s.resolveAgent(agentName, userTier)
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
func convertConfigCandidates(in map[string][]config.TierCandidate) map[string][]resolve.Candidate {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]resolve.Candidate, len(in))
	for tier, cands := range in {
		conv := make([]resolve.Candidate, 0, len(cands))
		for _, c := range cands {
			conv = append(conv, resolve.Candidate{Provider: c.Provider, Model: c.Model})
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
	agentDef, ok := s.lookupAgent(ctx, effectiveAgentName)
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
	allowedTools := filterTools(s.tools, agentDef.AllowedTools, in.AllowedTools)
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
	agentDef, promptProv := s.resolveSkillBodiesForRun(ctx, agentDef)
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
	identity := store.RunIdentity{AgentID: agentID, UserID: effectiveUserID, UserTier: in.UserTier, Model: model, ReplicaID: s.replicaID}
	sessionID, runID, sessErr := s.openOrCreateSessionAndRun(ctx, in.SessionID, effectiveAgentName, effectiveTenantID, effectiveUserID, identity)
	if sessErr != nil {
		var nf *store.ErrNotFound
		if errors.As(sessErr, &nf) {
			return fmt.Errorf("%w: %v", runner.ErrSessionNotFound, sessErr)
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
		RunID:   runID,
		AgentID: agentID,
		Agent:   effectiveAgentName,
		UserID:  effectiveUserID,
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

	loopCtx := tools.WithAgentTools(runCtx, toolNames(allowedTools))
	loopCtx = tools.WithRunIdentity(loopCtx, tools.RunIdentityValue{
		UserID:     effectiveUserID,
		AgentID:    agentID,
		UserTier:   in.UserTier,
		UserBearer: in.UserBearer, // v0.8.x: per-run MCP bearer
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
	})
	loopCtx = tools.WithChannelPolicy(loopCtx, s.channelPolicyForAgent(agentDef))
	loopCtx = tools.WithEventEmitter(loopCtx, emit)
	adPolicy, evPolicy := s.substratePoliciesForAgent(agentDef, effectiveAgentName)
	loopCtx = tools.WithAgentDefPolicy(loopCtx, adPolicy)
	loopCtx = tools.WithSkillDefPolicy(loopCtx, s.skillDefPolicyForAgent(agentDef))
	loopCtx = tools.WithEvaluationPolicy(loopCtx, evPolicy)
	loopCtx = tools.WithHistoryPolicy(loopCtx, s.historyPolicyForAgent(agentDef))
	loopCtx = tools.WithInterruptionPolicy(loopCtx, s.interruptionPolicyForAgent(agentDef))
	loopCtx = tools.WithRunID(loopCtx, runID)
	loopCtx = tools.WithDispatcher(loopCtx, dispatcher)

	heartbeat := s.makeHeartbeat(runID)

	fbPolicy, fbReResolve := s.fallbackForRun(effectiveAgentName, in.UserTier)
	res, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:               provider,
		Model:                  model,
		Tools:                  allowedTools,
		Dispatcher:             dispatcher,
		Segments:               segments,
		PriorMessages:          priorMessages,
		OnEvent:                emit,
		OnHeartbeat:            heartbeat,
		MaxTokens:              agentDef.MaxTokens,
		MaxIterations:          agentDef.MaxIterations, // 0 → loop default (16)
		Effort:                 effort,
		MarkStalled:            s.markStalledFn(providerID, model),
		MarkRateLimited:        s.markRateLimitedFn(in.UserTier),
		ClearStall:             s.clearStallFn(providerID, model),
		ToolParallelism:        s.cfg.Env.ToolParallelism,
		AgentName:              effectiveAgentName,
		UserTier:               in.UserTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForTier(in.UserTier),
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
func (s *Server) SetHookRegistry(r hooks.RegistryInterface) {
	s.hookRegistry = r
	s.hookDispatcher = hooks.NewDispatcher(r, nil)
}

// SetPgSessionLocker installs the v0.12.5 Phase 6 cluster-wide
// session lock. When non-nil, trySessionLock dispatches to it
// instead of the in-process SessionLockMap.
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
	// v0.9.x Library v2 — unified enumeration that merges static cfg
	// + substrate views into one envelope per entry. The names/* sister
	// endpoints above stay as-is for backwards compat with external
	// adapter consumers; these new endpoints back the /ui/library v2
	// tab which shows STATIC + DYNAMIC entries with source chips.
	mux.Handle("GET /v1/_library/agents", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListLibraryAgents))))
	mux.Handle("GET /v1/_library/skills", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListLibrarySkills))))
	mux.Handle("GET /v1/_library/mcp-servers", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleListLibraryMcpServers))))
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
	return mux
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
	Agent        string               `json:"agent"`
	Segments     []loop.PromptSegment `json:"segments"`
	AllowedTools []string             `json:"allowed_tools,omitempty"`
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
	// Cap body at 1 MiB so a malicious caller can't exhaust memory by
	// streaming a huge body. ReadHeaderTimeout doesn't cover the body.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Agent == "" {
		http.Error(w, `agent is required`, http.StatusBadRequest)
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

	agentDef, ok := s.lookupAgent(r.Context(), req.Agent)
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

	providerID, model, effort, err := s.resolveAgent(req.Agent, req.UserTier)
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
	allowedTools := filterTools(s.tools, agentDef.AllowedTools, req.AllowedTools)
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
	agentDef, promptProv := s.resolveSkillBodiesForRun(r.Context(), agentDef)
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
	identity := store.RunIdentity{AgentID: agentID, UserID: req.UserID, UserTier: req.UserTier, Model: model, ReplicaID: s.replicaID}
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

	// Derive the loop ctx with a cancel-cause function. The HTTP request
	// ctx remains the parent so client-disconnect still tears down. We
	// register the cancelFn under agent_id so an external cancel API
	// call can fire it.
	runCtx, cancelFn := context.WithCancelCause(r.Context())
	defer cancelFn(nil) // ensure ctx leaks don't survive the handler
	// v0.10.0 OTEL: top-level loomcycle.run span covers the whole run.
	runCtx, runSpan := lcotel.RecordRunStart(runCtx, lcotel.RunStartAttrs{
		RunID:     runID,
		AgentID:   agentID,
		AgentName: req.Agent,
		UserID:    req.UserID,
	})
	defer runSpan.End()
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
		RunID:    runID,
		AgentID:  agentID,
		Agent:    req.Agent,
		UserID:   req.UserID,
		otelSpan: runSpan,
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
	defer s.cancelReg.Deregister(agentID)
	s.publishRunState(meta, "running", "", "")

	// If we're persisting, record the caller's input segments as the first
	// event in the run. The loop never emits the caller's input itself, so
	// without this the transcript would start with the assistant's first
	// turn — and replay couldn't reconstruct the user prompt.
	if s.store != nil && runID != "" {
		if inputJSON, err := json.Marshal(req.Segments); err == nil {
			if err := s.store.AppendEvent(r.Context(), runID, "user_input", inputJSON); err != nil {
				log.Printf("store: AppendEvent(user_input) failed: %v", err)
			}
		}
	}
	// v0.9.x: persist the resolved system prompt + provenance so the
	// Web UI surfaces it as a card on the run timeline. Mirrors the
	// emission in RunOnce + handleMessages + runSubAgent.
	s.emitSystemPromptEvent(r.Context(), runID, agentDef.SystemPrompt, "", promptProv)

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
	})

	emit := s.makeRecordingEmit(r.Context(), runID, stream.send)

	// Pass the agent's effective tool names to the dispatcher so tools
	// that need a runtime view of "what this agent can use" (e.g. the
	// Skill tool's subset check on each call) read it via ctx instead
	// of being constructed per-run.
	loopCtx := tools.WithAgentTools(runCtx, toolNames(allowedTools))
	// Stash the run's identity so the Agent built-in tool's
	// SubAgentRunner can inherit user_id and set parent_agent_id on
	// any sub-runs it spawns.
	loopCtx = tools.WithRunIdentity(loopCtx, tools.RunIdentityValue{
		UserID:     req.UserID,
		AgentID:    agentID,
		UserTier:   req.UserTier,
		UserBearer: req.UserBearer, // v0.8.x: per-run MCP bearer
	})
	// Stash the caller's host policy so any sub-agents spawned by the
	// Agent tool inherit the same allowed_hosts / WebSearchFilter
	// narrowing the parent received.
	loopCtx = tools.WithHostPolicy(loopCtx, hostPolicy)
	loopCtx = tools.WithAgentName(loopCtx, req.Agent)
	loopCtx = tools.WithMemoryPolicy(loopCtx, tools.MemoryPolicyValue{
		AllowedScopes: agentDef.MemoryScopes,
		QuotaBytes:    agentDef.MemoryQuotaBytes,
	})
	loopCtx = tools.WithChannelPolicy(loopCtx, s.channelPolicyForAgent(agentDef))
	loopCtx = tools.WithEventEmitter(loopCtx, emit)
	adPolicy, evPolicy := s.substratePoliciesForAgent(agentDef, req.Agent)
	loopCtx = tools.WithAgentDefPolicy(loopCtx, adPolicy)
	loopCtx = tools.WithSkillDefPolicy(loopCtx, s.skillDefPolicyForAgent(agentDef))
	loopCtx = tools.WithEvaluationPolicy(loopCtx, evPolicy)
	loopCtx = tools.WithHistoryPolicy(loopCtx, s.historyPolicyForAgent(agentDef))
	loopCtx = tools.WithInterruptionPolicy(loopCtx, s.interruptionPolicyForAgent(agentDef))
	loopCtx = tools.WithRunID(loopCtx, runID)
	loopCtx = tools.WithDispatcher(loopCtx, dispatcher)

	// Heartbeat hook: each loop iteration updates last_heartbeat_at so a
	// future sweeper can detect crashed processes (no heartbeat for > N
	// minutes → presumed dead). Cheap (~10–100 calls per run).
	heartbeat := s.makeHeartbeat(runID)

	fbPolicy, fbReResolve := s.fallbackForRun(req.Agent, req.UserTier)
	loopRes, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:               provider,
		Model:                  model,
		Tools:                  allowedTools,
		Dispatcher:             dispatcher,
		Segments:               req.Segments,
		OnEvent:                emit,
		OnHeartbeat:            heartbeat,
		MaxTokens:              agentDef.MaxTokens,     // 0 → driver default
		MaxIterations:          agentDef.MaxIterations, // 0 → loop default (16)
		Effort:                 effort,
		MarkStalled:            s.markStalledFn(providerID, model),
		MarkRateLimited:        s.markRateLimitedFn(req.UserTier),
		ClearStall:             s.clearStallFn(providerID, model),
		ToolParallelism:        s.cfg.Env.ToolParallelism,
		AgentName:              req.Agent,
		UserTier:               req.UserTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForTier(req.UserTier),
	})
	if runErr != nil {
		stream.send(providers.Event{Type: providers.EventError, Error: runErr.Error()})
	}

	s.finishRunWithCancel(r.Context(), runCtx, runID, loopRes, runErr, meta)
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id is required", http.StatusBadRequest)
		return
	}

	var body messagesRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the session exists BEFORE taking the per-session lock.
	// Otherwise an attacker can spam unknown IDs and each LoadOrStore
	// grows sessionLocks permanently (entries are never GC'd at v0.3.2).
	sess, err := s.store.GetSession(r.Context(), id)
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
	agentDef, ok := s.lookupAgent(r.Context(), sess.Agent)
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
	providerID, model, effort, err := s.resolveAgent(sess.Agent, body.UserTier)
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

	allowedTools := filterTools(s.tools, agentDef.AllowedTools, body.AllowedTools)
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
	agentDef, promptProv := s.resolveSkillBodiesForRun(r.Context(), agentDef)
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
		AgentID:   agentID,
		UserID:    sess.UserID,
		UserTier:  body.UserTier,
		Model:     model,
		ReplicaID: s.replicaID,
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
		RunID:    run.ID,
		AgentID:  agentID,
		Agent:    sess.Agent,
		UserID:   sess.UserID,
		otelSpan: runSpan,
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
	})

	emit := s.makeRecordingEmit(r.Context(), run.ID, stream.send)
	heartbeat := s.makeHeartbeat(run.ID)

	loopCtx := tools.WithAgentTools(runCtx, toolNames(allowedTools))
	loopCtx = tools.WithRunIdentity(loopCtx, tools.RunIdentityValue{
		UserID:     sess.UserID,
		AgentID:    agentID,
		UserTier:   body.UserTier,
		UserBearer: body.UserBearer, // v0.8.x: per-run MCP bearer
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
	})
	loopCtx = tools.WithChannelPolicy(loopCtx, s.channelPolicyForAgent(agentDef))
	loopCtx = tools.WithEventEmitter(loopCtx, emit)
	adPolicy, evPolicy := s.substratePoliciesForAgent(agentDef, sess.Agent)
	loopCtx = tools.WithAgentDefPolicy(loopCtx, adPolicy)
	loopCtx = tools.WithSkillDefPolicy(loopCtx, s.skillDefPolicyForAgent(agentDef))
	loopCtx = tools.WithEvaluationPolicy(loopCtx, evPolicy)
	loopCtx = tools.WithHistoryPolicy(loopCtx, s.historyPolicyForAgent(agentDef))
	loopCtx = tools.WithInterruptionPolicy(loopCtx, s.interruptionPolicyForAgent(agentDef))
	loopCtx = tools.WithRunID(loopCtx, run.ID)
	loopCtx = tools.WithDispatcher(loopCtx, dispatcher)
	fbPolicy, fbReResolve := s.fallbackForRun(sess.Agent, body.UserTier)
	loopRes, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:               provider,
		Model:                  model,
		Tools:                  allowedTools,
		Dispatcher:             dispatcher,
		Segments:               segments,
		PriorMessages:          priorMessages,
		OnEvent:                emit,
		OnHeartbeat:            heartbeat,
		MaxTokens:              agentDef.MaxTokens,     // 0 → driver default
		MaxIterations:          agentDef.MaxIterations, // 0 → loop default (16)
		Effort:                 effort,
		MarkStalled:            s.markStalledFn(providerID, model),
		MarkRateLimited:        s.markRateLimitedFn(body.UserTier),
		ClearStall:             s.clearStallFn(providerID, model),
		ToolParallelism:        s.cfg.Env.ToolParallelism,
		AgentName:              sess.Agent,
		UserTier:               body.UserTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForTier(body.UserTier),
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
	sess, err := s.store.GetSession(r.Context(), id)
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
		mu.Lock()
		defer mu.Unlock()
		payload, err := json.Marshal(ev)
		if err == nil {
			if err := s.store.AppendEvent(ctx, runID, string(ev.Type), payload); err != nil {
				log.Printf("store: AppendEvent failed (run=%s type=%s): %v", runID, ev.Type, err)
			}
		}
		fwd(ev)
	}
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
func (s *Server) runSubAgent(ctx context.Context, name string, prompt string, defID string) (string, error) {
	def, ok := lookup.Agent(ctx, s.store, s.cfg, name)
	if !ok {
		return "", fmt.Errorf("unknown sub-agent %q (not in cfg.Agents, dynamic_agents, or agent_def_active)", name)
	}

	// v0.8.5 substrate: when defID is set, overlay the named def's
	// mutable fields (system_prompt, allowed_tools, model, tier,
	// effort, max_tokens, memory_scopes, etc.) over the static
	// cfg.Agents entry for this single sub-run. Name mismatch is a
	// hard refuse — pinning across names would let a parent bypass
	// the operator's static agent boundary.
	if defID != "" {
		if s.store == nil {
			return "", fmt.Errorf("Agent tool: def_id pinning requires a configured store backend")
		}
		row, err := s.store.AgentDefGet(ctx, defID)
		if err != nil {
			return "", fmt.Errorf("Agent tool: def_id %q lookup failed: %w", defID, err)
		}
		if row.Name != name {
			return "", fmt.Errorf("Agent tool: def_id %q is for agent %q, not %q (cross-name pinning refused)", defID, row.Name, name)
		}
		if row.Retired {
			return "", fmt.Errorf("Agent tool: def_id %q is retired", defID)
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
		return "", fmt.Errorf("resolve sub-agent %q model: %w", name, err)
	}
	provider, err := s.providers.Get(providerID)
	if err != nil {
		return "", fmt.Errorf("provider for sub-agent %q: %w", name, err)
	}

	// Read parent's identity from ctx to inherit user_id and pin
	// parent_agent_id on the sub-run. tools.RunIdentity returns zero
	// value if the parent didn't set it — sub-agents spawned from
	// callers that didn't supply user_id naturally inherit empty.
	parentIdentity := tools.RunIdentity(ctx)

	// Generate a fresh agent_id for the sub-run. Always generated;
	// callers can't override (the sub is loomcycle-controlled).
	subAgentID := newAgentID()

	// Sub-run gets its OWN session. Tenant inherited as empty for v0.4.0
	// MVP — multi-tenant agent inheritance lands when per-tenant
	// fairness does (later in v0.4 / v0.5).
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
		UserTier:   parentIdentity.UserTier, // v0.8.2: same user_tier across the sub-run tree
		AgentDefID: defID,                   // v0.8.5: pin defID on the sub-run for evaluation denormalisation
		Model:      model,                   // resolved model — written at create so the UI sees it during the run
	}
	subSessionID, subRunID, err := s.openOrCreateSessionAndRun(ctx, "", name, "", parentIdentity.UserID, subIdentity)
	if err != nil {
		return "", fmt.Errorf("create sub-session for %q: %w", name, err)
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
		ParentAgentID: parentIdentity.AgentID,
		otelSpan:      subRunSpan,
	}
	s.publishRunState(subMeta, "running", "", "")

	// v0.8.22: rebuild SystemPrompt from per-run SkillDef bodies
	// when any of the sub-agent's skills has a DB-active row. Same
	// call as the three top-level run-creation sites — without it,
	// sub-agents would silently keep the static baked body and
	// SkillDef promotions never take effect for agents only spawned
	// as sub-agents.
	def, promptProv := s.resolveSkillBodiesForRun(ctx, def)
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

	subTools := filterTools(s.tools, def.AllowedTools, nil)
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
		UserID:     parentIdentity.UserID,
		AgentID:    subAgentID,
		UserTier:   parentIdentity.UserTier,   // v0.8.2: sub-agents inherit parent's user_tier
		AgentDefID: defID,                     // v0.8.7: surface pinned def_id via Context.self
		UserBearer: parentIdentity.UserBearer, // v0.8.x: bearer inherited identically (same end-user)
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
	})
	// Sub-agent's Channel policy follows the same per-yaml shape as
	// MemoryPolicy above. The Channels map (operator-declared
	// channels) IS shared with the parent — those are operator
	// state, not agent state. The ALLOWLISTS (publish / subscribe)
	// come from the child's yaml.
	subCtx = tools.WithChannelPolicy(subCtx, s.channelPolicyForAgent(def))
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
	subCtx = tools.WithEvaluationPolicy(subCtx, subEvPolicy)
	subCtx = tools.WithHistoryPolicy(subCtx, s.historyPolicyForAgent(def))
	subCtx = tools.WithInterruptionPolicy(subCtx, s.interruptionPolicyForAgent(def))
	subCtx = tools.WithRunID(subCtx, subRunID)
	subCtx = tools.WithDispatcher(subCtx, subDispatcher)

	subHeartbeat := s.makeHeartbeat(subRunID)

	fbPolicy, fbReResolve := s.fallbackForRun(name, parentTier)
	res, runErr := loop.Run(subCtx, loop.RunOptions{
		Provider:               provider,
		Model:                  model,
		Tools:                  subTools,
		Dispatcher:             subDispatcher,
		Segments:               segs,
		OnEvent:                subEmit,
		OnHeartbeat:            subHeartbeat,
		MaxTokens:              def.MaxTokens,     // 0 → driver default
		MaxIterations:          def.MaxIterations, // 0 → loop default (16)
		Effort:                 effort,
		MarkStalled:            s.markStalledFn(providerID, model),
		MarkRateLimited:        s.markRateLimitedFn(parentTier),
		ClearStall:             s.clearStallFn(providerID, model),
		ToolParallelism:        s.cfg.Env.ToolParallelism,
		AgentName:              name,
		UserTier:               parentTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForTier(parentTier),
	})
	s.finishRunWithCancel(ctx, subRunCtx, subRunID, res, runErr, subMeta)

	if runErr != nil {
		// Wrap with session/run IDs so a developer reading parent logs
		// can locate the sub's transcript directly. The parent agent's
		// model sees the unwrapped error message. agent_id is the v0.4
		// addressable handle, so include it too — the easiest hint for
		// "GET /v1/agents/<this>" debugging.
		return "", fmt.Errorf("sub-agent %q failed (agent=%s session=%s run=%s): %w",
			name, subAgentID, subSessionID, subRunID, runErr)
	}
	// Surface the sub agent_id to the parent agent's transcript by
	// prefixing the tool_result text. Parent caller's model sees this
	// and can echo it to the UI. Cheap; unblocks future "cancel only
	// the sub" UX.
	return fmt.Sprintf("[sub-agent agent_id=%s]\n%s", subAgentID, res.FinalText), nil
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
		Live:      live,
		ReplicaID: r.ReplicaID,
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
	out := make([]agentResponse, 0, len(runs))
	for _, run := range runs {
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
	rows, err := s.store.InterruptListByRun(r.Context(), runID, statusFilter)
	if err != nil {
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
	rows, err := s.store.InterruptListByUser(r.Context(), userID, statusFilter)
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
	ParentAgentID string
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
		ParentAgentID: m.ParentAgentID,
		Status:        status,
		StopReason:    stopReason,
		Error:         errMsg,
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

// authMiddleware enforces LOOMCYCLE_AUTH_TOKEN bearer auth, except for /healthz which
// is mounted bare (this middleware is only wrapped around /v1/* routes).
//
// Comparison uses auth.CompareBearer, which hashes both sides to a
// fixed-length digest before subtle.ConstantTimeCompare so the
// compare is constant-time regardless of input length (raw
// ConstantTimeCompare returns early on length mismatch and leaks
// the expected token's length).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Env.AuthToken == "" {
			// No token configured = open mode (dev only). Startup logged a
			// warning so the operator knows.
			next.ServeHTTP(w, r)
			return
		}
		want := "Bearer " + s.cfg.Env.AuthToken
		// Standard bearer-header path (every adapter / curl / API client).
		if got := r.Header.Get("Authorization"); got != "" {
			if auth.CompareBearer(got, want) {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Cookie fallback for the embedded Web UI (v0.7.3). The /ui
		// landing handler converts a `?token=...` query into a
		// loomcycle_session HttpOnly cookie; subsequent /v1 calls
		// from the SPA carry the cookie automatically (same-origin
		// fetch). Operators using bearer headers via curl / SDKs are
		// unaffected.
		if cookie, err := r.Cookie(webui.SessionCookie); err == nil && cookie.Value != "" {
			cookieBearer := "Bearer " + cookie.Value
			if auth.CompareBearer(cookieBearer, want) {
				next.ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

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
