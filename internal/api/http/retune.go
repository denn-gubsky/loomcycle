package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/channelhooks"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
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

	// Interactive promotes (or demotes) the run at its turn boundaries. Three
	// states, hence the pointer: absent keeps what the run has, true parks it at
	// the next boundary instead of finishing, false lets a parked-by-default run
	// end. This is the field that lets an operator take hold of an agent that is
	// already running.
	Interactive *bool `json:"interactive,omitempty"`

	// Review arms (true) or disarms (false) the hold for an operator's verdict
	// when the model finishes. Disarming a run that is held releases it as
	// approved.
	Review *bool `json:"review,omitempty"`

	// Interruption narrows the run's interruption policy — kinds, max_pending,
	// or enabled:false to switch it off. It never grants the Interruption tool
	// to an agent whose definition does not hold it. A parked run adopts it at
	// its next operator turn; one that never parks, on resume.
	Interruption *config.AgentInterruptionACL `json:"interruption,omitempty"`

	// ToolChoice and OutputFormat REPLACE the run's own whole, as they do at
	// start — their fields constrain each other, so a per-field merge could
	// build a combination nobody asked for. A live run adopts them at its next
	// operator turn, the same boundary a routing change waits for, and a
	// replaced tool_choice starts its `until` afresh from there. tool_choice
	// mode "auto" removes the forcing; there is no way to remove an
	// output_format, only to replace it.
	ToolChoice   *config.ToolChoice   `json:"tool_choice,omitempty"`
	OutputFormat *config.OutputFormat `json:"output_format,omitempty"`
}

// isZero reports whether the caller supplied nothing at all.
//
// Written field-by-field rather than as `*w == runOverridesWire{}`: the struct
// now holds a pointer to AgentInterruptionACL, which carries a slice, so the
// struct comparison this used to be would no longer compile. A silent switch to
// reflect.DeepEqual would compile and then treat a zero-valued pointer as
// "supplied", which is the opposite of what this answers.
func (w *runOverridesWire) isZero() bool {
	if w == nil {
		return true
	}
	return w.Model == "" && w.Provider == "" && w.Tier == "" && w.Effort == "" &&
		w.MaxTokens == 0 && w.MaxIterations == 0 && w.UnboundedIterations == nil &&
		w.MaxConcurrentChildren == 0 && w.RetryAttempts == nil &&
		w.MemoryInjectMaxTokens == nil && w.MemoryIndexMaxBytes == nil &&
		w.InjectToolGuide == nil && w.Interactive == nil && w.Interruption == nil &&
		w.Review == nil && w.ToolChoice == nil && w.OutputFormat == nil
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

// runForSteer returns the run a steer/retune is addressed to — the one gate
// SteerRun, retune and the run-config reads share: the run must be live, and
// the caller must own it (liveRunOwnershipOK).
//
// Live means in this replica's steer registry, or — in a cluster — running on
// the replica that owns it: its row says running and names another replica,
// and a steer push routes there. Gating on the local registry alone refused
// every call that landed on a replica that does not own the run, so in a
// cluster steering, retune and a run's config worked only by luck of routing.
// A running row stamped with this replica but absent from its registry is not
// steerable here, and is not live anywhere else either.
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
		return s.remoteRunForSteer(ctx, runID)
	}
	if entry.VerdictsOnly {
		// A sub-agent its parent drives: it takes a verdict, nothing else.
		return store.Run{}, connector.ErrRunNotInFlight
	}
	if entry.Closed() {
		// Finishing: its queue is closed, so a steer is refused as for an
		// ended run, and a retune it would never read is refused with it.
		return store.Run{}, connector.ErrRunNotInFlight
	}
	if !s.liveRunOwnershipOK(ctx, entry) {
		return store.Run{}, connector.ErrRunNotInFlight
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return store.Run{}, connector.ErrRunNotInFlight
	}
	return run, nil
}

// remoteRunForSteer is runForSteer for a run this replica does not have: in a
// cluster, a run another replica owns and is running, read through the
// tenant-scoped store and the session gate, as ReviewRun reads it.
func (s *Server) remoteRunForSteer(ctx context.Context, runID string) (store.Run, error) {
	if !s.steerReg.Clustered() {
		return store.Run{}, connector.ErrRunNotInFlight
	}
	run, err := s.tenantStore(ctx).GetRun(ctx, runID)
	if err != nil || run.Status != store.RunRunning || run.ReplicaID == "" || run.ReplicaID == s.replicaID {
		return store.Run{}, connector.ErrRunNotInFlight
	}
	if takesOnlyVerdicts(run) {
		// The owner refuses it (its entry is VerdictsOnly), so this replica
		// must too: otherwise a retune from here changes the model, budget or
		// parking of a child its parent drives, which the owner answers 404.
		return store.Run{}, connector.ErrRunNotInFlight
	}
	if runsNoLoop(run) {
		// The owner never registers a steer entry for it, so it answers 404;
		// this replica must too, or a retune from here rewrites the config of
		// a run nothing reads it from.
		return store.Run{}, connector.ErrRunNotInFlight
	}
	if run.SessionID != "" {
		sess, serr := s.store.GetSession(ctx, run.SessionID)
		if serr != nil || !sessionOwnershipOK(ctx, sess) {
			return store.Run{}, connector.ErrRunNotInFlight
		}
	} else if !runOwnershipOK(ctx, run) {
		// No session to gate on: the run's own owner confines it instead.
		return store.Run{}, connector.ErrRunNotInFlight
	}
	return run, nil
}

// takesOnlyVerdicts is the steer entry's VerdictsOnly read off the run's row.
// The row carries no such flag, so it is derived: a sub-run (it has a parent)
// that is not a team-walk member (every member row carries its walk's id; a
// member's own sub-agents do not). That is the Agent tool's child, which its
// owner registers verdicts-only.
//
// Read where no entry decides: by a replica that does not have the run, by the
// compaction gate when the run is not live here, and by a resume, which
// registers the resumed sub-run's entry from it — so a resumed child is
// verdicts-only on its owner as on every other replica.
//
// A resident child shares the row shape and is where the two still disagree:
// while it runs its owner registers a full entry and admits a retune a
// non-owning replica refuses; resumed, it comes back verdicts-only. Refusing is
// the side a gate may err on; carrying the kind on the row would make them
// agree. A walk's or hook's run has no entry at all and is refused apart from
// this, by runsNoLoop.
func takesOnlyVerdicts(run store.Run) bool {
	return run.ParentRunID != "" && (run.ParentContext == nil || run.ParentContext.WalkID == "")
}

// runsNoLoop reports a run that is a record, not an agent loop: a team walk's
// own run and a channel hook's run. Neither has a steer entry on its owner, so
// the owner refuses a steer, retune or config read of it. It is read off the
// agent id those runs alone carry: any other run's is a generated handle or a
// caller's, and a caller's may not hold a colon.
//
// takesOnlyVerdicts does not catch a walk started from the substrate plane: it
// has no parent run. Today such a row also carries no replica id, which the
// gate above already refuses; this does not lean on that omission, so stamping
// the owner on those rows later does not open them to a remote retune.
func runsNoLoop(run store.Run) bool {
	return strings.HasPrefix(run.AgentID, teamWalkAgentPrefix) ||
		strings.HasPrefix(run.AgentID, channelhooks.HookAgentPrefix)
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
// retuneRun merges an override into a live run's stored configuration and
// returns the RESULT.
//
// It returns the merged record because the caller cannot recompute it: the merge
// is not a field-wise union. Setting `model` clears the tier and setting `tier`
// clears the model, so a panel that echoed back what it sent would display
// something the run does not hold. The function already had the answer and threw
// it away, which left a client with no way to confirm its own write.
func (s *Server) retuneRun(ctx context.Context, run store.Run, in *runOverridesWire) (runConfigRecord, error) {
	if in.isZero() {
		return runConfigRecord{}, nil
	}
	agentDef, ok := s.lookupAgent(ctx, run.TenantID, run.Agent)
	if !ok {
		return runConfigRecord{}, fmt.Errorf("%w: %s", runner.ErrUnknownAgent, run.Agent)
	}

	// Merged onto the record as it is when written, not as runForSteer read
	// it: a writer that got in between keeps its field. See updateRunConfig.
	merged, err := s.updateRunConfig(ctx, run.ID, func(rec *runConfigRecord, _ bool) error {
		cur := *rec
		next := in.split()
		// Start from the whole record and change only what a retune may change.
		// It used to be rebuilt field by field, which silently dropped every field
		// the list did not name — the hooks the run added and the hooks it pinned
		// at start — so a retuned run that later paused resumed under re-resolved
		// hooks. The start-only fields (sampling, compaction, hosts, the review
		// deadline, …) are kept by the copy.
		merged := cur
		merged.Routing = mergeRouting(cur.Routing, next.Routing)
		merged.Resources = mergeResources(cur.Resources, next.Resources)
		merged.Tuning = mergeTuning(cur.Tuning, next.Tuning)
		// Last writer wins for the single decisions below, unlike the merged
		// blocks above: "keep what is there unless this call says otherwise" IS
		// the merge.
		if in.Interactive != nil {
			merged.Interactive = in.Interactive
		}
		if in.Review != nil {
			merged.Review = in.Review
		}
		if in.Interruption != nil {
			merged.Interruption = in.Interruption
		}
		// The start-time validators, so a retune cannot store a block a run start
		// would have refused.
		if in.ToolChoice != nil {
			if err := in.ToolChoice.Validate(); err != nil {
				return fmt.Errorf("%w: %v", runner.ErrInvalidArgument, err)
			}
			// Plain auto asks for nothing, and an absent record field is how every
			// reader already spells "no forced choice".
			merged.ToolChoice = nil
			if !in.ToolChoice.IsZero() {
				merged.ToolChoice = in.ToolChoice.Clone()
			}
		}
		if in.OutputFormat != nil {
			// Validate refuses an empty schema, which is what keeps `{}` from
			// silently meaning "remove it".
			if err := in.OutputFormat.Validate(); err != nil {
				return fmt.Errorf("%w: %v", runner.ErrInvalidArgument, err)
			}
			merged.OutputFormat = in.OutputFormat.Clone()
		}

		// Refuse now, not next turn.
		if _, err := s.effectiveDef(ctx, agentDef, runOverrides{
			Routing: merged.Routing, Resources: merged.Resources, Tuning: merged.Tuning,
		}); err != nil {
			return err
		}
		*rec = merged
		return nil
	})
	if err != nil {
		return runConfigRecord{}, err
	}
	// After the write, never before: a transcript line about a change that did
	// not persist is worse than no line.
	s.appendRetuneEvent(ctx, run.ID, in.setFields())
	if in.Review != nil && !*in.Review {
		s.releaseHeldRun(ctx, run.ID)
	}
	return merged, nil
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
//
// The same read also moves the run's live interruption policy (when live is
// non-nil), so a retuned `interruption` takes effect at the same boundary as a
// retuned model — one rule for "when does a retune land", and no second read.
func (s *Server) reResolveOnOperatorTurnFn(runID, tenantID, userID, agentName, userTier string, restricted bool, live *runInterruption) func(context.Context) (providers.Provider, string, string, bool, error) {
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
		s.adoptRunInterruption(live, agentDef, cfg.Interruption)
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

// reReadShapeOnOperatorTurnFn builds the loop hook that lets a PARKED run adopt
// a retuned tool_choice / output_format on its next turn — the sibling of
// reResolveOnOperatorTurnFn, read at the same moment for the same reason.
//
// "Changed" is judged against what the loop last held, not against the
// definition: startTC / startOF are the values the run was STARTED with. For a
// resumed run that is its record's value even when resume dropped a tool_choice
// the run had already spent, so an unchanged record does not re-force it.
//
// Compared as JSON because a schema that came from yaml holds ints where the
// same schema decoded from the record holds float64s; equal-as-JSON is the
// equality the wire sees.
func (s *Server) reReadShapeOnOperatorTurnFn(runID string, startTC *config.ToolChoice, startOF *config.OutputFormat) func(context.Context) (loop.RetunedShape, error) {
	if s.store == nil || runID == "" {
		return nil
	}
	lastTC, lastOF := shapeKey(startTC), shapeKey(startOF)
	return func(ctx context.Context) (loop.RetunedShape, error) {
		run, err := s.store.GetRun(ctx, runID)
		if err != nil {
			return loop.RetunedShape{}, err
		}
		rec, ok := decodeRunConfig(run.RunConfig)
		if !ok {
			return loop.RetunedShape{}, nil
		}
		var out loop.RetunedShape
		if k := shapeKey(rec.ToolChoice); k != lastTC {
			lastTC, out.ToolChoice, out.ToolChoiceChanged = k, rec.ToolChoice, true
		}
		if k := shapeKey(rec.OutputFormat); k != lastOF {
			lastOF, out.OutputFormat, out.OutputFormatChanged = k, rec.OutputFormat, true
		}
		return out, nil
	}
}

// shapeKey is v's JSON, or "null" when it is a nil pointer. A marshal failure
// cannot happen for these config types (they are round-tripped through the
// record already); it collapses to "" rather than panicking.
func shapeKey(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
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
		http.Error(w, "at least one override is required (model, provider, tier, effort, max_tokens, max_iterations, unbounded_iterations, max_concurrent_children, retry_attempts, memory_inject_max_tokens, memory_index_max_bytes, inject_tool_guide, interactive, interruption, review, tool_choice, output_format)", http.StatusUnprocessableEntity)
		return
	}
	run, rerr := s.runForSteer(r.Context(), runID)
	if rerr != nil {
		http.Error(w, "no in-flight run for that run_id", http.StatusNotFound)
		return
	}
	merged, err := s.retuneRun(r.Context(), run, &req.runOverridesWire)
	if err != nil {
		writeResolveError(w, err)
		return
	}
	// The merged record, not an acknowledgement. A caller cannot recompute it —
	// setting `model` clears the tier and setting `tier` clears the model — so
	// answering {retuned:true} left a panel unable to confirm its own write
	// against anything but a guess.
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": runID, "retuned": true, "config": merged,
	})
}

// interactiveNowFn returns the callback the loop consults at each turn boundary
// to decide whether to park.
//
// It reads the run's stored configuration rather than a cached bool, because the
// question is "has an operator promoted this run SINCE it started" and the
// answer can arrive from another request, another goroutine, or another replica.
// The read costs one row per turn boundary — a turn contains a model call, so
// this is not the expensive part — and it is correct across replicas for free,
// which an in-process flag would not be.
//
// FAILS TO THE START-TIME ANSWER. A store error or an undecodable record returns
// what the run began as, never a guess: a read that did not work must not change
// whether a run terminates.
func (s *Server) interactiveNowFn(runID string, startedInteractive bool) func(context.Context) bool {
	return func(ctx context.Context) bool {
		// A storeless server has nowhere to have recorded a promotion, and this
		// runs at EVERY turn boundary — so the nil check is not defensive
		// paranoia, it is the difference between "no promotions here" and a panic
		// on every run. Found by the suite, not by review.
		if s == nil || s.store == nil || runID == "" {
			return startedInteractive
		}
		run, err := s.store.GetRun(ctx, runID)
		if err != nil {
			return startedInteractive
		}
		rec, ok := decodeRunConfig(run.RunConfig)
		if !ok || rec.Interactive == nil {
			return startedInteractive
		}
		return *rec.Interactive
	}
}

// reviewNowFn returns the callback the loop reads when its model finishes, and
// during a hold, to decide whether the run is held for review. The same shape
// as interactiveNowFn: the start-time answer unless a retune recorded another.
func (s *Server) reviewNowFn(runID string, startedArmed bool) func(context.Context) bool {
	return func(ctx context.Context) bool {
		if s == nil || s.store == nil || runID == "" {
			return startedArmed
		}
		run, err := s.store.GetRun(ctx, runID)
		if err != nil {
			return startedArmed
		}
		rec, ok := decodeRunConfig(run.RunConfig)
		if !ok || rec.Review == nil {
			return startedArmed
		}
		return *rec.Review
	}
}

// setFields returns the override keys this request actually set, in a stable
// order, so a reader can see WHAT an operator changed.
//
// It is the honest answer to a question the wire type has always claimed to
// answer and could not: OverrideInfo.Fields documents itself as "the override
// keys the request actually set, so a reader can see a budget or tuning change
// that moved no model at all", and the only site that filled it in was inside
// the loop — which sees a re-resolved ROUTING outcome, not a request. It
// therefore hardcoded {"model"}, and a retune of nothing but max_tokens put
// nothing on the transcript at all. A consumer built a branch against a
// documented capability that could never fire.
func (w *runOverridesWire) setFields() []string {
	if w == nil {
		return nil
	}
	var f []string
	add := func(name string, set bool) {
		if set {
			f = append(f, name)
		}
	}
	add("model", w.Model != "")
	add("provider", w.Provider != "")
	add("tier", w.Tier != "")
	add("effort", w.Effort != "")
	add("max_tokens", w.MaxTokens != 0)
	add("max_iterations", w.MaxIterations != 0)
	add("unbounded_iterations", w.UnboundedIterations != nil)
	add("max_concurrent_children", w.MaxConcurrentChildren != 0)
	add("retry_attempts", w.RetryAttempts != nil)
	add("memory_inject_max_tokens", w.MemoryInjectMaxTokens != nil)
	add("memory_index_max_bytes", w.MemoryIndexMaxBytes != nil)
	add("inject_tool_guide", w.InjectToolGuide != nil)
	add("interactive", w.Interactive != nil)
	add("review", w.Review != nil)
	add("interruption", w.Interruption != nil)
	add("tool_choice", w.ToolChoice != nil)
	add("output_format", w.OutputFormat != nil)
	return f
}

// appendRetuneEvent records a retune on the run's transcript AT THE MOMENT THE
// OPERATOR MADE IT.
//
// WHY HERE AND NOT ONLY IN THE LOOP. The loop emits an EventOverride when a
// parked run wakes and its routing has MOVED, carrying the from/to pair — real
// information, and the only place the post-resolution model is known. But it is
// gated on routing: a run whose budget changed and whose model did not resolves
// to the same model, reports no change, and leaves the transcript silent about
// an operator action that definitely happened.
//
// So the two events answer different questions and both are worth having: this
// one says WHAT WAS ASKED FOR and when; the loop's says WHAT THE RUN IS NOW
// USING, once it knows.
//
// Best-effort: a retune that succeeded must not be reported as failed because
// its transcript line could not be written.
func (s *Server) appendRetuneEvent(ctx context.Context, runID string, fields []string) {
	if s.store == nil || runID == "" || len(fields) == 0 {
		return
	}
	ev := providers.Event{
		Type: providers.EventOverride,
		Text: "run settings changed by operator: " + strings.Join(fields, ", "),
		Override: &providers.OverrideInfo{
			Source: "operator",
			Fields: fields,
		},
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		log.Printf("retune: could not encode the override event for run %s: %v", runID, err)
		return
	}
	if aerr := s.store.AppendEvent(ctx, runID, string(providers.EventOverride), payload); aerr != nil {
		log.Printf("retune: run %s was retuned but the transcript line was not written: %v", runID, aerr)
	}
}

// handleGetRunConfig serves GET /v1/runs/{run_id}/config — the run's own stored
// overrides.
//
// WHY THIS EXISTS. There is no GET /v1/runs/{run_id} at all, and runs.run_config
// is serialised nowhere, so a run's overrides were write-only: a client could
// set them and never read them back. /retune now answers with the merged record,
// but that only helps a caller that just wrote. A panel OPENING an existing chat
// had no way to learn what the run was already carrying.
//
// It reports what the RUN holds, not what the run will effectively use — a field
// no override set is absent here and resolves later from the definition, the
// tier, or the driver. Saying so is the point: absent means "not overridden",
// which is a different and more useful answer than a resolved value would be at
// this layer.
//
// The tenant gate is runForSteer's, so a run the caller may not touch answers
// the same 404 an unknown one does.
func (s *Server) handleGetRunConfig(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	run, err := s.runForSteer(r.Context(), runID)
	if err != nil {
		http.Error(w, "no in-flight run for that run_id", http.StatusNotFound)
		return
	}
	// A run that was never retuned has no record, and that is a 200 with an
	// empty config — not a 404. "This run overrides nothing" is an answer.
	cfg, _ := decodeRunConfig(run.RunConfig)
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": runID,
		"agent":  run.Agent,
		"model":  run.Model,
		"config": cfg,
	})
}
