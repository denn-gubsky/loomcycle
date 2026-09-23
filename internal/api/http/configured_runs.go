package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC DI D5 — configured (created, not started) runs over HTTP:
//
//	POST   /v1/runs {…, "start": false}  create a draft (201)
//	PATCH  /v1/runs/{run_id}             replace fields of the draft
//	POST   /v1/runs/{run_id}/start       start it; streams SSE like POST /v1/runs
//	DELETE /v1/runs/{run_id}             discard it (and its session)
//
// A draft holds no slot and is not admitted: admission, the budget check and
// every registration happen at start, through RunOnce. It never stores a
// secret — user_bearer / user_credentials are given to /start.

// runDraft is what runs.draft holds: the request that will start the run,
// minus secrets. It is the connector's spawn request — so /start maps it into
// a RunInput through the same function every spawn surface uses — plus the one
// POST /v1/runs field that request lacks.
type runDraft struct {
	connector.SpawnRunRequest
	RunTimeoutSeconds int `json:"run_timeout_seconds,omitempty"`
}

// draftFromRequest converts a validated POST /v1/runs body into a draft.
//
// Through JSON on purpose: the two shapes name their fields the same, so a
// field added to both is carried without an edit here, and a field added to
// runRequest alone is caught by TestRunDraft_CarriesEveryRequestField rather
// than silently dropped from every configured run.
func draftFromRequest(req runRequest) (runDraft, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return runDraft{}, err
	}
	var d runDraft
	if err := json.Unmarshal(b, &d); err != nil {
		return runDraft{}, err
	}
	// The row carries identity; the draft never carries a secret.
	d.SessionID, d.TenantID, d.UserID, d.AgentID = "", "", "", ""
	d.UserBearer, d.UserCredentials = "", nil
	return d, nil
}

// draftImmutableKeys are the fields a PATCH may not change: identity is fixed
// when the draft is created, secrets are supplied at start, and `start` is a
// verb of POST /v1/runs, not a field of the draft.
var draftImmutableKeys = map[string]string{
	"agent":            "the agent is fixed when the draft is created",
	"agent_id":         "the agent_id is fixed when the draft is created",
	"user_id":          "the user is fixed when the draft is created",
	"tenant_id":        "the tenant is fixed when the draft is created",
	"session_id":       "a configured run starts in its own session",
	"user_bearer":      "secrets are supplied to /start, never stored on a draft",
	"user_credentials": "secrets are supplied to /start, never stored on a draft",
	"start":            "start a draft with POST /v1/runs/{run_id}/start",
}

// createConfiguredRun is POST /v1/runs with start:false, called by handleRuns
// once the request is fully validated — identity made authoritative, the agent
// resolved, the overrides checked against it — and before any admission.
func (s *Server) createConfiguredRun(w http.ResponseWriter, r *http.Request, req runRequest, okr, isolated bool) {
	if s.store == nil {
		http.Error(w, "configured runs require persistence (no store configured)", http.StatusServiceUnavailable)
		return
	}
	if req.SessionID != "" {
		http.Error(w, "a configured run starts in its own session; session_id is not accepted with start:false", http.StatusBadRequest)
		return
	}
	if req.UserBearer != "" || len(req.UserCredentials) > 0 {
		http.Error(w, "a configured run never stores secrets; supply user_bearer / user_credentials to POST /v1/runs/{run_id}/start", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if n, err := s.store.CountConfiguredRuns(ctx, req.TenantID, req.UserID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if max := s.cfg().Env.MaxConfiguredRunsPerUser; max > 0 && n >= max {
		writeJSONError(w, http.StatusTooManyRequests, "configured_run_cap",
			fmt.Sprintf("this user already holds %d configured runs (the cap is %d); start or discard one first", n, max))
		return
	}
	agentID := req.AgentID
	if agentID == "" {
		agentID = newAgentID()
	} else if taken, err := s.agentIDTaken(ctx, agentID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if taken {
		writeJSONError(w, http.StatusConflict, "agent_id_in_use", fmt.Sprintf("agent_id %q is already in use by a live run or another configured run", agentID))
		return
	}
	draft, err := draftFromRequest(req)
	if err != nil {
		http.Error(w, "encode draft: "+err.Error(), http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(draft)
	if err != nil {
		http.Error(w, "encode draft: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sess, err := s.store.CreateSession(ctx, req.TenantID, req.Agent, req.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	run, err := s.store.CreateConfiguredRun(ctx, sess.ID, store.RunIdentity{
		AgentID: agentID, UserID: req.UserID, TenantID: req.TenantID, UserTier: req.UserTier,
		ParentContext: req.ParentContext, Interactive: req.Interactive,
		OperatorKeyRestricted: okr, Isolated: isolated,
	}, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"run_id": run.ID, "agent_id": run.AgentID, "session_id": run.SessionID,
		"status": string(store.RunConfigured), "draft": json.RawMessage(body),
	})
}

// agentIDTaken reports whether agentID belongs to a live run or to another
// draft. Reserved at create, because GetRunByAgentID answers with the latest
// row: a draft and a live run sharing an id would hide one another.
func (s *Server) agentIDTaken(ctx context.Context, agentID string) (bool, error) {
	if _, live := s.cancelReg.Get(agentID); live {
		return true, nil
	}
	run, err := s.store.GetRunByAgentID(ctx, agentID)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return run.Status == store.RunConfigured || run.Status == store.RunRunning, nil
}

// configuredRunFor resolves {run_id} for the draft routes under the same gates
// as every other run read: another tenant's run is the opaque 404 a missing
// one gets, and so is another user's run for an isolated member. It answers
// 409 for a run that is visible but not a draft.
func (s *Server) configuredRunFor(w http.ResponseWriter, r *http.Request) (store.Run, bool) {
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return store.Run{}, false
	}
	if s.store == nil {
		http.Error(w, "configured runs require persistence (no store configured)", http.StatusServiceUnavailable)
		return store.Run{}, false
	}
	run, err := s.tenantStore(r.Context()).GetRun(r.Context(), runID)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		writeJSONError(w, http.StatusNotFound, "unknown_run", "no run found for that run_id")
		return store.Run{}, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return store.Run{}, false
	}
	if sess, err := s.store.GetSession(r.Context(), run.SessionID); err != nil || !sessionOwnershipOK(r.Context(), sess) {
		writeJSONError(w, http.StatusNotFound, "unknown_run", "no run found for that run_id")
		return store.Run{}, false
	}
	if run.Status != store.RunConfigured {
		writeJSONError(w, http.StatusConflict, "run_not_configured",
			fmt.Sprintf("run %s is %s, not configured", run.ID, run.Status))
		return store.Run{}, false
	}
	return run, true
}

// handlePatchConfiguredRun serves PATCH /v1/runs/{run_id}: the body's keys
// replace the draft's (a JSON null removes one), and the result is validated
// as a create would be — against the agent as it is now.
func (s *Server) handlePatchConfiguredRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.configuredRunFor(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBytes())
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(patch) == 0 {
		http.Error(w, "an empty patch changes nothing", http.StatusUnprocessableEntity)
		return
	}
	for k := range patch {
		if why, bad := draftImmutableKeys[k]; bad {
			http.Error(w, fmt.Sprintf("%q cannot be patched: %s", k, why), http.StatusBadRequest)
			return
		}
	}
	current, err := s.store.GetRunDraft(r.Context(), run.ID)
	if err != nil {
		writeDraftStoreError(w, err)
		return
	}
	merged := map[string]json.RawMessage{}
	if len(current) > 0 {
		if err := json.Unmarshal(current, &merged); err != nil {
			http.Error(w, "stored draft is not an object: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// `prompt` is the create-time sugar for one user segment; honoured here so
	// a caller patches the way it created.
	if raw, ok := patch["prompt"]; ok {
		delete(patch, "prompt")
		var text string
		if err := json.Unmarshal(raw, &text); err != nil || text == "" {
			http.Error(w, `"prompt" must be a non-empty string`, http.StatusBadRequest)
			return
		}
		seg, _ := json.Marshal([]loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}}}})
		patch["segments"] = seg
	}
	for k, v := range patch {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	body, _ := json.Marshal(merged)
	var d runDraft
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		http.Error(w, "invalid draft: "+err.Error(), http.StatusBadRequest)
		return
	}
	if msg, ok := s.validateDraft(r.Context(), run, d); !ok {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	body, _ = json.Marshal(d)
	if err := s.store.UpdateRunDraft(r.Context(), run.ID, body); err != nil {
		writeDraftStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": run.ID, "status": string(store.RunConfigured), "draft": json.RawMessage(body),
	})
}

// validateDraft applies the checks POST /v1/runs makes before it would admit
// a run, so an edit that could never start is refused when it is made rather
// than at start. Start validates again: the definition may move in between.
func (s *Server) validateDraft(ctx context.Context, run store.Run, d runDraft) (string, bool) {
	if len(d.Segments) == 0 {
		return `no input: the draft must keep "segments" (or set a "prompt")`, false
	}
	if err := d.ToolChoice.Validate(); err != nil {
		return err.Error(), false
	}
	if err := d.OutputFormat.Validate(); err != nil {
		return err.Error(), false
	}
	if d.UserTier != "" && len(s.cfg().UserTiers) > 0 {
		if _, ok := s.cfg().UserTiers[d.UserTier]; !ok {
			return fmt.Sprintf("unknown user_tier %q", d.UserTier), false
		}
	}
	if msg, ok := validateParentContext(d.ParentContext); !ok {
		return msg, false
	}
	agentDef, ok := s.lookupAgent(ctx, run.TenantID, d.Agent)
	if !ok {
		return fmt.Sprintf("unknown agent %q", d.Agent), false
	}
	if _, err := s.effectiveDef(ctx, agentDef, runOverrides{
		Routing:   &routingOverride{Model: d.Model, Provider: d.Provider, Tier: d.Tier, Effort: d.Effort},
		Resources: &resourceOverride{MaxTokens: d.MaxTokens, MaxIterations: d.MaxIterations, UnboundedIterations: d.UnboundedIterations, MaxConcurrentChildren: d.MaxConcurrentChildren},
		Tuning:    &tuningOverride{RetryAttempts: d.RetryAttempts, MemoryInjectMaxTokens: d.MemoryInjectMaxTokens, MemoryIndexMaxBytes: d.MemoryIndexMaxBytes, InjectToolGuide: d.InjectToolGuide},
	}); err != nil {
		return err.Error(), false
	}
	return "", true
}

// handleDeleteConfiguredRun serves DELETE /v1/runs/{run_id}: discards a draft
// and its session. A live run is not discarded — cancel is its verb.
func (s *Server) handleDeleteConfiguredRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.configuredRunFor(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteConfiguredRun(r.Context(), run.ID); err != nil {
		writeDraftStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// startRequest is the optional body of POST /v1/runs/{run_id}/start: the
// secrets a draft never stores.
type startRequest struct {
	UserBearer      string            `json:"user_bearer,omitempty"`
	UserCredentials map[string]string `json:"user_credentials,omitempty"`
}

// handleStartConfiguredRun serves POST /v1/runs/{run_id}/start. The draft is
// turned back into a RunInput and started through RunOnce — the start path
// gRPC, MCP, the scheduler and webhooks share — which runs today's admission
// and then moves the row configured → running. A refusal before that point
// answers with a status code and leaves the draft as it was; once the run is
// registered the response is the run's SSE stream, as for POST /v1/runs.
func (s *Server) handleStartConfiguredRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.configuredRunFor(w, r)
	if !ok {
		return
	}
	var sreq startRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&sreq); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if sreq.UserBearer != "" && !validUserBearer(sreq.UserBearer) {
		http.Error(w, `user_bearer must match [A-Za-z0-9._\-+/=]{16,512}`, http.StatusBadRequest)
		return
	}
	if errMsg, ok := connector.ValidateUserCredentialsMap(sreq.UserCredentials); !ok {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	raw, err := s.store.GetRunDraft(r.Context(), run.ID)
	if err != nil {
		writeDraftStoreError(w, err)
		return
	}
	var d runDraft
	if err := json.Unmarshal(raw, &d); err != nil {
		http.Error(w, "stored draft is unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	in := spawnRequestToRunInput(d.SpawnRunRequest)
	in.RunTimeoutSeconds = d.RunTimeoutSeconds
	in.Interactive = d.Interactive != nil && *d.Interactive
	// Identity and confinement are the row's, fixed at create.
	in.ConfiguredRunID = run.ID
	in.SessionID = ""
	in.AgentID, in.TenantID, in.UserID = run.AgentID, run.TenantID, run.UserID
	in.ParentContext = run.ParentContext
	in.Isolated, in.OperatorKeyRestricted = run.Isolated, run.OperatorKeyRestricted
	in.UserBearer, in.UserCredentials = sreq.UserBearer, sreq.UserCredentials

	// The stream opens only once the run is registered, so every refusal that
	// happens before it is still an ordinary HTTP status.
	var stream *sse
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, sessionID, _ string) {
			st, ok := newSSE(w)
			if !ok {
				return
			}
			stream = st
			stream.start()
			stream.startKeepalive(r.Context(), s.cfg().Env.SSEKeepaliveInterval)
			stream.send(providers.Event{Type: "session", Text: sessionID})
			stream.sendRaw("agent", map[string]any{
				"agent_id": agentID, "run_id": runID, "session_id": sessionID,
				"parent_agent_id": nil, "parent_context": in.ParentContext,
			})
		},
		OnEvent: func(ev providers.Event) {
			if stream != nil {
				stream.send(ev)
			}
		},
	}
	runErr := s.RunOnce(r.Context(), in, cb)
	if stream == nil {
		if runErr == nil {
			// Registered is always called before the loop; reaching here
			// without a stream means the writer could not flush.
			http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
			return
		}
		writeRunOnceError(w, runErr)
		return
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		// The loop already emitted its error event for a failure it saw; an
		// error RunOnce returns after registration without one still reaches
		// the caller rather than ending the stream silently.
		stream.send(runErrorEvent(runErr))
	}
}

// writeRunOnceError maps a RunOnce refusal that happened before the run was
// registered to the status POST /v1/runs answers the same refusal with.
func writeRunOnceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, resolve.ErrOperatorKeyRestricted):
		writeResolveError(w, err)
	case errors.Is(err, runner.ErrRunNotConfigured):
		writeJSONError(w, http.StatusConflict, "run_not_configured", err.Error())
	case errors.Is(err, runner.ErrAgentIDInUse):
		writeJSONError(w, http.StatusConflict, "agent_id_in_use", err.Error())
	case errors.Is(err, runner.ErrInvalidArgument), errors.Is(err, runner.ErrUnknownAgent), errors.Is(err, runner.ErrUnknownProvider):
		writeJSONError(w, http.StatusBadRequest, "invalid_run", err.Error())
	case errors.Is(err, runner.ErrTokenLimitExceeded):
		writeJSONError(w, http.StatusTooManyRequests, "token_limit_exceeded", err.Error())
	case errors.Is(err, runner.ErrPerUserQuotaExhausted):
		w.Header().Set("Retry-After", "5")
		writeJSONError(w, http.StatusTooManyRequests, "per_user_quota_exhausted", err.Error())
	case errors.Is(err, runner.ErrProviderConcurrencyExhausted):
		w.Header().Set("Retry-After", "5")
		writeJSONError(w, http.StatusTooManyRequests, "provider_concurrency_exhausted", err.Error())
	case errors.Is(err, runner.ErrBackpressure):
		writeJSONError(w, http.StatusTooManyRequests, "backpressure", err.Error())
	case errors.Is(err, runner.ErrRuntimePaused):
		writeJSONError(w, http.StatusServiceUnavailable, "runtime_paused", err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// writeDraftStoreError maps the store's draft sentinels. A draft that stopped
// being one between the route's check and the store call (a concurrent start
// or discard) is the same 409 the check itself would have given.
func writeDraftStoreError(w http.ResponseWriter, err error) {
	var nf *store.ErrNotFound
	switch {
	case errors.Is(err, store.ErrRunNotConfigured):
		writeJSONError(w, http.StatusConflict, "run_not_configured", "the run is no longer configured")
	case errors.As(err, &nf):
		writeJSONError(w, http.StatusNotFound, "unknown_run", "no run found for that run_id")
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// RunConfiguredRunSweeper discards drafts older than the configured TTL, on
// every tick, until ctx ends. Every replica may run it: each discard is one
// guarded transaction, so two sweeps never delete the same draft twice and a
// draft started meanwhile is left alone.
func (s *Server) RunConfiguredRunSweeper(ctx context.Context) {
	ttl := s.cfg().Env.ConfiguredRunTTL
	if s.store == nil || ttl <= 0 {
		return
	}
	interval := ttl / 4
	if interval > 10*time.Minute {
		interval = 10 * time.Minute
	}
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepConfiguredRuns(ctx, time.Now().Add(-ttl))
		}
	}
}

// sweepConfiguredRuns is one expiry pass: every draft created before cutoff is
// discarded with its session.
func (s *Server) sweepConfiguredRuns(ctx context.Context, cutoff time.Time) {
	n, err := s.store.SweepExpiredConfiguredRuns(ctx, cutoff)
	if err != nil {
		log.Printf("configured runs: expiry sweep: %v", err)
	} else if n > 0 {
		log.Printf("configured runs: discarded %d draft(s) created before %s", n, cutoff.Format(time.RFC3339))
	}
}
