// Package runner is the wire-agnostic seam between the HTTP+SSE
// surface (internal/api/http) and the gRPC surface
// (internal/api/grpc). Both wires translate their request shape into
// runner.RunInput and pass two callbacks (OnRegistered, OnEvent) into
// a Runner implementation. The actual loop driver lives on
// internal/api/http.Server (which satisfies Runner); main.go hands
// the same instance to both wire surfaces so a cancel issued via
// gRPC reaches a run started via HTTP and vice versa.
//
// This package owns:
//
//   - RunInput / RunCallbacks — the shared request and observation types.
//   - Sentinel errors — wire-agnostic error vocabulary that each
//     surface maps to its own status codes (HTTP 4xx/5xx vs gRPC codes).
//   - SessionLockMap — refcounted + GC-able per-session continuation
//     lock map. Lives here (not in internal/api/http) because both
//     wire surfaces target the same session_id, so a single lock map
//     coordinates concurrent continuations across wires.
//
// The actual Run method lives on the type satisfying the Runner
// interface — currently *internal/api/http.Server. Keeping the
// implementation in the http package avoids moving 500+ LOC of
// helpers (openOrCreateSessionAndRun, makeRecordingEmit,
// makeHeartbeat, finishRun*, replayTranscript) for what is, at the
// abstraction layer, just exposing a public method.
package runner

import (
	"context"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Runner is the contract both wire surfaces depend on. internal/api/http
// satisfies it; internal/api/grpc holds a Runner reference and calls
// it from its Run / Continue stream handlers.
type Runner interface {
	// RunOnce drives one full agent run end-to-end. Returns nil on
	// successful completion (which may include the loop terminating
	// with status=failed; the wire surface decides whether the
	// stream conveys that). Returns one of the ErrFoo sentinels on a
	// setup-time failure; loop-internal errors are wrapped as
	// ErrInternal.
	//
	// The function blocks until the loop terminates. Caller's ctx
	// cancellation cascades into the loop ctx.
	RunOnce(ctx context.Context, in RunInput, cb RunCallbacks) error
}

// Sentinel errors exported for wire-layer error-code mapping.
var (
	// ErrUnknownAgent — agent name not in the operator yaml.
	// Wire: HTTP 400 / gRPC InvalidArgument.
	ErrUnknownAgent = errors.New("unknown agent")
	// ErrInvalidArgument — caller-supplied field failed validation.
	// Wire: HTTP 400 / gRPC InvalidArgument.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrUnknownProvider — agent's resolved provider isn't configured.
	// Wire: HTTP 400 / gRPC InvalidArgument.
	ErrUnknownProvider = errors.New("unknown provider")
	// ErrSessionRequired — caller asked for a session-bound action
	// but Store wasn't configured.
	// Wire: HTTP 400 / gRPC FailedPrecondition.
	ErrSessionRequired = errors.New("session-bound action requires store")
	// ErrSessionNotFound — caller-supplied session_id has no row.
	// Wire: HTTP 404 / gRPC NotFound.
	ErrSessionNotFound = errors.New("session not found")
	// ErrSessionBusy — another request is in flight on the same
	// session_id.
	// Wire: HTTP 409 / gRPC FailedPrecondition.
	ErrSessionBusy = errors.New("session busy")
	// ErrAgentIDInUse — caller-supplied agent_id is already mapped
	// to an active run.
	// Wire: HTTP 409 / gRPC AlreadyExists.
	ErrAgentIDInUse = errors.New("agent_id in use")
	// ErrBackpressure — concurrency semaphore rejected the run (global
	// queue full or timeout). Wire: HTTP 429 / gRPC ResourceExhausted.
	ErrBackpressure = errors.New("backpressure")
	// ErrPerUserQuotaExhausted — per-tenant fairness cap reached. The
	// run is otherwise valid but the user has hit their personal
	// active+queued ceiling. Distinct from ErrBackpressure because the
	// appropriate retry strategy differs — backpressure is operator-
	// wide load, per-user quota is "you specifically need to wait."
	// Wire: HTTP 429 + Retry-After: 5 / gRPC ResourceExhausted.
	// (v0.10.1)
	ErrPerUserQuotaExhausted = errors.New("per_user_quota_exhausted")
	// ErrRuntimePaused — a runtime-wide pause (POST /v1/_pause) is in
	// effect, so new runs are not admitted (RFC X / F41). The runtime is
	// quiescing for a snapshot; retry after resume. Wire: HTTP 503 / gRPC
	// Unavailable.
	ErrRuntimePaused = errors.New("runtime paused")
	// ErrInternal — unexpected error from store / loop / providers.
	// Wire: HTTP 500 / gRPC Internal.
	ErrInternal = errors.New("internal error")
	// ErrStreamingUnsupported — wire surface couldn't open a
	// streaming response (rare; HTTP-side ResponseWriter doesn't
	// implement Flusher). gRPC always supports streaming, so this
	// surfaces only on HTTP.
	// Wire: HTTP 500 / gRPC Internal.
	ErrStreamingUnsupported = errors.New("streaming unsupported")
)

// RunInput is the unified input shape both wire surfaces translate
// into. Field semantics match POST /v1/runs (when SessionID is
// empty) and POST /v1/sessions/{id}/messages (when SessionID is
// non-empty). The continuation path ignores Agent and TenantID —
// they're derived from the existing session row.
type RunInput struct {
	// Agent is the registered agent name. Required for fresh runs;
	// ignored for continuations (the session's stored agent is the
	// source of truth).
	Agent string

	// SessionID — empty starts a fresh session+run; non-empty
	// continues an existing session, replays its transcript, and
	// creates a new run inside it.
	SessionID string

	// TenantID is recorded on a fresh session. Ignored for continuations.
	TenantID string

	// Segments is the call's input prompt content. The caller does
	// NOT need to prepend the agent's system_prompt — the runner
	// does that internally.
	Segments []loop.PromptSegment

	// AllowedTools narrows the agent's tool surface for this call.
	// Empty = use the agent's full configured allowlist.
	AllowedTools []string

	// AllowedHosts narrows HTTP / WebFetch / WebSearch host policy.
	// Three states:
	//   nil          → no narrowing
	//   &[]string{}  → deny-all
	//   &[]string{"foo.com"} → intersection with operator's static list
	AllowedHosts *[]string

	// WebSearchFilter is "drop" or "keep". Ignored when AllowedHosts is nil.
	WebSearchFilter string

	// UserID binds the run to a user. Optional for fresh runs;
	// ignored for continuations (inherited from the session).
	UserID string

	// AgentID is the caller-supplied tracking handle. Optional;
	// the runner generates one when empty and emits it via
	// OnRegistered.
	AgentID string

	// UserTier is the v0.8.2 user-facing-tier policy name. Empty
	// falls through to cfg.UserTiers["default"] when configured.
	// See internal/api/http/server.go resolveAgent for the overlay
	// semantics and docs/PLAN.md → v0.8.2 for the full design.
	UserTier string

	// UserBearer is the v0.8.x per-run MCP bearer token. When non-
	// empty the HTTP MCP transport substitutes it at outbound
	// request-build time wherever the operator's YAML header
	// contains ${run.user_bearer}. Empty is backwards-compat:
	// headers without ${run.*} tokens are unaffected; headers with
	// ${run.user_bearer:-X} use fallback X; headers with bare
	// ${run.user_bearer} drop the header and emit a WARN.
	UserBearer string

	// UserCredentials is the v1.x RFC F named-credentials map —
	// per-tool/per-MCP-server bearers keyed by operator-chosen name.
	// Substituted into MCP HTTP header values containing
	// ${run.credentials.<name>} at outbound request-build time.
	// Sub-agents inherit identically via ctx.
	//
	// Back-compat: when UserBearer is non-empty but
	// UserCredentials["default"] is empty, WithRunIdentity promotes
	// UserBearer into the map as the "default" key, so the legacy
	// ${run.user_bearer} substitution and ${run.credentials.default}
	// both resolve to the same value. v0.8.x callers see no change.
	//
	// Validation: keys [a-zA-Z0-9_-]{1,64}; values arbitrary strings.
	// Enforced at wire entry points (HTTP, gRPC, MCP); this struct
	// trusts its caller. Empty map is valid (= run uses no per-tool
	// auth). Never persisted; never logged; not emitted in OTEL spans.
	UserCredentials map[string]string

	// ParentContext is opaque caller-tracking lineage the runtime
	// carries but never interprets: it is set once on the root run,
	// inherited UNCHANGED by every descendant sub-agent (the Agent
	// tool copies it parent→child, same as UserCredentials), persisted
	// on each run row, and echoed back on the per-agent report surfaces
	// (RunStateEvent, the agent status object, the SSE "agent" frame).
	// It lets an external consumer link a child sub-agent's usage back
	// to the user-initiated request that spawned the whole tree.
	//
	// Unlike UserBearer/UserCredentials it is NOT a secret — it is safe
	// to persist, log, and emit. Nil = no tracking context (today's
	// behavior; full back-compat). The canonical type lives in the
	// store package (next to RunIdentity) so every layer — runner,
	// tools, connector, store — can reference it without an import
	// cycle (store imports no internal packages).
	ParentContext *store.ParentContext

	// IdempotencyKey is the optional RFC H Decision 10 "Layer 2"
	// durable dedup key. Empty (the default) for interactive runs. Set
	// by the webhook spawn path to the delivery id; threaded into
	// RunIdentity so CreateRun persists it to runs.idempotency_key. When
	// a second run carries a key already claimed, CreateRun returns
	// store.ErrDuplicateIdempotencyKey and RunOnce aborts BEFORE the
	// agent loop, so the run never double-executes.
	IdempotencyKey string

	// Metadata is the optional NON-SECRET structured blob a trigger
	// (WebHook/Schedule def, or a first-party /v1/runs caller) passes to
	// the agent — repo name, review policy, preferred skills, etc. It is
	// TRUSTED (operator/def-authored or first-party): delivered to a code-js
	// agent as input.metadata, and to an LLM agent as a trusted-text prompt
	// segment. Not a secret (safe to log) — credentials stay on
	// UserCredentials, substituted only at the MCP transport.
	//
	// PER-CALL, not session state: like the agent's system prompt (re-derived
	// from the AgentDef on every call), Metadata is NOT persisted on the run
	// or session and is NOT replayed on a continuation. A
	// /v1/sessions/{id}/messages continuation that needs the same metadata
	// must re-supply it (same as it re-supplies the prompt). This keeps the
	// channel a per-invocation input rather than sticky session state.
	Metadata map[string]any

	// PayloadMetadata is the optional NON-SECRET structured blob projected
	// from an EXTERNAL trigger body (the webhook receiver maps
	// `payload_mapping` `run_metadata.*` targets here). It is UNTRUSTED —
	// attacker-influenceable — so it is fenced: delivered to a code-js agent
	// as input.payload_metadata, and to an LLM agent inside an
	// <run_metadata> untrusted block. Server-populated only; never a wire
	// field on /v1/runs (a first-party caller's data is trusted → Metadata).
	PayloadMetadata map[string]any

	// RunTimeoutSeconds is an optional per-run wall-clock budget override for
	// a code-js agent (the ad-hoc /v1/runs knob). 0 ⇒ inherit the agent's
	// run_timeout_seconds, else the global default. The server resolves
	// per-run > per-agent and passes the winner to loop.RunOptions; only the
	// code-js provider consumes it (LLM runs are bounded by MaxIterations +
	// provider HTTP timeouts, not this budget).
	RunTimeoutSeconds int

	// Interactive starts a PERSISTENT run that parks waiting for operator
	// steering at end_turn instead of terminating (the interactive terminal
	// session mode). Requires steering to be wired + the operator to drive
	// the run via POST /v1/runs/{run_id}/input; pair with an
	// unbounded_iterations agent for a true always-on terminal. Cancel ends it.
	Interactive bool

	// Sampling is an optional per-RUN LLM sampling-param override (temperature,
	// top_p, …). Merged PER FIELD over the agent's own Sampling (per-run field
	// wins, an unset field inherits the agent's). nil = inherit the agent's
	// sampling entirely. The server resolves the merge and passes the winner to
	// loop.RunOptions.
	Sampling *config.Sampling
}

// RunCallbacks is how the wire surfaces observe the run.
//
// OnRegistered fires exactly once after the cancel-registry entry
// is in place but before the loop starts. Both the SSE
// "session"/"agent" frames and the gRPC equivalent are emitted from
// here.
//
// OnEvent fires once per provider event, post-store-write. Wire
// surfaces forward to their stream/socket. Returning quickly is
// expected — the loop blocks on this callback synchronously.
//
// Both callbacks may be nil; the runner tolerates that for any
// fire-and-forget surface that doesn't need them.
type RunCallbacks struct {
	OnRegistered func(agentID, runID, sessionID, parentAgentID string)
	OnEvent      func(providers.Event)
}
