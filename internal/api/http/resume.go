package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// resume.go — F42 / RFC X Phase 2: re-dispatch snapshotted paused runs.
//
// A snapshot captures pause_state='paused' runs as DATA (the run row + its
// transcript), but nothing backs them with a live goroutine on the target
// instance — so before this they were a stuck "running" zombie that
// `POST /v1/_resume` couldn't wake (409 not_paused: the runtime isn't paused,
// only the row). ResumePausedRuns reconstructs each paused run's loop from its
// transcript and re-enters loop.Run mid-conversation, so a mid-run experiment
// genuinely continues after a snapshot→restore or a process restart.
//
// Called from two places (see the restore handler + boot wiring):
//   - after snapshot.Restore writes the rows (the explicit migration path), and
//   - at boot, scanning the store for any paused runs (crash recovery).
//
// LIMITATIONS (documented; the snapshot deliberately omits these):
//   - Per-run SECRETS (UserBearer / named UserCredentials) are never
//     snapshotted, so a resumed run can't restore them; a tool call that needs
//     ${run.user_bearer} / ${run.credentials.*} in an MCP header degrades.
//   - Per-run CALL-TIME OVERRIDES (allowed_hosts narrowing, per-run sampling,
//     metadata, run-timeout) aren't persisted — resume re-derives everything
//     from the agent definition (the operator's static floor applies for hosts).
//   - A run that was IDLE awaiting operator input when paused (its conversation
//     ends on an assistant turn, not a pending user/tool_result) is NOT
//     auto-resumable — re-entering the loop would send the provider a trailing
//     assistant turn. Such runs are flagged failed with a clear reason; the
//     operator re-attaches + steers to continue. (Mid-execution runs — the F42
//     repro — end on a clean tool_result boundary and resume cleanly.)

// ResumePausedRuns re-dispatches every pause_state='paused' run found in the
// store. Returns the count successfully re-dispatched and any per-run warnings
// (a run whose agent no longer resolves, or that isn't auto-resumable, is
// flagged failed and counted as a warning, not a hard error). Safe to call when
// the store is nil (no-op) and idempotent: a run already live (cancel-registry
// ErrInUse) is skipped.
func (s *Server) ResumePausedRuns(ctx context.Context) (int, []string) {
	if s.store == nil {
		return 0, nil
	}
	paused, err := s.store.ListPausedRuns(ctx)
	if err != nil {
		return 0, []string{fmt.Sprintf("list paused runs: %v", err)}
	}
	var warnings []string
	redispatched := 0
	for _, run := range paused {
		if err := s.resumePausedRun(ctx, run); err != nil {
			warnings = append(warnings, fmt.Sprintf("run %s (%s): %v", run.ID, run.Agent, err))
			continue
		}
		redispatched++
	}
	if redispatched > 0 || len(warnings) > 0 {
		log.Printf("resume: re-dispatched %d paused run(s); %d skipped/flagged", redispatched, len(warnings))
	}
	return redispatched, warnings
}

// resumePausedRun re-dispatches ONE paused run under its EXISTING run_id (no
// CreateRun — the row already exists). It mirrors handleRuns' dispatch but
// seeds the conversation from the transcript (PriorMessages) instead of a fresh
// prompt, re-derives provider/model/tools/system-prompt from the agent def, and
// runs the loop in a detached background goroutine (no HTTP request backs it).
func (s *Server) resumePausedRun(ctx context.Context, run store.Run) error {
	if run.ID == "" || run.AgentID == "" || run.SessionID == "" {
		return fmt.Errorf("missing id/agent_id/session_id")
	}

	// Resolve the agent at the run's authoritative tenant. If it no longer
	// exists (def deleted, or the static yaml changed across instances), flag
	// the run failed so it isn't a permanent "running" zombie.
	agentDef, ok := s.lookupAgent(ctx, run.TenantID, run.Agent)
	if !ok {
		s.flagRunUnresumable(run, fmt.Sprintf("agent %q no longer exists", run.Agent))
		return fmt.Errorf("agent %q not found", run.Agent)
	}
	providerID, model, effort, rerr := s.resolveAgentDef(agentDef, run.Agent, run.UserTier)
	if rerr != nil {
		s.flagRunUnresumable(run, fmt.Sprintf("resolve provider/model: %v", rerr))
		return fmt.Errorf("resolve agent: %w", rerr)
	}
	provider, perr := s.providers.Get(providerID)
	if perr != nil {
		s.flagRunUnresumable(run, fmt.Sprintf("provider %q unavailable: %v", providerID, perr))
		return fmt.Errorf("provider %q: %w", providerID, perr)
	}

	// Tools + dispatcher. No per-run host narrowing: the caller's allowed_hosts
	// was a call-time input, never snapshotted — the operator's static floor
	// (applied inside the tools) is what remains.
	allowedTools := filterTools(s.candidateTools(ctx, run.TenantID, agentDef.AllowedTools), agentDef.AllowedTools, nil)
	dispatcher := s.newDispatcher(allowedTools)

	// Re-derive the system prompt (skill bodies baked in) from the current
	// agent def — exactly as the continuation path does; the prompt is NOT
	// replayed from the transcript (it may have changed across instances).
	agentDef, _ = s.resolveSkillBodiesForRun(ctx, run.TenantID, agentDef)

	// Rebuild the conversation from THIS run's transcript events.
	events, terr := s.store.GetTranscript(ctx, run.SessionID)
	if terr != nil {
		return fmt.Errorf("read transcript: %w", terr)
	}
	runEvents := make([]store.Event, 0, len(events))
	for _, e := range events {
		if e.RunID == run.ID {
			runEvents = append(runEvents, e)
		}
	}
	priorMessages := replayTranscript(runEvents)

	// RFC X Phase 3: detect a parked fan-out PARENT — a parallel_spawn that the
	// Phase-3 watcher parked mid-wg.Wait, so its transcript ends on a dangling
	// tool_use (no envelope tool_result) backed by a spawn ledger. Such a run
	// can't take the endsWithPendingTurn path (it ends on an assistant turn) but
	// IS resumable: the goroutine below reconciles the children into the
	// envelope. Flag-gated — off ⇒ (_, false) ⇒ the byte-identical normal path.
	fanout, isFanout := detectFanoutParent(s.cfg.Env.ResumeFanout, runEvents)
	if isFanout && !lastTurnIsSoleToolUse(priorMessages, fanout.toolUseID) {
		// A parked fan-out that shared its turn with other in-flight tools (a
		// mixed tool iteration) isn't auto-reconcilable: synthesizing only the
		// fan-out's result would leave a sibling tool_use unanswered (the
		// provider 400s). Documented Phase-3 limitation; flag for manual
		// re-attach rather than emit a malformed continuation.
		s.flagRunUnresumable(run, "parked fan-out parent shared its turn with other in-flight tools; re-attach to continue")
		return fmt.Errorf("not auto-resumable (mixed fan-out tool turn)")
	}

	// Auto-resume only when there's a pending turn for the model to answer
	// (the conversation ends on a user/tool_result message). A conversation
	// ending on an assistant turn means the run was idle awaiting operator
	// input when paused — re-entering the loop would send the provider a
	// trailing assistant message. Flag it for manual re-attach instead. A
	// detected fan-out parent is exempt: its tool_result is synthesized below.
	if !isFanout && !endsWithPendingTurn(priorMessages) {
		s.flagRunUnresumable(run, "run was idle awaiting input when paused; re-attach + steer to continue")
		return fmt.Errorf("not auto-resumable (no pending turn)")
	}

	// System prompt segment (the conversation itself is in priorMessages).
	var segments []loop.PromptSegment
	if agentDef.SystemPrompt != "" {
		segments = []loop.PromptSegment{{
			Role: "system",
			Content: []loop.PromptContentBlock{{
				Type:      "trusted-text",
				Text:      agentDef.SystemPrompt,
				Cacheable: true,
			}},
		}}
	}

	// Flip the row back to running (mirror Manager.Resume) so a concurrent
	// Pause accounts for it and it's no longer "paused" data on disk.
	if err := s.store.SetRunPauseState(ctx, run.ID, store.PauseStateRunning); err != nil {
		return fmt.Errorf("set pause_state=running: %w", err)
	}
	// Stamp a fresh heartbeat NOW. The restored row carries an old started_at
	// and a NULL last_heartbeat_at; the stale-run sweeper's
	// "last_heartbeat_at IS NULL AND started_at < cutoff" branch would mark
	// the run failed in the window between flipping to running and the loop's
	// first OnHeartbeat. (The sweeper also skips pause_state='paused', which
	// covers the pre-resume window.)
	if err := s.store.UpdateHeartbeat(ctx, run.ID); err != nil {
		log.Printf("resume: heartbeat stamp for %s failed: %v", run.ID, err)
	}

	// Detached background context: keep ctx VALUES but do NOT die when the
	// caller (restore handler / boot) returns. Stops only via the cancel
	// registry (operator cancel) or process exit. Mirrors the interactive
	// background-goroutine pattern in handleRuns.
	runParent := context.WithoutCancel(ctx)
	runCtx, cancelFn := context.WithCancelCause(runParent)
	runCtx, runSpan := lcotel.RecordRunStart(runCtx, lcotel.RunStartAttrs{
		RunID:     run.ID,
		AgentID:   run.AgentID,
		AgentName: run.Agent,
		UserID:    run.UserID,
	})
	meta := runStateMeta{
		RunID:         run.ID,
		AgentID:       run.AgentID,
		Agent:         run.Agent,
		UserID:        run.UserID,
		ParentContext: run.ParentContext,
		otelSpan:      runSpan,
	}

	// Claim the run in the cancel registry. ErrInUse ⇒ already live on this
	// instance (e.g. a double restore, or boot racing a restore) — skip
	// without disturbing the running copy.
	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:   run.AgentID,
		RunID:     run.ID,
		SessionID: run.SessionID,
		UserID:    run.UserID,
		StartedAt: time.Now(),
	}, cancelFn)
	if regErr != nil {
		runSpan.End()
		cancelFn(nil)
		return fmt.Errorf("cancel registry: %w", regErr)
	}
	s.publishRunState(meta, "running", "", "")

	// Store-only emit (no live client to forward to) — the resumed turns
	// append to the same run's transcript so a re-attaching operator tails them.
	emit := s.makeRecordingEmit(runCtx, run.ID, func(providers.Event) {})
	steerQ, onSteer, deregSteer := s.makeSteer(runCtx, run.ID, run.AgentID, run.SessionID, run.UserID, emit)

	loopCtx := tools.WithAgentTools(runCtx, toolNames(allowedTools))
	loopCtx = tools.WithRunIdentity(loopCtx, tools.RunIdentityValue{
		UserID:        run.UserID,
		TenantID:      run.TenantID,
		AgentID:       run.AgentID,
		UserTier:      run.UserTier,
		ParentContext: run.ParentContext,
		// UserBearer / UserCredentials are intentionally absent — secrets are
		// never snapshotted, so a resumed run cannot restore them.
	})
	loopCtx = tools.WithHostPolicy(loopCtx, tools.HostPolicyValue{}) // no caller narrowing snapshotted; operator floor applies
	loopCtx = tools.WithAgentName(loopCtx, run.Agent)
	loopCtx = tools.WithMemoryPolicy(loopCtx, tools.MemoryPolicyValue{
		AllowedScopes: agentDef.MemoryScopes,
		QuotaBytes:    agentDef.MemoryQuotaBytes,
		Backend:       agentDef.MemoryBackend,
	})
	// Compaction re-derived from the agent def (per-run override not snapshotted),
	// stamped for sub-agent inheritance.
	loopCtx = tools.WithCompactionPolicy(loopCtx, agentDef.Compaction)
	loopCtx = tools.WithChannelPolicy(loopCtx, s.channelPolicyForAgent(loopCtx, agentDef))
	loopCtx = tools.WithEventEmitter(loopCtx, emit)
	adPolicy, evPolicy := s.substratePoliciesForAgent(agentDef, run.Agent)
	loopCtx = tools.WithAgentDefPolicy(loopCtx, adPolicy)
	loopCtx = tools.WithSkillDefPolicy(loopCtx, s.skillDefPolicyForAgent(agentDef))
	loopCtx = tools.WithEvaluationPolicy(loopCtx, evPolicy)
	loopCtx = tools.WithHistoryPolicy(loopCtx, s.historyPolicyForAgent(agentDef))
	loopCtx = tools.WithInterruptionPolicy(loopCtx, s.interruptionPolicyForAgent(agentDef))
	loopCtx = tools.WithRunID(loopCtx, run.ID)
	loopCtx = tools.WithDispatcher(loopCtx, dispatcher)

	heartbeat := s.makeHeartbeat(run.ID)
	fbPolicy, fbReResolve := s.fallbackForRun(run.TenantID, run.Agent, run.UserTier)
	gate, deregGate := s.newPauseGate(run.ID)
	// RFC X Phase 3: a re-dispatched run that itself fans out can park too.
	loopCtx = tools.WithPauseGate(loopCtx, gate)

	runOpts := loop.RunOptions{
		Provider:               provider,
		Model:                  model,
		Tools:                  allowedTools,
		Dispatcher:             dispatcher,
		Segments:               segments,
		PriorMessages:          priorMessages,
		OnEvent:                emit,
		OnHeartbeat:            heartbeat,
		MaxTokens:              agentDef.MaxTokens,
		MaxIterations:          agentDef.MaxIterations,
		UnboundedIterations:    agentDef.UnboundedIterations,
		SteerQueue:             steerQ,
		OnSteer:                onSteer,
		Effort:                 effort,
		MarkStalled:            s.markStalledFn(providerID, model),
		MarkRateLimited:        s.markRateLimitedFn(run.UserTier),
		ClearStall:             s.clearStallFn(providerID, model),
		ToolParallelism:        s.cfg.Env.ToolParallelism,
		AgentName:              run.Agent,
		CodeBody:               agentDef.Code,
		RunTimeoutSeconds:      agentDef.RunTimeoutSeconds, // per-run override not snapshotted
		Interactive:            run.Interactive,
		Sampling:               agentDef.Sampling,   // per-run override not snapshotted
		Compaction:             agentDef.Compaction, // per-run override not snapshotted
		UserTier:               run.UserTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForAgent(agentDef, run.UserTier),
		PauseGate:              gate,
	}

	go func() {
		// Panic-safe teardown: a panic must not crash the process (no
		// recoveryMiddleware wraps this detached goroutine) nor leak the run in
		// the cancel / steer / pause-barrier registries (a leaked pause entry
		// never parks, so every future Pause times out on a ghost).
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("resumed run %s panicked: %v", run.ID, rec)
				s.finishRunFailedReason(run.ID, fmt.Sprintf("panic: %v", rec), meta)
			}
			deregSteer()
			deregGate()
			s.cancelReg.Deregister(run.AgentID)
			runSpan.End()
			cancelFn(nil)
		}()
		// RFC X Phase 3: reconcile a parked fan-out parent BEFORE acquiring a run
		// slot. The reconcile awaits this parent's children (which acquire their
		// OWN per-user slots when ResumePausedRuns re-dispatches them) — holding a
		// slot here while awaiting could deadlock the per-user semaphore. The
		// synthesized parallel_spawn tool_result is appended to PriorMessages so
		// loop.Run continues past the dangling tool_use.
		if isFanout {
			toolResult, rerr := s.reconcileFanoutParent(runCtx, run, runEvents, fanout, emit)
			if rerr != nil {
				s.finishRunFailedReason(run.ID, "resume: reconcile fan-out parent: "+rerr.Error(), meta)
				return
			}
			runOpts.PriorMessages = append(runOpts.PriorMessages, toolResult)
		}

		// Respect the per-tenant fairness semaphore so a boot that resumes many
		// runs doesn't blow past MAX_CONCURRENT_RUNS. Acquired inside the
		// goroutine so the caller's resume loop never blocks.
		release, aerr := s.sem.AcquireForUser(runCtx, run.UserID)
		if aerr != nil {
			s.finishRunFailedReason(run.ID, "resume: acquire run slot: "+aerr.Error(), meta)
			return
		}
		defer release()

		loopRes, runErr := loop.Run(loopCtx, runOpts)
		if runErr != nil {
			emit(providers.Event{Type: providers.EventError, Error: runErr.Error()})
		}
		s.finishRunWithCancel(context.WithoutCancel(runCtx), runCtx, run.ID, loopRes, runErr, meta)
	}()
	return nil
}

// flagRunUnresumable marks a paused run terminal so a restored-but-unresumable
// run isn't a permanent "running" zombie (the F42 symptom). Best-effort.
func (s *Server) flagRunUnresumable(run store.Run, reason string) {
	meta := runStateMeta{
		RunID:         run.ID,
		AgentID:       run.AgentID,
		Agent:         run.Agent,
		UserID:        run.UserID,
		ParentContext: run.ParentContext,
	}
	s.finishRunFailedReason(run.ID, "resume failed: "+reason, meta)
}

// endsWithPendingTurn reports whether the reconstructed conversation ends on a
// turn the model is expected to answer (a user / tool_result message). A clean
// pause boundary (mid-tool-cycle) ends here; an idle-awaiting-input run ends on
// an assistant turn and is not auto-resumable.
func endsWithPendingTurn(msgs []providers.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	return msgs[len(msgs)-1].Role == "user"
}

// --- RFC X Phase 3: parked fan-out parent reconcile ---------------------------

// fanoutParkInfo describes a detected parked fan-out parent: the parallel_spawn
// tool_use that never received a tool_result, plus its raw input (the
// authoritative spawn list, so the reconcile knows the full child count even
// when a child past the concurrency cap never started and thus has no ledger).
type fanoutParkInfo struct {
	toolUseID string
	input     json.RawMessage
}

// detectFanoutParent scans one run's transcript events for a parked fan-out
// parent: an Agent parallel_spawn tool_use that (a) has at least one
// spawn_child_started ledger event (so it IS a fan-out, not some other tool)
// and (b) has no matching tool_result (so the Phase-3 watcher parked it
// mid-wg.Wait before the envelope was emitted). Returns the dangling
// tool_use_id + its tool_call input. Flag-gated: returns (_, false) when
// ResumeFanout is off, so the run takes the normal endsWithPendingTurn path —
// byte-identical to pre-Phase-3. At most one tool_use can be unanswered at a
// pause boundary (the loop answers every tool_use within an iteration before
// the next), so the first unanswered ledger-backed id is the parked parent.
func detectFanoutParent(enabled bool, runEvents []store.Event) (fanoutParkInfo, bool) {
	if !enabled {
		return fanoutParkInfo{}, false
	}
	answered := map[string]bool{}         // tool_use_id → has a tool_result
	hasLedger := map[string]bool{}        // tool_use_id → has ≥1 spawn_child_started
	input := map[string]json.RawMessage{} // tool_use_id → tool_call input
	for _, ev := range runEvents {
		switch ev.Type {
		case "tool_call":
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil && pe.ToolUse != nil {
				input[pe.ToolUse.ID] = pe.ToolUse.Input
			}
		case "tool_result":
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil && pe.ToolUse != nil {
				answered[pe.ToolUse.ID] = true
			}
		case string(providers.EventSpawnChildStarted):
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil && pe.SpawnChild != nil {
				hasLedger[pe.SpawnChild.ToolUseID] = true
			}
		}
	}
	for tuID := range hasLedger {
		if !answered[tuID] {
			return fanoutParkInfo{toolUseID: tuID, input: input[tuID]}, true
		}
	}
	return fanoutParkInfo{}, false
}

// lastTurnIsSoleToolUse confirms the reconstructed conversation ends on an
// assistant turn whose ONLY tool_use is the fan-out tool_use_id. A turn with
// other tool_use blocks means the fan-out shared its iteration with sibling
// tools (also dangling) — not auto-reconcilable, see the caller.
func lastTurnIsSoleToolUse(msgs []providers.Message, toolUseID string) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" {
		return false
	}
	n, match := 0, false
	for _, c := range last.Content {
		if c.Type == "tool_use" {
			n++
			if c.ToolUseID == toolUseID {
				match = true
			}
		}
	}
	return n == 1 && match
}

// parseSpawnNames reads the authoritative child count + names from a
// parallel_spawn tool_call input. Used so the reconcile can produce a result
// entry even for a child that was never dispatched (blocked past the
// concurrency cap when the pause hit, so it has no ledger entry). Returns
// (0, nil) on any unmarshal problem — the caller falls back to the ledger count.
func parseSpawnNames(input json.RawMessage) (int, []string) {
	if len(input) == 0 {
		return 0, nil
	}
	var in struct {
		Spawns []struct {
			Name string `json:"name"`
		} `json:"spawns"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return 0, nil
	}
	names := make([]string, len(in.Spawns))
	for i, sp := range in.Spawns {
		names[i] = sp.Name
	}
	return len(in.Spawns), names
}

// reconcileFanoutParent reconstructs the parallel_spawn tool_result for a
// restored fan-out parent and returns it as a user(tool_result) Message to
// append to PriorMessages so the loop continues past the dangling tool_use.
//
// Per child index it prefers the durable spawn_child_result ledger event (a
// child that completed BEFORE the snapshot — whose run row isn't captured);
// otherwise it awaits the child run (re-dispatched independently by
// ResumePausedRuns) to terminal and reads its result. A child with no ledger
// entry (never dispatched — blocked past the concurrency cap when the pause
// hit) or one that can't be recovered becomes an error result the parent's
// model can choose to re-issue. The synthesized envelope matches
// executeParallelSpawn's exact JSON shape, and is also appended to the parent's
// transcript via emit so a re-attaching operator and any future re-snapshot
// see a complete tool cycle.
func (s *Server) reconcileFanoutParent(ctx context.Context, run store.Run, runEvents []store.Event, fanout fanoutParkInfo, emit func(providers.Event)) (providers.Message, error) {
	// Keep the parent's heartbeat fresh during the (possibly long) child await:
	// the parent's own loop heartbeat doesn't start until this returns, and the
	// stale-run sweeper (default 10m) would otherwise reap the parent
	// mid-reconcile if a child legitimately runs longer than the cutoff.
	hbStop := make(chan struct{})
	go func() {
		t := time.NewTicker(fanoutHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.store.UpdateHeartbeat(ctx, run.ID); err != nil {
					log.Printf("resume: fan-out parent %s heartbeat pump: %v", run.ID, err)
				}
			}
		}
	}()
	defer close(hbStop)

	// Gather the ledger keyed by child index.
	type childLedger struct {
		runID  string
		agent  string
		result *providers.SpawnChildEventInfo
	}
	children := map[int]*childLedger{}
	get := func(idx int) *childLedger {
		if c := children[idx]; c != nil {
			return c
		}
		c := &childLedger{}
		children[idx] = c
		return c
	}
	for _, ev := range runEvents {
		switch ev.Type {
		case string(providers.EventSpawnChildStarted):
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err != nil || pe.SpawnChild == nil || pe.SpawnChild.ToolUseID != fanout.toolUseID {
				continue
			}
			c := get(pe.SpawnChild.Index)
			c.runID = pe.SpawnChild.RunID
			c.agent = pe.SpawnChild.Agent
		case string(providers.EventSpawnChildResult):
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err != nil || pe.SpawnChild == nil || pe.SpawnChild.ToolUseID != fanout.toolUseID {
				continue
			}
			c := get(pe.SpawnChild.Index)
			scCopy := *pe.SpawnChild
			c.result = &scCopy
			if c.agent == "" {
				c.agent = pe.SpawnChild.Agent
			}
		}
	}

	// Child count must span every index we have a ledger row for — derive it
	// from the HIGHEST observed index, not len(children): the dispatched set can
	// be sparse (a child past the concurrency cap never emitted a started event,
	// and goroutine scheduling means the dispatched indices aren't a contiguous
	// prefix), so len(children) could be < maxIndex+1 and the loop below would
	// silently drop the highest-index child's real result. parseSpawnNames then
	// widens it further to the authoritative input count (covers a never-
	// dispatched tail) when the persisted tool_call input is still parseable.
	count := 0
	for idx := range children {
		if idx+1 > count {
			count = idx + 1
		}
	}
	var names []string
	if n, nm := parseSpawnNames(fanout.input); n > count {
		count = n
		names = nm
	}
	if count == 0 {
		return providers.Message{}, fmt.Errorf("fan-out tool_use %s has no children to reconcile", fanout.toolUseID)
	}

	results := make([]builtin.ParallelSpawnResult, count)
	for i := 0; i < count; i++ {
		c := children[i]
		name := ""
		if c != nil {
			name = c.agent
		}
		if name == "" && i < len(names) {
			name = names[i]
		}
		switch {
		case c != nil && c.result != nil:
			// Completed before the snapshot — durable result in the ledger.
			results[i] = builtin.ParallelSpawnResult{Index: i, Agent: c.result.Agent, Ok: c.result.Ok, Output: c.result.Output, Error: c.result.Error}
		case c != nil && c.runID != "":
			// Still running (parked) at snapshot → re-dispatched independently
			// by ResumePausedRuns. Await it + read its result.
			results[i] = s.awaitChildResult(ctx, i, name, c.runID)
		default:
			results[i] = builtin.ParallelSpawnResult{Index: i, Agent: name, Ok: false,
				Error: "child was not dispatched before the snapshot; re-issue if its result is required"}
		}
	}

	envelope := struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}{Results: results}
	body, err := json.Marshal(envelope)
	if err != nil {
		return providers.Message{}, fmt.Errorf("marshal parallel_spawn envelope: %w", err)
	}

	// Persist the synthesized tool_result to the parent's transcript so a
	// re-attaching operator AND any future re-snapshot see a complete cycle.
	// makeRecordingEmit stores it under the parent's run id (mutex-guarded).
	emit(providers.Event{
		Type:    providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: fanout.toolUseID},
		Text:    string(body),
	})

	return providers.Message{
		Role: "user",
		Content: []providers.ContentBlock{{
			Type:      "tool_result",
			ToolUseID: fanout.toolUseID,
			Text:      string(body),
		}},
	}, nil
}

// fanoutChildPollInterval / fanoutChildAwaitTimeout bound the reconcile's wait
// for a re-dispatched child to reach terminal. The timeout is a backstop: a
// child has its OWN run_timeout + the stale-run sweeper as ultimate ceilings,
// so this only fires if a child genuinely wedges. Generous so a long-running
// solver child still resolves to a real result rather than a timeout error.
const (
	fanoutChildPollInterval = 500 * time.Millisecond
	fanoutChildAwaitTimeout = 30 * time.Minute
	// fanoutChildMaxErrPolls bounds how long a persistent NON-ErrNotFound store
	// error (DB outage) is retried before awaitChildTerminal surfaces it — ~10s
	// at the 500ms poll interval, vs silently burning the full 30m backstop.
	fanoutChildMaxErrPolls = 20
	// fanoutHeartbeatInterval keeps the parent's last_heartbeat_at fresh during
	// the await. Well under the sweeper's default 10m StaleAfter; matches the
	// cadence the loop itself would heartbeat at once it starts.
	fanoutHeartbeatInterval = 30 * time.Second
)

// awaitChildResult awaits one re-dispatched child to terminal and maps it to a
// builtin.ParallelSpawnResult: completed → ok + the child's final text (prefixed exactly like
// runSubAgent so it reads identically to a live collection); failed/cancelled →
// an error result. A child whose row was never captured (the narrow race where
// it completed in the instant before the snapshot, so pause_state never flipped
// to 'paused') resolves to an error result via awaitChildTerminal's fast-fail.
func (s *Server) awaitChildResult(ctx context.Context, index int, name, childRunID string) builtin.ParallelSpawnResult {
	child, err := s.awaitChildTerminal(ctx, childRunID)
	if err != nil {
		return builtin.ParallelSpawnResult{Index: index, Agent: name, Ok: false,
			Error: fmt.Sprintf("await child run %s: %v", childRunID, err)}
	}
	if name == "" {
		name = child.Agent
	}
	switch child.Status {
	case store.RunCompleted:
		out := s.childFinalText(ctx, child)
		if out == "" {
			out = fmt.Sprintf("(sub-agent %q completed with no final text)", name)
		}
		return builtin.ParallelSpawnResult{Index: index, Agent: name, Ok: true,
			Output: formatSubAgentOutput(child.AgentID, out)}
	default:
		msg := child.ErrorMsg
		if msg == "" {
			msg = string(child.Status)
		}
		return builtin.ParallelSpawnResult{Index: index, Agent: name, Ok: false, Error: msg}
	}
}

// awaitChildTerminal polls a child run row until it reaches a terminal status
// or a backstop deadline elapses, returning early on:
//   - a persistent ErrNotFound (the row wasn't captured in the snapshot — the
//     narrow race where the child completed in the instant before capture);
//   - a persistent NON-ErrNotFound store error (a DB outage shouldn't burn the
//     whole backstop silently — surface it after a short streak).
//
// A child that a CONCURRENT pause re-parks (pause_state set, Status still
// 'running') is alive, not wedged: its presence RESETS the deadline so a long
// second pause doesn't turn a healthy parked child into a spurious timeout
// error. ctx (the parent's runCtx) cancellation — operator cancel — always wins.
func (s *Server) awaitChildTerminal(ctx context.Context, childRunID string) (store.Run, error) {
	tick := time.NewTicker(fanoutChildPollInterval)
	defer tick.Stop()
	deadline := time.Now().Add(fanoutChildAwaitTimeout)
	notFoundStreak, errStreak := 0, 0
	for {
		run, err := s.store.GetRun(ctx, childRunID)
		switch {
		case err == nil:
			notFoundStreak, errStreak = 0, 0
			if isTerminalRunStatus(run.Status) {
				return run, nil
			}
			// Re-parked by a concurrent pause → don't penalize parked time.
			if run.PauseState == store.PauseStatePaused || run.PauseState == store.PauseStatePausing {
				deadline = time.Now().Add(fanoutChildAwaitTimeout)
			}
		default:
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				notFoundStreak++
				if notFoundStreak >= 3 {
					return store.Run{}, fmt.Errorf("child run not found (not captured in snapshot)")
				}
			} else if errStreak++; errStreak >= fanoutChildMaxErrPolls {
				return store.Run{}, fmt.Errorf("get child run: %w", err)
			}
		}
		if time.Now().After(deadline) {
			return store.Run{}, fmt.Errorf("child run %s did not reach terminal within %s", childRunID, fanoutChildAwaitTimeout)
		}
		select {
		case <-ctx.Done():
			return store.Run{}, ctx.Err()
		case <-tick.C:
		}
	}
}

// formatSubAgentOutput wraps a sub-agent's final text with the parseable
// "[sub-agent agent_id=...]" header the parent agent's model reads to attribute
// child output. Shared by the live collection path (runSubAgent) and the
// cross-instance reconcile (awaitChildResult) so the wire contract can't drift.
func formatSubAgentOutput(agentID, finalText string) string {
	return fmt.Sprintf("[sub-agent agent_id=%s]\n%s", agentID, finalText)
}

// childFinalText reads a completed child's final assistant text from its
// transcript — the same last-assistant-text that runSubAgent surfaces via
// loop.RunResult.FinalText. Returns "" if the transcript is unreadable or has
// no assistant text (the caller substitutes a "no final text" placeholder).
func (s *Server) childFinalText(ctx context.Context, child store.Run) string {
	if child.SessionID == "" {
		return ""
	}
	events, err := s.store.GetTranscript(ctx, child.SessionID)
	if err != nil {
		return ""
	}
	runEvents := make([]store.Event, 0, len(events))
	for _, e := range events {
		if e.RunID == child.ID {
			runEvents = append(runEvents, e)
		}
	}
	msgs := replayTranscript(runEvents)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		var b strings.Builder
		for _, c := range msgs[i].Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	return ""
}
