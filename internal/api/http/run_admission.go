package http

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"go.opentelemetry.io/otel/trace"
)

// runAdmission carries one RunOnce call through its phases: what the phases
// share, which were the locals of the single function they were split from.
// Each phase is a method that reads and sets these fields; RunOnce (server.go)
// calls them in their order. A runAdmission lives for one RunOnce call only.
type runAdmission struct {
	s   *Server
	ctx context.Context
	in  runner.RunInput
	cb  runner.RunCallbacks

	// defers are the cleanups a phase queued to run when RunOnce returns
	// (deferToEnd), never when the phase returns.
	defers []func()
	// handOff flips true once a detached run's goroutine takes over the
	// teardown (startDetachedLoop): from then on the defers here leave alone
	// what it releases — the session lock among them.
	handOff bool

	startingDraft  bool
	restarting     bool
	isContinuation bool

	effectiveAgentName string
	effectiveTenantID  string
	effectiveUserID    string

	priorMessages []providers.Message
	// The session lock a continuation holds; nil for a run in a new session.
	sessHold *sessionHold

	operatorKeyRestricted bool
	isolated              bool

	agentDef     config.AgentDef
	retention    string
	runRouting   *routingOverride
	runResources *resourceOverride
	runTuning    *tuningOverride

	sessionEvents []store.Event
	startCtx      context.Context
	providerID    string
	provider      providers.Provider
	model         string
	effort        string

	agentID   string
	detached  bool
	runParent context.Context
	runCtx    context.Context
	cancelFn  context.CancelCauseFunc

	registered bool
	runSlot    *concurrency.RunSlot
	release    func()
	queued     *queuedRow
	queueWait  time.Duration
	limitDec   limits.Decision

	allowedTools    []tools.Tool
	callerACL       *authz.Effective
	aclRemovedTools []string
	hostPolicy      tools.HostPolicyValue
	dispatcher      *tools.Dispatcher

	segments   []loop.PromptSegment
	promptProv runPromptProvenance
	coreBlocks []config.CoreBlock
	runCfg     runConfigRecord

	sessionID  string
	runID      string
	runSpan    trace.Span
	meta       runStateMeta
	untrackRun func()

	rid        tools.RunIdentityValue
	emit       func(providers.Event)
	callerGone func()
	steerQ     <-chan steer.Message
	onSteer    func(steer.Message)
	closeSteer func() bool
	deregSteer func()

	loopCtx          context.Context
	mergedContext    *config.Context
	liveInterruption *runInterruption
	heartbeat        func()
	gate             tools.PauseGate
	deregGate        func()
	runOpts          loop.RunOptions
}

// deferToEnd queues f to run when RunOnce returns, as a defer written in
// RunOnce itself would: whatever a phase queues outlives the phase.
func (a *runAdmission) deferToEnd(f func()) {
	a.defers = append(a.defers, f)
}

// runDeferred runs the queued cleanups, last queued first. Each is a real
// defer here, so one that panics does not skip the rest — as in a function
// holding all of them.
func (a *runAdmission) runDeferred() {
	for _, f := range a.defers {
		defer f()
	}
}

// admitRate counts the run against its caller's start rate, unless the caller
// already counted it (RunInput.RateAdmitted).
func (a *runAdmission) admitRate() error {
	s := a.s
	s.runPhase("rate")
	// The caller's rate, before anything else: every run a transport asks for
	// through here (each child of a batch included) is one start — counted
	// once, so not here when the caller already counted it.
	if !a.in.RateAdmitted {
		if err := s.admitPrincipalRate(a.ctx); err != nil {
			return err
		}
	}
	return nil
}

// validate refuses a malformed request before any lookup: the input fields,
// the segment types, a draft's or a restarted run's preconditions, an agent_id
// a draft holds, and the caller's parent_context.
func (a *runAdmission) validate() error {
	s := a.s
	s.runPhase("validate")
	// ---- Validation phase ----
	if a.in.UserID != "" && !connector.UserIDAllowed(principalSubject(a.ctx), a.in.UserID) {
		return fmt.Errorf("%w: %s", runner.ErrInvalidArgument, connector.UserIDRule)
	}
	if a.in.AgentID != "" && !validIdent(a.in.AgentID) {
		return fmt.Errorf("%w: agent_id must match [A-Za-z0-9_-]{1,128}", runner.ErrInvalidArgument)
	}
	if err := a.in.ToolChoice.Validate(); err != nil {
		return fmt.Errorf("%w: %v", runner.ErrInvalidArgument, err)
	}
	if err := a.in.OutputFormat.Validate(); err != nil {
		return fmt.Errorf("%w: %v", runner.ErrInvalidArgument, err)
	}
	if _, err := retentionmode.Parse(a.in.Retention); err != nil {
		return fmt.Errorf("%w: %v", runner.ErrInvalidArgument, err)
	}
	if msg, ok := connector.ValidateMaxWallSeconds(a.in.MaxWallSeconds); !ok {
		return fmt.Errorf("%w: %s", runner.ErrInvalidArgument, msg)
	}
	if !runner.ValidAdmission(a.in.Admission) {
		return fmt.Errorf("%w: admission must be %q or %q", runner.ErrInvalidArgument, runner.AdmissionWait, runner.AdmissionReject)
	}
	// The one seam gRPC Run / Continue, the spawn tools, a started draft and
	// the triggers share: a block type nobody knows is the caller's mistake,
	// reported as one, and never run as trusted text.
	if err := loop.ValidateSegmentTypes(a.in.Segments); err != nil {
		return fmt.Errorf("%w: %v", runner.ErrInvalidArgument, err)
	}
	a.in.Segments = loop.DemoteRuntimeKinds(a.in.Segments)
	a.startingDraft = a.in.ConfiguredRunID != ""
	if a.startingDraft && s.store == nil {
		return runner.ErrSessionRequired
	}
	// A run a restart found queued (RestartQueuedRuns): its row exists, and
	// its identity and keys are the row's.
	a.restarting = a.in.QueuedRunID != ""
	if a.restarting && (s.store == nil || a.startingDraft) {
		return fmt.Errorf("%w: a restarted queued run needs a store and is not a draft", runner.ErrInternal)
	}
	if !a.startingDraft && !a.restarting {
		// A draft's reserved id is not this run's to take (agentIDHeldByDraft).
		if held, err := s.agentIDHeldByDraft(a.ctx, a.in.AgentID); err != nil {
			return fmt.Errorf("%w: %v", runner.ErrInternal, err)
		} else if held {
			return fmt.Errorf("%w: "+agentIDInUseMsg, runner.ErrAgentIDInUse, a.in.AgentID)
		}
	}
	if a.startingDraft && a.in.SessionID != "" {
		return fmt.Errorf("%w: a configured run starts in its own session; session_id must be empty", runner.ErrInvalidArgument)
	}
	// Every RunOnce caller hands in a CALLER's parent_context (gRPC Run and
	// Continue, the spawn_run(s) tools, runs:batch, a started draft) — the walk
	// spawns its runs through the sub-run path, never here — so this is the one
	// seam those transports share for dropping the runtime-owned fields.
	a.in.ParentContext = connector.StripRuntimeParentContext(a.in.ParentContext)
	// ...and for bounding the caller's own fields. POST /v1/runs, a session
	// continuation, spawn_run(s) and a draft refuse an over-long field before
	// they get here; gRPC Run / Continue reach RunOnce with nothing else in
	// between, so without this their parent_context went unbounded into the
	// run row. The check reads only caller-owned fields, which the strip above
	// leaves alone, so the order of the two does not change the outcome.
	if msg, ok := connector.ValidateParentContext(a.in.ParentContext); !ok {
		return fmt.Errorf("%w: %s", runner.ErrInvalidArgument, msg)
	}
	return nil
}

// resolveIdentity fixes the run's tenant and user (the principal's, for a
// fresh run) and resolves a caller's idempotency_key against them.
func (a *runAdmission) resolveIdentity() error {
	s := a.s
	s.runPhase("identity")
	// ---- Session resolution (continuation only) ----
	a.isContinuation = a.in.SessionID != ""
	a.effectiveAgentName = a.in.Agent
	a.effectiveTenantID = a.in.TenantID
	a.effectiveUserID = a.in.UserID
	// RFC L: on an authenticated entry (gRPC interceptor stamps the
	// principal), the principal is authoritative over the wire fields
	// (Decision 5). Fresh runs only — a continuation inherits the
	// session's identity, set authoritatively when the session was
	// created. Un-authed programmatic callers (scheduler / webhook /
	// A2A) carry no principal and keep their Def-supplied identity.
	// A draft being started keeps the identity it was created with: the
	// caller hands RunOnce the row's own tenant/user, and the starter's
	// principal must not re-attribute the run (an admin starting a tenant's
	// draft does not make it the admin's run).
	// A restarted queued run is the same: it runs as the identity it was
	// accepted under, recorded on its row.
	if !a.isContinuation && !a.startingDraft && !a.restarting {
		a.effectiveTenantID, a.effectiveUserID = s.applyPrincipal(a.ctx, a.in.TenantID, a.in.UserID)
	}
	// A caller's idempotency_key is resolved HERE: after the identity it is
	// scoped to is known, and before admission, so a retry takes no slot,
	// spends no budget check and creates no session.
	if a.in.ClientIdempotencyKey != "" {
		if msg, ok := connector.ValidateIdempotencyKey(a.in.ClientIdempotencyKey); !ok {
			return fmt.Errorf("%w: %s", runner.ErrInvalidArgument, msg)
		}
		if a.isContinuation || a.startingDraft || a.in.IdempotencyKey != "" || s.store == nil {
			return fmt.Errorf("%w: idempotency_key applies to a fresh run on a runtime with a store", runner.ErrInvalidArgument)
		}
		a.in.IdempotencyKey = clientRunKey(a.effectiveTenantID, a.effectiveUserID, a.in.ClientIdempotencyKey)
		if dup, err := s.runHoldingClientKey(a.ctx, a.in.IdempotencyKey, a.effectiveTenantID, a.effectiveUserID, false); err != nil || dup != nil {
			if err != nil {
				return err
			}
			return dup
		}
	}
	return nil
}

// resolveSession, for a continuation, checks the session may be continued
// by this caller, takes its identity and team scope, and takes its lock.
func (a *runAdmission) resolveSession() error {
	s := a.s
	s.runPhase("session")
	// A fresh top-level run is outside every team, whatever its caller's ctx
	// carried: a team's own agents are reachable only from a walk of the team.
	if !a.isContinuation {
		a.ctx = store.WithTeamScope(a.ctx, store.TeamScope{})
	}
	if a.isContinuation {
		if s.store == nil {
			return runner.ErrSessionRequired
		}
		sess, err := s.store.GetSession(a.ctx, a.in.SessionID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return fmt.Errorf("%w: %s", runner.ErrSessionNotFound, a.in.SessionID)
			}
			return fmt.Errorf("%w: %v", runner.ErrInternal, err)
		}
		// RFC L: cross-principal session-ownership guard (see
		// sessionOwnershipOK). Opaque not-found on mismatch, no oracle.
		if !sessionOwnershipOK(a.ctx, sess) {
			return fmt.Errorf("%w: %s", runner.ErrSessionNotFound, a.in.SessionID)
		}
		// A token limited to some agents does not reach a session outside its
		// list, and is answered as for a session that does not exist —
		// whatever the request names (no agent, the session's, another one),
		// and before the session's team, whose refusal names the agent and
		// the team. Any other answer confirms the session exists, or lets a
		// caller enumerate names until it learns the session's agent.
		reachable, err := s.continuationReachesSession(a.ctx, sess)
		if err != nil {
			return fmt.Errorf("%w: %v", runner.ErrInternal, err)
		}
		if !reachable {
			return fmt.Errorf("%w: %s", runner.ErrSessionNotFound, a.in.SessionID)
		}
		// A continuation runs its session's agent (RFC ET P1 decision A18).
		// A caller naming another agent is refused rather than silently run
		// as the session's.
		if a.in.Agent != "" && a.in.Agent != sess.Agent {
			return s.sessionAgentMismatch(a.ctx, a.in.SessionID, sess.Agent, a.in.Agent)
		}
		// A no-trace session has no transcript to continue from. Refused here,
		// before any slot or lock, with its own answer; it used to reach the row
		// creation and come back as an internal error.
		if sess.Retention == retentionmode.None {
			return errNoTraceSession
		}
		a.effectiveAgentName = sess.Agent
		a.effectiveTenantID = sess.TenantID
		a.effectiveUserID = sess.UserID
		// As the session's user, so no more widely than its trigger's run.
		// Nor more widely than its sender may: another user does not get the
		// session user's keys by continuing (actingForAnother).
		a.in.CredentialsNoUserScope = a.in.CredentialsNoUserScope || s.sessionNoUserScope(a.ctx, a.in.SessionID) ||
			actingForAnother(a.ctx, sess.TenantID, sess.UserID)
		// Continued inside the team it began in, if any — and refused if that
		// team version has been retired (continuationTeamScopeCtx).
		scopedCtx, scopeErr := s.continuationTeamScopeCtx(a.ctx, sess)
		if errors.Is(scopeErr, errContinuationRefused) {
			// Both wrapped: the text is unchanged, and an HTTP caller still
			// answers with why the session cannot be continued.
			return fmt.Errorf("%w: %s: %w", runner.ErrUnknownAgent, sess.Agent, scopeErr)
		}
		if scopeErr != nil {
			return fmt.Errorf("%w: %v", runner.ErrInternal, scopeErr)
		}
		a.ctx = scopedCtx

		hold, err := s.takeSession(a.ctx, a.in.SessionID, a.in.SessionLockAcrossReplicas, a.in.QueuedRunID)
		if err != nil {
			return err
		}
		a.sessHold = hold
		// A detached continuation holds its session until it ends, not until
		// its caller leaves: its teardown releases it (see handOff).
		a.deferToEnd(func() {
			if !a.handOff {
				a.sessHold.release()
			}
		})
	}
	return nil
}

// stampOwner settles the run's operator-key and isolation bits and puts the
// ACL owner of a trigger run on ctx.
func (a *runAdmission) stampOwner() error {
	s := a.s
	s.runPhase("owner")
	// RFC AX: use the live principal when one is on ctx (gRPC/MCP/connector);
	// else fall back to the bit CAPTURED on the trigger def and supplied via
	// RunInput (scheduler/webhook/A2A have no principal at fire time). A
	// continuation recomputes from the presenting principal (current token is
	// authority), matching handleMessages.
	a.operatorKeyRestricted = s.operatorKeyRestrictedOrCaptured(a.ctx, a.in.OperatorKeyRestricted)
	// RFC BX P2b: same posture — live principal (gRPC/MCP/connector) wins, else the
	// bit CAPTURED on the trigger def and supplied via RunInput.Isolated. Confines
	// the fired run's data tools to its own user/agent scope (anti-bypass).
	a.isolated = s.isolatedOrCaptured(a.ctx, a.in.Isolated)
	if a.startingDraft || a.restarting {
		// Fail toward confinement: the bits captured when the draft was
		// created (or the run queued) still bind, whoever starts it.
		a.operatorKeyRestricted = a.operatorKeyRestricted || a.in.OperatorKeyRestricted
		a.isolated = a.isolated || a.in.Isolated
	}

	if a.effectiveAgentName == "" {
		return fmt.Errorf("%w: agent is required", runner.ErrInvalidArgument)
	}
	// RFC N: resolve the ENTRY agent at the run's authoritative tenant
	// (effectiveTenantID), NOT tenantFromCtx(ctx) — WithRunIdentity isn't
	// stamped until ~line 1500, so for non-HTTP-principal spawn surfaces
	// the ctx tenant is wrong here.
	// The agent is final here (a continuation has taken its session's, a
	// started draft its row's), so this covers all three.
	// (A continuation's agent was checked with its session, above.)
	var err error
	a.ctx, err = s.triggerOwnerCtx(a.ctx, a.in.ACLOwner, a.effectiveAgentName)
	if err != nil {
		return err
	}
	return nil
}

// stampConditions puts the facts a rule's condition reads about this run on
// ctx, before resolveAgent asks the first question that reads them.
func (a *runAdmission) stampConditions() {
	s := a.s
	s.runPhase("conditions")
	// What a condition reads about this run (RFC FY Part E), for the intake
	// question below and every question the run asks. Each is what the run
	// is: the request picks interactive, but an interactive run is then what
	// runs, so a rule on it describes the run truthfully. The tiers the
	// request names are not stamped: they are the caller's own say
	// (authz.RunFacts).
	a.ctx = authz.WithRunFacts(a.ctx, authz.RunFacts{Agent: a.effectiveAgentName, Depth: builtin.AgentDepth(a.ctx), Interactive: a.in.Interactive})
}

// resolveAgent admits the principal to the agent and to the overrides it
// asks for, resolves the entry agent at the run's own tenant, its retention,
// and the one effective definition the rest of the run uses.
func (a *runAdmission) resolveAgent() error {
	s := a.s
	s.runPhase("agent")
	if err := s.admitPrincipalAgent(a.ctx, a.effectiveAgentName); err != nil {
		return err
	}
	if err := s.admitPrincipalOverrides(a.ctx, overridesSetIn(&a.in)); err != nil {
		return err
	}
	var ok bool
	a.agentDef, ok = s.lookupAgent(a.ctx, a.effectiveTenantID, a.effectiveAgentName)
	if !ok {
		return &unknownAgentError{agent: a.effectiveAgentName}
	}
	if err := admitAgentOverrides(a.effectiveAgentName, a.agentDef, overridesSetIn(&a.in)); err != nil {
		return err
	}
	a.retention = s.resolveRetention(a.ctx, a.effectiveTenantID, a.effectiveAgentName, a.agentDef, a.in.Retention)
	if err := admitNoTraceStart(a.retention, a.in.Interactive, a.in.Review, a.in.SessionID); err != nil {
		return err
	}
	if a.retention == retentionmode.None && a.startingDraft {
		// A draft's row, and the prompt in it, were stored when it was made.
		return &noTraceRefusal{Status: 409, Msg: "a configured run cannot be started as a no-trace run: its draft is already stored"}
	}
	// resolveAgent only consults s.cfg().Agents; dynamic agents loaded
	// from the v0.8.15 fallback above are NOT in that map. Pass the
	// already-resolved AgentDef through resolveAgentDef instead (same
	// path the v0.8.5 sub-agent overlay uses).
	// RFC DC P1: this run's own routing, if the caller chose one. Applied to a
	// COPY of the definition, so the stored def and its content hash are
	// untouched (D2).
	a.runRouting = &routingOverride{Model: a.in.Model, Provider: a.in.Provider, Tier: a.in.Tier, Effort: a.in.Effort}
	a.runResources = &resourceOverride{
		MaxTokens: a.in.MaxTokens, MaxIterations: a.in.MaxIterations,
		UnboundedIterations: a.in.UnboundedIterations, MaxConcurrentChildren: a.in.MaxConcurrentChildren,
	}
	a.runTuning = &tuningOverride{
		RetryAttempts: a.in.RetryAttempts, MemoryInjectMaxTokens: a.in.MemoryInjectMaxTokens,
		MemoryIndexMaxBytes: a.in.MemoryIndexMaxBytes, InjectToolGuide: a.in.InjectToolGuide,
	}
	// ONE effective definition from here on. A second name beside agentDef has
	// already cost twice: it let this function build its loop options from the
	// un-overridden one unnoticed, AND made the fail-before probe for that gap
	// pass, because both names held the same value.
	var rerr error
	a.agentDef, rerr = s.effectiveDef(a.ctx, a.agentDef, runOverrides{
		Routing: a.runRouting, Resources: a.runResources, Tuning: a.runTuning,
		QueueRouting: a.in.Routing,
	})
	if rerr != nil {
		return rerr
	}
	return nil
}

// replaySession reads a continuation's transcript into the prior messages.
func (a *runAdmission) replaySession() error {
	s := a.s
	s.runPhase("replay")
	// ---- Transcript replay (continuation only) ----
	// Before the provider is chosen: an image in the history needs a provider
	// that can take it as much as one in the new input does (runStartCtx).
	if a.isContinuation {
		transcript, err := s.store.GetTranscript(a.ctx, a.in.SessionID)
		if err != nil {
			return fmt.Errorf("%w: %v", runner.ErrInternal, err)
		}
		a.sessionEvents = transcript
		a.priorMessages = replayTranscript(transcript)
	}
	return nil
}

// resolveProvider picks the provider and model the run starts on.
func (a *runAdmission) resolveProvider() error {
	s := a.s
	s.runPhase("provider")
	a.startCtx = runStartCtx(a.ctx, a.in.Segments, a.priorMessages)
	var err error
	a.providerID, a.model, a.effort, err = s.resolveAgentDef(a.startCtx, a.agentDef, a.effectiveTenantID, a.effectiveUserID, a.effectiveAgentName, a.in.UserTier, a.operatorKeyRestricted)
	if err != nil {
		return err // already wrapped with runner.Err* sentinel
	}
	a.provider, err = s.providers.Get(a.providerID)
	if err != nil {
		return fmt.Errorf("%w: %v", runner.ErrUnknownProvider, err)
	}
	return nil
}

// checkPause refuses a new run while the runtime pauses.
func (a *runAdmission) checkPause() error {
	s := a.s
	s.runPhase("pause")
	// Runtime-pause admission gate (RFC X / F41) — reject new runs while
	// pausing/paused. Covers the gRPC / webhook / A2A / scheduler surfaces
	// that drive runs through RunOnce. Checked before the concurrency slot.
	// A run a restart is queuing again is not new: it waits the pause out in
	// admitRun instead.
	if !a.restarting {
		if err := s.pausedRunErr(); err != nil {
			return err
		}
	}
	return nil
}

// admitBudget refuses a run whose tenant or user is over a hard token
// ceiling, before it may queue.
func (a *runAdmission) admitBudget() error {
	s := a.s
	s.runPhase("budget")
	// RFC AW token-budget admission, first before the run may queue: a run
	// whose (tenant, user) is over a hard ceiling is refused, not queued.
	if dec := s.limits.Check(a.effectiveTenantID, a.effectiveUserID); !dec.Allowed {
		return &tokenLimitError{info: dec.Refusal}
	}
	return nil
}

// assignAgentID takes the caller's agent_id or makes one.
func (a *runAdmission) assignAgentID() {
	s := a.s
	s.runPhase("agent_id")
	// ---- agent_id: caller-supplied or generated ----
	// Before admission: a run that has to wait is named in the queue by it.
	a.agentID = a.in.AgentID
	if a.agentID == "" {
		a.agentID = newAgentID()
	}
}

// buildRunContext makes the run's cancellable context (detached from the
// caller's cancellation for a detached run).
func (a *runAdmission) buildRunContext() {
	s := a.s
	s.runPhase("run_context")
	// ---- Run context ----
	// A detached run outlives this call (RunInput.Detached): its ctx keeps the
	// caller's values but not its cancellation, and startDetachedLoop runs the
	// loop in a goroutine that takes over the teardown — the same goroutine
	// POST /v1/runs uses for an interactive run. Until handOff, and always for
	// a run that is not detached, the defers here own the teardown. runParent
	// is ctx for a run that is not detached, so that path is unchanged.
	// Built before admission so a cancel reaches a run while it is queued.
	a.detached = a.in.Detached
	a.runParent = a.ctx
	if a.detached {
		a.runParent = context.WithoutCancel(a.ctx)
	}
	a.runCtx, a.cancelFn = context.WithCancelCause(a.runParent)
	a.deferToEnd(func() {
		if !a.handOff {
			a.cancelFn(nil)
		}
	})
}

// runRowErr maps a failure to create the run's row to what RunOnce
// returns; nil means "answered with an existing run" was not possible.
func (a *runAdmission) runRowErr(sessErr error) error {
	s := a.s
	var nf *store.ErrNotFound
	if errors.As(sessErr, &nf) {
		return fmt.Errorf("%w: %v", runner.ErrSessionNotFound, sessErr)
	}
	// RFC H Decision 10: a duplicate idempotency_key means an earlier
	// run already claimed this key. CRITICAL: return BEFORE the cancel
	// registry + agent loop so the run never double-executes. Wrap the
	// sentinel verbatim (no ErrInternal masking) so the webhook
	// receiver can errors.Is-detect it and resolve to the existing
	// run. (CreateSession ran before the failed CreateRun, leaving an
	// orphan session with no run — acceptable: it carries no events
	// and is never returned to a caller.)
	if errors.Is(sessErr, store.ErrDuplicateIdempotencyKey) {
		if a.in.ClientIdempotencyKey != "" {
			// Lost the race to another request with the same key, between
			// the lookup above and CreateRun: answer with the winner.
			if dup, err := s.runHoldingClientKey(a.ctx, a.in.IdempotencyKey, a.effectiveTenantID, a.effectiveUserID, true); err != nil || dup != nil {
				if err != nil {
					return err
				}
				return dup
			}
		}
		return sessErr
	}
	return fmt.Errorf("%w: %v", runner.ErrInternal, sessErr)
}

// takeSlot admits the run to a concurrency slot, queuing it (row and
// registration first) when none is free, and after a wait takes the session
// lock back and re-reads the session.
func (a *runAdmission) takeSlot() error {
	s := a.s
	s.runPhase("slot")
	// ---- Run slot (RFC EI P3) ----
	// Keyed on the run's own tenant: a trigger run (scheduler, webhook, A2A)
	// has no principal on ctx, so tenantFromCtx would count every tenant's
	// trigger runs under the empty tenant. A run that cannot start at once
	// is queued: its row (status queued) and its registration exist before
	// it waits, so it can be read, cancelled and re-attached meanwhile.
	var rowErr error
	a.registered = false
	clusterReleased := false // createRow let the cluster session lock go
	// RunConfig is nil on purpose: the run's configuration is resolved after it
	// waits, and StartQueuedRun writes it from the identity built at start.
	queuedIdentity := store.RunIdentity{AgentID: a.agentID, UserID: a.effectiveUserID, TenantID: a.effectiveTenantID, UserTier: a.in.UserTier, Model: a.model, ReplicaID: s.replicaID, ParentContext: a.in.ParentContext, IdempotencyKey: a.in.IdempotencyKey, DeliveryAltKey: a.in.DeliveryAltKey, Interactive: a.in.Interactive, OperatorKeyRestricted: a.operatorKeyRestricted, Isolated: a.isolated, CredentialsNoUserScope: a.in.CredentialsNoUserScope, Retention: a.retention, RunConfig: nil}
	acquireStart := time.Now()
	a.runSlot = &concurrency.RunSlot{}
	var err error
	a.release, a.queued, err = s.admitRun(a.runCtx, admitArgs{
		slot:         a.runSlot,
		tenantID:     a.effectiveTenantID,
		userID:       a.effectiveUserID,
		agentID:      a.agentID,
		reject:       a.in.Admission == runner.AdmissionReject,
		waitOutPause: a.restarting,
		createRow: func() (string, string, error) {
			// The cluster half of the session lock is let go while the run
			// waits (see sessionHold), and taken back once it is admitted —
			// whenever it was let go, which is whenever the row is made here
			// (a run that queues, or a restarted one placed into a free slot).
			a.sessHold.releaseCluster()
			clusterReleased = true
			if a.restarting {
				// Queued already, with its request stored.
				r, err := s.store.GetRun(a.ctx, a.in.QueuedRunID)
				if err != nil {
					rowErr = err
				}
				return r.SessionID, r.ID, err
			}
			if a.startingDraft {
				if err := s.store.QueueConfiguredRun(a.ctx, a.in.ConfiguredRunID, a.in.ConfiguredDraft); err != nil {
					rowErr = err
					return "", "", err
				}
				r, err := s.store.GetRun(a.ctx, a.in.ConfiguredRunID)
				if err != nil {
					rowErr = err
					return "", "", err
				}
				s.recordQueuedInput(a.ctx, r.ID, a.effectiveTenantID, a.retention, queuedRecordOf(a.in, a.effectiveAgentName, a.effectiveTenantID, a.effectiveUserID))
				return r.SessionID, r.ID, nil
			}
			sid, rid, err := s.openOrCreateSessionAndQueuedRun(a.ctx, a.in.SessionID, a.effectiveAgentName, a.effectiveTenantID, a.effectiveUserID, queuedIdentity)
			if err != nil {
				rowErr = err
				return sid, rid, err
			}
			s.recordQueuedInput(a.ctx, rid, a.effectiveTenantID, a.retention, queuedRecordOf(a.in, a.effectiveAgentName, a.effectiveTenantID, a.effectiveUserID))
			return sid, rid, nil
		},
		announce: func(sessionID, runID string) error {
			if err := s.cancelReg.Register(cancel.Entry{
				AgentID: a.agentID, RunID: runID, SessionID: sessionID,
				UserID: a.effectiveUserID, StartedAt: time.Now(),
			}, a.cancelFn); err != nil {
				return err
			}
			a.registered = true
			if a.cb.OnQueued != nil {
				a.cb.OnQueued()
			}
			if a.cb.OnRegistered != nil {
				a.cb.OnRegistered(a.agentID, runID, sessionID, "")
			}
			return nil
		},
		forward: a.cb.OnEvent,
	})
	a.queueWait = time.Since(acquireStart)
	if a.registered {
		a.deferToEnd(func() {
			if !a.handOff {
				s.cancelReg.Deregister(a.agentID)
			}
		})
	}
	if err != nil {
		switch {
		case rowErr != nil && a.startingDraft:
			return draftStartErr(rowErr, a.in.ConfiguredRunID)
		case rowErr != nil:
			return a.runRowErr(rowErr)
		case errors.Is(err, cancel.ErrInUse):
			return fmt.Errorf("%w: agent_id %q is already mapped to an active run", runner.ErrAgentIDInUse, a.agentID)
		}
		return admissionError(err)
	}
	if clusterReleased {
		// It let the cluster session lock go while it waited, and another
		// replica may have taken the session meanwhile: it starts only if it
		// can take the lock back, and then reads the session's history again.
		rowID := a.in.QueuedRunID
		if a.queued != nil {
			rowID = a.queued.runID
		}
		startErr := s.retakeSession(a.ctx, a.sessHold, a.in.SessionID, a.isContinuation && a.in.SessionLockAcrossReplicas, rowID)
		if startErr == nil && a.isContinuation && a.in.SessionLockAcrossReplicas {
			if transcript, terr := s.store.GetTranscript(a.ctx, a.in.SessionID); terr != nil {
				startErr = fmt.Errorf("%w: %v", runner.ErrInternal, terr)
			} else {
				a.sessionEvents = transcript
				a.priorMessages = replayTranscript(transcript)
			}
		}
		if startErr != nil {
			a.release()
			s.endQueuedRun(rowID, startErr)
			return startErr
		}
	}
	return nil
}

// admitBudgetAtStart asks the token budget again: a run that queued may have
// crossed its ceiling while it waited.
func (a *runAdmission) admitBudgetAtStart() error {
	s := a.s
	s.runPhase("budget_at_start")
	// RFC AW token-budget admission, again at start: a run that queued may
	// have crossed its ceiling while it waited. Checked BEFORE `defer
	// release()` so the manual release() here isn't double-fired. Soft
	// crossings (limitDec.Soft) don't block — they're emitted as warning
	// events once the run's emit exists.
	a.limitDec = s.limits.Check(a.effectiveTenantID, a.effectiveUserID)
	if !a.limitDec.Allowed {
		a.release()
		if a.queued != nil {
			s.endQueuedRun(a.queued.runID, fmt.Errorf("%s", a.limitDec.Refusal.Message))
		}
		return &tokenLimitError{info: a.limitDec.Refusal}
	}
	a.deferToEnd(a.release)
	return nil
}

// reresolveAfterQueue re-picks the provider for a run that queued.
func (a *runAdmission) reresolveAfterQueue() {
	s := a.s
	s.runPhase("reresolve")
	if a.queued != nil {
		if pid, p, m, e, ok := s.resolveAfterQueue(a.startCtx, a.agentDef, a.effectiveTenantID, a.effectiveUserID, a.effectiveAgentName, a.in.UserTier, a.operatorKeyRestricted); ok {
			a.providerID, a.provider, a.model, a.effort = pid, p, m, e
		}
	}
}
