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
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/policy"
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
	s := &Server{
		cfg:          cfg,
		providers:    pr,
		tools:        builtinTools,
		sem:          sem,
		store:        st,
		cancelReg:    cancel.NewRegistry(),
		sessionLocks: runner.NewSessionLockMap(),
	}
	s.tools = append(s.tools, &builtin.AgentTool{Run: s.runSubAgent})
	return s
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
// The closure captures the resolver-scoped (provider, model) for the
// current iteration; the loop calls it on driver errors that suggest
// the model itself is broken (5xx after retry, mid-stream errors).
//
// Returns nil when no resolver is wired (back-compat path) — RunOptions
// treats nil as "stall feedback disabled".
func (s *Server) markStalledFn(provider, model string) func(p, m, reason string) {
	if s.resolver == nil {
		return nil
	}
	// Closure captures only what the loop needs. Loop passes
	// (provider, model, reason); we ignore the loop's args and use
	// the resolved pair from the call site, since they're the
	// authoritative inputs to the resolver — the loop wouldn't know
	// to discriminate between OpenAI vs DeepSeek without us telling
	// it via opts.Provider.ID() (which is what it'll pass anyway,
	// but pinning here keeps the contract explicit).
	return func(_, _, reason string) {
		s.resolver.MarkStalled(provider, model, reason)
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
func (s *Server) resolveAgent(agentName string) (providerID, model, effort string, err error) {
	def, ok := s.cfg.Agents[agentName]
	if !ok {
		return "", "", "", fmt.Errorf("%w: %s", runner.ErrUnknownAgent, agentName)
	}

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
		}
		dec, rerr := s.resolver.Resolve(req)
		if rerr != nil {
			return "", "", "", rerr
		}
		return dec.Provider, dec.Model, dec.Effort, nil
	}

	// Pin path (or fallback to defaults): use the v0.6.x logic.
	if !hasPin && s.cfg.Defaults.Model == "" {
		return "", "", "", fmt.Errorf("%w: agent %q has no pin, no tier, and no defaults", runner.ErrInvalidArgument, agentName)
	}
	providerID, model, err = s.cfg.ResolveAgentModel(agentName)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: %v", runner.ErrInvalidArgument, err)
	}
	// Effort still flows through on the pin path — an explicit-pin
	// agent can declare effort and the driver will translate it
	// where supported. Empty when not declared.
	return providerID, model, def.Effort, nil
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
	agentDef, ok := s.cfg.Agents[effectiveAgentName]
	if !ok {
		return fmt.Errorf("%w: %s", runner.ErrUnknownAgent, effectiveAgentName)
	}
	providerID, model, effort, err := s.resolveAgent(effectiveAgentName)
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
	release, err := s.sem.Acquire(ctx)
	if err != nil {
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
	dispatcher := tools.NewDispatcher(allowedTools)

	// ---- Segments: prepend agent's system prompt ----
	segments := in.Segments
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
	identity := store.RunIdentity{AgentID: agentID, UserID: effectiveUserID}
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
	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:   agentID,
		RunID:     runID,
		SessionID: sessionID,
		UserID:    effectiveUserID,
		StartedAt: time.Now(),
	}, cancelFn)
	if errors.Is(regErr, cancel.ErrInUse) {
		s.finishRunFailedReason(runID, "agent_id collision; run never started")
		return fmt.Errorf("%w: agent_id %q is already mapped to an active run", runner.ErrAgentIDInUse, agentID)
	}
	if regErr != nil {
		s.finishRunFailedReason(runID, "registry register failed: "+regErr.Error())
		return fmt.Errorf("%w: %v", runner.ErrInternal, regErr)
	}
	defer s.cancelReg.Deregister(agentID)

	// ---- Persist input segments ----
	if s.store != nil && runID != "" {
		if inputJSON, err := json.Marshal(in.Segments); err == nil {
			if err := s.store.AppendEvent(ctx, runID, "user_input", inputJSON); err != nil {
				log.Printf("store: AppendEvent(user_input) failed: %v", err)
			}
		}
	}

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
		UserID:  effectiveUserID,
		AgentID: agentID,
	})
	loopCtx = tools.WithHostPolicy(loopCtx, hostPolicy)

	heartbeat := s.makeHeartbeat(runID)

	res, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:      provider,
		Model:         model,
		Tools:         allowedTools,
		Dispatcher:    dispatcher,
		Segments:      segments,
		PriorMessages: priorMessages,
		OnEvent:       emit,
		OnHeartbeat:   heartbeat,
		MaxTokens:     agentDef.MaxTokens,
		Effort:        effort,
		MarkStalled:   s.markStalledFn(providerID, model),
	})
	s.finishRunWithCancel(ctx, runCtx, runID, res, runErr)
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
	return s.sessionLocks.TryLock(id)
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

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
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

	agentDef, ok := s.cfg.Agents[req.Agent]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown agent %q", req.Agent), http.StatusBadRequest)
		return
	}

	providerID, model, effort, err := s.resolveAgent(req.Agent)
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
	release, err := s.sem.Acquire(r.Context())
	if err != nil {
		if concurrency.IsBackpressure(err) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"code":"backpressure","error":%q}`, err.Error())
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	dispatcher := tools.NewDispatcher(allowedTools)

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
	identity := store.RunIdentity{AgentID: agentID, UserID: req.UserID}
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

	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:   agentID,
		RunID:     runID,
		SessionID: sessionID,
		UserID:    req.UserID,
		StartedAt: time.Now(),
	}, cancelFn)
	if errors.Is(regErr, cancel.ErrInUse) {
		// We've already created the session+run row in the store
		// (session creation is unavoidable to satisfy the FK on
		// runs). Leaving the run at status=running would orphan it
		// permanently — the heartbeat sweeper (when it lands) would
		// eventually catch it, but in the meantime it pollutes
		// ListActiveRunsByUser. Mark it failed with a clear reason
		// so the row is terminal from this exit path.
		s.finishRunFailedReason(runID, "agent_id collision; run never started")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"code":"agent_id_in_use","error":"agent_id %q is already mapped to an active run"}`, agentID)
		return
	}
	if regErr != nil {
		s.finishRunFailedReason(runID, "registry register failed: "+regErr.Error())
		http.Error(w, regErr.Error(), http.StatusInternalServerError)
		return
	}
	defer s.cancelReg.Deregister(agentID)

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

	stream, ok := newSSE(w)
	if !ok {
		// ResponseWriter doesn't implement http.Flusher — every frame would
		// be buffered until handler return, defeating SSE. Refuse cleanly so
		// the caller gets a useful error instead of silent buffering.
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()

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
		UserID:  req.UserID,
		AgentID: agentID,
	})
	// Stash the caller's host policy so any sub-agents spawned by the
	// Agent tool inherit the same allowed_hosts / WebSearchFilter
	// narrowing the parent received.
	loopCtx = tools.WithHostPolicy(loopCtx, hostPolicy)

	// Heartbeat hook: each loop iteration updates last_heartbeat_at so a
	// future sweeper can detect crashed processes (no heartbeat for > N
	// minutes → presumed dead). Cheap (~10–100 calls per run).
	heartbeat := s.makeHeartbeat(runID)

	loopRes, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:    provider,
		Model:       model,
		Tools:       allowedTools,
		Dispatcher:  dispatcher,
		Segments:    req.Segments,
		OnEvent:     emit,
		OnHeartbeat: heartbeat,
		MaxTokens:   agentDef.MaxTokens, // 0 → driver default
		Effort:      effort,
		MarkStalled: s.markStalledFn(providerID, model),
	})
	if runErr != nil {
		stream.send(providers.Event{Type: providers.EventError, Error: runErr.Error()})
	}

	s.finishRunWithCancel(r.Context(), runCtx, runID, loopRes, runErr)
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
	agentDef, ok := s.cfg.Agents[sess.Agent]
	if !ok {
		http.Error(w, fmt.Sprintf("session refers to unknown agent %q", sess.Agent), http.StatusBadRequest)
		return
	}
	providerID, model, effort, err := s.resolveAgent(sess.Agent)
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
	// is reported as 429.
	release, err := s.sem.Acquire(r.Context())
	if err != nil {
		if concurrency.IsBackpressure(err) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"code":"backpressure","error":%q}`, err.Error())
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer release()

	allowedTools := filterTools(s.tools, agentDef.AllowedTools, body.AllowedTools)
	if body.AllowedHosts != nil || s.cfg.Env.HTTPCallerAuthoritative {
		var caller []string
		if body.AllowedHosts != nil {
			caller = *body.AllowedHosts
		}
		allowedTools = builtin.NarrowHosts(allowedTools, caller, body.WebSearchFilter, s.cfg.Env.HTTPCallerAuthoritative)
	}
	dispatcher := tools.NewDispatcher(allowedTools)

	// Re-prepend the agent's system prompt — it isn't in the transcript
	// (it's per-call configuration, not conversation content).
	segments := body.Segments
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
	// inherited from the session (set at original creation).
	run, err := s.store.CreateRun(r.Context(), id, store.RunIdentity{
		AgentID: agentID,
		UserID:  sess.UserID,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Derive a runCtx with cancel-cause and register in the cancel
	// registry. Same shape as handleRuns.
	runCtx, cancelFn := context.WithCancelCause(r.Context())
	defer cancelFn(nil)
	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:   agentID,
		RunID:     run.ID,
		SessionID: id,
		UserID:    sess.UserID,
		StartedAt: time.Now(),
	}, cancelFn)
	if errors.Is(regErr, cancel.ErrInUse) {
		// Same orphan-row mitigation as handleRuns — the run was
		// already inserted at status=running.
		s.finishRunFailedReason(run.ID, "agent_id collision; run never started")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"code":"agent_id_in_use","error":"agent_id %q is already mapped to an active run"}`, agentID)
		return
	}
	if regErr != nil {
		s.finishRunFailedReason(run.ID, "registry register failed: "+regErr.Error())
		http.Error(w, regErr.Error(), http.StatusInternalServerError)
		return
	}
	defer s.cancelReg.Deregister(agentID)

	// Persist the new user input segments so a future replay sees them.
	if inputJSON, err := json.Marshal(body.Segments); err == nil {
		if err := s.store.AppendEvent(r.Context(), run.ID, "user_input", inputJSON); err != nil {
			log.Printf("store: AppendEvent(user_input) failed: %v", err)
		}
	}

	stream, ok := newSSE(w)
	if !ok {
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()
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
		UserID:  sess.UserID,
		AgentID: agentID,
	})
	loopRes, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:      provider,
		Model:         model,
		Tools:         allowedTools,
		Dispatcher:    dispatcher,
		Segments:      segments,
		PriorMessages: priorMessages,
		OnEvent:       emit,
		OnHeartbeat:   heartbeat,
		MaxTokens:     agentDef.MaxTokens, // 0 → driver default
		Effort:        effort,
		MarkStalled:   s.markStalledFn(providerID, model),
	})
	if runErr != nil {
		stream.send(providers.Event{Type: providers.EventError, Error: runErr.Error()})
	}

	s.finishRunWithCancel(r.Context(), runCtx, run.ID, loopRes, runErr)
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
type transcriptEvent struct {
	Seq   int64           `json:"seq"`
	RunID string          `json:"run_id"`
	TsNs  int64           `json:"ts_ns"`
	Type  string          `json:"type"`
	Event providers.Event `json:"event"`
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
		// Decode payload back to a typed Event. If it fails (corrupt row),
		// surface a minimal record so the rest of the transcript still ships.
		if err := json.Unmarshal(ev.Payload, &te.Event); err != nil {
			te.Event = providers.Event{Type: providers.EventType(ev.Type)}
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
func (s *Server) makeRecordingEmit(ctx context.Context, runID string, fwd func(providers.Event)) func(providers.Event) {
	if s.store == nil || runID == "" {
		return fwd
	}
	return func(ev providers.Event) {
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
// built-in tool. It looks up the named agent in cfg.Agents, builds a
// fresh session+run for the sub-execution, drives loop.Run with the
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
func (s *Server) runSubAgent(ctx context.Context, name string, prompt string) (string, error) {
	def, ok := s.cfg.Agents[name]
	if !ok {
		return "", fmt.Errorf("unknown sub-agent %q (not in loomcycle.yaml agents map)", name)
	}

	providerID, model, effort, err := s.resolveAgent(name)
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
		// ParentRunID is left empty here — we don't have the parent's
		// run.ID handy without an extra registry lookup. Cascade
		// works via parent_agent_id alone; ParentRunID is informational
		// for transcript stitching and can be filled in by a future
		// refactor that threads parent run.ID through ctx.
		UserID: parentIdentity.UserID,
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
	subDispatcher := tools.NewDispatcher(subTools)

	// Persist the input segments as the first event so transcript
	// replay reconstructs the user prompt the same way fresh runs do.
	if s.store != nil && subRunID != "" {
		if inputJSON, err := json.Marshal(segs); err == nil {
			if err := s.store.AppendEvent(ctx, subRunID, "user_input", inputJSON); err != nil {
				log.Printf("store: AppendEvent(user_input) failed for sub-run %s: %v", subRunID, err)
			}
		}
	}

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
		UserID:  parentIdentity.UserID,
		AgentID: subAgentID,
	})

	subHeartbeat := s.makeHeartbeat(subRunID)

	res, runErr := loop.Run(subCtx, loop.RunOptions{
		Provider:    provider,
		Model:       model,
		Tools:       subTools,
		Dispatcher:  subDispatcher,
		Segments:    segs,
		OnEvent:     subEmit,
		OnHeartbeat: subHeartbeat,
		MaxTokens:   def.MaxTokens, // 0 → driver default
		Effort:      effort,
		MarkStalled: s.markStalledFn(providerID, model),
	})
	s.finishRunWithCancel(ctx, subRunCtx, subRunID, res, runErr)

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
	AgentID         string             `json:"agent_id"`
	RunID           string             `json:"run_id"`
	SessionID       string             `json:"session_id"`
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
}

type agentResponseUsage struct {
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheCreationTokens int    `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int    `json:"cache_read_tokens,omitempty"`
	Model               string `json:"model,omitempty"`
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
		},
		Live: live,
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
	writeJSON(w, http.StatusOK, runToAgentResponse(run, live))
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
		writeJSON(w, http.StatusOK, cancelResponse{
			Cancelled: true,
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
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
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

// makeHeartbeat returns a callback the loop fires at each iteration.
// It updates runs.last_heartbeat_at via a fire-and-forget background
// context (the loop's ctx may be cancelled mid-write; the heartbeat
// shouldn't gate on it).
//
// nil store or runID makes this a no-op so v0.2 callers stay
// hands-off.
func (s *Server) makeHeartbeat(runID string) func() {
	if s.store == nil || runID == "" {
		return nil
	}
	return func() {
		bg, cancel := context.WithTimeout(context.Background(), 1*time.Second)
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
func (s *Server) finishRunWithCancel(ctx context.Context, runCtx context.Context, runID string, res loop.RunResult, runErr error) {
	if cause := context.Cause(runCtx); errors.Is(cause, cancel.ErrCancelledByAPI) {
		// API-cancel terminal write. Reason text comes from the
		// optional wrapper; falls back to the sentinel string.
		reason := cancel.ReasonFromCause(cause)
		if reason == "" {
			reason = "cancelled by api"
		}
		s.finishRunCancelled(ctx, runID, res, reason)
		return
	}
	s.finishRun(ctx, runID, res, runErr)
}

// finishRunFailedReason marks a run terminal with status=failed and
// the supplied error string, no usage. Used by the BLOCKING-fix paths
// where we created a run row but bailed before the loop ran (e.g.
// agent_id collision in the registry). Without this, those rows
// orphan at status=running and pollute ListActiveRunsByUser.
//
// Mirrors finishRun's structure: fresh background ctx with 5s timeout
// so the write isn't lost when the request ctx is already torn down.
func (s *Server) finishRunFailedReason(runID, reason string) {
	if s.store == nil || runID == "" {
		return
	}
	bg, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFn()
	if err := s.store.FinishRun(bg, runID, store.RunFailed, "", store.Usage{}, reason); err != nil {
		log.Printf("store: FinishRun(failed reason=%q) failed (run=%s): %v", reason, runID, err)
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
func (s *Server) finishRunCancelled(_ context.Context, runID string, res loop.RunResult, reason string) {
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
	}
	if err := s.store.FinishRun(bg, runID, store.RunCancelled, reason, usage, ""); err != nil {
		log.Printf("store: FinishRun(cancelled) failed (run=%s): %v", runID, err)
	}
}

// finishRun marks the run terminal in the store. status is derived from
// runErr: nil → completed, non-nil → failed. ctx may already be cancelled
// (the client disconnected); we use a fresh background context with a short
// timeout so the FinishRun write isn't lost.
func (s *Server) finishRun(_ context.Context, runID string, res loop.RunResult, runErr error) {
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
	}
	if err := s.store.FinishRun(bg, runID, status, res.StopReason, usage, errMsg); err != nil {
		log.Printf("store: FinishRun failed (run=%s): %v", runID, err)
	}
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
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.cfg.Env.AuthToken
		if !auth.CompareBearer(got, want) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
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
