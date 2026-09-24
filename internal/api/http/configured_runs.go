package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC DI D5 — configured (created, not started) runs.
//
//	POST   /v1/runs {…, "start": false}  create a draft (201)
//	PATCH  /v1/runs/{run_id}             replace fields of the draft
//	POST   /v1/runs/{run_id}/start       start it; streams SSE like POST /v1/runs
//	DELETE /v1/runs/{run_id}             discard it (and its session)
//
// A draft holds no slot and is not admitted: admission, the budget check and
// every registration happen at start, through RunOnce. It never stores a
// secret — user_bearer / user_credentials are given to the start.
//
// The rules live in the *Core functions below, which every transport calls:
// the HTTP handlers here, and gRPC / MCP through the connector methods. Each
// returns a *draftErr carrying the HTTP status, which the other transports map
// to their own codes — so a draft is refused for the same reason everywhere.

// runDraft is what runs.draft holds: the request that will start the run,
// minus secrets and identity. It is the connector's spawn request — so the
// start maps it into a RunInput through the same function every spawn surface
// uses — plus the one POST /v1/runs field that request lacks.
type runDraft = connector.ConfiguredRunRequest

// draftErr is a refusal from a configured-run operation.
type draftErr struct {
	status     int
	code       string
	msg        string
	retryAfter int
}

func (e *draftErr) Error() string { return e.msg }

// HTTPStatus lets gRPC and MCP map the refusal without importing this type
// (the compactErr / replayErr pattern).
func (e *draftErr) HTTPStatus() int { return e.status }

func draftRefusal(status int, code, format string, args ...any) *draftErr {
	return &draftErr{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

// writeDraftErr writes a configured-run refusal as the JSON error body the
// other run routes use.
func writeDraftErr(w http.ResponseWriter, err error) {
	var de *draftErr
	if !errors.As(err, &de) {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if de.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(de.retryAfter))
	}
	writeJSONError(w, de.status, de.code, de.msg)
}

// draftStoreErr maps the store's draft sentinels. A draft that stopped being
// one between the gate and the store call (a concurrent start or discard) is
// the same 409 the gate itself would have given.
func draftStoreErr(err error) error {
	var nf *store.ErrNotFound
	switch {
	case errors.Is(err, store.ErrRunNotConfigured):
		return draftRefusal(http.StatusConflict, "run_not_configured", "the run is no longer configured")
	case errors.As(err, &nf):
		return draftRefusal(http.StatusNotFound, "unknown_run", "no run found for that run_id")
	default:
		return draftRefusal(http.StatusInternalServerError, "internal", "%v", err)
	}
}

// draftImmutableKeys are the fields a PATCH may not change: identity is fixed
// when the draft is created, secrets are supplied at start, and `start` is a
// verb, not a field of the draft. parent_context is identity too: the row holds
// it and the start takes it from there, so a patched copy would be shown and
// ignored.
var draftImmutableKeys = map[string]string{
	"agent":            "the agent is fixed when the draft is created",
	"agent_id":         "the agent_id is fixed when the draft is created",
	"user_id":          "the user is fixed when the draft is created",
	"tenant_id":        "the tenant is fixed when the draft is created",
	"session_id":       "a configured run starts in its own session",
	"parent_context":   "the parent_context is fixed when the draft is created",
	"user_bearer":      "secrets are supplied at start, never stored on a draft",
	"user_credentials": "secrets are supplied at start, never stored on a draft",
	"start":            "start a draft with its start operation",
}

// requestToDraft converts a POST /v1/runs body into a configured-run request.
//
// Through JSON on purpose: the two shapes name their fields the same, so a
// field added to both is carried without an edit here, and a field added to
// runRequest alone is caught by TestRunDraft_CarriesEveryRequestField rather
// than silently dropped from every configured run.
func requestToDraft(req runRequest) (runDraft, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return runDraft{}, err
	}
	var d runDraft
	if err := json.Unmarshal(b, &d); err != nil {
		return runDraft{}, err
	}
	return d, nil
}

// createConfiguredRunCore validates req exactly as a run would be — identity
// made authoritative, the agent resolved, the overrides checked against it —
// and stores it as a draft instead of admitting it.
func (s *Server) createConfiguredRunCore(ctx context.Context, req runDraft) (connector.ConfiguredRun, error) {
	if s.store == nil {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusServiceUnavailable, "no_store", "configured runs require persistence (no store configured)")
	}
	if req.SessionID != "" {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "a configured run starts in its own session; session_id is not accepted")
	}
	if req.UserBearer != "" || len(req.UserCredentials) > 0 {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "a configured run never stores secrets; supply user_bearer / user_credentials when starting it")
	}
	if req.Agent == "" {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "agent is required")
	}
	if req.UserID != "" && !validIdent(req.UserID) {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "user_id must match [A-Za-z0-9_-]{1,128}")
	}
	if req.AgentID != "" && !validIdent(req.AgentID) {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "agent_id must match [A-Za-z0-9_-]{1,128}")
	}
	tenant, user := s.applyPrincipal(ctx, req.TenantID, req.UserID)
	okr := s.operatorKeyRestrictedForCtx(ctx)
	isolated := s.isolatedForCtx(ctx)
	if req.ParentContext.IsZero() {
		req.ParentContext = nil
	}
	if msg, ok := s.validateDraft(ctx, tenant, req); !ok {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "%s", msg)
	}
	if n, err := s.store.CountConfiguredRuns(ctx, tenant, user); err != nil {
		return connector.ConfiguredRun{}, draftStoreErr(err)
	} else if max := s.cfg().Env.MaxConfiguredRunsPerUser; max > 0 && n >= max {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusTooManyRequests, "configured_run_cap",
			"this user already holds %d configured runs (the cap is %d); start or discard one first", n, max)
	}
	agentID := req.AgentID
	if agentID == "" {
		agentID = newAgentID()
	} else if taken, err := s.agentIDTaken(ctx, agentID); err != nil {
		return connector.ConfiguredRun{}, draftStoreErr(err)
	} else if taken {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusConflict, "agent_id_in_use", agentIDInUseMsg, agentID)
	}
	// The row carries identity; the draft never carries it, nor a secret.
	stored := req
	stored.SessionID, stored.TenantID, stored.UserID, stored.AgentID = "", "", "", ""
	stored.UserBearer, stored.UserCredentials = "", nil
	body, err := json.Marshal(stored)
	if err != nil {
		return connector.ConfiguredRun{}, draftStoreErr(err)
	}
	sess, err := s.store.CreateSession(ctx, tenant, req.Agent, user)
	if err != nil {
		return connector.ConfiguredRun{}, draftStoreErr(err)
	}
	run, err := s.store.CreateConfiguredRun(ctx, sess.ID, store.RunIdentity{
		AgentID: agentID, UserID: user, TenantID: tenant, UserTier: req.UserTier,
		ParentContext: req.ParentContext, Interactive: req.Interactive != nil && *req.Interactive,
		OperatorKeyRestricted: okr, Isolated: isolated,
	}, body)
	if err != nil {
		return connector.ConfiguredRun{}, draftStoreErr(err)
	}
	return connector.ConfiguredRun{
		RunID: run.ID, AgentID: run.AgentID, SessionID: run.SessionID,
		Status: string(store.RunConfigured), Draft: body,
	}, nil
}

// agentIDInUseMsg is the refusal for an explicit agent_id another run or a
// draft already holds — one wording for the draft create and every run start.
const agentIDInUseMsg = "agent_id %q is already in use by a live run or another configured run"

// agentIDHeldByDraft reports whether a configured run holds agentID. A draft
// reserves its id at create (agentIDTaken) only against what exists then; a
// run started later with the same explicit id would share it, and
// GetRunByAgentID — which answers with the newest row — would show that run in
// place of the draft. Every run start that takes an explicit agent_id refuses a
// draft's with agentIDInUseMsg. Without a store there are no drafts.
func (s *Server) agentIDHeldByDraft(ctx context.Context, agentID string) (bool, error) {
	if s.store == nil || agentID == "" {
		return false, nil
	}
	run, err := s.store.GetRunByAgentID(ctx, agentID)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return run.Status == store.RunConfigured, nil
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

// configuredRunCore resolves a run id for the draft operations under the same
// gates as every other run read: another tenant's run is the opaque 404 a
// missing one gets, and so is another user's run for an isolated member. A run
// that is visible but not a draft is a 409.
func (s *Server) configuredRunCore(ctx context.Context, runID string) (store.Run, error) {
	if !validIdent(runID) {
		return store.Run{}, draftRefusal(http.StatusBadRequest, "invalid_run_id", "run_id must match [A-Za-z0-9_-]{1,128}")
	}
	if s.store == nil {
		return store.Run{}, draftRefusal(http.StatusServiceUnavailable, "no_store", "configured runs require persistence (no store configured)")
	}
	run, err := s.tenantStore(ctx).GetRun(ctx, runID)
	if err != nil {
		return store.Run{}, draftStoreErr(err)
	}
	if sess, err := s.store.GetSession(ctx, run.SessionID); err != nil || !sessionOwnershipOK(ctx, sess) {
		return store.Run{}, draftRefusal(http.StatusNotFound, "unknown_run", "no run found for that run_id")
	}
	if run.Status != store.RunConfigured {
		return store.Run{}, draftRefusal(http.StatusConflict, "run_not_configured", "run %s is %s, not configured", run.ID, run.Status)
	}
	return run, nil
}

// updateConfiguredRunCore applies patch — a JSON object in the wire's keys; a
// null removes a field; `prompt` is honoured as at create — to a draft, and
// validates the result as a create would be, against the agent as it is now.
func (s *Server) updateConfiguredRunCore(ctx context.Context, runID string, patchJSON json.RawMessage) (connector.ConfiguredRun, error) {
	run, err := s.configuredRunCore(ctx, runID)
	if err != nil {
		return connector.ConfiguredRun{}, err
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(patchJSON, &patch); err != nil {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "the patch must be a JSON object: %v", err)
	}
	if len(patch) == 0 {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusUnprocessableEntity, "empty_patch", "an empty patch changes nothing")
	}
	for k := range patch {
		if why, bad := draftImmutableKeys[k]; bad {
			return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "%q cannot be patched: %s", k, why)
		}
	}
	current, err := s.store.GetRunDraft(ctx, run.ID)
	if err != nil {
		return connector.ConfiguredRun{}, draftStoreErr(err)
	}
	merged := map[string]json.RawMessage{}
	if len(current) > 0 {
		if err := json.Unmarshal(current, &merged); err != nil {
			return connector.ConfiguredRun{}, draftRefusal(http.StatusInternalServerError, "internal", "stored draft is not an object: %v", err)
		}
	}
	if raw, ok := patch["prompt"]; ok {
		delete(patch, "prompt")
		var text string
		if err := json.Unmarshal(raw, &text); err != nil || text == "" {
			return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", `"prompt" must be a non-empty string`)
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
		return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "invalid draft: %v", err)
	}
	if msg, ok := s.validateDraft(ctx, run.TenantID, d); !ok {
		return connector.ConfiguredRun{}, draftRefusal(http.StatusBadRequest, "invalid_draft", "%s", msg)
	}
	body, _ = json.Marshal(d)
	if err := s.store.UpdateRunDraft(ctx, run.ID, body); err != nil {
		return connector.ConfiguredRun{}, draftStoreErr(err)
	}
	return connector.ConfiguredRun{
		RunID: run.ID, AgentID: run.AgentID, SessionID: run.SessionID,
		Status: string(store.RunConfigured), Draft: body,
	}, nil
}

// validateDraft applies the checks a run makes before it would be admitted,
// so a draft that could never start is refused when it is made or edited
// rather than at start. Start validates again: the definition may move.
func (s *Server) validateDraft(ctx context.Context, tenant string, d runDraft) (string, bool) {
	if len(d.Segments) == 0 {
		return `no input: the draft must have "segments" (or a "prompt")`, false
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
	agentDef, ok := s.lookupAgent(ctx, tenant, d.Agent)
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

// deleteConfiguredRunCore discards a draft and its session. A live run is not
// discarded — cancel is its verb.
func (s *Server) deleteConfiguredRunCore(ctx context.Context, runID string) error {
	run, err := s.configuredRunCore(ctx, runID)
	if err != nil {
		return err
	}
	if err := s.store.DeleteConfiguredRun(ctx, run.ID); err != nil {
		return draftStoreErr(err)
	}
	return nil
}

// configuredRunInputCore turns a draft back into the RunInput that starts it
// through RunOnce, with the secrets the draft never stored. Identity and
// confinement are the row's, fixed at create.
func (s *Server) configuredRunInputCore(ctx context.Context, runID string, secrets connector.RunSecrets) (runner.RunInput, error) {
	run, err := s.configuredRunCore(ctx, runID)
	if err != nil {
		return runner.RunInput{}, err
	}
	if secrets.UserBearer != "" && !validUserBearer(secrets.UserBearer) {
		return runner.RunInput{}, draftRefusal(http.StatusBadRequest, "invalid_secrets", `user_bearer must match [A-Za-z0-9._\-+/=]{16,512}`)
	}
	if errMsg, ok := connector.ValidateUserCredentialsMap(secrets.UserCredentials); !ok {
		return runner.RunInput{}, draftRefusal(http.StatusBadRequest, "invalid_secrets", "%s", errMsg)
	}
	raw, err := s.store.GetRunDraft(ctx, run.ID)
	if err != nil {
		return runner.RunInput{}, draftStoreErr(err)
	}
	var d runDraft
	if err := json.Unmarshal(raw, &d); err != nil {
		return runner.RunInput{}, draftRefusal(http.StatusInternalServerError, "internal", "stored draft is unreadable: %v", err)
	}
	in := spawnRequestToRunInput(d.SpawnRunRequest)
	in.RunTimeoutSeconds = d.RunTimeoutSeconds
	in.Interactive = d.Interactive != nil && *d.Interactive
	in.ConfiguredRunID = run.ID
	in.SessionID = ""
	in.AgentID, in.TenantID, in.UserID = run.AgentID, run.TenantID, run.UserID
	in.ParentContext = run.ParentContext
	in.Isolated, in.OperatorKeyRestricted = run.Isolated, run.OperatorKeyRestricted
	in.UserBearer, in.UserCredentials = secrets.UserBearer, secrets.UserCredentials
	return in, nil
}

// --- connector operations (gRPC, MCP) ---

// CreateConfiguredRun implements connector.Connector.
func (s *Server) CreateConfiguredRun(ctx context.Context, req connector.ConfiguredRunRequest) (connector.ConfiguredRun, error) {
	return s.createConfiguredRunCore(ctx, req)
}

// UpdateConfiguredRun implements connector.Connector.
func (s *Server) UpdateConfiguredRun(ctx context.Context, runID string, patch json.RawMessage) (connector.ConfiguredRun, error) {
	return s.updateConfiguredRunCore(ctx, runID, patch)
}

// ConfiguredRunInput implements connector.Connector.
func (s *Server) ConfiguredRunInput(ctx context.Context, runID string, secrets connector.RunSecrets) (runner.RunInput, error) {
	return s.configuredRunInputCore(ctx, runID, secrets)
}

// StartConfiguredRun implements connector.Connector: the blocking start.
func (s *Server) StartConfiguredRun(ctx context.Context, runID string, secrets connector.RunSecrets) (connector.SpawnRunResult, error) {
	in, err := s.configuredRunInputCore(ctx, runID, secrets)
	if err != nil {
		return connector.SpawnRunResult{}, err
	}
	return s.runBlocking(ctx, in, in.ParentContext)
}

// DeleteConfiguredRun implements connector.Connector.
func (s *Server) DeleteConfiguredRun(ctx context.Context, runID string) error {
	return s.deleteConfiguredRunCore(ctx, runID)
}

// --- HTTP ---

// createConfiguredRun is POST /v1/runs with start:false, called by handleRuns
// once it has validated the request and before any admission.
func (s *Server) createConfiguredRun(w http.ResponseWriter, r *http.Request, req runRequest) {
	d, err := requestToDraft(req)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "encode draft: "+err.Error())
		return
	}
	created, err := s.createConfiguredRunCore(r.Context(), d)
	if err != nil {
		writeDraftErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handlePatchConfiguredRun serves PATCH /v1/runs/{run_id}.
func (s *Server) handlePatchConfiguredRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBytes())
	var patch json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_draft", "bad json: "+err.Error())
		return
	}
	updated, err := s.updateConfiguredRunCore(r.Context(), r.PathValue("run_id"), patch)
	if err != nil {
		writeDraftErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteConfiguredRun serves DELETE /v1/runs/{run_id}.
func (s *Server) handleDeleteConfiguredRun(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteConfiguredRunCore(r.Context(), r.PathValue("run_id")); err != nil {
		writeDraftErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStartConfiguredRun serves POST /v1/runs/{run_id}/start. The draft is
// started through RunOnce — the start path gRPC, MCP, the scheduler and
// webhooks share — which runs today's admission and then moves the row
// configured → running. A refusal before that point answers with a status
// code and leaves the draft as it was; once the run is registered the response
// is the run's SSE stream, as for POST /v1/runs.
func (s *Server) handleStartConfiguredRun(w http.ResponseWriter, r *http.Request) {
	var secrets connector.RunSecrets
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&secrets); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_secrets", "bad json: "+err.Error())
			return
		}
	}
	in, err := s.configuredRunInputCore(r.Context(), r.PathValue("run_id"), secrets)
	if err != nil {
		writeDraftErr(w, err)
		return
	}

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
