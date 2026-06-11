package http

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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

	// Auto-resume only when there's a pending turn for the model to answer
	// (the conversation ends on a user/tool_result message). A conversation
	// ending on an assistant turn means the run was idle awaiting operator
	// input when paused — re-entering the loop would send the provider a
	// trailing assistant message. Flag it for manual re-attach instead.
	if !endsWithPendingTurn(priorMessages) {
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
		Sampling:               agentDef.Sampling, // per-run override not snapshotted
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
