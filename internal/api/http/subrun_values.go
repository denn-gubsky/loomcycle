package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"go.opentelemetry.io/otel/trace"
)

// The steps of a sub-run start, split out of prepareSubRunValues (server.go)
// for reading. Each is called once, from there, in the order written above it;
// what a step needs from the ones before it is passed in, and what it settles
// comes back.

// resolveSubRunAgent resolves the name the parent asked for to the child's
// definition and the name it runs under, and lays a def_id pin over it.
func (s *Server) resolveSubRunAgent(ctx context.Context, tenant, name string, src nameSource, defID string) (config.AgentDef, string, error) {
	s.runPhase("subrun.resolve")
	// RFC N: a parent in tenant T resolves the sub-agent name within T's
	// view (parent tenant flows via ctx RunIdentity, inherited by every
	// sub-agent). Confirms RFC N's open-question on cross-boundary spawn:
	// the lookup is tenant-scoped, so a parent cannot spawn another
	// tenant's private agent by name.
	//
	// Inside a team the name may be one of the team's OWN agents, and from
	// here on `name` is the name the child RUNS under ("<team>/<name>" for
	// one of those, however the caller spelled it): its run row, session,
	// events and agent-scoped state all key on it.
	def, runName, err := s.resolveAgentName(ctx, tenant, name, src)
	if errors.Is(err, errAgentNotFound) {
		return config.AgentDef{}, "", fmt.Errorf("unknown sub-agent %q (not in cfg.Agents, dynamic_agents, or agent_def_active)", name)
	}
	if err != nil {
		return config.AgentDef{}, "", fmt.Errorf("sub-agent %q: %w", name, err)
	}
	name = runName
	// A team's own agent has no versions of its own to pin: it is whatever
	// the team version this run belongs to says it is.
	if def.TeamDefID != "" && defID != "" {
		return config.AgentDef{}, "", fmt.Errorf("Agent tool: def_id does not apply to %q — it is a team's own agent and runs as its team's version defines it", name)
	}

	// v0.8.5 substrate: when defID is set, overlay the named def's
	// mutable fields (system_prompt, tools, model, tier,
	// effort, max_tokens, memory_scopes, etc.) over the static
	// cfg.Agents entry for this single sub-run. Name mismatch is a
	// hard refuse — pinning across names would let a parent bypass
	// the operator's static agent boundary.
	if defID != "" {
		if s.store == nil {
			return config.AgentDef{}, "", fmt.Errorf("Agent tool: def_id pinning requires a configured store backend")
		}
		row, err := s.store.AgentDefGet(ctx, defID)
		// A def_id is a global handle, so the version must be one the name
		// could resolve to: the run's tenant's or the shared one's. Another
		// tenant's is refused as if it did not exist, and before the name
		// check, whose message would name that tenant's agent.
		if err == nil && row.TenantID != tenant && row.TenantID != "" {
			err = &store.ErrNotFound{Kind: "agent_def", ID: defID}
		}
		if err != nil {
			return config.AgentDef{}, "", fmt.Errorf("Agent tool: def_id %q lookup failed: %w", defID, err)
		}
		if row.Name != name {
			return config.AgentDef{}, "", fmt.Errorf("Agent tool: def_id %q is for agent %q, not %q (cross-name pinning refused)", defID, row.Name, name)
		}
		if row.Retired {
			return config.AgentDef{}, "", fmt.Errorf("Agent tool: def_id %q is retired", defID)
		}
		def = pinnedSubAgentDef(def, row.Definition)
	}
	return def, name, nil
}

// admitSubRun decides the child's retention and refuses a spawn its retention
// or its definition's override lock does not allow.
func (s *Server) admitSubRun(ctx context.Context, tenant, name string, src nameSource, def config.AgentDef, interactive bool, parentPolicy tools.RunPolicy) (string, error) {
	s.runPhase("subrun.admit")
	subRetention := s.resolveRetention(ctx, tenant, name, def, "")
	if err := admitNoTraceStart(subRetention, interactive, false, ""); err != nil {
		return "", fmt.Errorf("sub-agent %q: %w", name, err)
	}
	if subRetention == retentionmode.None {
		// A walk records each member's answer for the states after it, so a
		// no-trace member would be stored through the walk (as a no-trace run
		// cannot start one, admitTeamRun).
		if src == nameFromDefinition {
			return "", fmt.Errorf("sub-agent %q: %w", name, errNoTraceTeamMember)
		}
		// The child's answer comes back as the result of the calling tool
		// call: that call's stored copy is a marker (makeRecordingEmit).
		tools.MarkContentNotRetained(ctx)
	}
	// A parent's per-spawn override is an override like a caller's: a child
	// that runs on its definition only refuses it. Checked on the definition
	// the child will run — after a def_id pin, so the pinned version's own
	// lock is the one that decides.
	var spawnSet []string
	if parentPolicy.CompactionOverride != nil {
		spawnSet = append(spawnSet, "compaction")
	}
	if interactive {
		spawnSet = append(spawnSet, "interactive")
	}
	if err := admitAgentOverrides(name, def, spawnSet); err != nil {
		return "", fmt.Errorf("sub-agent %q: %w", name, err)
	}
	return subRetention, nil
}

// subRunConfigOf is the child's own part of its configuration record.
func subRunConfigOf(ctx context.Context, def config.AgentDef) runConfigRecord {
	// The sub-run's own configuration record, written with the run row a few
	// lines below. Sampling and the timeout are the child's own — a breeder
	// varies temperature by FORKING a def. What it inherits (hosts,
	// compaction, context, fan-out) is filled in from its policy once
	// inheritOverridesForChild has settled its definition.
	subRunCfg := runConfigRecord{
		Hooks:             additionsRecord(hooks.AdditionsFrom(ctx)), // inherited from the parent run
		SourcedHooks:      hooks.AdditionsFrom(ctx).Sourced,          // and a team state's, with their source
		Sampling:          def.Sampling,
		ToolChoice:        def.ToolChoice, // the child's own, like sampling
		OutputFormat:      def.OutputFormat,
		MaxContextTokens:  def.MaxContextTokens,
		RunTimeoutSeconds: def.RunTimeoutSeconds,
		// The version the child's NAME resolved to — under a def_id pin, the
		// base the pinned version was laid over — so a resume rebuilds the same
		// definition; the pin itself is AgentDefID on the row.
		AgentVersion: agentVersionOf(def),
		// The team this run belongs to, inherited from the spawning run's ctx
		// and passed on below (it is not cleared for sub-agents, unlike the
		// walk and wave stamps): a resume reads the team's own agents from it.
		TeamScope: teamScopeRecordOf(ctx),
	}
	return subRunCfg
}

// decideSubRunPolicy admits the spawn under the originating caller's ACL and
// decides the child's policy from its parent's. The ctx it returns carries
// the child's run facts; everything after it prepares the child on that ctx.
func (s *Server) decideSubRunPolicy(ctx context.Context, name string, def config.AgentDef, interactive bool, parentPolicy tools.RunPolicy, subRunCfg runConfigRecord) (context.Context, tools.ChildGrant, tools.RunPolicy, error) {
	s.runPhase("subrun.policy")
	// The child's policy, decided by Inherit from its parent's and its own
	// definition's. Built here, before the record is written, because the
	// record keeps what the child inherited (a resume cannot re-derive it
	// once the parent's ctx is gone) and must agree with what it runs with.
	// Its tool set and core blocks are only known once its prompt is
	// assembled below; they are its own (ruleOwn), so the decision is
	// repeated there with them filled in and every other field unchanged.
	//
	// ctx still carries the PARENT's identity, which matches the child's in
	// every field the definition's defaults read (user, tenant, isolation).
	// Safe to read def before the prompt rewrites below
	// (resolveSkillBodiesForRun / applyMemoryInjection): both return a copy
	// with only SystemPrompt changed, which
	// TestSubRunConfig_DefRewritesLeaveTuningUntouched pins.
	// RFC EY §3.7: the originating principal's set binds every hop. May it
	// spawn this agent at all, and what of the child's own definition may
	// it reach. The set is the parent policy's (inherited), never re-read.
	if err := s.aclAdmitSpawn(ctx, parentPolicy.ACL, name); err != nil {
		return ctx, tools.ChildGrant{}, tools.RunPolicy{}, err
	}
	// From here ctx prepares the child: a condition reads the child's agent,
	// depth and interactivity (RFC FY Part E) — a resident child runs
	// interactive, every other child does not.
	ctx = authz.WithRunFacts(ctx, authz.RunFacts{Agent: name, Depth: builtin.AgentDepth(ctx), Interactive: interactive})
	childOwn, _ := s.agentRunPolicy(ctx, def, name, runPolicySource{
		Volumes:      s.volumePolicyForAgent(ctx, def),
		Crypto:       cryptoPolicyForAgent(def),
		Compaction:   def.Compaction,
		Context:      def.Context,
		FanoutCap:    def.MaxConcurrentChildren,
		Interruption: subRunCfg.Interruption,
		ACL:          parentPolicy.ACL,
	})
	childGrant := tools.ChildGrant{
		Own:             childOwn,
		DeclaresVolumes: len(def.Volumes) > 0,
		LocksOverrides:  agentLocksOverrides(def),
		HostsNarrowOnly: callerHostsNarrowOnly(ctx, def),
	}
	childPolicy := parentPolicy.Inherit(childGrant)
	return ctx, childGrant, childPolicy, nil
}

// recordInheritedPolicy writes what the child inherited into its
// configuration record, which a resume reads when the parent's ctx is gone.
func recordInheritedPolicy(ctx context.Context, subRunCfg *runConfigRecord, parentPolicy, childPolicy tools.RunPolicy) {
	subRunCfg.Hosts = hostRecordOf(childPolicy.Hosts)
	// The originating caller's set binds the child as it binds the parent;
	// a resume compiles that caller's set again.
	subRunCfg.ACLOrigin = aclOriginOf(ctx, parentPolicy.ACL)
	subRunCfg.Compaction = childPolicy.Compaction
	subRunCfg.Context = childPolicy.Context
	subRunCfg.EphemeralReadOnly = !childPolicy.EphemeralWritable
	// The parent's volume confinement and key scopes are the ceilings the
	// child was narrowed against, and its fan-out width the one it runs with:
	// a resume narrows against the first two again and restores the third.
	subRunCfg.Spawn = spawnRecordOf(parentPolicy.Volumes, childPolicy.FanoutCap)
	subRunCfg.Spawn.CryptoScopes = parentPolicy.Crypto.Scopes
	subRunCfg.Spawn.ShellCredentials = shellCredsRecordOf(parentPolicy.ShellCreds)
	subRunCfg.Spawn.GitRemote = parentPolicy.GitRemote.Ops()
	// The offer the child passes on to its own children is its parent's
	// (Inherit), which a resume cannot read off a parent ctx.
	subRunCfg.Spawn.OfferedOverrides = offeredOverridesRecordOf(childPolicy.Overrides)
}

// subRunProvider is the provider the child's model resolved to, and settles
// its iteration limit and residency in its configuration record.
func (s *Server) subRunProvider(providerID, name string, def config.AgentDef, interactive bool, subRunCfg *runConfigRecord) (providers.Provider, error) {
	provider, err := s.providers.Get(providerID)
	if err != nil {
		return nil, fmt.Errorf("provider for sub-agent %q: %w", name, err)
	}

	// Read off the definition as the parent's overrides left it.
	subRunCfg.IterationLimit = s.startIterationLimit(def, provider, interactive)
	// Only a resident child runs interactive; its resume must know it is one.
	if subRunCfg.Spawn != nil {
		subRunCfg.Spawn.Resident = interactive
	}
	return provider, nil
}

// stampSubRunLineage stamps the board task, the walk state (or the walk
// lineage) and the wave the spawning run carries onto the child's identity.
func stampSubRunLineage(ctx context.Context, subIdentity *store.RunIdentity) {
	// RFC BT P4: a board-bound `TeamDef op=run` puts the task key on ctx; stamp it
	// onto this handler run's ParentContext so a client folding the run-state
	// stream can pin the live agent to its kanban card. Cleared from subRunCtx
	// below, so only the DIRECT handler is tagged (not its own sub-agents).
	if bt, ok := store.BoardTaskFromContext(ctx); ok {
		if subIdentity.ParentContext == nil {
			subIdentity.ParentContext = &store.ParentContext{}
		}
		subIdentity.ParentContext.BoardScope = bt.Scope
		subIdentity.ParentContext.BoardChunkID = bt.ChunkID
		subIdentity.ParentContext.BoardDocumentID = bt.DocumentID
	}
	// Every member a walk state spawns carries the walk, the state and the
	// visit, so a walk-filtered stream shows a walk with no starter too. The
	// wave stamp below then adds a Starter member's wave fields on top; both
	// carry the same walk id. Cleared from subRunCtx below like the rest.
	//
	// A run a member starts, at any depth, is not a member and still belongs
	// to the walk: it carries the walk, the state and the visit of the member
	// it descends from, and its parent run says it is not one. Without them it
	// was in neither the walk's listing nor its stream, so a member waiting on
	// its children named runs nothing could follow.
	if wk, ok := store.WalkTaskFromContext(ctx); ok {
		if subIdentity.ParentContext == nil {
			subIdentity.ParentContext = &store.ParentContext{}
		}
		subIdentity.ParentContext.WalkID = wk.WalkID
		subIdentity.ParentContext.State = wk.State
		subIdentity.ParentContext.StateVisit = wk.Visit
	} else {
		subIdentity.ParentContext = store.StampWalkLineage(ctx, subIdentity.ParentContext)
	}
	// RFC CY L4: a Starter puts its wave on ctx per spawned run; stamp it the
	// same way, so a wave's runs can be recovered by a join instead of being
	// copied anywhere. Cleared from subRunCtx below with the board task, so
	// only the DIRECT handler run carries it and not its own sub-agents.
	if wt, ok := store.WaveTaskFromContext(ctx); ok {
		if subIdentity.ParentContext == nil {
			subIdentity.ParentContext = &store.ParentContext{}
		}
		subIdentity.ParentContext.WalkID = wt.WalkID
		subIdentity.ParentContext.WaveID = wt.WaveID
		subIdentity.ParentContext.WaveIndex = wt.Index
	}
}

// recordSpawnChildStarted writes the spawn ledger's started row on the
// parent's transcript.
func (s *Server) recordSpawnChildStarted(ctx context.Context, parentPolicy tools.RunPolicy, subRunID, name string) {
	// RFC X Phase 3 spawn ledger: when this sub-run is a parallel_spawn child
	// (the parent threaded a spawn index + the parent's tool_use id is on ctx)
	// and the feature is on, record a spawn_child_started row on the PARENT's
	// transcript NOW — while the child run id is known but the child may park
	// before it completes. The emitter on ctx is still the parent's (the
	// sub-run's own emitter is swapped in below). Gives a restored fan-out
	// parent the (index → run_id) mapping to await + re-collect this child.
	if s.cfg().Env.ResumeFanout {
		if idx, ok := parentPolicy.SpawnChildIndex(); ok {
			if tuID := tools.ToolUseID(ctx); tuID != "" {
				if emit := tools.EventEmitter(ctx); emit != nil {
					emit(providers.Event{
						Type: providers.EventSpawnChildStarted,
						SpawnChild: &providers.SpawnChildEventInfo{
							ToolUseID: tuID,
							Index:     idx,
							RunID:     subRunID,
							Agent:     name,
							TimeoutMs: parentPolicy.SpawnTimeoutMs,
						},
					})
				}
			}
		}
	}
}

// clearSpawnerStamps clears from the child's execution ctx what was the
// spawning run's alone, and sets the walk lineage its own spawns inherit.
func clearSpawnerStamps(subRunCtx context.Context, subIdentity store.RunIdentity) context.Context {
	// Board task tags only the DIRECT handler run; clear it on the handler's own
	// execution ctx so its sub-agents aren't also pinned onto the card (RFC BT P4).
	subRunCtx = store.WithBoardTask(subRunCtx, store.BoardTask{})
	// Same for the wave: a wave is the runs the Starter dispatched, not
	// everything they went on to spawn. Leaving it set would make a wave's
	// apparent width depend on how chatty its agents were.
	subRunCtx = store.WithWaveTask(subRunCtx, store.WaveTask{})
	// And the walk state: a member's own sub-agents are not the walk's members,
	// and a walk nested inside a member stamps its own.
	subRunCtx = store.WithWalkTask(subRunCtx, store.WalkTask{})
	// What this run's own spawns inherit instead: the walk state this run
	// carries, as a member or as a member's descendant. Set from the stamped
	// row, so a run with none clears it.
	var lineage store.WalkTask
	if pc := subIdentity.ParentContext; pc != nil {
		lineage = store.WalkTask{WalkID: pc.WalkID, State: pc.State, Visit: pc.StateVisit}
	}
	subRunCtx = store.WithWalkLineage(subRunCtx, lineage)
	// And the walk's review arming and deadline. runTeamMember has read this
	// member's own from ctx already; left on its execution ctx, a walk nested
	// inside the member would hold its runs on the OUTER state's arming.
	subRunCtx = teamrun.WithReviewTTL(teamrun.WithReviewArming(subRunCtx, nil), nil)
	// And the id this run was created under: its own children mint theirs.
	subRunCtx = tools.WithChildRunID(subRunCtx, "")
	// A spawn's untrusted inputs are this run's alone: without the clear, a
	// child this run spawns with none of its own would be handed them again.
	subRunCtx = tools.WithSpawnUntrusted(subRunCtx, nil)
	// Likewise where this run reports its outcome (a spawn with result
	// "object"): a run it spawns must not write over it.
	subRunCtx = tools.WithChildOutcome(subRunCtx, nil)
	// And a pre hook's host grant: the loop attaches it to the one tool call it
	// judged, and when that call is this spawn it is on ctx here. Left on, it
	// widened the child's every HTTP call for its whole run; the child's calls
	// are judged by its own hooks.
	subRunCtx = tools.WithoutExtraAllowedHosts(subRunCtx)
	return subRunCtx
}

// registerSubRunCancel enters the child in the cancel registry and reports
// whether it is registered.
func (s *Server) registerSubRunCancel(subAgentID, subRunID, subSessionID string, parentIdentity tools.RunIdentityValue, subCancelFn context.CancelCauseFunc) bool {
	regErr := s.cancelReg.Register(cancel.Entry{
		AgentID:       subAgentID,
		RunID:         subRunID,
		SessionID:     subSessionID,
		UserID:        parentIdentity.UserID,
		ParentAgentID: parentIdentity.AgentID,
		StartedAt:     time.Now(),
	}, subCancelFn)
	registered := false
	if regErr != nil {
		// A duplicate-active collision is essentially impossible here
		// (subAgentID is freshly generated); log and continue without
		// registering rather than fail the run for a registry hiccup.
		slog.Warn("cancel registry: sub-agent register failed", "agent_id", subAgentID, "err", regErr)
	} else {
		registered = true
	}
	return registered
}

// publishSubRunStart publishes the child's running state and returns the
// meta its run-state events carry.
func (s *Server) publishSubRunStart(ctx context.Context, subRunID, subAgentID, name string, parentIdentity tools.RunIdentityValue, subRunSpan trace.Span, subIdentity store.RunIdentity) runStateMeta {
	// v0.9.x: sub-agent meta for runstate.Bus. ParentAgentID lets
	// SSE subscribers see the lineage edge.
	subMeta := runStateMeta{
		RunID:         subRunID,
		AgentID:       subAgentID,
		Agent:         name,
		UserID:        parentIdentity.UserID,
		TenantID:      parentIdentity.TenantID, // sub-agent inherits the parent run's tenant
		ParentAgentID: parentIdentity.AgentID,
		ParentRunID:   tools.RunID(ctx),
		otelSpan:      subRunSpan,
		// v0.12.x root lineage + RFC BT P4 board task (subIdentity's is the clone
		// of the parent's plus any board stamp above), echoed on this sub-run's
		// run-state events so a board client can pin it to its card.
		ParentContext: subIdentity.ParentContext,
	}
	s.publishRunState(subMeta, "running", "", "")
	return subMeta
}

// subRunPrompt is what buildSubRunPrompt settles: the child's definition with
// its prompt rewritten, its tool set narrowed by the ACL, its core blocks and
// its input segments.
type subRunPrompt struct {
	def        config.AgentDef
	promptProv runPromptProvenance
	tools      []tools.Tool
	aclRemoved []string
	coreBlocks []config.CoreBlock
	segs       []loop.PromptSegment
}

// buildSubRunPrompt resolves the child's skill bodies and tool set, narrows
// the set by the ACL, injects memory and composes its input segments.
func (s *Server) buildSubRunPrompt(ctx context.Context, def config.AgentDef, name string, parentIdentity tools.RunIdentityValue, parentPolicy tools.RunPolicy, values, dataSlots map[string]string, systemExtra, prompt string, systemAuthored, inputAuthored bool) subRunPrompt {
	s.runPhase("subrun.prompt")
	// v0.8.22: rebuild SystemPrompt from per-run SkillDef bodies
	// when any of the sub-agent's skills has a DB-active row. Same
	// call as the three top-level run-creation sites — without it,
	// sub-agents would silently keep the static baked body and
	// SkillDef promotions never take effect for agents only spawned
	// as sub-agents.
	// RFC N: a sub-agent runs with the parent's RunIdentity already on
	// ctx, so tenantFromCtx(ctx) returns the parent's (== the run's)
	// authoritative tenant — resolve the sub-agent's skills there.
	def, promptProv := s.resolveSkillBodiesForRun(ctx, tenantFromCtx(ctx), def)
	// RFC N FIX 2-mcp: sub-agents run with the parent's RunIdentity already
	// on ctx, so tenantFromCtx returns the run's authoritative tenant —
	// the sub-agent advertises the same tenant's MCP tools as its parent.
	//
	// Resolved BEFORE applyMemoryInjection because a {{tool:...}} placeholder
	// renders this list. Safe to hoist: applyMemoryInjection rewrites only
	// SystemPrompt, so def.Tools is the same value it was below.
	subTools := filterTools(s.candidateTools(ctx, tenantFromCtx(ctx), def.Tools), def.Tools, nil)
	subTools = s.grantRecallTool(subTools, def.Context) // RFC CT: recall-enabled sub-agent gets Recall
	// After the Recall grant, as a top-level run and a resume narrow: an
	// auto-granted tool is still one the caller's rules must allow.
	subTools, subACLRemoved := tools.NarrowToolsByACL(ctx, parentPolicy.ACL, subTools)
	// RFC BL P1: memory injection for the sub-agent. ctx still carries the
	// PARENT run's core-block policy, so an inherit_core_blocks sub-agent picks
	// up the parent's user/tenant blocks here (agent-scope never crosses).
	def, coreBlocks := s.applyMemoryInjection(ctx, def, memInject{
		Tenant: parentIdentity.TenantID, UserID: parentIdentity.UserID, AgentName: name,
		InitialInput: prompt, Tools: subTools,
		ACL: parentPolicy.ACL, ACLRemovedTools: subACLRemoved,
	})
	// The CALLER's segments are expanded too, with the same families and the
	// same single pass. Until this, a {{document:...}} in a team node's prompt
	// was inert — it reached the model as literal text — so the workflow
	// bindings the team design is built on did not resolve at all.
	systemExtra, prompt = s.composeCallerText(ctx, memInject{
		Tenant: parentIdentity.TenantID, UserID: parentIdentity.UserID, AgentName: name,
	}, values, dataSlots, systemExtra, prompt, systemAuthored, inputAuthored)
	// The spawn's untrusted inputs join the segments HERE, after the caller's
	// text is composed: they are data, so no placeholder in them is expanded,
	// and they sit after the prompt and whatever a start hook added to it.
	segs := withUntrustedBlocks(composeSubRunSegments(def.SystemPrompt, systemExtra, prompt), tools.SpawnUntrusted(ctx))
	return subRunPrompt{def: def, promptProv: promptProv, tools: subTools, aclRemoved: subACLRemoved, coreBlocks: coreBlocks, segs: segs}
}

// narrowSubRunHosts narrows the child's tools to the host list it inherited and
// builds the child's dispatcher over the narrowed set. The two are one step so
// the dispatcher cannot be built ahead of the narrowing: it is what executes
// the child's calls.
func (s *Server) narrowSubRunHosts(subTools []tools.Tool, childPolicy tools.RunPolicy) ([]tools.Tool, *tools.Dispatcher) {
	s.runPhase("subrun.hosts")
	// Inherit the parent's caller-authoritative host policy. Without
	// this, sub-agents fall back to the operator's static
	// HTTPHostAllowlist — which typically doesn't include localhost
	// callbacks — and a parent that worked against ["localhost"]
	// silently spawns children that can't reach the caller's API.
	// Production case: cv-batch-adapter (parent has localhost via
	// caller-authoritative) → cv-adapter children that need to PATCH
	// /api/applications/<id> back to jobs-search-web, hit
	// "host \"localhost\" not in allowlist", waste iterations
	// guessing hostnames, get capped by max_iterations, never write
	// the documents (2026-05-06).
	// The child's hosts are the parent's, made narrow-only for a locked child.
	if hp := childPolicy.Hosts; hp.HasList || s.cfg().Env.HTTPCallerAuthoritative {
		subTools = builtin.NarrowHosts(subTools, hp.AllowedHosts, hp.WebSearchFilter, s.cfg().Env.HTTPCallerAuthoritative && !hp.NarrowOnly)
	}
	subDispatcher := s.newDispatcher(subTools)
	return subTools, subDispatcher
}

// persistSubRunInput records the child's input segments and its system prompt
// as the first events of its transcript.
func (s *Server) persistSubRunInput(ctx context.Context, subRunID string, segs []loop.PromptSegment, def config.AgentDef, defID string, promptProv runPromptProvenance) {
	s.runPhase("subrun.persist")
	// Persist the input segments as the first event so transcript
	// replay reconstructs the user prompt the same way fresh runs do.
	//
	// ⚠️ DELIBERATELY NOT ROUTED THROUGH persistUserInput, which is the writer every
	// other `user_input` append uses. This one is not a person's words: it is the
	// prompt a PARENT AGENT composed for its sub-agent, stored under `user_input`
	// because that is the role the sub-agent's loop reads it in. Indexing it would
	// fill a user's trace index with machine-generated text they never typed and
	// cannot recognise — and a search for "what did I say" would answer with what an
	// agent said on their behalf.
	if s.store != nil && subRunID != "" {
		if inputJSON, err := json.Marshal(segs); err == nil {
			if err := s.store.AppendEvent(ctx, subRunID, "user_input", inputJSON); err != nil {
				slog.Error("store: AppendEvent(user_input) failed for sub-run", "run_id", subRunID, "err", err)
			}
		}
	}
	// v0.9.x: also persist the resolved system prompt + provenance.
	// On sub-runs we have the explicit def_id (parameter), so the
	// event payload includes agent_def_id — unique to this path.
	s.emitSystemPromptEvent(ctx, subRunID, def.SystemPrompt, defID, promptProv)
}

// subRunIdentityValue is the child's run identity on its ctx.
func subRunIdentityValue(parentIdentity tools.RunIdentityValue, def config.AgentDef, subAgentID, subSessionID, defID string, subLatch *atomic.Bool, subRetention string) tools.RunIdentityValue {
	subRID := tools.RunIdentityValue{
		UserID:          parentIdentity.UserID,
		TenantID:        parentIdentity.TenantID, // RFC L: sub-agents inherit the parent's authoritative tenant (same isolation boundary)
		AgentID:         subAgentID,
		RootRunID:       parentIdentity.RootRunID, // RFC AH Phase 2b: INHERIT the tree's root id (do NOT overwrite)
		SessionID:       subSessionID,             // its OWN chat, not the parent's — a sub-agent gets a session of its own
		UserTier:        parentIdentity.UserTier,  // v0.8.2: sub-agents inherit parent's user_tier
		ModelTier:       usageModelTier(def),      // its own agent's tier, not the parent's
		InternalAgent:   def.Internal,
		AgentDefID:      defID,                                // v0.8.7: surface pinned def_id via Context.self
		UserBearer:      parentIdentity.UserBearer,            // v0.8.x: bearer inherited identically (same end-user)
		UserCredentials: parentIdentity.UserCredentials,       // v1.x RFC F: credentials map inherited identically
		ParentContext:   parentIdentity.ParentContext.Clone(), // v0.12.x: tracking lineage flows to grandchildren too
		// RFC AX: inherit the parent's operator-key restriction (a child cannot
		// escape). The providers ctx value inherits automatically via
		// subRunCtx←ctx (same as WithCredentialResolver), so no re-stamp needed.
		OperatorKeyRestricted: parentIdentity.OperatorKeyRestricted,
		// A child resolves $cred: no more widely than its parent.
		CredentialsNoUserScope: parentIdentity.NoUserScope(),
		UserScopeWithheld:      subLatch,
		// RFC BX P2b: inherit the parent's isolation confinement (a child cannot
		// escape by spawning). Read from ctx RunIdentity by the data tools.
		Isolated:  parentIdentity.Isolated,
		Retention: subRetention,
	}
	return subRID
}

// stampChildRunCtx puts the child's policy, decided again with its own tools
// and core blocks, and its emitter, run ids, hooks and dispatcher on its ctx.
func (s *Server) stampChildRunCtx(subCtx context.Context, childGrant tools.ChildGrant, parentPolicy tools.RunPolicy, subTools []tools.Tool, subACLRemoved []string, coreBlocks []config.CoreBlock, subRunID string, subEmit func(providers.Event), def config.AgentDef, name string, subDispatcher *tools.Dispatcher) (_ context.Context, childPolicy tools.RunPolicy) {
	s.runPhase("subrun.child_ctx")
	// The child's policy, decided again now that its own tool set and core
	// blocks are known — both are its own, so every other field is what the
	// record above was written from. It REPLACES the parent's: whatever the
	// parent held reaches the child only where Inherit says it does.
	childGrant.Own.Tools = toolNames(subTools)
	childGrant.Own.ACLRemovedTools = subACLRemoved
	// RFC BL P1: the child's effective core blocks (its own + any inherited
	// user/tenant blocks), so the Memory tool enforces THIS agent's blocks and
	// a grandchild inherits from here.
	childGrant.Own.CoreBlocks = tools.CoreBlocksPolicyValue{Blocks: coreBlocks}
	childPolicy = parentPolicy.Inherit(childGrant)
	subCtx = tools.WithRunPolicy(subCtx, childPolicy)
	subCtx = s.recordShadowNarrowing(subCtx, subRunID, childPolicy)
	// RFC AH Phase 2b: the run-tree's ephemeral volume SET is deliberately NOT
	// re-attached here — subCtx derives from the parent's ctx (via subRunCtx ←
	// WithCancelCause(ctx)), so the SAME *EphemeralVolumeSet pointer flows down
	// unchanged. A sub-agent creating an ephemeral volume thus registers it in
	// the shared set the whole tree resolves through, and it is purged with the
	// tree at the top-level run's completion (sub-agents never purge).
	//
	// Sub-agent event emitter writes to the SUB's transcript (per
	// subEmit above). Channel-tool publishes from inside the sub
	// surface on the sub's SSE stream, not the parent's.
	subCtx = tools.WithEventEmitter(subCtx, subEmit)
	subCtx = tools.WithSearchProviders(subCtx, def.SearchProviders)
	// subCtx still carries the PARENT's run id here, which is exactly the
	// parent id; it is replaced by the child's on the next line.
	subCtx = tools.WithParentRunID(subCtx, tools.RunID(subCtx))
	subCtx = tools.WithRunID(subCtx, subRunID)
	subCtx = s.withRunHooks(subCtx, subRunID, name, def, hooks.Additions{})
	subCtx = tools.WithDispatcher(subCtx, subDispatcher)
	return subCtx, childPolicy
}

// subRunCleanup is the child's teardown, which its caller owns once the
// start succeeds.
func (s *Server) subRunCleanup(subRunID, subAgentID string, registered bool, deregSubGate func(), subRunSpan trace.Span, subCancelFn context.CancelCauseFunc) func() {
	// cleanup fires the SAME releases as the guarded error-defers above, in the
	// SAME order runSubAgent's original defers ran LIFO (deregSubGate →
	// subRunSpan.End → cancelReg.Deregister when registered → subCancelFn).
	cleanup := func() {
		s.releaseUserScopeLatch(subRunID)
		deregSubGate()
		subRunSpan.End()
		if registered {
			s.cancelReg.Deregister(subAgentID)
		}
		subCancelFn(nil)
	}
	return cleanup
}
