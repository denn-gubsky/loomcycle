package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// runOverridesWire is the `overrides` object on POST /v1/runs/{run_id}/input.
//
// An OBJECT here, unlike the flat fields on /v1/runs, and the asymmetry is
// deliberate: that endpoint already had a flat vocabulary of per-run settings to
// be consistent with, and this one has none — its body is `{"text": …}`. Naming
// the group also says what the request is doing, which matters more on an
// endpoint whose other job is sending a message.
type runOverridesWire struct {
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Tier     string `json:"tier,omitempty"`
	Effort   string `json:"effort,omitempty"`

	MaxTokens             int   `json:"max_tokens,omitempty"`
	MaxIterations         int   `json:"max_iterations,omitempty"`
	UnboundedIterations   *bool `json:"unbounded_iterations,omitempty"`
	MaxConcurrentChildren int   `json:"max_concurrent_children,omitempty"`

	RetryAttempts         *int  `json:"retry_attempts,omitempty"`
	MemoryInjectMaxTokens *int  `json:"memory_inject_max_tokens,omitempty"`
	MemoryIndexMaxBytes   *int  `json:"memory_index_max_bytes,omitempty"`
	InjectToolGuide       *bool `json:"inject_tool_guide,omitempty"`
}

func (w *runOverridesWire) isZero() bool {
	return w == nil || (*w == runOverridesWire{})
}

// split turns the wire object into the three records the run's configuration
// already stores, so a retune and a run-start override are the same thing in
// the same place rather than a parallel shape.
func (w *runOverridesWire) split() runOverrides {
	if w == nil {
		return runOverrides{}
	}
	return runOverrides{
		Routing: &routingOverride{Model: w.Model, Provider: w.Provider, Tier: w.Tier, Effort: w.Effort},
		Resources: &resourceOverride{
			MaxTokens: w.MaxTokens, MaxIterations: w.MaxIterations,
			UnboundedIterations: w.UnboundedIterations, MaxConcurrentChildren: w.MaxConcurrentChildren,
		},
		Tuning: &tuningOverride{
			RetryAttempts: w.RetryAttempts, MemoryInjectMaxTokens: w.MemoryInjectMaxTokens,
			MemoryIndexMaxBytes: w.MemoryIndexMaxBytes, InjectToolGuide: w.InjectToolGuide,
		},
	}
}

// runForSteer returns the run a steer/retune is addressed to, gated EXACTLY as
// SteerRun gates it: the run must be live in the steer registry, and its session
// must pass the tenant-ownership check.
//
// Deliberately the same gate and the same opaque failure. A retune that could
// distinguish "not yours" from "does not exist" would turn this endpoint into an
// existence oracle for other tenants' run_ids, which is the thing the steer gate
// is shaped to avoid — and it would do it on the endpoint that already got that
// right.
func (s *Server) runForSteer(ctx context.Context, runID string) (store.Run, error) {
	if s.steerReg == nil || s.store == nil {
		return store.Run{}, connector.ErrRunNotInFlight
	}
	entry, ok := s.steerReg.Get(runID)
	if !ok {
		return store.Run{}, connector.ErrRunNotInFlight
	}
	if entry.SessionID != "" {
		sess, err := s.store.GetSession(ctx, entry.SessionID)
		if err != nil || !sessionOwnershipOK(ctx, sess) {
			return store.Run{}, connector.ErrRunNotInFlight
		}
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return store.Run{}, connector.ErrRunNotInFlight
	}
	return run, nil
}

// retuneRun merges an override into a live run's stored configuration.
//
// MERGE, not replace: an operator changing the model must not silently drop the
// temperature they set at run start. Only the fields actually present in the
// request move; everything else keeps the value the run already had (D4 — an
// override is state with a lifetime, not a request parameter).
//
// Validated against the run's CURRENT definition before it is stored, so an
// override the definition would refuse is a 400 at the moment it is sent rather
// than a surprise on the next turn — and so a stored record is always one the
// run could actually adopt.
func (s *Server) retuneRun(ctx context.Context, run store.Run, in *runOverridesWire) error {
	if in.isZero() {
		return nil
	}
	agentDef, ok := s.lookupAgent(ctx, run.TenantID, run.Agent)
	if !ok {
		return fmt.Errorf("%w: %s", runner.ErrUnknownAgent, run.Agent)
	}

	cur, _ := decodeRunConfig(run.RunConfig)
	next := in.split()
	merged := runConfigRecord{
		Sampling:          cur.Sampling,
		Compaction:        cur.Compaction,
		Context:           cur.Context,
		MaxContextTokens:  cur.MaxContextTokens,
		RunTimeoutSeconds: cur.RunTimeoutSeconds,
		Hosts:             cur.Hosts,
		Routing:           mergeRouting(cur.Routing, next.Routing),
		Resources:         mergeResources(cur.Resources, next.Resources),
		Tuning:            mergeTuning(cur.Tuning, next.Tuning),
	}

	// Refuse now, not next turn.
	if _, err := s.effectiveDef(ctx, agentDef, runOverrides{
		Routing: merged.Routing, Resources: merged.Resources, Tuning: merged.Tuning,
	}); err != nil {
		return err
	}
	return s.store.SetRunConfig(ctx, run.ID, merged.marshal())
}

func mergeRouting(cur, next *routingOverride) *routingOverride {
	if next.isZero() {
		return cur
	}
	out := routingOverride{}
	if cur != nil {
		out = *cur
	}
	if next.Model != "" {
		// Naming a model pins it, so a previously-chosen provider must not
		// linger and contradict the new choice.
		out.Model, out.Provider = next.Model, next.Provider
	}
	if next.Provider != "" {
		out.Provider = next.Provider
	}
	if next.Tier != "" {
		out.Tier, out.Model = next.Tier, ""
	}
	if next.Effort != "" {
		out.Effort = next.Effort
	}
	return &out
}

func mergeResources(cur, next *resourceOverride) *resourceOverride {
	if next.isZero() {
		return cur
	}
	out := resourceOverride{}
	if cur != nil {
		out = *cur
	}
	if next.MaxTokens > 0 {
		out.MaxTokens = next.MaxTokens
	}
	if next.MaxIterations > 0 {
		out.MaxIterations = next.MaxIterations
	}
	if next.UnboundedIterations != nil {
		out.UnboundedIterations = next.UnboundedIterations
	}
	if next.MaxConcurrentChildren > 0 {
		out.MaxConcurrentChildren = next.MaxConcurrentChildren
	}
	return &out
}

func mergeTuning(cur, next *tuningOverride) *tuningOverride {
	if next.isZero() {
		return cur
	}
	out := tuningOverride{}
	if cur != nil {
		out = *cur
	}
	if next.RetryAttempts != nil {
		out.RetryAttempts = next.RetryAttempts
	}
	if next.MemoryInjectMaxTokens != nil {
		out.MemoryInjectMaxTokens = next.MemoryInjectMaxTokens
	}
	if next.MemoryIndexMaxBytes != nil {
		out.MemoryIndexMaxBytes = next.MemoryIndexMaxBytes
	}
	if next.InjectToolGuide != nil {
		out.InjectToolGuide = next.InjectToolGuide
	}
	return &out
}

// reResolveOnOperatorTurnFn builds the loop hook that lets a PARKED run adopt a
// retune on its next turn.
//
// It re-reads the run row rather than closing over the record captured at run
// start, because the whole point is that the record changed since then — and it
// changed on a different goroutine, through an HTTP handler, possibly on another
// replica. The store is the only place both sides agree.
//
// One read per OPERATOR TURN, not per iteration: a parked run does nothing until
// someone types, so this is bounded by human typing rather than by the loop.
func (s *Server) reResolveOnOperatorTurnFn(runID, tenantID, userID, agentName, userTier string, restricted bool) func(context.Context) (providers.Provider, string, string, bool, error) {
	if s.store == nil {
		return nil
	}
	return func(ctx context.Context) (providers.Provider, string, string, bool, error) {
		run, err := s.store.GetRun(ctx, runID)
		if err != nil {
			return nil, "", "", false, err
		}
		cfg, ok := decodeRunConfig(run.RunConfig)
		if !ok {
			return nil, "", "", false, nil
		}
		agentDef, found := s.lookupAgent(ctx, tenantID, agentName)
		if !found {
			return nil, "", "", false, fmt.Errorf("%w: %s", runner.ErrUnknownAgent, agentName)
		}
		eff, err := s.effectiveDef(ctx, agentDef, runOverrides{
			Routing: cfg.Routing, Resources: cfg.Resources, Tuning: cfg.Tuning,
		})
		if err != nil {
			return nil, "", "", false, err
		}
		providerID, model, effort, err := s.resolveAgentDef(ctx, eff, tenantID, userID, agentName, userTier, restricted)
		if err != nil {
			return nil, "", "", false, err
		}
		// runs.model records what this run last resolved to, so it is the
		// comparison that answers "did anything actually move". Reporting a
		// change that did not happen would put a misleading line in the
		// transcript.
		if model == run.Model {
			return nil, "", "", false, nil
		}
		provider, err := s.providers.Get(providerID)
		if err != nil {
			return nil, "", "", false, err
		}
		if uerr := s.store.SetRunModel(ctx, runID, providerID, model); uerr != nil {
			log.Printf("retune: run %s adopted %s/%s but the row still says %q: %v",
				runID, providerID, model, run.Model, uerr)
		}
		return provider, model, effort, true, nil
	}
}

// retuneRequest is the JSON body for POST /v1/runs/{run_id}/retune.
//
// The overrides are INLINE here, not nested under `overrides` as they are on
// /input. On that endpoint the object names what the group is doing, because the
// request's other job is sending a message; here changing the settings IS the
// request, and a wrapper would be ceremony.
type retuneRequest struct {
	runOverridesWire
}

// handleRetuneRun serves POST /v1/runs/{run_id}/retune — change a run's
// settings WITHOUT sending it a turn.
//
// WHY THIS IS NOT A FLAG ON /input. The retune already rides that endpoint, and
// it stays there: retuning and speaking in one atomic call is a real operation.
// But `text` is required there, so the only way to change a parked chat's model
// was to put a message in the transcript the operator never wanted to send —
// reported from a consumer that wanted exactly this and could not express it.
//
// Relaxing the 422 would have made an endpoint whose body is `{"text": …}` also
// mean "and change the model". These are two different acts and they get two
// different routes; the shared half is s.retuneRun, so they cannot diverge in
// what a retune actually does.
//
// The tenant gate is the same one /input uses, for the same reason: a run the
// caller may not touch answers 404 exactly as an unknown one does, so the gate
// never becomes an existence oracle for run_ids that are not secrets.
func (s *Server) handleRetuneRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	var req retuneRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
		return
	}
	// An empty body is a caller mistake, not a no-op to absorb: it means the
	// override names were misspelled or nested, and answering 200 would report
	// success for a call that changed nothing.
	if req.runOverridesWire.isZero() {
		http.Error(w, "at least one override is required (model, provider, tier, effort, max_tokens, max_iterations, unbounded_iterations, max_concurrent_children, retry_attempts, memory_inject_max_tokens, memory_index_max_bytes, inject_tool_guide)", http.StatusUnprocessableEntity)
		return
	}
	run, rerr := s.runForSteer(r.Context(), runID)
	if rerr != nil {
		http.Error(w, "no in-flight run for that run_id", http.StatusNotFound)
		return
	}
	if err := s.retuneRun(r.Context(), run, &req.runOverridesWire); err != nil {
		writeResolveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "retuned": true})
}
