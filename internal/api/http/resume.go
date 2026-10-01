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
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/errkind"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/steer"
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
// Called from two places (see finishRestore + the boot wiring):
//   - after a snapshot restore writes the rows, on every transport that
//     restores (HTTP, gRPC, MCP), and
//   - at boot, scanning the store for any paused runs (crash recovery).
//
// WHAT A RESUMED RUN IS BUILT FROM: its own row and its run_config record
// (runConfigRecord), not from the definition as it stands now and not from
// whoever triggered the resume. The row gives its tenant, user, tier, starting
// model, interactive flag, parent run and operator-key / isolation bits. The
// record gives its sampling, tool choice, output format, compaction, context
// mode, budgets, run timeout, routing, interactive / review state, interruption
// narrowing, caller host narrowing, added and pinned hooks, a sub-run's spawn
// ceiling (its parent's volumes and fan-out width), and the definition it
// started on — an AgentDef version by id, a registered agent's row by digest,
// or the operator's static definition, read past any registered agent or
// AgentDef that has shadowed its name since; one gone or changed since fails
// the run, one in another tenant is refused. A row recorded before a piece of
// the record existed resumes that piece from the definition, as before —
// except a sub-run's inherited ceilings, which fail closed (no caller hosts,
// no volumes, serial fan-out).
//
// LIMITATIONS:
//   - Per-run SECRETS (UserBearer / named UserCredentials) are never persisted
//     with the run, so a resumed run can't restore them; a tool call that needs
//     ${run.user_bearer} / ${run.credentials.*} in an MCP header degrades.
//   - Run METADATA is not recorded: the loop is re-entered with none, so a
//     code-js orchestrator reads no metadata on its resumed turns.
//   - SKILLS are not pinned. The Skill tool loads the SkillDef version active
//     when it is called, for a live run and a resumed one alike; a skill loaded
//     before the pause stays as loaded, in the replayed transcript.
//   - The MODEL restored is the one the run STARTED on. A mid-run provider
//     fallback is not recorded on the row, so a resumed run does not continue
//     on the fallback target.
//   - A STATIC agent is pinned by kind, not by content. A run that started on
//     one resumes on the static definition as the operator's configuration
//     holds it at resume — an edit applied by a config reload in between is
//     followed, as every run of it follows the operator's yaml — and fails if
//     it was removed. A run recorded before the static marker existed resumes
//     by name, so a registered agent that has shadowed its name since takes
//     it over (an AgentDef shadow still fails it).
//   - Registered agents (dynamic_agents) are not carried by snapshots, so a
//     paused run of one restored on another instance fails unless the same
//     registration exists there, as stored.
//   - A run that was IDLE when paused (its conversation ends on an assistant
//     turn, not a pending user/tool_result) cannot re-enter the loop directly —
//     that would send the provider a trailing assistant turn. It is restored to
//     what it was doing instead: an interactive run waiting for input parks
//     again, and a run held for review is held again. An idle non-interactive
//     run that was not held has nobody to wait for and is flagged failed.
//     (Mid-execution runs — the F42 repro — end on a clean tool_result boundary
//     and resume cleanly.)

// ResumePausedRuns re-dispatches every pause_state='paused' run found in the
// store. Returns the count successfully re-dispatched and any per-run warnings
// (a run whose agent no longer resolves, or that isn't auto-resumable, is
// flagged failed and counted as a warning, not a hard error). Safe to call when
// the store is nil (no-op) and idempotent: a run that is already live is left
// alone and is neither counted nor warned about (see resumePausedRunsReport).
func (s *Server) ResumePausedRuns(ctx context.Context) (int, []string) {
	r := s.resumePausedRunsReport(ctx)
	return r.Resumed, r.Warnings
}

// resumeReport is what one resume pass did with the paused rows it found.
type resumeReport struct {
	Resumed int
	// AlreadyLive counts rows left untouched because a live loop still owns
	// them: in this process, or on another replica that is alive. Not a
	// failure — nothing was wrong with them — and not a warning.
	AlreadyLive int
	Warnings    []string
}

// errRunAlreadyLive is resumePausedRun's answer for a run a live loop still
// owns. It is returned before anything about the run is written.
var errRunAlreadyLive = errors.New("run is already live")

// resumeLivenessProbeTimeout bounds the replicas-table read that decides
// whether a run's owner is alive. The resume runs on a context with no
// deadline, and a hung database must not hang boot's resume pass.
const resumeLivenessProbeTimeout = 5 * time.Second

func (s *Server) resumePausedRunsReport(ctx context.Context) resumeReport {
	var r resumeReport
	if s.store == nil {
		return r
	}
	paused, err := s.store.ListPausedRuns(ctx)
	if err != nil {
		r.Warnings = []string{fmt.Sprintf("list paused runs: %v", err)}
		return r
	}
	for _, run := range paused {
		if err := s.resumePausedRun(run); err != nil {
			if errors.Is(err, errRunAlreadyLive) {
				r.AlreadyLive++
				continue
			}
			r.Warnings = append(r.Warnings, fmt.Sprintf("run %s (%s): %v", run.ID, run.Agent, err))
			continue
		}
		r.Resumed++
	}
	if r.Resumed > 0 || r.AlreadyLive > 0 || len(r.Warnings) > 0 {
		log.Printf("resume: re-dispatched %d paused run(s); %d already live; %d flagged", r.Resumed, r.AlreadyLive, len(r.Warnings))
	}
	return r
}

// pausedRunIsLive reports whether a live loop still owns a paused row, so the
// resume must not touch it. A row says pause_state='paused' both when it is
// restored data with no goroutine behind it and when its loop is alive and
// parked at a runtime pause; only the second is live. It is live when:
//   - this process holds it: its agent_id is in the cancel registry, or its
//     loop is registered with the pause manager; or
//   - its replica_id names ANOTHER replica whose heartbeat is fresh (by the
//     coordinators' own cutoff). Resuming it here would run the conversation
//     twice, once on each replica.
//
// A row naming this replica but held by nothing here, naming no replica, or
// naming a replica that is gone or stale is the crash-recovery case and is not
// live. Without a replicas table (single instance) only this process counts.
// A failed liveness read is an error, not "dead": resuming on a guess could
// start the second copy, while leaving the row paused costs only a later pass.
//
// An agent_id held here by a DIFFERENT run is neither: the row cannot be
// resumed under an id already in use, and it is refused before it is touched.
func (s *Server) pausedRunIsLive(ctx context.Context, run store.Run) (bool, error) {
	if live, err := s.heldInCancelRegistry(run); live || err != nil {
		return live, err
	}
	if s.pauseMgr.HoldsRun(run.ID) {
		return true, nil
	}
	if s.replicaStore == nil || run.ReplicaID == "" || run.ReplicaID == s.replicaID {
		return false, nil
	}
	probeCtx, stop := context.WithTimeout(ctx, resumeLivenessProbeTimeout)
	defer stop()
	alive, err := s.replicaStore.IsReplicaAlive(probeCtx, run.ReplicaID, coord.StaleReplicaThreshold)
	if err != nil {
		return false, fmt.Errorf("owning replica %q liveness: %w", run.ReplicaID, err)
	}
	return alive, nil
}

// heldInCancelRegistry reports whether this process's cancel registry holds
// the run's loop, and errors when its agent_id is held by some other run.
func (s *Server) heldInCancelRegistry(run store.Run) (bool, error) {
	e, ok := s.cancelReg.Get(run.AgentID)
	if !ok {
		return false, nil
	}
	if e.RunID != run.ID {
		return false, fmt.Errorf("agent_id %q is held by another live run (%s)", run.AgentID, e.RunID)
	}
	return true, nil
}

// resumePausedRun re-dispatches ONE paused run under its EXISTING run_id (no
// CreateRun — the row already exists). It mirrors handleRuns' dispatch but
// seeds the conversation from the transcript (PriorMessages) instead of a fresh
// prompt, re-derives provider/model/tools/system-prompt from the agent def it
// started on (resumedAgentDef), and runs the loop in a detached background
// goroutine (no HTTP request backs it).
//
// It takes no context from its caller, on purpose. A restore resumes runs on
// the restore request's context, which carries the restoring operator's
// auth.Principal — and the principal readers (the def tools' admin bypass,
// tenantFromCtx, Context op=self) prefer that principal over the run's own
// identity, so each tenant's resumed run, and every sub-agent it spawned, acted
// as the operator. A resumed run is the run its row records, not whoever
// triggered the resume: it starts from an empty context, the one a boot resume
// has always had, and gets its identity, tenant, isolation, operator-key
// restriction and policies only from the row and its definition, stamped
// below. Nothing it needs comes from the caller — the boot path, which passes
// no values, is proof of that.
func (s *Server) resumePausedRun(run store.Run) error {
	ctx := context.Background()
	if run.ID == "" || run.AgentID == "" || run.SessionID == "" {
		return fmt.Errorf("missing id/agent_id/session_id")
	}
	// First, before any store write: a row a live loop still owns is not ours
	// to resume. Everything below mutates the row (flags it failed, flips it
	// to running, re-stamps its heartbeat and replica), which on a live run
	// corrupts what its own loop and the next snapshot read.
	live, liveErr := s.pausedRunIsLive(ctx, run)
	if liveErr != nil {
		return liveErr
	}
	if live {
		return errRunAlreadyLive
	}

	// RFC AX: restore the operator-key restriction from the runs row so a resumed
	// run's credential-aware routing matches the original admission.
	//
	// RFC DC P1: resume on the run's OWN routing. Decoded before the resolve,
	// because the definition is not the authority here — the run is. A record
	// the definition no longer permits (a model dropped since) falls back to
	// the definition and says so, rather than failing a run that is otherwise
	// fine: refusing to resume is a heavier answer than resolving normally.
	runCfg, haveRunCfg := decodeRunConfig(run.RunConfig)

	// The definition the run started on, at its authoritative tenant. One that
	// no longer exists fails the run so it isn't a permanent "running" zombie;
	// it never falls back to the version active now.
	agentDef, gone, derr := s.resumedAgentDef(ctx, run, runCfg.AgentVersion)
	if derr != nil {
		if gone {
			s.flagRunUnresumable(run, derr.Error())
		}
		return derr
	}
	// ONE effective definition, restored from the run's own record. A record the
	// definition no longer permits — a model withdrawn, a fan-out ceiling
	// lowered since the run started — falls back to the definition and says so
	// rather than failing the resume: refusing is a heavier answer than
	// resolving normally, and the run is otherwise fine.
	//
	// Note the direction on the fan-out ceiling: a stale record can only ever
	// have asked for LESS width than the definition allows, so the fallback is
	// the WIDER of the two, which is why it is reported.
	if ed, oerr := s.effectiveDef(ctx, agentDef, runOverrides{
		Routing: runCfg.Routing, Resources: runCfg.Resources, Tuning: runCfg.Tuning,
	}); oerr == nil {
		agentDef = ed
	} else {
		log.Printf("resume: run %s had an override the definition no longer permits (%v); resolving from the definition", run.ID, oerr)
	}
	providerID, model, effort, rerr := s.resolveAgentDef(ctx, agentDef, run.TenantID, run.UserID, run.Agent, run.UserTier, run.OperatorKeyRestricted)
	if rerr != nil {
		s.flagRunUnresumable(run, fmt.Sprintf("resolve provider/model: %v", rerr))
		return fmt.Errorf("resolve agent: %w", rerr)
	}
	// RFC DD Gap 5: prefer the model the RUN was started with over whatever the
	// definition resolves to now. The row records it at CreateRun, and a
	// definition can be promoted, a pin can change, or tier availability can
	// move between pause and restore — so re-deriving silently routes a resumed
	// run somewhere the original never went, with nothing in the transcript
	// saying it moved.
	//
	// Re-validated, never merely trusted: the restored model must still be one
	// the CURRENT definition resolves to, so a promotion that dropped a model
	// cannot carry it back in through a snapshot. Falling back to the re-derived
	// value keeps legacy rows (no model recorded) working unchanged.
	//
	// SCOPE: run.Model is written once, at CreateRun. A mid-run provider
	// fallback emits EventProviderFallback but does not update the row, so this
	// restores the run's own STARTING model, not a fallback target. Recording
	// fallbacks on the row is a hot-path write and a separate decision.
	if run.Model != "" && run.Model != model {
		if pid, ok := s.providerForModel(ctx, agentDef, run.TenantID, run.UserID, run.Agent, run.UserTier, run.OperatorKeyRestricted, run.Model); ok {
			log.Printf("resume: run %s restoring its started model %q (definition now resolves %q)", run.ID, run.Model, model)
			providerID, model = pid, run.Model
		} else {
			log.Printf("resume: run %s started on model %q which the definition no longer permits; using %q", run.ID, run.Model, model)
		}
	}
	provider, perr := s.providers.Get(providerID)
	if perr != nil {
		s.flagRunUnresumable(run, fmt.Sprintf("provider %q unavailable: %v", providerID, perr))
		return fmt.Errorf("provider %q: %w", providerID, perr)
	}

	// RFC DD Gap 1: the run's OWN configuration, persisted at run start. Resume
	// used to rebuild each of these from the definition, so a long chat that
	// paused and resumed silently reverted its temperature, its compaction
	// policy and its layered-context mode mid-conversation — with nothing in the
	// transcript saying it changed.
	//
	// A run with no record (started before the column existed, or an
	// unreadable one) falls back to the definition, which is what every run did
	// before, so legacy rows resume exactly as they do today.
	if !haveRunCfg {
		runCfg = runConfigRecord{
			Sampling:          agentDef.Sampling,
			ToolChoice:        agentDef.ToolChoice,
			OutputFormat:      agentDef.OutputFormat,
			Compaction:        agentDef.Compaction,
			Context:           agentDef.Context,
			MaxContextTokens:  agentDef.MaxContextTokens,
			RunTimeoutSeconds: agentDef.RunTimeoutSeconds,
		}
	}
	// A sub-run's host narrowing was its PARENT's, and only its own record
	// holds it. A sub-run with no record (written before records existed, or
	// unreadable) was narrowed by a list nobody can now name, so it resumes on
	// the narrowest one: an empty caller list, which denies every host in
	// intersect mode and leaves only the operator's static list in
	// caller-authoritative mode. Its definition's reach is not a safe guess —
	// the parent's may have been far narrower.
	if !haveRunCfg && isSubRun(run) {
		log.Printf("resume: sub-run %s has no configuration record; its parent's host narrowing is unknown, so it resumes allowed no caller hosts", run.ID)
		runCfg.Hosts = &runHostRecord{HasList: true}
	}

	// Tools + dispatcher, narrowed by the caller's own allowed_hosts when the
	// run recorded one (RFC DD Gap 2). Restoring a narrowing can only SUBTRACT:
	// the operator's static floor still applies inside the tools, so this makes
	// a resumed run a subset of the original instead of handing it the bare
	// floor back.
	//
	// The MODE is operator config re-read now, not run state: whether a caller's
	// list replaces the operator floor or intersects with it is the operator's
	// current call, exactly as it is for a fresh run with the same inputs. Only
	// the caller's list and filter come from the run.
	allowedTools := filterTools(s.candidateTools(ctx, run.TenantID, agentDef.Tools), agentDef.Tools, nil)
	hostPolicy := runCfg.hostPolicy()
	callerAuthoritative := s.cfg().Env.HTTPCallerAuthoritative
	if hostPolicy.HasList || callerAuthoritative {
		allowedTools = builtin.NarrowHosts(allowedTools, runCfg.callerHosts(), hostPolicy.WebSearchFilter, callerAuthoritative)
	}
	allowedTools = s.grantRecallTool(allowedTools, runCfg.Context) // RFC CT: keep Recall on a resumed recall run
	dispatcher := s.newDispatcher(allowedTools)

	// Re-derive the system prompt (skill bodies baked in) from the current
	// agent def — exactly as the continuation path does; the prompt is NOT
	// replayed from the transcript (it may have changed across instances).
	agentDef, _ = s.resolveSkillBodiesForRun(ctx, run.TenantID, agentDef)
	// RFC BL P1: re-derive the memory injection + core-block set from the agent
	// def, exactly as the fresh/continue paths do (the injected prompt is not
	// replayed from the transcript). No initial input on a resume.
	var coreBlocks []config.CoreBlock
	agentDef, coreBlocks = s.applyMemoryInjection(ctx, agentDef, memInject{
		Tenant: run.TenantID, UserID: run.UserID, AgentName: run.Agent,
		Tools: allowedTools,
	})

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
	// RFC DH P2: a stateful run's history is Σ, not its messages — and neither
	// its first observation nor whether it can continue at all may be read off
	// a replayed conversation it never had. Both come from its stateful events.
	// The mode is resolved here the way Run will resolve it.
	stateful := loop.StatefulMode(runCfg.Context, provider.Capabilities().Local, run.Interactive)
	var seed statefulSeed
	if stateful {
		seed = statefulSeedFromEvents(runEvents, nil, run.Interactive)
	}

	// RFC X Phase 3: detect a parked fan-out PARENT — a parallel_spawn that the
	// Phase-3 watcher parked mid-wg.Wait, so its transcript ends on a dangling
	// tool_use (no envelope tool_result) backed by a spawn ledger. Such a run
	// can't take the endsWithPendingTurn path (it ends on an assistant turn) but
	// IS resumable: the goroutine below reconciles the children into the
	// envelope. Flag-gated — off ⇒ (_, false) ⇒ the byte-identical normal path.
	fanout, isFanout := detectFanoutParent(s.cfg().Env.ResumeFanout, runEvents)
	if isFanout && !lastTurnIsSoleToolUse(priorMessages, fanout.toolUseID) {
		// A parked fan-out that shared its turn with other in-flight tools (a
		// mixed tool iteration) isn't auto-reconcilable: synthesizing only the
		// fan-out's result would leave a sibling tool_use unanswered (the
		// provider 400s). Documented Phase-3 limitation; flag for manual
		// re-attach rather than emit a malformed continuation.
		s.flagRunUnresumable(run, "parked fan-out parent shared its turn with other in-flight tools; re-attach to continue")
		return fmt.Errorf("not auto-resumable (mixed fan-out tool turn)")
	}

	// A conversation ending on an ASSISTANT turn has nothing for the model to
	// answer: the run was idle awaiting the operator when it paused, and
	// re-entering the loop would send the provider a trailing assistant
	// message. A detected fan-out parent is exempt — its tool_result is
	// synthesized below.
	//
	// RFC DD Gap 3: an INTERACTIVE run in that state is restored to what it was
	// actually doing — parked, waiting — instead of being marked failed. It
	// re-registers in the steer registry like any resumed run, so the operator
	// re-attaches and continues on their next turn. This is the case that
	// mattered: a parked chat is the run an operator most expects to survive a
	// restart, and "your conversation is marked failed, re-attach and steer" was
	// a poor answer for it.
	//
	// A NON-interactive run stays refused. It has no operator to wait for, so
	// parking it would replace a loud failure with a run that idles forever
	// holding a concurrency slot.
	//
	// The steer registry is part of the condition, not an implementation
	// detail: parking means blocking on the steer queue, and makeSteer returns
	// a NIL queue when no registry is wired. The loop skips a park it has no
	// queue for, so claiming startParked without one would send the provider
	// the trailing assistant turn this whole branch exists to prevent — a
	// silent degrade into the exact malformed request. Refuse loudly instead.
	startParked := false
	// A stateful run has something to continue with when its seed carries an
	// observation (an action's result, an operator's message); with none, it
	// was waiting for the operator — the same two outcomes as below, decided
	// from its own events.
	idle := !endsWithPendingTurn(priorMessages)
	if stateful {
		idle = seed.Observation == ""
	}
	//
	// A run HELD FOR REVIEW when it paused is idle the same way — its
	// conversation ends on the answer being reviewed — and is restored to the
	// hold, interactive or not: it is waiting for a verdict, and a person owes
	// it one. Refusing it would lose the answer under review.
	var resumeHeld *loop.HeldReview
	if !isFanout && idle {
		held := heldReviewFrom(runEvents)
		switch {
		case held != nil && !stateful && s.steerReg != nil:
			resumeHeld = held
		case !run.Interactive || s.steerReg == nil:
			s.flagRunUnresumable(run, "run was idle awaiting input when paused; re-attach + steer to continue")
			return fmt.Errorf("not auto-resumable (no pending turn)")
		default:
			startParked = true
		}
	}

	// A run held by review arming stays held unless its record says review is
	// off. A record that says nothing is not a disarm: a team member's arming
	// lived on its walk, and a member held before its arming was recorded
	// would otherwise be approved here with no verdict. A hook's hold does not
	// depend on arming, so it is left as it was.
	reviewArmed := runCfg.Review != nil && *runCfg.Review
	if resumeHeld != nil && resumeHeld.HeldBy == "" && runCfg.Review == nil {
		reviewArmed = true
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
	// This replica now owns the run's live state (its steer queue, its cancel).
	// The row still names the replica it was created on, and a cross-replica
	// steer, verdict or cancel routes by the row: to a replica that is gone,
	// it answers "not in flight". Logged, not fatal — the run itself is fine,
	// and a verdict posted to this replica still reaches it.
	if s.replicaID != "" && run.ReplicaID != s.replicaID {
		if err := s.store.SetRunReplica(ctx, run.ID, s.replicaID); err != nil {
			log.Printf("resume: replica stamp for %s failed: %v", run.ID, err)
		}
	}

	// Detached background context: it does not die when the caller (restore
	// handler / boot) returns. Stops only via the cancel registry (operator
	// cancel) or process exit. Mirrors the interactive background-goroutine
	// pattern in handleRuns.
	runCtx, cancelFn := context.WithCancelCause(ctx)
	// A resumed sub-run is still its parent's child: it reports the same parent
	// and belongs to the same spawn tree as it did before the pause. The row
	// holds the parent; the tree's root is found by walking up from it.
	rootRunID, rootErr := resumedRootRunID(ctx, s.store.GetRun, run)
	if rootErr != nil {
		log.Printf("resume: run %s: %v; it resumes as the root of its own tree", run.ID, rootErr)
	}
	runCtx, runSpan := lcotel.RecordRunStart(runCtx, lcotel.RunStartAttrs{
		RunID:         run.ID,
		AgentID:       run.AgentID,
		AgentName:     run.Agent,
		UserID:        run.UserID,
		ParentAgentID: run.ParentAgentID,
	})
	meta := runStateMeta{
		RunID:         run.ID,
		AgentID:       run.AgentID,
		Agent:         run.Agent,
		UserID:        run.UserID,
		TenantID:      run.TenantID,
		ParentAgentID: run.ParentAgentID,
		ParentRunID:   run.ParentRunID,
		ParentContext: run.ParentContext,
		otelSpan:      runSpan,
		// Only the tree's root purges the tree's ephemeral volumes and run-scope
		// SQL database at completion. A resumed child that claimed it would tear
		// them down under a parent and siblings still using them.
		IsTopLevel: rootRunID == run.ID,
		RootRunID:  rootRunID,
	}

	// Claim the run in the cancel registry. ErrInUse ⇒ another resume in this
	// process claimed it between the liveness check above and here (a double
	// restore, or boot racing a restore) — that copy runs, this one stops.
	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:   run.AgentID,
		RunID:     run.ID,
		SessionID: run.SessionID,
		UserID:    run.UserID,
		// A resumed child runs under its own detached context, not its parent's,
		// so the registry's cascade is the only way a parent cancel reaches it.
		ParentAgentID: run.ParentAgentID,
		StartedAt:     time.Now(),
	}, cancelFn)
	if regErr != nil {
		runSpan.End()
		cancelFn(nil)
		if held, _ := s.heldInCancelRegistry(run); held {
			return errRunAlreadyLive
		}
		return fmt.Errorf("cancel registry: %w", regErr)
	}
	s.publishRunState(meta, "running", "", "")

	// Hoisted above the recording emit so resumed-turn usage is attributed under
	// the run's true identity (see makeRecordingEmit) and WithRunIdentity reuses it.
	rid := tools.RunIdentityValue{
		UserID:        run.UserID,
		TenantID:      run.TenantID,
		AgentID:       run.AgentID,
		RootRunID:     rootRunID,     // the tree it was spawned into, not a new one
		SessionID:     run.SessionID, // restored from the run row, like every other durable field here
		UserTier:      run.UserTier,
		ParentContext: run.ParentContext,
		// UserBearer / UserCredentials are intentionally absent — secrets are
		// never snapshotted, so a resumed run cannot restore them.
		// RFC AX: RESTORE the operator-key restriction from the persisted runs
		// column — the original principal isn't on ctx for a resumed/crash-
		// recovered run, so the durable bit is the authority.
		OperatorKeyRestricted: run.OperatorKeyRestricted,
		// RFC BX P2b: RESTORE the isolation confinement from the persisted runs
		// column — same rationale: no principal on ctx for a resumed run, so the
		// durable bit is the authority. Without this a paused isolated run could
		// escape its data-scope confinement on resume.
		Isolated: run.Isolated,
	}
	// Store-only emit (no live client to forward to) — the resumed turns
	// append to the same run's transcript so a re-attaching operator tails them.
	emit := s.makeRecordingEmit(runCtx, run.ID, rid, run.SessionID, meta, func(providers.Event) {})
	// A child the Agent tool started takes a verdict and nothing else, live
	// (runSubRun registers it VerdictsOnly) and so after a resume: its parent
	// drives it, and a full entry here handed an operator its steer, retune and
	// compaction the moment it came back. The row carries no kind, so it is
	// derived as the remote gate derives it. That also makes a resumed resident
	// child verdicts-only, though its live entry was full — the side a gate may
	// err on. No sub-run row is interactive, so none is parked on a queue that
	// would refuse its next turn.
	steerQ, onSteer, deregSteer := s.makeSteerEntry(runCtx, steer.Entry{
		RunID: run.ID, AgentID: run.AgentID, SessionID: run.SessionID, UserID: run.UserID,
		VerdictsOnly: takesOnlyVerdicts(run),
	}, emit)

	loopCtx := tools.WithAgentTools(runCtx, toolNames(allowedTools))
	loopCtx = tools.WithAgentToolPatterns(loopCtx, agentDef.Tools) // raw globs for the Skill subset check
	// RFC AR: honor a tenant/user provider-key override on the resumed run too.
	loopCtx = providers.WithCredentialResolver(loopCtx, s.credResolver)
	// RFC AX: mirror the restored negative permission bit onto ctx for the
	// stage-2 driver backstop (drivers import providers, not auth/tools).
	loopCtx = providers.WithOperatorKeyAllowed(loopCtx, !run.OperatorKeyRestricted)
	loopCtx = tools.WithRunIdentity(loopCtx, rid)
	// RFC DD Gap 2: the run's own narrowing, so the resumed run's sub-agents
	// inherit the same reach the original's did. Zero value (no record) leaves
	// the operator floor as the only bound, exactly as before.
	loopCtx = tools.WithHostPolicy(loopCtx, hostPolicy)
	loopCtx = tools.WithAgentName(loopCtx, run.Agent)
	loopCtx = tools.WithMemoryPolicy(loopCtx, tools.MemoryPolicyValue{
		AllowedScopes:      tools.EffectiveMemoryScopes(loopCtx, agentDef.MemoryScopes),
		QuotaBytes:         agentDef.MemoryQuotaBytes,
		Backend:            agentDef.MemoryBackend,
		Consolidation:      agentDef.MemoryConsolidation,
		RecallIncludeTurns: agentDef.RecallIncludeTurns,
		RecallAttachTraces: agentDef.RecallAttachTraces,
		Rerank:             agentDef.MemoryRerank,
		Units:              agentDef.MemoryUnits,
	})
	// RFC BL P1: re-stamp the run's core blocks (Memory-tool enforcement +
	// sub-agent inherit) — lost across pause/snapshot/resume otherwise.
	loopCtx = tools.WithCoreBlocksPolicy(loopCtx, tools.CoreBlocksPolicyValue{Blocks: coreBlocks})
	// RFC AA SQL Memory: re-derive the sql_scopes ACL from the agent def too —
	// without this a resumed/restored run reads a zero SqlMemPolicy and every
	// sql_query/sql_exec default-denies, a silent loss of SQL Memory access
	// across pause / snapshot / cross-instance resume (mirrors the memory +
	// volume policy re-derivation above).
	loopCtx = tools.WithSqlMemPolicy(loopCtx, tools.SqlMemPolicyValue{
		AllowedScopes: tools.EffectiveSqlScopes(loopCtx, agentDef.SqlScopes),
		QuotaBytes:    agentDef.SqlQuotaBytes,
	})
	// Restored from the run's own record, stamped for sub-agent inheritance.
	// The context policy is stamped too: every non-resume run path does, so
	// without it a resumed run's children inherited a different retention mode
	// from the original's children.
	loopCtx = tools.WithCompactionPolicy(loopCtx, runCfg.Compaction)
	// RFC DC P2: the resumed run's fan-out width, so its children are as narrow
	// as the original's were. A live sub-run never sets its own: it runs on
	// the width inherited from its ancestors, which its record kept.
	//
	// A sub-run with no record of it (spawned before the record existed) fails
	// closed to serial, as its volumes and hosts do: the width its ancestors
	// allowed is unknown, and its definition's is exactly the widening the
	// record guards against.
	fanoutCap := agentDef.MaxConcurrentChildren
	switch {
	case runCfg.Spawn != nil && runCfg.Spawn.FanoutCap > 0:
		fanoutCap = runCfg.Spawn.FanoutCap
	case runCfg.Spawn == nil && isSubRun(run):
		log.Printf("resume: sub-run %s has no recorded fan-out width; its ancestors' width is unknown, so it resumes serial", run.ID)
		fanoutCap = 1
	}
	loopCtx = tools.WithFanoutCap(loopCtx, fanoutCap)
	// RFC DC P5: offer this run's overrides to its children. Only a child of the
	// SAME definition will take them (tools.RunOverridesValue.SameDefinitionAs).
	loopCtx = tools.WithRunOverrides(loopCtx, tools.RunOverridesValue{
		Record: runCfg.marshal(), DefID: run.AgentDefID, AgentName: run.Agent,
	})

	loopCtx = tools.WithContextPolicy(loopCtx, runCfg.Context)
	loopCtx = tools.WithChannelPolicy(loopCtx, s.channelPolicyForAgent(loopCtx, agentDef))
	loopCtx = tools.WithOperatorAuthored(loopCtx, agentDef.OperatorAuthored)
	// Volume confinement (RFC AH attach-gap fix): without it a volume-bound
	// agent would resume with no volume policy at all. A top-level run's is its
	// agent's own; a sub-run's is narrowed to its parent's, as it was live.
	loopCtx = tools.WithVolumePolicy(loopCtx, s.resumedVolumePolicy(loopCtx, run, runCfg.Spawn, agentDef))
	// RFC AH Phase 2b: re-attach a fresh run-scoped ephemeral set, REHYDRATED
	// from any ephemeral_volume_defs rows this run created before it was
	// paused/snapshotted (the sweeper skips paused runs, so the rows + on-disk
	// dirs survived). Without this a resumed paused run would lose in-memory
	// resolution of its own ephemeral volumes. Best-effort: a store fault
	// leaves an empty set (the agent can re-create) rather than failing resume.
	// Keyed by the tree's root, which is what a volume is created under.
	loopCtx = tools.WithEphemeralVolumes(loopCtx, s.rehydrateEphemeralVolumes(loopCtx, rootRunID))
	loopCtx = tools.WithEventEmitter(loopCtx, emit)
	adPolicy, evPolicy := s.substratePoliciesForAgent(agentDef, run.Agent)
	loopCtx = tools.WithAgentDefPolicy(loopCtx, adPolicy)
	loopCtx = tools.WithSkillPolicy(loopCtx, s.skillPolicyForAgent(agentDef))
	// RFC BB: per-agent web-search fallback list (empty = global search_priority).
	loopCtx = tools.WithSearchProviders(loopCtx, agentDef.SearchProviders)
	loopCtx = tools.WithVolumeDefPolicy(loopCtx, s.volumeDefPolicyForAgent(agentDef))
	loopCtx = tools.WithEvaluationPolicy(loopCtx, evPolicy)
	loopCtx = tools.WithHistoryPolicy(loopCtx, s.historyPolicyForAgent(loopCtx, agentDef))
	// From the run's record, which a retune may have changed since it started —
	// an autonomous run that never parked adopts a retune here.
	loopCtx, liveInterruption := s.startRunInterruption(loopCtx, agentDef, runCfg.Interruption)
	loopCtx = tools.WithParentRunID(loopCtx, run.ParentRunID) // "" for a top-level run: stays unset
	loopCtx = tools.WithRunID(loopCtx, run.ID)
	if added := runCfg.additions(); !added.Empty() {
		// What the run added before it paused, restored as it was — a
		// definition's with its source: already checked at its start, so
		// carried like an inheritance.
		loopCtx = hooks.WithAdditions(loopCtx, added)
	}
	loopCtx = s.withResumedRunHooks(loopCtx, run, agentDef, runCfg.PinnedHooks)
	loopCtx = tools.WithDispatcher(loopCtx, dispatcher)

	heartbeat := s.makeHeartbeat(run.ID)
	// RFC BF P2b: the per-provider slot is acquired INSIDE the resume goroutine
	// (below), after the fan-out reconcile, so the caller's resume loop never
	// blocks on a full gate. Create the holder empty here so fallbackForRun's
	// reResolve closure can capture it; it's populated before loop.Run runs.
	provSlot := &providerSlot{}
	fbPolicy, fbReResolve := s.fallbackForRun(run.TenantID, run.UserID, run.Agent, run.UserTier, run.OperatorKeyRestricted, provSlot, runCfg.Routing)
	gate, deregGate := s.newPauseGate(run.ID)
	// RFC X Phase 3: a re-dispatched run that itself fans out can park too.
	loopCtx = tools.WithPauseGate(loopCtx, gate)

	if stateful {
		// The stateful loop renders an observation from these when it is given
		// none — which is exactly the replayed conversation it must not see.
		priorMessages = nil
	}
	resumedToolChoice := runCfg.ToolChoice
	if toolChoiceSpent(ctx, s.store, run.ID, resumedToolChoice) {
		resumedToolChoice = nil
	}
	runOpts := loop.RunOptions{
		// Re-entered under its own run id: it already started, so its
		// agent_start hooks do not run again.
		Resumed:             true,
		Provider:            provider,
		Model:               model,
		Tools:               allowedTools,
		Dispatcher:          dispatcher,
		Segments:            segments,
		PriorMessages:       priorMessages,
		InitialState:        seed.Sigma, // RFC DH P2: nil unless the run was stateful
		InitialObservation:  seed.Observation,
		OnEvent:             emit,
		OnHeartbeat:         heartbeat,
		MaxTokens:           agentDef.MaxTokens,      // RFC DC P2: restored, not re-derived
		MaxContextTokens:    runCfg.MaxContextTokens, // RFC CJ; restored, not re-derived
		MaxIterations:       agentDef.MaxIterations,
		UnboundedIterations: agentDef.UnboundedIterations,
		SteerQueue:          steerQ,
		OnSteer:             onSteer,
		Effort:              effort,
		MarkStalled:         s.markStalledFn(providerID, model),
		MarkRateLimited:     s.markRateLimitedFn(run.UserTier),
		ClearStall:          s.clearStallFn(providerID, model),
		ToolParallelism:     s.cfg().Env.ToolParallelism,
		AgentName:           run.Agent,
		CodeBody:            agentDef.Code,
		RunTimeoutSeconds:   runCfg.RunTimeoutSeconds,
		Interactive:         run.Interactive,
		InteractiveNow:      s.interactiveNowFn(run.ID, run.Interactive),
		Review:              reviewArmed,
		ReviewNow:           s.reviewNowFn(run.ID, reviewArmed),
		ReviewTTL:           runCfg.reviewTTL(), // the deadline runs from when the hold began, restart or not
		StartParked:         startParked,        // RFC DD Gap 3: it was waiting; put it back to waiting
		ResumeHeld:          resumeHeld,         // it was held for a verdict; hold it again
		Sampling:            runCfg.Sampling,    // restored from the run, not re-derived
		ToolChoice:          resumedToolChoice,  // restored, minus what the run already spent
		OutputFormat:        runCfg.OutputFormat,
		Compaction:          runCfg.Compaction, // restored from the run, not re-derived
		Context:             runCfg.Context,    // restored from the run, not re-derived (RFC CR)
		// BankCompactedSpan is deliberately ABSENT (RFC BL P3). A resumed run
		// replays a compaction that already happened, and its span was banked when
		// it first ran — wiring it here would re-bank the same conversation on
		// every resume, quietly duplicating candidates for the dedup band to
		// absorb. TestReplayCompaction_DoesNotRebank pins this.
		ContextPlugins:         s.contextPlugins, // RFC Z runtime-wide chain (code-js exempt in the loop)
		UserTier:               run.UserTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForAgent(agentDef, run.UserTier),
		// RFC DC P3: a RESTORED parked chat is the case this matters most for —
		// it is the one an operator comes back to and retunes.
		ReResolveOnOperatorTurn: s.reResolveOnOperatorTurnFn(run.ID, run.TenantID, run.UserID, run.Agent, run.UserTier, run.OperatorKeyRestricted, liveInterruption),
		PauseGate:               gate,

		// The RECORD's tool_choice, not resumedToolChoice: the baseline is what
		// the run holds, so a choice resume dropped as spent is not re-forced.
		ReReadShapeOnOperatorTurn: s.reReadShapeOnOperatorTurnFn(run.ID, runCfg.ToolChoice, runCfg.OutputFormat),
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
		// loop.Run continues past the dangling tool_use. It runs on the loop's
		// context, which carries the parent's hooks and identity: a child that
		// finishes during the reconcile goes through the parent's
		// subagent_stop hooks there.
		if isFanout {
			toolResult, rerr := s.reconcileFanoutParent(loopCtx, run, runEvents, fanout, emit)
			if rerr != nil {
				s.finishRunFailedReason(run.ID, "resume: reconcile fan-out parent: "+rerr.Error(), meta)
				return
			}
			if stateful {
				// The envelope IS the parked action's result, so it is the
				// observation — the stateful loop reads nothing else.
				runOpts.InitialObservation = toolResultText(toolResult)
			} else {
				runOpts.PriorMessages = append(runOpts.PriorMessages, toolResult)
			}
		}

		// RFC BF P2b per-provider gate: acquire BEFORE the global slot (same
		// ordering rationale as a fresh run) and AFTER the fan-out reconcile
		// above (holding a provider slot while awaiting children could deadlock).
		// Populate the holder so a mid-run fallback swaps the slot; released via
		// the holder at run end. A saturated cap fails the resume loudly rather
		// than silently un-resuming.
		provRelease, provErr := s.providerGates.Acquire(runCtx, providerID)
		if provErr != nil {
			s.finishRunFailedReason(run.ID, "resume: acquire provider slot: "+provErr.Error(), meta)
			return
		}
		provSlot.release = provRelease
		provSlot.providerID = providerID
		defer provSlot.releaseCurrent()
		// RFC BF P2c: a resumed run holds provSlot for its whole re-entered loop,
		// so publish its provider into the ancestor-held set — sub-agents it spawns
		// (a resumed fan-out parent) skip the gate for the SAME provider (deadlock
		// carve-out). Without this a same-provider parent→child spawn after resume
		// would self-deadlock. No-op for uncapped/noop slots.
		loopCtx = s.heldSlotCtx(loopCtx, provSlot)

		// Respect the per-tenant fairness semaphore so a boot that resumes many
		// runs doesn't blow past MAX_CONCURRENT_RUNS. Acquired inside the
		// goroutine so the caller's resume loop never blocks.
		release, aerr := s.sem.AcquireForUser(runCtx, run.TenantID, run.UserID)
		if aerr != nil {
			s.finishRunFailedReason(run.ID, "resume: acquire run slot: "+aerr.Error(), meta)
			return
		}
		defer release()

		loopRes, runErr := loop.Run(loopCtx, runOpts)
		if runErr != nil {
			emit(runErrorEvent(runErr))
		}
		s.finishRunWithCancel(context.WithoutCancel(runCtx), runCtx, run.ID, loopRes, runErr, meta)
	}()
	return nil
}

// resumedAgentDef is the definition a paused run continues on: the one it
// started on, not the one its agent name resolves to now. A version promoted,
// forked or retired while the run was paused does not change it — a newer
// version may offer wider tools, a different prompt or different policies, and
// a pause is not a moment at which a run's capabilities may change.
//
//   - A recorded version is read by its id, retired or not (the rule a pinned
//     HookDef follows, see pinnedLookup).
//   - A record marking the operator's static configuration reads the static
//     definition again, by name but past the tiers that shadow it — a
//     registered agent or a tenant's AgentDef of the same name that appeared
//     since did not start the run. Static definitions are the operator's yaml,
//     which only the operator changes and every run of it follows; one removed
//     since fails the run.
//   - Any other record naming no version started on a registered agent — or on
//     the operator's yaml, recorded before the static marker was. It resolves
//     by name — unless the name now resolves to an AgentDef version, which is
//     not the definition it started on but one that has shadowed it since, or
//     the run started on a registered agent whose row has changed or gone
//     since (its recorded digest no longer matches).
//   - A run recorded before versions were resolves by name, as it always did.
//
// A child its parent pinned by def_id (runs.agent_def_id) ran on that version
// laid over its base, as the live spawn builds it; resume lays it over the same
// base.
//
// gone reports that the definition is not there to resume on — the run should
// be marked failed. A store fault is not that: the run stays paused, to be
// tried again.
func (s *Server) resumedAgentDef(ctx context.Context, run store.Run, ver *agentVersionRecord) (config.AgentDef, bool, error) {
	var def config.AgentDef
	if ver != nil && ver.DefID != "" {
		row, gone, err := s.agentVersionRow(ctx, run, ver.DefID)
		if err != nil {
			return config.AgentDef{}, gone, err
		}
		d, ok := lookup.AgentFromDefRow(row)
		if !ok {
			return config.AgentDef{}, true, fmt.Errorf("agent %q: the version it started on (%s) is unreadable", run.Agent, ver.DefID)
		}
		def = d
	} else if ver != nil && ver.Static {
		d, ok := lookup.StaticAgent(s.cfg(), run.Agent)
		if !ok {
			return config.AgentDef{}, true, fmt.Errorf("agent %q: the static agent the run started on was removed from the configuration since it started", run.Agent)
		}
		def = d
	} else {
		d, ok := s.lookupAgent(ctx, run.TenantID, run.Agent)
		if !ok {
			return config.AgentDef{}, true, fmt.Errorf("agent %q no longer exists", run.Agent)
		}
		if ver != nil && d.DefID != "" {
			return config.AgentDef{}, true, fmt.Errorf("agent %q now resolves to AgentDef version %s, not the definition the run started on", run.Agent, d.DefID)
		}
		// A registered agent is re-registered in place, so its name resolving
		// is not enough: the row must still be the one the run started on. A
		// run recorded before the digest was carries none and is not checked.
		if ver != nil && ver.RegisteredSHA256 != "" && d.RegisteredSHA256 != ver.RegisteredSHA256 {
			return config.AgentDef{}, true, fmt.Errorf("agent %q: the registered agent the run started on was changed or removed since it started", run.Agent)
		}
		def = d
	}
	if run.AgentDefID != "" {
		row, gone, err := s.agentVersionRow(ctx, run, run.AgentDefID)
		if err != nil {
			return config.AgentDef{}, gone, err
		}
		def = pinnedSubAgentDef(def, row.Definition)
	}
	return def, false, nil
}

// agentVersionRow reads one AgentDef version of the run's agent by id, retired
// or not. It must be a version of the run's agent in the run's tenant or the
// shared one: the only rows its name can have resolved to. The bool is
// resumedAgentDef's gone.
func (s *Server) agentVersionRow(ctx context.Context, run store.Run, defID string) (store.AgentDefRow, bool, error) {
	if s.store == nil {
		return store.AgentDefRow{}, false, fmt.Errorf("agent %q: no store to read the version it started on", run.Agent)
	}
	row, err := s.store.AgentDefGet(ctx, defID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return store.AgentDefRow{}, true, fmt.Errorf("agent %q: the version it started on (%s) no longer exists", run.Agent, defID)
		}
		return store.AgentDefRow{}, false, fmt.Errorf("agent %q: read the version it started on (%s): %w", run.Agent, defID, err)
	}
	if row.Name != run.Agent || (row.TenantID != run.TenantID && row.TenantID != "") {
		return store.AgentDefRow{}, true, fmt.Errorf("agent %q: the version it recorded (%s) is not one of this agent's in the run's tenant", run.Agent, defID)
	}
	return row, false, nil
}

// maxResumeAncestry bounds the walk from a resumed run up to its tree's root.
// Far deeper than any spawn tree; it exists so a corrupt chain cannot stall a
// resume.
const maxResumeAncestry = 64

// resumedRootRunID is the id of the top-level run at the root of run's spawn
// tree, found by following parent_run_id up the stored rows — the id a live
// sub-run inherits from its parent's identity, which a resume has no parent to
// inherit from. A top-level run is its own root.
//
// When the chain cannot be followed to a top-level run (a pre-parent_run_id
// row's parent is unknown, a restored run's parent was not restored, a store
// fault, a cycle, another tenant's run), it returns run's own id with the
// reason: the run then keeps its tree state to itself, as every resumed run
// did before, rather than guessing whose tree it joins.
func resumedRootRunID(ctx context.Context, getRun func(context.Context, string) (store.Run, error), run store.Run) (string, error) {
	cur := run
	seen := map[string]bool{run.ID: true}
	for hops := 0; cur.ParentRunID != ""; hops++ {
		id := cur.ParentRunID
		if hops == maxResumeAncestry {
			return run.ID, fmt.Errorf("spawn tree deeper than %d runs above it", maxResumeAncestry)
		}
		if seen[id] {
			return run.ID, fmt.Errorf("its ancestry loops back to run %s", id)
		}
		seen[id] = true
		parent, err := getRun(ctx, id)
		if err != nil {
			return run.ID, fmt.Errorf("read ancestor run %s: %w", id, err)
		}
		if parent.ID != id {
			return run.ID, fmt.Errorf("ancestor run %s not found", id)
		}
		// A sub-run always shares its parent's tenant; a row that does not is
		// not one this run may share tree state with.
		if parent.TenantID != run.TenantID {
			return run.ID, fmt.Errorf("ancestor run %s is in another tenant", id)
		}
		cur = parent
	}
	return cur.ID, nil
}

// isSubRun reports whether a row is a spawned child. Every sub-run row carries
// parent_agent_id; parent_run_id came later, so either one marks it.
func isSubRun(run store.Run) bool {
	return run.ParentAgentID != "" || run.ParentRunID != ""
}

// resumedVolumePolicy is the volume confinement a resumed run gets back: the
// agent's own for a top-level run, and for a sub-run the same narrowing
// against its parent's policy it had live (childVolumePolicy), with the
// parent's policy rebuilt from the sub-run's record.
//
// A sub-run with no recorded ceiling (spawned before the record existed) fails
// CLOSED: confined to no volume at all. Its parent's bindings are unknown, and
// its definition's are exactly the widening this guards against. The empty
// policy is Active, not the zero value, so any child it spawns is narrowed to
// nothing too rather than resolved as if its parent were unconfined.
func (s *Server) resumedVolumePolicy(ctx context.Context, run store.Run, rec *spawnRecord, def config.AgentDef) tools.VolumePolicyValue {
	if !isSubRun(run) {
		return s.volumePolicyForAgent(ctx, def)
	}
	if rec == nil {
		log.Printf("resume: sub-run %s has no recorded volume ceiling; its parent's volumes are unknown, so it resumes with none", run.ID)
		return tools.VolumePolicyValue{Active: true}
	}
	return s.childVolumePolicy(ctx, s.recordedParentVolumes(ctx, rec.Volumes), def)
}

// recordedParentVolumes rebuilds the parent's volume policy a sub-run was
// spawned under. Names, modes and defaults come from the record; each root is
// resolved now, by the same lookup a fresh run uses, so the record can never
// introduce a path. A name that no longer resolves is dropped (narrower), and a
// volume that has since become read-only stays read-only.
func (s *Server) recordedParentVolumes(ctx context.Context, rec volumeCeilingRecord) tools.VolumePolicyValue {
	if !rec.Active {
		return tools.VolumePolicyValue{}
	}
	out := tools.VolumePolicyValue{Active: true}
	if len(rec.Bindings) == 0 {
		// Not handed to volumePolicyForAgent: an empty list there means
		// "undeclared", which binds the operator's default volume.
		return out
	}
	names := make([]string, 0, len(rec.Bindings))
	for _, b := range rec.Bindings {
		names = append(names, b.Name)
	}
	now := make(map[string]tools.VolumeBinding, len(names))
	for _, b := range s.volumePolicyForAgent(ctx, config.AgentDef{Volumes: names}).Bindings {
		now[b.Name] = b
	}
	for _, b := range rec.Bindings {
		cur, ok := now[b.Name]
		if !ok {
			continue
		}
		out.Bindings = append(out.Bindings, tools.VolumeBinding{
			Name: b.Name, Root: cur.Root, ReadOnly: b.ReadOnly || cur.ReadOnly, Default: b.Default,
		})
	}
	return out
}

// flagRunUnresumable marks a paused run terminal so a restored-but-unresumable
// run isn't a permanent "running" zombie (the F42 symptom). Best-effort.
//
// RFC DD §2: refuse LOUDLY, with a category. The terminal status and the reason
// on the run row are only half of it — they are visible to someone already
// looking at the runs list. The person who cares is the one RE-ATTACHING to a
// conversation, and they read the transcript, which said nothing at all about
// why the run stopped. So the refusal is also written there as a classified
// error event.
//
// Every refusal is business / not retryable, and that is not a shortcut: none
// of them resolves by waiting or by asking again. A missing agent, a withdrawn
// provider, a fan-out that cannot be reconstructed, a run with no pending turn
// — each needs an operator, and an agent or a supervisor loop that retries them
// is burning attempts against a wall.
func (s *Server) flagRunUnresumable(run store.Run, reason string) {
	meta := runStateMeta{
		RunID:         run.ID,
		AgentID:       run.AgentID,
		Agent:         run.Agent,
		UserID:        run.UserID,
		TenantID:      run.TenantID,
		ParentAgentID: run.ParentAgentID,
		ParentRunID:   run.ParentRunID,
		ParentContext: run.ParentContext,
	}
	s.appendResumeRefusal(run, reason)
	s.finishRunFailedReason(run.ID, "resume failed: "+reason, meta)
}

// appendResumeRefusal writes the refusal onto the run's own transcript, so it
// is there for whoever re-attaches. Best-effort by design: a store fault here
// must not stop the run being marked terminal, which is the part that keeps it
// out of the zombie state.
func (s *Server) appendResumeRefusal(run store.Run, reason string) {
	if s.store == nil || run.ID == "" {
		return
	}
	ev := providers.Event{
		Type:  providers.EventError,
		Error: "resume refused: " + reason,
		ErrorInfo: &errkind.Info{
			Category:  errkind.CategoryBusiness,
			Retryable: false,
			Description: "This run could not be restored and has been marked failed. " +
				"Re-running the resume will refuse again for the same reason; an operator has to act on it.",
		},
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	bg, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFn()
	if aerr := s.store.AppendEvent(bg, run.ID, string(providers.EventError), payload); aerr != nil {
		log.Printf("resume: could not record the refusal on run %s's transcript: %v", run.ID, aerr)
	}
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
			results[i] = builtin.ParallelSpawnResult{Index: i, Agent: c.result.Agent, Ok: c.result.Ok, Output: c.result.Output, Error: c.result.Error, State: c.result.State}
		case c != nil && c.runID != "":
			// Still running (parked) at snapshot → re-dispatched independently
			// by ResumePausedRuns. Await it + read its result, which has not
			// yet been through the parent's subagent_stop hooks.
			results[i] = s.resumedChildThroughStop(ctx, s.awaitChildResult(ctx, i, name, c.runID), c.runID)
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

// resumedChildThroughStop passes the result of a child that finished after the
// snapshot through the parent's subagent_stop hooks, as parallel_spawn would
// have: it records a child's result only once the hooks have seen it, so a
// ledger result is already through them and this one is not. A refusal
// replaces the result with the reason, as a live refusal does.
func (s *Server) resumedChildThroughStop(ctx context.Context, r builtin.ParallelSpawnResult, childRunID string) builtin.ParallelSpawnResult {
	status := string(store.RunCompleted)
	var runErr error
	if !r.Ok {
		status, runErr = string(store.RunFailed), errors.New(r.Error)
	}
	out, err := s.subagentStop(ctx, r.Agent, childRunID, status, r.Output, runErr)
	switch {
	case err == nil:
		r.Output = out
	case err != runErr:
		r = builtin.ParallelSpawnResult{Index: r.Index, Agent: r.Agent, Ok: false, Error: err.Error(), RunID: r.RunID}
	}
	return r
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

// providerForModel reports which provider serves `want` for this agent AS THE
// DEFINITION STANDS NOW, or false when the definition no longer offers it.
//
// It exists for RFC DD Gap 5: a resumed run should come back on the model it
// started with, but "the model it started with" is a value read off a row that
// was written before the pause — possibly before a definition was promoted, a
// tier re-pointed, or a provider withdrawn. Restoring it unchecked would let a
// snapshot reintroduce a model the operator has since removed.
//
// So the restored value is re-validated against the SAME resolution path that
// would pick a model today: the agent's own tier request, run through
// resolver.Cascade. Reusing Cascade rather than matching against config means
// this cannot drift from what Resolve would actually do — the same reason
// GET /v1/_routing uses it.
//
// Returns false for a PINNED agent (no tier ⇒ no cascade): a pin means the
// definition names the model outright, so the re-derived value already is the
// definition's answer and there is nothing to restore.
func (s *Server) providerForModel(ctx context.Context, def config.AgentDef, tenantID, userID, agentName, userTier string, restricted bool, want string) (string, bool) {
	if s.resolver == nil || def.Tier == "" || want == "" {
		return "", false
	}
	req := resolve.AgentRequest{
		Name:      agentName,
		Tier:      def.Tier,
		Effort:    def.Effort,
		Providers: def.Providers,
		Models:    convertConfigCandidates(def.Models, s.cfg().Models),
		UserTier:  s.userTierOverlay(userTier),
	}
	// Mirror resolveAgentDef: a restricted run only sees providers the tenant
	// can key, so a restored model on an un-keyable provider is refused here
	// rather than reaching the driver backstop as a run-time failure.
	if restricted {
		req.KeyableProviders = s.keyableProvidersFor(ctx, req, tenantID, agentName, userID)
	}
	for _, c := range s.resolver.Cascade(req) {
		if c.Model == want {
			return c.Provider, true
		}
	}
	return "", false
}

// toolResultText is the text of a synthesised tool_result message.
func toolResultText(m providers.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "tool_result" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}
