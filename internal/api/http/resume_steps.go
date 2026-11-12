package http

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// resumeAttempt carries what the steps of one resumePausedRun call share,
// which were the locals of the single function they were split from. Each
// step is a method that reads the fields it needs into locals of their old
// names, runs its part, and stores what a later step reads; resumePausedRun
// (resume.go) calls them in their order. A resumeAttempt lives for one
// resumePausedRun call only.
type resumeAttempt struct {
	s   *Server
	run store.Run
	ctx context.Context

	// settled: the run was dispatched, or ended (refused, or cancelled as an
	// orphan). A pass that stops short of both — a transient read error —
	// gives the row back as paused, so a later pass tries it again instead of
	// leaving a running row with no loop for the stale-run sweeper.
	settled          bool
	ownerSlotHandled bool // another copy here settled the run and its slot
	handedOff        bool // the run's goroutine releases the hold from here on

	runCfg     runConfigRecord
	haveRunCfg bool
	resident   bool

	agentDef      config.AgentDef
	history       []store.Event
	runEvents     []store.Event
	priorMessages []providers.Message

	route      resumedRoute
	acl        *authz.Effective
	providerID string
	provider   providers.Provider
	model      string
	effort     string

	allowedTools    []tools.Tool
	aclRemovedTools []string
	dispatcher      *tools.Dispatcher
	promptProv      runPromptProvenance
	coreBlocks      []config.CoreBlock

	stateful       bool
	seed           statefulSeed
	pollLedger     []*pollChildLedger
	residents      []store.Run
	fanout         fanoutParkInfo
	isFanout       bool
	startParked    bool
	resumeHeld     *loop.HeldReview
	resumeChildren bool
	reviewArmed    bool
	segments       []loop.PromptSegment

	hold *sessionHold

	runCtx    context.Context
	cancelFn  context.CancelCauseFunc
	rootRunID string
	runSpan   trace.Span
	meta      runStateMeta

	rid        tools.RunIdentityValue
	rc         *residentChild
	emit       func(providers.Event)
	steerQ     <-chan steer.Message
	onSteer    func(steer.Message)
	closeSteer func() bool
	deregSteer func()

	slot             *concurrency.RunSlot
	loopCtx          context.Context
	liveInterruption *runInterruption
	releaseEphSet    func()
	gate             tools.PauseGate
	deregGate        func()

	runOpts      loop.RunOptions
	residentDone func(string, *builtin.ChildCappedError)
}

// moveOwnerSlot: settled — dispatched here, or ended — so the per-user slot
// the process that parked the run took for it goes back (RFC ES F1): in a
// cluster that slot is a row in user_quotas, which outlived the process, and
// the resumed run takes its own. Before the resumed run's loop starts, which
// records its own slot on the same row.
func (r *resumeAttempt) moveOwnerSlot() {
	if r.ownerSlotHandled {
		return
	}
	r.ownerSlotHandled = true
	r.s.releaseStoppedOwnersSlot(r.run)
}

// flag fails the run as not resumable, saying why; the run is settled.
func (r *resumeAttempt) flag(reason string) {
	r.settled = true
	r.s.flagRunUnresumable(r.run, reason)
}

// claim refuses a run with no identity or one a live loop still owns, then
// claims the row (paused to running) before anything writes to it.
func (r *resumeAttempt) claim() error {
	s, run, ctx := r.s, r.run, r.ctx
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
	// Claim the row before writing anything to it (RFC ES F1): a compare-and-
	// set from paused to running that exactly one pass wins. Passes race for
	// the same rows — a snapshot restore (which takes no lock) and another
	// replica's periodic pass, a boot pass and an operator's resume — and the
	// liveness check above cannot see a pass that has listed the row and not
	// yet dispatched it. The loser leaves the row as the winner writes it.
	claimed, cerr := s.store.ClaimPausedRun(ctx, run.ID)
	if cerr != nil {
		return fmt.Errorf("claim paused run: %w", cerr)
	}
	if !claimed {
		return errRunAlreadyLive
	}
	return nil
}

// endIfOrphaned ends a child whose parent has ended, as cancelled.
func (r *resumeAttempt) endIfOrphaned() error {
	s, run, ctx := r.s, r.run, r.ctx
	// A child whose parent has ended has nobody to read it or to cancel it:
	// resumed, it would run on, orphaned, to an end nobody collects. It is
	// ended cancelled instead, saying why.
	if reason, orphaned := s.orphanedBy(ctx, run); orphaned {
		r.settled = true
		s.cancelOrphanedRun(run, reason)
		// The walks it started in poll mode went with its instance as they
		// would have had it resumed, and are closed as its resume closes them.
		if events, err := s.store.GetTranscript(ctx, run.SessionID); err == nil {
			own := slices.DeleteFunc(events, func(e store.Event) bool { return e.RunID != run.ID })
			s.endInterruptedWalks(ctx, pollLedgerOf(own))
		} else {
			slog.Warn("resume: read orphaned run's transcript for its walks failed", "run_id", run.ID, "err", err)
		}
		return fmt.Errorf("not resumed: %s", reason)
	}
	return nil
}

// restoreRecord decodes the run's own configuration record and puts the
// run back in the team version it started under.
func (r *resumeAttempt) restoreRecord() {
	s, run, ctx := r.s, r.run, r.ctx
	// RFC AX: restore the operator-key restriction from the runs row so a resumed
	// run's credential-aware routing matches the original admission.
	//
	// RFC DC P1: resume on the run's OWN routing. Decoded before the resolve,
	// because the definition is not the authority here — the run is. A record
	// the definition no longer permits (a model dropped since) falls back to
	// the definition and says so, rather than failing a run that is otherwise
	// fine: refusing to resume is a heavier answer than resolving normally.
	runCfg, haveRunCfg := decodeRunConfig(run.RunConfig)
	// A resident child (Agent op=open) is rebuilt as one: parked between its
	// parent's sends — or finishing the turn it was in — and filed in this
	// instance's resident registry under its run id, so its parent (resumed
	// here with it) can send, poll and close it as before the pause.
	resident := runCfg.Spawn != nil && runCfg.Spawn.Resident && s.residentReg != nil && s.steerReg != nil
	// The team the run belongs to, from its own record: its agent — and every
	// agent it goes on to spawn — resolves in the team VERSION it started
	// under, whatever the team has been forked or promoted to since.
	ctx = runCfg.withTeamScope(ctx)
	r.ctx, r.runCfg, r.haveRunCfg, r.resident = ctx, runCfg, haveRunCfg, resident
}

// stampConditions puts on ctx what a condition reads about the run.
func (r *resumeAttempt) stampConditions() {
	s, run, ctx := r.s, r.run, r.ctx
	// What a condition reads about the run, as at its start (RFC FY Part E).
	// Before the first question that reads it — the model the run resolves to
	// is asked of the caller's `use` rules — as RunOnce stamps it before its
	// agent admission. Asked without it, a deny conditioned on the run's agent
	// read no agent, did not hold, and was skipped: the run resumed on a model
	// its caller's rules deny it.
	s.runPhase("resume.conditions")
	ctx = authz.WithRunFacts(ctx, authz.RunFacts{Agent: run.Agent, Depth: s.recordedSpawnDepth(ctx, run), Interactive: run.Interactive})
	r.ctx = ctx
}

// loadAgentDef finds the definition the run started on.
func (r *resumeAttempt) loadAgentDef() error {
	s, run, ctx, runCfg, flag := r.s, r.run, r.ctx, r.runCfg, r.flag
	// The definition the run started on, at its authoritative tenant. One that
	// no longer exists fails the run so it isn't a permanent "running" zombie;
	// it never falls back to the version active now.
	s.runPhase("resume.agent")
	agentDef, gone, derr := s.resumedAgentDef(ctx, run, runCfg.AgentVersion)
	if derr != nil {
		if gone {
			flag(derr.Error())
		}
		return derr
	}
	r.agentDef = agentDef
	return nil
}

// replayHistory reads the session's transcript and rebuilds the conversation
// the run continues.
func (r *resumeAttempt) replayHistory() error {
	s, run, ctx := r.s, r.run, r.ctx
	// Rebuild the conversation as RunOnce built it for this run — the
	// session's history up to the run's start — with the run's own turns on
	// top. Before the provider is chosen: an image in it needs a provider
	// that can take it.
	events, terr := s.store.GetTranscript(ctx, run.SessionID)
	if terr != nil {
		return fmt.Errorf("read transcript: %w", terr)
	}
	history, runEvents := sessionThroughRun(events, run.ID)
	priorMessages := replayTranscript(history)
	r.history, r.runEvents, r.priorMessages = history, runEvents, priorMessages
	return nil
}

// resolveRoute compiles the caller's rules and resolves the provider and model
// the run resumes on (resumeRoute).
func (r *resumeAttempt) resolveRoute() error {
	s, run, ctx, runCfg, agentDef, priorMessages, flag := r.s, r.run, r.ctx, r.runCfg, r.agentDef, r.priorMessages, r.flag
	route, refusal, rerr := s.resumeRoute(ctx, run, runCfg, agentDef, priorMessages)
	if rerr != nil {
		if refusal != "" {
			flag(refusal)
		}
		return rerr // with no refusal, not settled: the row goes back to paused for a later pass
	}
	ctx, acl, agentDef := route.ctx, route.acl, route.def
	providerID, provider, model, effort := route.providerID, route.provider, route.model, route.effort
	r.route, r.ctx, r.acl, r.agentDef = route, ctx, acl, agentDef
	r.providerID, r.provider, r.model, r.effort = providerID, provider, model, effort
	return nil
}

// defaultRunConfig fills the configuration of a run that recorded none.
func (r *resumeAttempt) defaultRunConfig() {
	run, runCfg, haveRunCfg, agentDef := r.run, r.runCfg, r.haveRunCfg, r.agentDef
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
		slog.Warn("resume: sub-run has no configuration record; its parent's host narrowing is unknown, so it resumes allowed no caller hosts", "run_id", run.ID)
		runCfg.Hosts = &runHostRecord{HasList: true}
	}
	r.runCfg = runCfg
}

// buildToolset builds the run's tools and dispatcher, in RunOnce's order.
func (r *resumeAttempt) buildToolset() {
	s, run, ctx, runCfg, agentDef, acl := r.s, r.run, r.ctx, r.runCfg, r.agentDef, r.acl
	// Tools + dispatcher, in RunOnce's order (runToolset), narrowed by the
	// caller's own allowed_hosts when the run recorded one (RFC DD Gap 2).
	// Restoring a narrowing can only SUBTRACT: the operator's static floor
	// still applies inside the tools, so this makes a resumed run a subset of
	// the original instead of handing it the bare floor back. The Recall grant
	// (RFC CT) comes from the recorded context; what the caller may not invoke
	// is withheld from the schema again, as at the run's start.
	//
	// The MODE is operator config re-read now, not run state: whether a caller's
	// list replaces the operator floor or intersects with it is the operator's
	// current call, exactly as it is for a fresh run with the same inputs. Only
	// the caller's lists (hosts, tools) and filter come from the run.
	s.runPhase("resume.tools")
	hostPolicy := runCfg.hostPolicy()
	callerAuthoritative := s.cfg().Env.HTTPCallerAuthoritative
	allowedTools, aclRemovedTools := s.runToolset(ctx, ctx, run.TenantID, agentDef, runCfg.callerTools(), runCfg.Context, acl, func(in []tools.Tool) []tools.Tool {
		if !hostPolicy.HasList && !callerAuthoritative {
			return in
		}
		// A list the run recorded as narrow-only stays a narrowing whatever
		// the mode is now: it was a locked caller's.
		return builtin.NarrowHosts(in, runCfg.callerHosts(), hostPolicy.WebSearchFilter, callerAuthoritative && !hostPolicy.NarrowOnly)
	})
	dispatcher := s.newDispatcher(allowedTools)
	r.allowedTools, r.aclRemovedTools, r.dispatcher = allowedTools, aclRemovedTools, dispatcher
}

// buildPrompt re-derives the system prompt and the memory injection.
func (r *resumeAttempt) buildPrompt() {
	s, run, ctx, agentDef, acl := r.s, r.run, r.ctx, r.agentDef, r.acl
	allowedTools, aclRemovedTools := r.allowedTools, r.aclRemovedTools
	// Re-derive the system prompt (skill bodies baked in) from the current
	// agent def — exactly as the continuation path does; the prompt is NOT
	// replayed from the transcript (it may have changed across instances), so
	// the one it runs on is recorded again below, with its provenance.
	s.runPhase("resume.prompt")
	agentDef, promptProv := s.resolveSkillBodiesForRun(ctx, run.TenantID, agentDef)
	// RFC BL P1: re-derive the memory injection + core-block set from the agent
	// def, exactly as the fresh/continue paths do (the injected prompt is not
	// replayed from the transcript). No initial input on a resume.
	var coreBlocks []config.CoreBlock
	agentDef, coreBlocks = s.applyMemoryInjection(ctx, agentDef, memInject{
		Tenant: run.TenantID, UserID: run.UserID, AgentName: run.Agent,
		Tools: allowedTools, ACL: acl, ACLRemovedTools: aclRemovedTools,
	})
	r.agentDef, r.promptProv, r.coreBlocks = agentDef, promptProv, coreBlocks
}

// restoreChildren reads a stateful run's seed, or the background children a
// run started, from its own events.
func (r *resumeAttempt) restoreChildren() {
	s, run, ctx, runCfg, provider := r.s, r.run, r.ctx, r.runCfg, r.provider
	history, runEvents := r.history, r.runEvents
	// RFC DH P2: a stateful run's history is Σ, not its messages — and neither
	// its first observation nor whether it can continue at all may be read off
	// a replayed conversation it never had. Both come from its stateful events.
	// The mode is resolved here the way Run will resolve it.
	stateful := loop.StatefulMode(runCfg.Context, provider.Capabilities().Local, run.Interactive)
	var seed statefulSeed
	if stateful {
		seed = resumedStatefulSeed(history, runEvents, runCfg.Context, provider.Capabilities().Local, run.Interactive)
	}
	// The poll-mode children the run started, as its ledger records them. A
	// stateful run has no background table, so it has none.
	var pollLedger []*pollChildLedger
	var residents []store.Run
	if !stateful {
		pollLedger = pollLedgerOf(runEvents)
		residents = s.residentChildrenOf(ctx, run)
		s.endInterruptedWalks(ctx, pollLedger)
	}
	r.stateful, r.seed, r.pollLedger, r.residents = stateful, seed, pollLedger, residents
}

// detectFanout finds a parked fan-out parent and refuses one it cannot
// reconcile.
func (r *resumeAttempt) detectFanout() error {
	runEvents, priorMessages, flag := r.runEvents, r.priorMessages, r.flag
	// RFC X Phase 3: detect a parked fan-out PARENT — a parallel_spawn that the
	// Phase-3 watcher parked mid-wg.Wait, so its transcript ends on a dangling
	// tool_use (no envelope tool_result) backed by a spawn ledger. Such a run
	// can't take the endsWithPendingTurn path (it ends on an assistant turn) but
	// IS resumable: the goroutine below reconciles the children into the
	// envelope.
	fanout, isFanout := detectFanoutParent(runEvents)
	if isFanout && !lastTurnIsSoleToolUse(priorMessages, fanout.toolUseID) {
		// A parked fan-out that shared its turn with other in-flight tools (a
		// mixed tool iteration) isn't auto-reconcilable: synthesizing only the
		// fan-out's result would leave a sibling tool_use unanswered (the
		// provider 400s). Documented Phase-3 limitation; flag for manual
		// re-attach rather than emit a malformed continuation.
		flag("parked fan-out parent shared its turn with other in-flight tools; re-attach to continue")
		return fmt.Errorf("not auto-resumable (mixed fan-out tool turn)")
	}
	r.fanout, r.isFanout = fanout, isFanout
	return nil
}

// decideStart decides how a run with nothing pending comes back: parked,
// held, waiting for its children, or refused.
func (r *resumeAttempt) decideStart() error {
	s, run, runEvents, priorMessages, flag := r.s, r.run, r.runEvents, r.priorMessages, r.flag
	stateful, seed, pollLedger, resident, isFanout := r.stateful, r.seed, r.pollLedger, r.resident, r.isFanout
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
	// A run that ended its turn and was waiting for its background children
	// when it paused waits for them again: its children were started, nobody
	// owes it a message, and it wakes when they end, as it would have.
	resumeChildren := false
	if !isFanout && idle {
		held := heldReviewFrom(runEvents)
		switch {
		case held != nil && !stateful && s.steerReg != nil:
			resumeHeld = held
		case len(pollLedger) > 0 && !run.Interactive && parkedForChildren(runEvents):
			resumeChildren = true
		case resident:
			startParked = true // waiting for its parent's next send
		case !run.Interactive || s.steerReg == nil:
			flag("run was idle awaiting input when paused; re-attach + steer to continue")
			return fmt.Errorf("not auto-resumable (no pending turn)")
		default:
			startParked = true
		}
	}
	r.startParked, r.resumeHeld, r.resumeChildren = startParked, resumeHeld, resumeChildren
	return nil
}

// armReview decides whether review stays armed.
func (r *resumeAttempt) armReview() {
	runCfg, resumeHeld := r.runCfg, r.resumeHeld
	// A run held by review arming stays held unless its record says review is
	// off. A record that says nothing is not a disarm: a team member's arming
	// lived on its walk, and a member held before its arming was recorded
	// would otherwise be approved here with no verdict. A hook's hold does not
	// depend on arming, so it is left as it was.
	reviewArmed := runCfg.Review != nil && *runCfg.Review
	if resumeHeld != nil && resumeHeld.HeldBy == "" && runCfg.Review == nil {
		reviewArmed = true
	}
	r.reviewArmed = reviewArmed
}

// systemSegments builds the system prompt segment.
func (r *resumeAttempt) systemSegments() {
	agentDef := r.agentDef
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
	r.segments = segments
}

// takeSession takes a top-level run's session.
func (r *resumeAttempt) takeSession() error {
	s, run, ctx := r.s, r.run, r.ctx
	// A top-level run takes its session as a continuation does: no other turn
	// on it while it runs. Busy (a paused sibling is not, sessionLiveForResume),
	// the resume is not settled: the row goes back to paused and is tried again
	// in this process (retryBusyResume). A sub-run takes none, as live.
	s.runPhase("resume.session")
	var hold *sessionHold
	if run.ParentRunID == "" {
		h, herr := s.takeSessionWith(ctx, run.SessionID, true, run.ID, s.sessionLiveForResume)
		if herr != nil {
			return s.retryIfBusy(run.ID, herr)
		}
		hold = h
	}
	r.hold = hold
	return nil
}

// restampRow ends the run's pause on its record and stamps its heartbeat and
// replica.
func (r *resumeAttempt) restampRow() {
	s, run, ctx, runCfg, haveRunCfg := r.s, r.run, r.ctx, r.runCfg, r.haveRunCfg
	// The row reads running already: the claim above moved it.
	// The run was paused until now — through the restart, or from the snapshot
	// on the instance it parked on — so its pause ends here, and a parent's
	// re-armed timeout_ms leaves the downtime out (runConfigRecord.Pauses).
	if haveRunCfg && runCfg.pauseOpen() {
		if err := recordRunPause(ctx, s.store, run.ID, time.Time{}, time.Now()); err != nil {
			slog.Warn("resume: end the run's pause on its record failed — a resumed timeout_ms on it counts the downtime", "run_id", run.ID, "err", err)
		}
	}
	// Stamp a fresh heartbeat NOW. The restored row carries an old started_at
	// and a NULL last_heartbeat_at; the stale-run sweeper's
	// "last_heartbeat_at IS NULL AND started_at < cutoff" branch would mark
	// the run failed in the window between flipping to running and the loop's
	// first OnHeartbeat. (The sweeper also skips pause_state='paused', which
	// covers the pre-resume window.)
	if err := s.store.UpdateHeartbeat(ctx, run.ID); err != nil {
		slog.Warn("resume: heartbeat stamp failed", "run_id", run.ID, "err", err)
	}
	// This replica now owns the run's live state (its steer queue, its cancel).
	// The row still names the replica it was created on, and a cross-replica
	// steer, verdict or cancel routes by the row: to a replica that is gone,
	// it answers "not in flight". Logged, not fatal — the run itself is fine,
	// and a verdict posted to this replica still reaches it.
	if s.replicaID != "" && run.ReplicaID != s.replicaID {
		if err := s.store.SetRunReplica(ctx, run.ID, s.replicaID); err != nil {
			slog.Warn("resume: replica stamp failed", "run_id", run.ID, "err", err)
		}
	}
}

// openRunContext opens the run's detached context, its span and its state
// record.
func (r *resumeAttempt) openRunContext() {
	s, run, ctx, agentDef := r.s, r.run, r.ctx, r.agentDef
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
		slog.Warn("resume: the run resumes as the root of its own tree", "run_id", run.ID, "err", rootErr)
	}
	runCtx, runSpan := s.startRunSpan(s.withResumedNeverAncestry(runCtx, run), agentDef, lcotel.RunStartAttrs{
		RunID:         run.ID,
		AgentID:       run.AgentID,
		AgentName:     run.Agent,
		UserID:        run.UserID,
		ParentAgentID: run.ParentAgentID,
		Tenant:        run.TenantID,
		SessionID:     run.SessionID,
		Trigger:       lcotel.TriggerResume, NoTrace: run.Retention == retentionmode.None,
	})
	// A resumed run records into a new trace; the run now points at it.
	s.recordRunTraceID(runCtx, run.ID)
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
	r.runCtx, r.cancelFn, r.rootRunID, r.runSpan, r.meta = runCtx, cancelFn, rootRunID, runSpan, meta
}

// registerCancel claims the run in the cancel registry.
func (r *resumeAttempt) registerCancel() error {
	s, run, cancelFn, runSpan := r.s, r.run, r.cancelFn, r.runSpan
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
			r.settled = true // a copy here holds it: not ours to give back
			r.ownerSlotHandled = true
			return errRunAlreadyLive
		}
		return fmt.Errorf("cancel registry: %w", regErr)
	}
	return nil
}

// goLive marks the run live: settled, tracked on its session, published, and
// its system prompt recorded.
func (r *resumeAttempt) goLive() {
	s, run, ctx, hold, meta, agentDef, promptProv := r.s, r.run, r.ctx, r.hold, r.meta, r.agentDef, r.promptProv
	r.settled = true // from here the run is live: its loop ends it
	// Known to this replica as one of its session's runs until it has ended
	// and let the session go, as RunOnce tracks its runs (takeSession waits
	// out a run of the session that is being cancelled or is finishing).
	if hold != nil {
		hold.onRelease = s.sessionRuns.track(run.SessionID, run.AgentID, run.ID)
	}
	s.publishRunState(meta, "running", "", "")
	// The system prompt it runs on now, re-derived above, recorded with its
	// provenance as at a run's start: it may differ from the one its start
	// recorded.
	s.emitSystemPromptEvent(ctx, run.ID, agentDef.SystemPrompt, "", promptProv)
}

// runIdentity builds the run's identity from its row.
func (r *resumeAttempt) runIdentity() {
	s, run, rootRunID, agentDef := r.s, r.run, r.rootRunID, r.agentDef
	// Hoisted above the recording emit so resumed-turn usage is attributed under
	// the run's true identity (see makeRecordingEmit) and WithRunIdentity reuses it.
	rid := tools.RunIdentityValue{
		UserID:        run.UserID,
		TenantID:      run.TenantID,
		AgentID:       run.AgentID,
		RootRunID:     rootRunID,     // the tree it was spawned into, not a new one
		SessionID:     run.SessionID, // restored from the run row, like every other durable field here
		UserTier:      run.UserTier,
		ModelTier:     usageModelTier(agentDef),
		InternalAgent: agentDef.Internal,
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
		// A trigger's run that may not use its user's own scope: the trigger
		// is gone, so the row is the only authority, as for the two above.
		CredentialsNoUserScope: run.CredentialsNoUserScope,
		UserScopeWithheld:      s.userScopeLatch(run.ID), // set if another user sends it a turn
		// Never "none" in practice — a no-trace run is not paused, so there is
		// none to resume — but the row is the authority, as for the two above.
		Retention: run.Retention,
	}
	r.rid = rid
}

// recordingEmit wires the run's recording emit and its steer entry, and says
// its start-time warnings again.
func (r *resumeAttempt) recordingEmit() {
	s, run, runCtx, cancelFn, meta, rid, resident := r.s, r.run, r.runCtx, r.cancelFn, r.meta, r.rid, r.resident
	agentDef, aclRemovedTools, allowedTools, runCfg := r.agentDef, r.aclRemovedTools, r.allowedTools, r.runCfg
	// Store-only emit (no live client to forward to) — the resumed turns
	// append to the same run's transcript so a re-attaching operator tails them.
	fwd := func(providers.Event) {}
	var rc *residentChild
	if resident {
		rc = &residentChild{runID: run.ID, agentID: run.AgentID, agentName: run.Agent, parentAgentID: run.ParentAgentID,
			tenantID: run.TenantID, userID: run.UserID, cancel: cancelFn,
			// The idle TTL its parent opened it with is not recorded; it
			// comes back with the operator's.
			idleTTL: s.residentChildIdleTTL(), maxTurn: s.residentMaxTurn()}
		fwd = rc.observe
	}
	emit := s.makeRecordingEmit(runCtx, run.ID, rid, run.SessionID, meta, fwd)
	// A child the Agent tool started takes a verdict and nothing else, live
	// (runSubRun registers it VerdictsOnly) and so after a resume: its parent
	// drives it, and a full entry here handed an operator its steer, retune and
	// compaction the moment it came back. The row carries no kind, so it is
	// derived as the remote gate derives it. That also makes a resumed resident
	// child verdicts-only, though its live entry was full — the side a gate may
	// err on. No sub-run row is interactive, so none is parked on a queue that
	// would refuse its next turn.
	steerQ, onSteer, closeSteer, deregSteer := s.makeSteerEntry(runCtx, steer.Entry{
		RunID: run.ID, AgentID: run.AgentID, SessionID: run.SessionID, UserID: run.UserID,
		VerdictsOnly: takesOnlyVerdicts(run) && !resident, // a resident child takes its parent's sends
	}, emit)
	closeSteer = s.sessionRuns.finishingWhenClosed(run.SessionID, run.ID, closeSteer)
	// What its start said once about a tool it holds and cannot use, said
	// again: the ACL is compiled anew and may withhold other tools now.
	emitRunStartWarnings(agentDef, aclRemovedTools, allowedTools, runCfg.Interruption, emit)
	r.rc, r.emit = rc, emit
	r.steerQ, r.onSteer, r.closeSteer, r.deregSteer = steerQ, onSteer, closeSteer, deregSteer
}

// loopContext builds the context the loop runs under.
func (r *resumeAttempt) loopContext() {
	s, run, runCtx, rid, runCfg, agentDef, rootRunID, emit := r.s, r.run, r.runCtx, r.rid, r.runCfg, r.agentDef, r.rootRunID, r.emit
	allowedTools, acl, aclRemovedTools, coreBlocks, dispatcher := r.allowedTools, r.acl, r.aclRemovedTools, r.coreBlocks, r.dispatcher
	// RFC AR: honor a tenant/user provider-key override on the resumed run too.
	// Its turn calls lend its global slot while they wait for a provider
	// lane, as a fresh run's do (concurrency.RunSlot); the slot is bound when
	// the run takes it, below.
	slot := &concurrency.RunSlot{}
	loopCtx := concurrency.WithRunSlot(runCtx, slot)
	loopCtx = providers.WithCredentialResolver(loopCtx, s.credResolver)
	// RFC AX: mirror the restored negative permission bit onto ctx for the
	// stage-2 driver backstop (drivers import providers, not auth/tools).
	loopCtx = providers.WithOperatorKeyAllowed(loopCtx, !run.OperatorKeyRestricted)
	loopCtx = tools.WithRunIdentity(loopCtx, rid)
	loopCtx = tools.WithAgentName(loopCtx, run.Agent)
	src := s.resumedPolicySource(loopCtx, run, runCfg, agentDef)
	src.Tools = allowedTools
	// The set narrows the volumes, keyspaces and skills in agentRunPolicy and
	// rides the policy to the run's children, as a live run's does.
	src.ACL, src.ACLRemovedTools = acl, aclRemovedTools
	// RFC BL P1: re-stamp the run's core blocks (Memory-tool enforcement +
	// sub-agent inherit) — lost across pause/snapshot/resume otherwise.
	src.CoreBlocks = coreBlocks
	runPolicy, liveInterruption := s.agentRunPolicy(loopCtx, agentDef, run.Agent, src)
	loopCtx = tools.WithRunPolicy(loopCtx, runPolicy)
	loopCtx = s.recordShadowNarrowing(loopCtx, run.ID, runPolicy)
	// RFC AH Phase 2b: re-attach a fresh run-scoped ephemeral set, REHYDRATED
	// from any ephemeral_volume_defs rows this run created before it was
	// paused/snapshotted (the sweeper skips paused runs, so the rows + on-disk
	// dirs survived). Without this a resumed paused run would lose in-memory
	// resolution of its own ephemeral volumes. Best-effort: a store fault
	// leaves an empty set (the agent can re-create) rather than failing resume.
	// Keyed by the tree's root, which is what a volume is created under.
	//
	// Every resumed run of one tree gets the SAME set, as a live tree does:
	// a volume one of them creates or makes read-only after the resume has to
	// hold for the others too.
	ephSet, releaseEphSet := s.treeSets.acquire(rootRunID)
	loopCtx = tools.WithEphemeralVolumes(loopCtx, s.loadEphemeralVolumes(loopCtx, rootRunID, ephSet))
	loopCtx = inbox.With(loopCtx, s.inboxService(), s.treeSets.inbox(rootRunID))
	loopCtx = s.withEphemeralRecord(loopCtx, rootRunID)
	loopCtx = tools.WithEventEmitter(loopCtx, emit)
	// RFC BB: per-agent web-search fallback list (empty = global search_priority).
	loopCtx = tools.WithSearchProviders(loopCtx, agentDef.SearchProviders)
	loopCtx = tools.WithParentRunID(loopCtx, run.ParentRunID) // "" for a top-level run: stays unset
	loopCtx = tools.WithRunID(loopCtx, run.ID)
	if added := runCfg.additions(); !added.Empty() {
		// What the run added before it paused, restored as it was — a
		// definition's with its source: already checked at its start, so
		// carried like an inheritance.
		loopCtx = hooks.WithAdditions(loopCtx, added)
	}
	loopCtx = s.withResumedRunHooks(loopCtx, run, agentDef, runCfg.PinnedHooks)
	// RFC EZ §1.2: a refused definition op of its tools is recorded, as a
	// fresh run's is.
	loopCtx = tools.WithACLRecorder(loopCtx, s.recordToolACLEvent)
	loopCtx = tools.WithDispatcher(loopCtx, dispatcher)
	r.slot, r.loopCtx, r.liveInterruption, r.releaseEphSet = slot, loopCtx, liveInterruption, releaseEphSet
}

// buildRunOptions arms the pause gate and builds the loop's options.
func (r *resumeAttempt) buildRunOptions() {
	s, run, ctx, loopCtx, runCfg, agentDef, runEvents, priorMessages := r.s, r.run, r.ctx, r.loopCtx, r.runCfg, r.agentDef, r.runEvents, r.priorMessages
	provider, providerID, model, effort := r.provider, r.providerID, r.model, r.effort
	allowedTools, dispatcher, segments, seed, emit, liveInterruption := r.allowedTools, r.dispatcher, r.segments, r.seed, r.emit, r.liveInterruption
	steerQ, onSteer, closeSteer := r.steerQ, r.onSteer, r.closeSteer
	stateful, resident, reviewArmed, startParked, resumeHeld := r.stateful, r.resident, r.reviewArmed, r.startParked, r.resumeHeld
	pollLedger, residents, resumeChildren := r.pollLedger, r.residents, r.resumeChildren
	heartbeat := s.makeHeartbeat(run.ID)
	fbPolicy, fbReResolve := s.fallbackForRun(run.TenantID, run.UserID, run.Agent, run.UserTier, run.OperatorKeyRestricted, runCfg.Routing)
	gate, deregGate := s.newPauseGate(run.ID, run.Retention == retentionmode.None)
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
		PriceCall:           s.priceCall,
		Tools:               allowedTools,
		Dispatcher:          dispatcher,
		Segments:            segments,
		PriorMessages:       priorMessages,
		Untrusted:           runUntrustedFromEvents(runEvents),
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
		CloseSteerIfEmpty:   closeSteer,
		IdleTimeout:         s.resumedIdleTimeout(run), // a restored chat waits for its operator as a live one does
		Effort:              effort,
		MarkStalled:         s.markStalledFn(providerID, model),
		ObserveCall:         s.callObserver(run.ID),
		MarkRateLimited:     s.markRateLimitedFn(run.UserTier),
		ClearStall:          s.clearStallFn(providerID, model),
		AgentName:           run.Agent,
		CodeBody:            agentDef.Code,
		RunTimeoutSeconds:   runCfg.RunTimeoutSeconds,
		MaxWallSeconds:      runCfg.MaxWallSeconds,
		OnWallLimit:         s.wallLimitCancel(run.AgentID, run.ID),
		RunClockCarry:       runCfg.clockCarryFrom(run.StartedAt), // the budget continues where the run paused
		Interactive:         run.Interactive || resident,
		InteractiveNow:      s.interactiveNowFn(run.ID, run.Interactive || resident),
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
		MoveForQueue:           s.queueMoveFn(agentDef, runCfg.Routing, run.ID, run.TenantID, run.UserID, run.Agent, run.UserTier, run.OperatorKeyRestricted),
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForAgent(agentDef, run.UserTier),
		// RFC DC P3: a RESTORED parked chat is the case this matters most for —
		// it is the one an operator comes back to and retunes.
		ReResolveOnOperatorTurn: s.reResolveOnOperatorTurnFn(run.ID, run.TenantID, run.UserID, run.Agent, run.UserTier, run.OperatorKeyRestricted, runCfg.Routing, liveInterruption),
		PauseGate:               gate,
		BudgetGate:              s.budgetGate(run.TenantID, run.UserID),

		// The RECORD's tool_choice, not resumedToolChoice: the baseline is what
		// the run holds, so a choice resume dropped as spent is not re-forced.
		ReReadShapeOnOperatorTurn: s.reReadShapeOnOperatorTurnFn(run.ID, runCfg.ToolChoice, runCfg.OutputFormat),

		// Its background children, rebuilt from its ledger, and its wait for
		// them when it paused waiting.
		RestoreBackground:      s.restoreBackgroundFn(run, pollLedger, residents),
		ResumeAwaitingChildren: resumeChildren,
	}
	// A refusal of the operator's own key: the provider leaves routing if its probe agrees.
	runOpts.ReportOperatorKeyRefusal = s.operatorKeyRefusalFn()
	// An interactive run's turn can be stopped, and it parks (RFC BH), as a
	// fresh one's.
	runOpts.ArmTurnCancel = s.armTurnCancelIf(run.Interactive, run.ID)
	r.loopCtx, r.gate, r.deregGate, r.runOpts = loopCtx, gate, deregGate, runOpts
}

// armResident files a resident child again and lets a top-level run's
// session go while it is parked.
func (r *resumeAttempt) armResident() {
	s, run, runCtx, loopCtx, rc, startParked, hold, stateful := r.s, r.run, r.runCtx, r.loopCtx, r.rc, r.startParked, r.hold, r.stateful
	residentDone := func(string, *builtin.ChildCappedError) {}
	if rc != nil {
		// Its parent's op=cancel stops its current turn, as it did live.
		r.runOpts.ArmTurnCancel = s.armTurnCancel(run.ID)
		if startParked {
			rc.parked(time.Now())
		} else {
			rc.beginTurn(time.Now())
		}
		s.residentReg.add(rc)
		// One of its parent's live children again, until it ends.
		live := s.liveChildren.Hold(run.ParentRunID, 1)[0]
		var once sync.Once
		residentDone = func(state string, capped *builtin.ChildCappedError) {
			once.Do(func() {
				rc.noteCancelled(context.Cause(runCtx))
				rc.markDone(state, capped)
				s.residentReg.remove(rc)
				live()
			})
		}
	}
	if hold != nil {
		// Its session's cluster lock is let go while it is parked, as a
		// continuation's is.
		s.releaseSessionWhileParked(&r.runOpts, &parkLock{hold: hold, sessionID: run.SessionID}, loopCtx, stateful,
			func(reason string) { s.cancelReg.CancelLocalRun(run.AgentID, run.ID, reason) })
	}
	r.residentDone = residentDone
}

// launch hands the run to its goroutine (runResumed).
func (r *resumeAttempt) launch() {
	s, run, meta, rootRunID, runCtx, loopCtx, runOpts, emit := r.s, r.run, r.meta, r.rootRunID, r.runCtx, r.loopCtx, r.runOpts, r.emit
	residentDone, runEvents, stateful, slot, hold, route := r.residentDone, r.runEvents, r.stateful, r.slot, r.hold, r.route
	deregSteer, deregGate, runSpan, cancelFn, releaseEphSet := r.deregSteer, r.deregGate, r.runSpan, r.cancelFn, r.releaseEphSet
	isFanout, fanout, moveOwnerSlot := r.isFanout, r.fanout, r.moveOwnerSlot
	// A resumed sub-run runs detached from its parent, whose cancel reaches it
	// through the registry and is not waited for: it holds its tree until its
	// goroutine has returned, so the root's end leaves the purge to it.
	releaseTree := func() {}
	if !meta.IsTopLevel {
		releaseTree = s.holdRunTree(rootRunID)
	}
	moveOwnerSlot()
	l := resumedLaunch{
		run: run, meta: meta, runCtx: runCtx, loopCtx: loopCtx, opts: runOpts, emit: emit,
		done: residentDone, runEvents: runEvents, stateful: stateful, slot: slot, hold: hold,
		route: route,
		teardown: func() {
			residentDone("failed", nil) // a no-op once the loop's end has filed it
			deregSteer()
			deregGate()
			s.cancelReg.Deregister(run.AgentID)
			runSpan.End()
			cancelFn(nil)
			hold.release() // and untracks the run (onRelease)
			releaseTree()
			releaseEphSet()
		},
	}
	if isFanout {
		l.fanout = &fanout
	}
	r.handedOff = true
	s.goDetached(func() { s.runResumed(l) })
}
