package http

// The phases of a run start after admission: from the toolset to the launch
// of the loop. RunOnce (server.go) calls them in order; runAdmission and the
// earlier phases are in run_admission.go.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// narrowTools builds the run's toolset: the agent's tools, the caller's
// narrowing, the Recall grant, the caller's ACL removing what it may not
// invoke, and the caller's host list (narrowing the operator's, never
// widening it).
func (a *runAdmission) narrowTools() {
	s := a.s
	s.runPhase("tools")
	// ---- Tool filtering + host narrowing ----
	// RFC N FIX 2-mcp: advertise MCP tools at the run's authoritative
	// tenant (effectiveTenantID), not the ctx tenant — WithRunIdentity
	// isn't stamped yet, so for non-HTTP-principal spawn surfaces the ctx
	// tenant is "" and the run would not see its OWN dynamic MCP tools.
	//
	// The run's own credentials go to the run-start MCP discovery: the run is
	// not stamped yet, and a server that authenticates tools/list with
	// ${run.credentials.<name>} would otherwise refuse the handshake and leave
	// the run with none of its tools (a sub-agent, discovering on its parent's
	// stamped ctx, never had this gap).
	discoverCtx := tools.WithMCPDiscoveryCredentials(a.ctx, a.in.UserBearer, a.in.UserCredentials)
	// RFC EY §3.7: the caller's ACL removes what it may not invoke, from
	// the schema the model sees and not only on call (owner decision D6).
	a.callerACL = runCallerACL(a.ctx)
	// RFC CT: context.recall auto-grants the Recall tool so enabling recall works
	// without also listing Recall in tools:.
	a.allowedTools, a.aclRemovedTools = s.runToolset(a.ctx, discoverCtx, a.effectiveTenantID, a.agentDef, a.in.Tools,
		config.MergeContext(a.agentDef.Context, a.in.Context), a.callerACL, func(in []tools.Tool) []tools.Tool {
			if a.in.AllowedHosts == nil && !s.cfg().Env.HTTPCallerAuthoritative {
				return in
			}
			var caller []string
			if a.in.AllowedHosts != nil {
				caller = *a.in.AllowedHosts
			}
			// A locked caller's list only narrows, in every mode.
			narrowOnly := a.in.AllowedHosts != nil && callerHostsNarrowOnly(a.ctx, a.agentDef)
			a.hostPolicy = tools.HostPolicyValue{
				AllowedHosts:    caller,
				HasList:         a.in.AllowedHosts != nil,
				WebSearchFilter: a.in.WebSearchFilter,
				NarrowOnly:      narrowOnly,
			}
			return builtin.NarrowHosts(in, caller, a.in.WebSearchFilter, s.cfg().Env.HTTPCallerAuthoritative && !narrowOnly)
		})
	a.dispatcher = s.newDispatcher(a.allowedTools)
}

// runToolset builds a run's toolset in the one order a fresh run and a
// resumed one share: the agent's tools narrowed by the caller's tools list,
// the Recall grant, the caller's ACL removing what it may not invoke, then
// narrowHosts — the caller's host list narrowing, never widening, the
// operator's. discoverCtx is the MCP discovery's (a fresh run's carries its
// credentials); the ACL is asked on ctx.
func (s *Server) runToolset(ctx, discoverCtx context.Context, tenantID string, def config.AgentDef, callerTools []string,
	recallCtx *config.Context, acl *authz.Effective, narrowHosts func([]tools.Tool) []tools.Tool) ([]tools.Tool, []string) {
	s.runPhase("tools.filter")
	allowed := filterTools(s.candidateTools(discoverCtx, tenantID, def.Tools), def.Tools, callerTools)
	s.runPhase("tools.recall")
	allowed = s.grantRecallTool(allowed, recallCtx)
	s.runPhase("tools.acl")
	allowed, removed := tools.NarrowToolsByACL(ctx, acl, allowed)
	s.runPhase("tools.hosts")
	return narrowHosts(allowed), removed
}

// emitRunStartWarnings says once, at a run's start, what the run holds and
// cannot use: a tool its definition grants inertly, a tool the caller's ACL
// withheld, an interruption setting on an agent without the tool.
func emitRunStartWarnings(def config.AgentDef, aclRemoved []string, allowed []tools.Tool, interruption *config.AgentInterruptionACL, emit func(providers.Event)) {
	emitInertCapabilityWarnings(def, emit)
	emitACLRemovedTools(aclRemoved, emit)
	emitInertInterruption(holdsInterruptionTool(allowed), interruption, emit)
}

// buildPrompt resolves the system prompt (skill bodies, memory injection)
// from the toolset narrowTools left, and prepends it to the segments.
func (a *runAdmission) buildPrompt() {
	s := a.s
	s.runPhase("prompt")
	// ---- Segments: prepend agent's system prompt ----
	a.segments = a.in.Segments
	// v0.8.22: rebuild SystemPrompt from per-run SkillDef bodies
	// when any of the agent's skills has a DB-active row. No-op
	// fast path when none do.
	// RFC N: resolve skills at the run's authoritative tenant
	// (effectiveTenantID), NOT tenantFromCtx(ctx) — WithRunIdentity isn't
	// stamped until below, so for non-HTTP-principal spawn surfaces the
	// ctx tenant is wrong here. Mirrors the lookupAgent fix above.
	a.agentDef, a.promptProv = s.resolveSkillBodiesForRun(a.ctx, a.effectiveTenantID, a.agentDef)
	// RFC BL P1: expand {{memory:...}} + core memory blocks into the system
	// prompt and resolve the effective core-block set (for the Memory tool's
	// write enforcement + inherit_core_blocks sub-agents). ctx has no parent
	// policy here (top-level run) → no inheritance.
	a.agentDef, a.coreBlocks = s.applyMemoryInjection(a.ctx, a.agentDef, memInject{
		Tenant: a.effectiveTenantID, UserID: a.effectiveUserID, AgentName: a.effectiveAgentName,
		InitialInput: firstUserText(a.in.Segments), Tools: a.allowedTools,
		ACL: a.callerACL, ACLRemovedTools: a.aclRemovedTools,
	})
	if a.agentDef.SystemPrompt != "" {
		a.segments = append([]loop.PromptSegment{{
			Role: "system",
			Content: []loop.PromptContentBlock{{
				Type: "trusted-text", Text: a.agentDef.SystemPrompt, Cacheable: true,
			}},
		}}, a.segments...)
	}
}

// mergeRunConfig merges the run's configuration once, for the loop and the
// durable run record alike.
func (a *runAdmission) mergeRunConfig() {
	s := a.s
	s.runPhase("run_config")
	// The run's own resolved configuration, merged ONCE here — after agentDef
	// has taken its final form — and used both by the loop below and by the
	// durable run_config record, so the two cannot drift. A resumed run comes
	// back on these values rather than on whatever the definition resolves to
	// by then.
	a.runCfg = runConfigRecord{
		Sampling:          config.MergeSampling(a.agentDef.Sampling, a.in.Sampling), // per-run wins per field
		EphemeralReadOnly: a.agentDef.EphemeralVolumes == config.EphemeralVolumesReadOnly,
		ToolChoice:        config.MergeToolChoice(a.agentDef.ToolChoice, a.in.ToolChoice), // per-run replaces whole
		OutputFormat:      config.MergeOutputFormat(a.agentDef.OutputFormat, a.in.OutputFormat),
		Compaction:        config.MergeCompaction(a.agentDef.Compaction, a.in.Compaction), // per-run wins per field
		Context:           config.MergeContext(a.agentDef.Context, a.in.Context),          // per-run wins per field
		MaxContextTokens:  config.MergeMaxContextTokens(a.agentDef.MaxContextTokens, a.in.MaxContextTokens),
		RunTimeoutSeconds: pickRunTimeout(a.in.RunTimeoutSeconds, a.agentDef.RunTimeoutSeconds),
		MaxWallSeconds:    a.in.MaxWallSeconds,
		Routing:           persistedRouting(a.runRouting),
		QueueRouting:      a.in.Routing.Clone(),
		Resources:         persistedResources(a.runResources),
		Tuning:            persistedTuning(a.runTuning),
		Hosts:             hostRecordOf(a.hostPolicy),
		Tools:             toolsRecordOf(a.in.Tools),       // the caller's tool narrowing, for a resume
		ACLOrigin:         aclOriginOf(a.ctx, a.callerACL), // whose rules a resume narrows it by again
		Review:            reviewRecord(a.in.Review),
		ReviewTTLSeconds:  positiveOrZero(a.in.ReviewTTLSeconds),
		Hooks:             additionsRecord(hooks.Additions{Hooks: a.in.Hooks, ToolHooks: a.in.ToolHooks}),
		Interruption:      a.in.Interruption,          // the run's own block, so a resume re-narrows from it
		AgentVersion:      agentVersionOf(a.agentDef), // the version it resumes on
		TeamScope:         teamScopeRecordOf(a.ctx),   // a continued team session's; nil for a fresh run
		IterationLimit:    s.startIterationLimit(a.agentDef, a.provider, a.in.Interactive),
	}
}

// createRows writes the run's row (and session): a queued or restarted run's
// row moves to running, a draft starts, otherwise a new run is created.
func (a *runAdmission) createRows() error {
	s := a.s
	s.runPhase("rows")
	// ---- Session+run creation ----
	identity := store.RunIdentity{AgentID: a.agentID, UserID: a.effectiveUserID, TenantID: a.effectiveTenantID, UserTier: a.in.UserTier, Model: a.model, ReplicaID: s.replicaID, ParentContext: a.in.ParentContext, IdempotencyKey: a.in.IdempotencyKey, DeliveryAltKey: a.in.DeliveryAltKey, Interactive: a.in.Interactive, OperatorKeyRestricted: a.operatorKeyRestricted, Isolated: a.isolated, CredentialsNoUserScope: a.in.CredentialsNoUserScope, Retention: a.retention, RunConfig: a.runCfg.marshal(),
		// What the admission counted, for the resume of a run whose replica
		// stops (RFC ES C4). Known only now: a queued row is written before it.
		UserSlot: a.runSlot.SharedUserCount()}
	switch {
	case a.queued != nil || a.restarting:
		// It waited, or was waiting when loomcycle restarted: its row exists,
		// queued, and only moves to running now. A cancel that ended it
		// meanwhile wins.
		qRunID := a.in.QueuedRunID
		if a.queued != nil {
			qRunID = a.queued.runID
		} else if r, err := s.store.GetRun(a.ctx, qRunID); err == nil {
			// Restarted into a free slot: its transcript says queued, so it
			// says admitted too, counting the wait from when it first queued.
			s.emitAdmissionEvent(a.ctx, qRunID, a.cb.OnEvent, providers.Event{
				Type:      providers.EventAdmitted,
				Admission: &providers.AdmissionInfo{WaitMs: time.Since(r.StartedAt).Milliseconds()},
			})
		}
		started, err := s.store.StartQueuedRun(a.ctx, qRunID, identity)
		if err != nil {
			if errors.Is(err, store.ErrRunNotQueued) {
				return fmt.Errorf("%w: run %s was cancelled while it was queued", runner.ErrInternal, qRunID)
			}
			s.endQueuedRun(qRunID, err)
			return fmt.Errorf("%w: %v", runner.ErrInternal, err)
		}
		a.sessionID, a.runID = started.SessionID, started.ID
		// As for a draft below: the row the start left decides, never wider.
		a.in.CredentialsNoUserScope = a.in.CredentialsNoUserScope || started.CredentialsNoUserScope
	case a.startingDraft:
		// RFC DI D5: after admission, so a refusal above leaves the draft as it
		// was; before registration, so every failure path below finds a
		// running row that FinishRun can end.
		started, err := s.store.StartConfiguredRun(a.ctx, a.in.ConfiguredRunID, identity, a.in.ConfiguredDraft)
		if err != nil {
			return draftStartErr(err, a.in.ConfiguredRunID)
		}
		a.sessionID, a.runID = started.SessionID, started.ID
		// The row as the start left it decides, not the read the input was
		// built from: an edit by another user that landed in between marked the
		// row before writing the draft this start just matched. The bit only
		// ever narrows, so a later read can be no wider than the start.
		a.in.CredentialsNoUserScope = a.in.CredentialsNoUserScope || started.CredentialsNoUserScope
	default:
		var sessErr error
		a.sessionID, a.runID, sessErr = s.openOrCreateSessionAndRun(a.ctx, a.in.SessionID, a.effectiveAgentName, a.effectiveTenantID, a.effectiveUserID, identity)
		if sessErr != nil {
			return a.runRowErr(sessErr)
		}
	}
	return nil
}

// wireRun opens the run span, registers the run for cancel, publishes it as
// running, persists its input and system prompt, and asks the caller's Admit.
func (a *runAdmission) wireRun() error {
	s := a.s
	s.runPhase("wiring")
	// v0.10.0 OTEL: top-level loomcycle.run span covers the entire
	// run. Loop iterations + provider calls + tool dispatch nest
	// under it via context propagation. Span name + attribute set
	// stable across the run-creation sites (RunOnce, runSubAgent).
	a.runCtx, a.runSpan = s.startRunSpan(a.runCtx, a.agentDef, lcotel.RunStartAttrs{
		RunID:     a.runID,
		AgentID:   a.agentID,
		AgentName: a.effectiveAgentName,
		UserID:    a.effectiveUserID,
		Tenant:    a.effectiveTenantID,
		SessionID: a.sessionID,
		NoTrace:   a.retention == retentionmode.None,
	})
	s.recordRunTraceID(a.runCtx, a.runID)
	a.deferToEnd(func() {
		if !a.handOff {
			a.runSpan.End()
		}
	})
	// v0.10.1: surface the semaphore queue wait on the run span. 0
	// means immediate acquire; a sustained non-zero distribution per
	// user_id is the operator's signal that fairness is engaging.
	lcotel.RecordQueueWait(a.runSpan, a.queueWait)
	// A run that queued was registered before it waited (admitRun's announce).
	var regErr error
	if !a.registered {
		regErr = s.cancelReg.Register(cancel.Entry{
			AgentID:   a.agentID,
			RunID:     a.runID,
			SessionID: a.sessionID,
			UserID:    a.effectiveUserID,
			StartedAt: time.Now(),
		}, a.cancelFn)
	}
	// v0.9.x n8n RFC Phase 0: identity bundle for runstate.Bus
	// publishes. Constructed once; passed to every finishRun* below
	// AND used right now for the "running" transition.
	a.meta = runStateMeta{
		RunID:         a.runID,
		AgentID:       a.agentID,
		Agent:         a.effectiveAgentName,
		UserID:        a.effectiveUserID,
		TenantID:      a.effectiveTenantID,
		ParentContext: a.in.ParentContext,
		// RFC AH Phase 2b: this is a TOP-LEVEL run — it owns the ephemeral
		// volume tree purge at completion. RootRunID is its own id.
		IsTopLevel: true,
		RootRunID:  a.runID,
	}
	// Stash the run span on the meta so finishRun* can close it with
	// final attrs (usage totals + stop_reason + error status).
	a.meta.otelSpan = a.runSpan
	if errors.Is(regErr, cancel.ErrInUse) {
		s.finishRunFailedReason(a.runID, "agent_id collision; run never started", a.meta)
		return fmt.Errorf("%w: agent_id %q is already mapped to an active run", runner.ErrAgentIDInUse, a.agentID)
	}
	if regErr != nil {
		s.finishRunFailedReason(a.runID, "registry register failed: "+regErr.Error(), a.meta)
		return fmt.Errorf("%w: %v", runner.ErrInternal, regErr)
	}
	if !a.registered {
		a.deferToEnd(func() {
			if !a.handOff {
				s.cancelReg.Deregister(a.agentID)
			}
		})
	}
	// Known to this replica as one of its session's runs until it has ended
	// and let the session go: a continuation that finds the session busy
	// waits out its end only while it is being cancelled or finishing with
	// its input closed (takeSession).
	a.untrackRun = s.sessionRuns.track(a.sessionID, a.agentID, a.runID)
	if a.sessHold != nil {
		a.sessHold.onRelease = a.untrackRun // after the session is let go
	} else {
		a.deferToEnd(func() {
			if !a.handOff {
				a.untrackRun()
			}
		})
	}
	s.publishRunState(a.meta, "running", "", "")

	// ---- Persist input segments ----
	s.persistUserInput(a.runParent, a.runID, a.sessionID, a.effectiveTenantID, a.effectiveUserID, a.in.Segments)
	// v0.9.x: persist the resolved system prompt + provenance so the
	// transcript carries WHAT the agent received, not just WHAT the
	// model emitted. The companion Web UI rendering surfaces this as
	// the first card on /ui run views. agent_def_id is empty here —
	// RunInput doesn't pin a def, so the field stays unset (operators
	// inspecting can look up the run row's AgentDefID column directly).
	s.emitSystemPromptEvent(a.runParent, a.runID, a.agentDef.SystemPrompt, "", a.promptProv)

	// ---- Caller registration callback ----
	// A run that queued told its caller before it waited, and so did a
	// restarted run that had to be ready to wait out a pause (admitRun
	// announced both).
	if a.cb.OnRegistered != nil && !a.registered {
		a.cb.OnRegistered(a.agentID, a.runID, a.sessionID, "")
	}
	// The caller's last word before the loop — a run that queued is asked
	// now that it leaves the queue. A refusal fails the run before its first
	// model call.
	if a.cb.Admit != nil {
		if aerr := a.cb.Admit(a.runID); aerr != nil {
			s.finishRunFailedReason(a.runID, "not admitted by its caller: "+aerr.Error(), a.meta)
			return fmt.Errorf("run not admitted by its caller: %w", aerr)
		}
	}
	return nil
}

// buildEmit builds the run's recording emit, emits the start-time warnings,
// and makes the steering queue.
func (a *runAdmission) buildEmit() {
	s := a.s
	s.runPhase("emit")
	// ---- Build emit chain (record + forward to caller) ----
	// Hoisted so the recording emit attributes usage under the run's true
	// identity (makeRecordingEmit is built before WithRunIdentity below) and
	// both consumers stamp the SAME value.
	a.rid = tools.RunIdentityValue{
		UserID:                a.effectiveUserID,
		TenantID:              a.effectiveTenantID, // RFC L: authoritative tenant (memory tenancy key)
		AgentID:               a.agentID,
		RootRunID:             a.runID,     // RFC AH Phase 2b: top-level run roots its own spawn tree
		SessionID:             a.sessionID, // the chat a run-time write records itself against
		UserTier:              a.in.UserTier,
		ModelTier:             usageModelTier(a.agentDef),
		InternalAgent:         a.agentDef.Internal,
		UserBearer:            a.in.UserBearer,      // v0.8.x: per-run MCP bearer
		UserCredentials:       a.in.UserCredentials, // v1.x RFC F: per-tool named credentials
		ParentContext:         a.in.ParentContext,   // v0.12.x: opaque tracking lineage, inherited by sub-agents
		OperatorKeyRestricted: a.operatorKeyRestricted,
		Isolated:              a.isolated, // RFC BX P2b: confine data tools to own scope
		Retention:             a.retention,
		// A trigger's: $cred: resolves without the user's own scope.
		CredentialsNoUserScope: a.in.CredentialsNoUserScope,
		UserScopeWithheld:      s.userScopeLatch(a.runID), // set if another user sends it a turn
	}
	fwd := func(ev providers.Event) {
		if a.cb.OnEvent != nil {
			a.cb.OnEvent(ev)
		}
	}
	// Forwarding to the caller stops once it has gone (callerGone), and
	// recording goes on: a detached run's later reader re-attaches from the
	// store. Every run's caller goes when RunOnce returns, and a background
	// child winding down after its parent's end still emits through this emit
	// — into a transport whose handler may have returned.
	fwd, a.callerGone = forwardWhileAttached(fwd)
	a.deferToEnd(a.callerGone)
	a.emit = s.makeRecordingEmit(a.runParent, a.runID, a.rid, a.sessionID, a.meta, fwd)
	// RFC AW: emit any soft budget crossings the admission check found, so the
	// warning lands at run start in the transcript/stream (dedup'd once-per-run
	// by makeRecordingEmit's seenLimit set).
	for _, info := range a.limitDec.Soft {
		a.emit(providers.Event{Type: providers.EventLimit, Limit: &info})
	}

	// A tool the agent holds and cannot use, said once here rather than
	// discovered by the model being refused mid-task.
	emitRunStartWarnings(a.agentDef, a.aclRemovedTools, a.allowedTools, a.in.Interruption, a.emit)

	// PR 2: operator steering queue for this run (in-flight input injection).
	a.steerQ, a.onSteer, a.closeSteer, a.deregSteer = s.makeSteer(a.runParent, a.runID, a.agentID, a.sessionID, a.effectiveUserID, a.emit)
	a.closeSteer = s.sessionRuns.finishingWhenClosed(a.sessionID, a.runID, a.closeSteer)
	a.deferToEnd(func() {
		if !a.handOff {
			a.deregSteer()
		}
	})
}

// buildLoopCtx stamps the run's identity, policy and per-run services on the
// loop context, and makes the cooperative pause gate.
func (a *runAdmission) buildLoopCtx() {
	s := a.s
	s.runPhase("loop_ctx")
	// The run's own turn calls lend its global slot while they wait for a
	// provider lane (concurrency.RunSlot).
	a.loopCtx = concurrency.WithRunSlot(a.runCtx, a.runSlot)
	// RFC AR: honor a tenant/user provider-key override (ANTHROPIC_API_KEY,
	// BRAVE_API_KEY, …) for this run. Sub-agents inherit it via subRunCtx←ctx.
	a.loopCtx = providers.WithCredentialResolver(a.loopCtx, s.credResolver)
	// RFC AX: mirror the negative permission bit onto ctx for the stage-2
	// driver backstop (drivers import providers, not auth/tools).
	a.loopCtx = providers.WithOperatorKeyAllowed(a.loopCtx, !a.operatorKeyRestricted)
	a.loopCtx = tools.WithRunIdentity(a.loopCtx, a.rid)
	// The Memory tool resolves `agent` scope to this name.
	a.loopCtx = tools.WithAgentName(a.loopCtx, a.effectiveAgentName)
	// The run's whole policy, after its identity: its memory, SQL and history
	// defaults resolve from the user, tenant and isolation on ctx.
	a.mergedContext = a.runCfg.Context // per-run > per-agent (RFC CR); resolved once, above
	var runPolicy tools.RunPolicy
	runPolicy, a.liveInterruption = s.agentRunPolicy(a.loopCtx, a.agentDef, a.effectiveAgentName, runPolicySource{
		Tools:           a.allowedTools,
		Hosts:           a.hostPolicy,
		ACL:             a.callerACL,
		ACLRemovedTools: a.aclRemovedTools,
		// RFC AH: the run's filesystem-volume bindings. Unbound agents get an
		// inactive policy (every file tool refuses); sub-agents inherit and
		// narrow this.
		Volumes:    s.volumePolicyForAgent(a.loopCtx, a.agentDef),
		CoreBlocks: a.coreBlocks,
		Crypto:     cryptoPolicyForAgent(a.agentDef),
		// Resolved compaction and layered-context settings flow down the spawn
		// tree: a sub-agent inherits the parent's effective policy (its def
		// fills any gaps the parent left unset), compaction overridable
		// per-spawn by the Agent tool.
		Compaction: a.runCfg.Compaction,
		Context:    a.mergedContext,
		// RFC DC P2: the run's own fan-out width. It is otherwise resolved from
		// the calling agent's NAME at spawn time, which a per-run value cannot
		// reach.
		FanoutCap: a.agentDef.MaxConcurrentChildren,
		// RFC DC P5: offer this run's overrides to its children. Only a child of
		// the SAME definition will take them (RunOverridesValue.SameDefinitionAs).
		// A top-level run pins no def_id (RunInput does not carry one), so
		// identity here is the agent NAME.
		Overrides:    tools.RunOverridesValue{Record: a.runCfg.marshal(), AgentName: a.effectiveAgentName, RunID: a.runID},
		Interruption: a.in.Interruption,
	})
	a.loopCtx = tools.WithRunPolicy(a.loopCtx, runPolicy)
	// Shadow mode narrowed nothing above; record what enforce would have.
	a.loopCtx = s.recordShadowNarrowing(a.loopCtx, a.runID, runPolicy)
	// RFC AH Phase 2b: a FRESH run-scoped ephemeral volume set, attached ONCE
	// at this top-level start. Sub-agents inherit this SAME pointer via the
	// ctx chain (runSubAgent must not re-attach it) — never shared across
	// top-level runs (the load-bearing isolation property).
	// Registered under the tree's root, so a run of this tree that resume
	// starts again joins this set and not a copy of it. Dropped with the
	// tree's purge.
	treeSet, _ := s.treeSets.acquire(a.runID)
	a.loopCtx = tools.WithEphemeralVolumes(a.loopCtx, treeSet)
	// RFC FF: the tree's inbox, where files its tools return are kept.
	a.loopCtx = inbox.With(a.loopCtx, s.inboxService(), s.treeSets.inbox(a.runID))
	a.loopCtx = s.withEphemeralRecord(a.loopCtx, a.runID)
	a.loopCtx = tools.WithEventEmitter(a.loopCtx, a.emit)
	// RFC BB: per-agent web-search fallback list (empty = global search_priority).
	a.loopCtx = tools.WithSearchProviders(a.loopCtx, a.agentDef.SearchProviders)
	a.loopCtx = tools.WithRunID(a.loopCtx, a.runID)
	// RFC EZ §1.2: where a definition op this run's tools ask is refused is
	// recorded, as an off-run one is. Sub-agents inherit it with the ctx.
	a.loopCtx = tools.WithACLRecorder(a.loopCtx, s.recordToolACLEvent)
	a.loopCtx = s.withRunHooks(a.loopCtx, a.runID, a.effectiveAgentName, a.agentDef, hooks.Additions{Hooks: a.in.Hooks, ToolHooks: a.in.ToolHooks})
	a.loopCtx = tools.WithDispatcher(a.loopCtx, a.dispatcher)

	a.heartbeat = s.makeHeartbeat(a.runID)

	// Cooperative pause quiesce (RFC X / F41): park at an iteration boundary
	// while paused. Deregistered here, or by the detached goroutine.
	a.gate, a.deregGate = s.newPauseGate(a.runID, a.retention == retentionmode.None)
	a.deferToEnd(func() {
		if !a.handOff {
			a.deregGate()
		}
	})
	// RFC X Phase 3: expose the gate to the Agent tool so a parallel_spawn
	// fan-out parent can park mid-tool-call (no-op unless LOOMCYCLE_RESUME_FANOUT).
	a.loopCtx = tools.WithPauseGate(a.loopCtx, a.gate)
}

// buildOptions builds the loop's options.
func (a *runAdmission) buildOptions() {
	s := a.s
	s.runPhase("options")
	fbPolicy, fbReResolve := s.fallbackForRun(a.effectiveTenantID, a.effectiveUserID, a.effectiveAgentName, a.in.UserTier, a.operatorKeyRestricted, a.runRouting)
	seed, seeded := statefulContinuationSeed(a.sessionEvents, a.mergedContext, a.provider.Capabilities().Local, a.in.Interactive, a.segments)
	if seeded {
		a.priorMessages = nil
	}
	a.runOpts = loop.RunOptions{
		Provider:            a.provider,
		Model:               a.model,
		PriceCall:           s.priceCall,
		Tools:               a.allowedTools,
		Dispatcher:          a.dispatcher,
		Segments:            injectMetadataSegments(a.segments, a.provider.Capabilities().MetadataViaInput, a.in.Metadata, a.in.PayloadMetadata),
		PriorMessages:       a.priorMessages,
		InitialState:        seed.Sigma,
		InitialObservation:  seed.Observation,
		PauseGate:           a.gate,
		BudgetGate:          s.budgetGate(a.effectiveTenantID, a.effectiveUserID),
		OnEvent:             a.emit,
		OnHeartbeat:         a.heartbeat,
		MaxTokens:           a.agentDef.MaxTokens,      // RFC DC: the run's own budget
		MaxContextTokens:    a.runCfg.MaxContextTokens, // RFC CJ; per-run wins, 0 → provider/driver default
		MaxIterations:       a.agentDef.MaxIterations,  // 0 → loop default (16)
		UnboundedIterations: a.agentDef.UnboundedIterations,
		SteerQueue:          a.steerQ,
		OnSteer:             a.onSteer,
		CloseSteerIfEmpty:   a.closeSteer,
		IdleTimeout:         s.cfg().Env.InteractiveIdleTimeout, // top-level only: sub-agents start elsewhere, with their own lifecycle
		Effort:              a.effort,
		MarkStalled:         s.markStalledFn(a.providerID, a.model),
		ObserveCall:         s.callObserver(a.runID),
		MarkRateLimited:     s.markRateLimitedFn(a.in.UserTier),
		ClearStall:          s.clearStallFn(a.providerID, a.model),
		ToolParallelism:     s.cfg().Env.ToolParallelism,
		AgentName:           a.effectiveAgentName,
		CodeBody:            a.agentDef.Code, // inline code-js body (RFC J); "" → FS fallback
		Metadata:            a.in.Metadata,
		PayloadMetadata:     a.in.PayloadMetadata,
		RunTimeoutSeconds:   a.runCfg.RunTimeoutSeconds,
		MaxWallSeconds:      a.runCfg.MaxWallSeconds,
		OnWallLimit:         s.wallLimitCancel(a.agentID, a.runID),
		Interactive:         a.in.Interactive,
		InteractiveNow:      s.interactiveNowFn(a.runID, a.in.Interactive),
		ArmTurnCancel:       s.armTurnCancelIf(a.in.Interactive, a.runID), // an interactive run's turn can be stopped; it parks
		Review:              a.in.Review,
		ReviewNow:           s.reviewNowFn(a.runID, a.in.Review),
		ReviewTTL:           a.runCfg.reviewTTL(),
		Sampling:            a.runCfg.Sampling,   // merged once, above
		ToolChoice:          a.runCfg.ToolChoice, // merged once, above
		OutputFormat:        a.runCfg.OutputFormat,
		Compaction:          a.runCfg.Compaction, // merged once, above
		Context:             a.mergedContext,     // merged once, above (RFC CR)
		// Recall-augmented distillation: nil unless the agent set context.recall AND
		// an embedder is configured, so an unopted run is byte-identical (RFC CT).
		RecallIndex: s.recallIndexForRun(a.mergedContext),
		// RFC BL P3 + RFC CT P2: installed when compaction.memory_flush OR
		// context.harvest_to_memory is set, else nil (unopted path byte-identical).
		BankCompactedSpan:      s.bankCompactedSpanFn(a.agentDef, config.HarvestToMemoryEnabled(a.mergedContext), a.rid.TenantID, a.effectiveUserID, a.effectiveAgentName, a.runID, a.sessionID),
		ContextPlugins:         s.contextPlugins, // RFC Z runtime-wide chain (code-js exempt in the loop)
		UserTier:               a.in.UserTier,
		FallbackPolicy:         fbPolicy,
		ReResolve:              fbReResolve,
		MoveForQueue:           s.queueMoveFn(a.agentDef, a.runCfg.Routing, a.runID, a.effectiveTenantID, a.effectiveUserID, a.effectiveAgentName, a.in.UserTier, a.operatorKeyRestricted),
		Hooks:                  s.hookDispatcher,
		MaxSameProviderRetries: s.retryAttemptsForAgent(a.agentDef, a.in.UserTier),
		// RFC DC P3: a parked run adopts a retune on its next operator turn.
		ReResolveOnOperatorTurn: s.reResolveOnOperatorTurnFn(a.runID, a.effectiveTenantID, a.effectiveUserID, a.effectiveAgentName, a.in.UserTier, a.operatorKeyRestricted, a.runCfg.Routing, a.liveInterruption),

		// A parked run adopts a retuned tool_choice / output_format at the same
		// boundary; the baseline is what the run starts with.
		ReReadShapeOnOperatorTurn: s.reReadShapeOnOperatorTurnFn(a.runID, a.runCfg.ToolChoice, a.runCfg.OutputFormat),
	}
	// A refusal of the operator's own key: the provider leaves routing if its probe agrees.
	a.runOpts.ReportOperatorKeyRefusal = s.operatorKeyRefusalFn()
	if a.sessHold != nil {
		stateful := loop.StatefulMode(a.mergedContext, a.provider.Capabilities().Local, a.in.Interactive)
		s.releaseSessionWhileParked(&a.runOpts, &parkLock{hold: a.sessHold, sessionID: a.in.SessionID}, a.loopCtx, stateful,
			func(reason string) { s.cancelReg.CancelLocalRun(a.agentID, a.runID, reason) })
	}
}

// launch runs the loop: in a goroutine that owns the teardown for a detached
// run, else inline until it ends.
func (a *runAdmission) launch() error {
	s := a.s
	s.runPhase("launch")
	if a.detached {
		a.handOff = true
		done := s.startDetachedLoop(detachedLoop{
			runID: a.runID, meta: a.meta, loopCtx: a.loopCtx, runCtx: a.runCtx, opts: a.runOpts, emit: a.emit,
			teardown: func() {
				a.deregSteer()
				a.deregGate()
				s.cancelReg.Deregister(a.agentID)
				a.runSpan.End()
				a.cancelFn(nil)
				// A continuation's session is the run's until it ends: no
				// other turn on it meanwhile, whoever is attached.
				a.sessHold.release()
				if a.sessHold == nil {
					a.untrackRun()
				}
			},
		})
		switch {
		case a.in.HoldUntilEnd:
			// Wait for the end even if the caller leaves: the session lock and
			// the slot this call holds are the run's until then.
			<-done
		default:
			// Forward to the caller until the run ends or the caller leaves;
			// the run is not stopped by the latter.
			select {
			case <-done:
			case <-a.ctx.Done():
			}
		}
		a.callerGone()
		return nil
	}
	res, runErr := loop.Run(a.loopCtx, a.runOpts)
	if runErr != nil && a.cb.OnRunError != nil {
		a.cb.OnRunError(runErr)
	}
	s.finishRunWithCancel(a.ctx, a.runCtx, a.runID, res, runErr, a.meta)
	return nil
}
