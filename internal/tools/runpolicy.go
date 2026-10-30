package tools

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RunPolicy is the trust-boundary policy of one run: what its agent may reach
// and how far. It travels on the run's ctx as ONE value (WithRunPolicy /
// PolicyOf), built once per run entry point and, for a spawned child, derived
// from its parent's by Inherit, which decides every field explicitly.
//
// ZERO MEANS DENY. A run whose entry point built no policy reads the zero
// RunPolicy, and every grant in it is withheld: no tools ceiling, no volume,
// no memory, SQL, history, channel, interruption, def-family, skill,
// evaluation or key scope, no ephemeral write, no Git network operation, not
// operator-authored. The fields that NARROW something the operator configured
// rather than grant it — Hosts, Decision, CoreBlocks, FanoutCap, Overrides,
// Compaction, Context, ShellCreds — fall back to that floor when zero: the
// operator's host list and decision models, no core blocks to guard, the
// definition's fan-out width, no inherited override, no compaction or context
// setting, every shell credential the operator lists.
//
// Before RunPolicy each of these rode its own ctx key with its own setter, and
// a child inherited whatever its spawn path forgot to restamp: a per-spawn
// override, a ledger index, a fan-out cap wider than the child's own. With one
// value there is one place that says what a child keeps (Inherit) and a test
// that refuses a field nobody wrote a rule for (inheritRules).
//
// What is NOT here, on purpose:
//   - plumbing: run ids, the dispatcher, emitters, observers, the pause gate,
//     and per-call transients (ToolUseID, HookedExecute, ExtraAllowedHosts);
//   - identity: RunIdentity (already one typed value, persisted per run);
//   - the operator plane's own authority (SubstrateOperator and the
//     operator-only def policies): a run never holds it (stripOperatorPlane).
type RunPolicy struct {
	// Tools is the agent's effective tool names (after the definition's and
	// the caller's narrowing): the ceiling the Skill, AgentDef and SkillDef
	// subset checks hold a request to.
	Tools []string
	// ToolPatterns is the agent's RAW tool declaration, globs preserved, for
	// the Skill tool's subset check (a resolved list can never cover a glob).
	ToolPatterns []string

	// Hosts is the caller's HTTP host narrowing (see HostPolicyValue).
	Hosts HostPolicyValue
	// Volumes is the run's filesystem-volume confinement (see
	// VolumePolicyValue). Zero = bound to no volume, every file tool refuses.
	Volumes VolumePolicyValue
	// EphemeralWritable lets the run write to its tree's ephemeral volumes
	// (each still subject to the volume's own mode). False — the zero value,
	// or an agent whose definition says `ephemeral_volumes: ro` — resolves
	// every ephemeral volume read-only.
	EphemeralWritable bool
	// VolumeDef gates VolumeDef create/delete/purge.
	VolumeDef VolumeDefPolicyValue

	Memory     MemoryPolicyValue
	SqlMem     SqlMemPolicyValue
	CoreBlocks CoreBlocksPolicyValue
	// Decision is the agent's narrowing of the operator's decision models;
	// nil narrows nothing.
	Decision *config.AgentDecision
	// Transcription is the agent's transcription block: whether its File
	// tool runs OCR (never, auto, always) and the caps it lowers. nil is OCR
	// never, the default an agent opts in from.
	Transcription *config.AgentTranscription
	History       HistoryPolicyValue

	Channel ChannelPolicyValue
	// Interruption is the run's live interruption policy. A holder rather
	// than a value so a retune of a parked run reaches the Interruption tool
	// on its next turn without rebuilding ctx; nil grants nothing.
	Interruption *InterruptionPolicyHolder

	AgentDef   AgentDefPolicyValue
	Skill      SkillPolicyValue
	Evaluation EvaluationPolicyValue
	Crypto     CryptoPolicyValue
	// ShellCreds narrows which operator-listed credentials the Bash tool and
	// the Bashbox host-command fallback inject (RFC EW). Zero: all of them.
	ShellCreds ShellCredsPolicyValue
	// GitRemote is what the Git tool may do over the network (RFC FB). Zero:
	// nothing, local repository work only.
	GitRemote GitRemotePolicyValue
	// StatementMethods is which statement methods the run may stamp on the
	// relations it writes: "rule" and/or "extractor". Only a code-js agent
	// is ever given any, because a rule-stamped relation claims code made
	// it. Zero: none, every relation the run writes is stamped manual.
	StatementMethods []string
	// OperatorAuthored is whether the run's agent definition was written by
	// an operator rather than by an agent at runtime.
	OperatorAuthored bool
	// ACL is the compiled access-control set of the principal that started
	// the run's tree (RFC EY). It is inherited unchanged by every sub-agent —
	// the originating user's authority binds every hop — and each hop's own
	// definition is intersected with it again (NarrowToolsByACL and the
	// scope narrowings in the entry points). nil: no principal (open mode,
	// a trigger firing under a definition) or no resolver.
	ACL *authz.Effective
	// ACLRemovedTools names the tools this run's definition granted and its
	// ACL removed, for Context op=self; each hop's own.
	ACLRemovedTools []string
	// LiveACL, set only under LOOMCYCLE_ACL_LIVE=1, re-reads the caller's
	// set during the run and holds the policy PolicyOf returns to the current
	// rules within ACL (see aclive.go). nil: the set is frozen at the start.
	LiveACL *LiveACL

	// FanoutCap is the run's own parallel_spawn width; 0 lets the calling
	// agent's definition decide, by name, at spawn time.
	FanoutCap int
	// Overrides offers the run's override record to children of the same
	// definition (see RunOverridesValue).
	Overrides RunOverridesValue
	// Compaction and Context are the run's resolved-but-sparse compaction and
	// layered-context settings, which flow down the spawn tree.
	Compaction *config.Compaction
	Context    *config.Context

	// The per-spawn inputs the Agent tool hands the ONE sub-run it is about to
	// start. They are this spawn's, never the child's own: Inherit clears them.
	//
	// CompactionOverride is a spawn entry's `compaction`, blended over the
	// child's effective policy.
	CompactionOverride *config.Compaction
	// SpawnIndex is a parallel_spawn child's index + 1 (0 = not a
	// parallel_spawn child; see SpawnChildIndex).
	SpawnIndex int
	// SpawnTimeoutMs is a parallel_spawn child's timeout_ms (0 = none).
	SpawnTimeoutMs int
}

type ctxKeyRunPolicy struct{}

// WithRunPolicy attaches p as the run's whole policy, replacing any policy ctx
// already carried.
func WithRunPolicy(ctx context.Context, p RunPolicy) context.Context {
	return context.WithValue(ctx, ctxKeyRunPolicy{}, p)
}

// PolicyOf returns the run's policy, or the zero RunPolicy when ctx carries
// none — which grants nothing (see the field docs for what zero means each).
// A run with a live view of its caller's set (LiveACL) gets the policy
// narrowed by the latest reading of that set.
func PolicyOf(ctx context.Context) RunPolicy {
	p, _ := ctx.Value(ctxKeyRunPolicy{}).(RunPolicy)
	if p.LiveACL != nil {
		return applyLive(ctx, p)
	}
	return p
}

// UpdateRunPolicy returns ctx carrying a copy of its policy with fn applied.
// The copy is shallow: fn must replace a slice or pointer field, never write
// through one, since the original ctx still shares it.
func UpdateRunPolicy(ctx context.Context, fn func(*RunPolicy)) context.Context {
	p := PolicyOf(ctx)
	fn(&p)
	return WithRunPolicy(ctx, p)
}

// SpawnChildIndex returns a parallel_spawn child's index and whether this
// spawn is one.
func (p RunPolicy) SpawnChildIndex() (int, bool) {
	if p.SpawnIndex <= 0 {
		return 0, false
	}
	return p.SpawnIndex - 1, true
}

// ChildGrant is what a spawned child brings to its own policy: the policy its
// definition would give it on its own, and the facts about that definition
// Inherit needs to combine it with its parent's.
type ChildGrant struct {
	// Own is the policy the child's definition grants, built by the same
	// constructor a top-level run of that definition uses.
	Own RunPolicy
	// DeclaresVolumes is whether the child's definition names volumes. A
	// child that names none works within its parent's.
	DeclaresVolumes bool
	// LocksOverrides is whether the child runs on its definition only
	// (allow_overrides: false). Its parent's compaction and context carry its
	// parent's caller's per-run values, which such a child refuses from
	// anyone, so it takes neither.
	LocksOverrides bool
	// HostsNarrowOnly is whether the parent's host list may only narrow for
	// this child (a locked child, or a caller held by its token's override
	// lock). Once set it stays set down the tree.
	HostsNarrowOnly bool
}

// inheritRule names how Inherit decides one RunPolicy field for a child.
type inheritRule string

const (
	// ruleOwn: the child's own definition decides; nothing of the parent's
	// reaches it.
	ruleOwn inheritRule = "own"
	// ruleInherit: the parent's value, unchanged.
	ruleInherit inheritRule = "inherit"
	// ruleNarrow: the parent's value narrowed by the child's own; never wider
	// than either.
	ruleNarrow inheritRule = "narrow"
	// ruleMerge: per field, the parent's setting wins and the child's fills
	// what the parent left unset (the spawn-tree settings).
	ruleMerge inheritRule = "merge"
	// ruleClear: this spawn's own input; consumed by the spawn, never the
	// child's.
	ruleClear inheritRule = "clear"
)

// inheritRules is the documentation of Inherit, one line per RunPolicy field.
// TestRunPolicy_EveryFieldHasAnInheritanceRule fails when a field has no
// entry, and TestRunPolicy_InheritFollowsItsRules checks Inherit against it,
// so a field added to RunPolicy cannot reach a child by accident.
var inheritRules = map[string]inheritRule{
	"Tools":              ruleOwn,     // the child's tool ceiling is its definition's
	"ToolPatterns":       ruleOwn,     //
	"Hosts":              ruleInherit, // the caller's narrowing holds for the whole tree; narrow-only is sticky
	"Volumes":            ruleNarrow,  // child-declared ∩ parent; undeclared works within the parent's
	"EphemeralWritable":  ruleNarrow,  // writable only if both are
	"VolumeDef":          ruleOwn,     //
	"Memory":             ruleOwn,     // a child's surface is its own definition's
	"SqlMem":             ruleOwn,     //
	"CoreBlocks":         ruleOwn,     // already folds in the parent's inheritable blocks (applyMemoryInjection)
	"Decision":           ruleOwn,     //
	"Transcription":      ruleOwn,     // whether OCR runs is each agent's own opt-in; the spend is bounded per run tree
	"History":            ruleOwn,     //
	"Channel":            ruleOwn,     //
	"Interruption":       ruleOwn,     // from the child's definition and its record
	"AgentDef":           ruleOwn,     //
	"Skill":              ruleOwn,     //
	"Evaluation":         ruleOwn,     //
	"Crypto":             ruleNarrow,  // the child's own scopes its parent also holds
	"ShellCreds":         ruleNarrow,  // the credential names both the parent and the child allow (RFC EW)
	"GitRemote":          ruleNarrow,  // each network op only when both the parent and the child allow it (RFC FB)
	"StatementMethods":   ruleOwn,     // each agent's own: a child is code itself or stamps nothing
	"OperatorAuthored":   ruleOwn,     //
	"ACL":                ruleInherit, // the originating principal's set binds the whole tree (RFC EY)
	"ACLRemovedTools":    ruleOwn,     // what this hop's definition lost to the ACL
	"LiveACL":            ruleOwn,     // each run's own view, built from the set it was spawned with
	"FanoutCap":          ruleNarrow,  // min(parent, child) when both are set, else the one set
	"Overrides":          ruleInherit, // only a child of the same definition takes them
	"Compaction":         ruleMerge,   // parent wins per field, then this spawn's override
	"Context":            ruleMerge,   // parent wins per field
	"CompactionOverride": ruleClear,   // consumed by this spawn (into Compaction)
	"SpawnIndex":         ruleClear,   // this spawn's ledger index
	"SpawnTimeoutMs":     ruleClear,   // this spawn's ledger bound
}

// InheritRule names the rule Inherit applies to the RunPolicy field named
// field — "own", "inherit", "narrow", "merge" or "clear" — or "" when there is
// no such field. Read by the guards that check a policy-stamping site against
// what a spawned child would take from it.
func InheritRule(field string) string {
	return string(inheritRules[field])
}

// Inherit returns a spawned child's policy: for every field, its parent's (p),
// its own definition's (c.Own), the narrower of the two, or nothing — as
// inheritRules says. It is the one place a child's policy is decided; the
// spawn path builds c and stamps the result.
func (p RunPolicy) Inherit(c ChildGrant) RunPolicy {
	own := c.Own
	parentCompaction, parentContext := p.Compaction, p.Context
	if c.LocksOverrides {
		parentCompaction, parentContext = nil, nil
	}
	hosts := p.Hosts
	if hosts.HasList && c.HostsNarrowOnly {
		hosts.NarrowOnly = true
	}
	return RunPolicy{
		Tools:             own.Tools,
		ToolPatterns:      own.ToolPatterns,
		Hosts:             hosts,
		Volumes:           ChildVolumes(p.Volumes, own.Volumes, c.DeclaresVolumes),
		EphemeralWritable: p.EphemeralWritable && own.EphemeralWritable,
		VolumeDef:         own.VolumeDef,
		Memory:            own.Memory,
		SqlMem:            own.SqlMem,
		CoreBlocks:        own.CoreBlocks,
		Decision:          own.Decision,
		Transcription:     own.Transcription,
		History:           own.History,
		Channel:           own.Channel,
		Interruption:      own.Interruption,
		AgentDef:          own.AgentDef,
		Skill:             own.Skill,
		Evaluation:        own.Evaluation,
		Crypto:            CryptoPolicyValue{Scopes: NarrowCryptoScopes(p.Crypto.Scopes, own.Crypto.Scopes)},
		ShellCreds:        NarrowShellCreds(p.ShellCreds, own.ShellCreds),
		GitRemote:         NarrowGitRemote(p.GitRemote, own.GitRemote),
		StatementMethods:  own.StatementMethods,
		OperatorAuthored:  own.OperatorAuthored,
		ACL:               p.ACL,
		ACLRemovedTools:   own.ACLRemovedTools,
		LiveACL:           own.LiveACL,
		FanoutCap:         NarrowFanoutCap(p.FanoutCap, own.FanoutCap),
		Overrides:         p.Overrides,
		// A child's own settings fill what its parent left unset; a per-spawn
		// override wins over both. A child that runs on its definition only
		// is refused an override before it gets here (admitAgentOverrides).
		Compaction: config.MergeCompaction(config.MergeCompaction(own.Compaction, parentCompaction), p.CompactionOverride),
		Context:    config.MergeContext(own.Context, parentContext),
		// CompactionOverride, SpawnIndex, SpawnTimeoutMs: zero. They were the
		// spawn's request, and a child that spawns without them must not pass
		// them on.
	}
}

// NarrowFanoutCap is a child's parallel_spawn width: the smaller of its
// parent's run cap and its own definition's when both are set, and whichever
// one is set otherwise. A child never fans out wider than an ancestor allowed,
// nor wider than the definition it was spawned (or resumed) under says. 0 only
// when neither is set: then the cap lookup resolves the agent by name.
//
// The child's own cap is kept under an uncapped parent rather than left to that
// name lookup, which reads whatever the name resolves to now — a version
// promoted since the spawn, or not the version a def_id pin ran.
func NarrowFanoutCap(parent, own int) int {
	switch {
	case parent <= 0:
		return max(own, 0)
	case own > 0 && own < parent:
		return own
	default:
		return parent
	}
}

// ChildVolumes is a spawned child's volume confinement (RFC AH §4):
//
//   - parent not confined (inactive): the child's own, as if top-level — there
//     is no parent scope to narrow against;
//   - a child that declares no volumes: the parent's, verbatim — it works
//     within the parent's scope, as it inherits the parent's hosts;
//   - otherwise: child-declared ∩ parent (NarrowVolumes).
func ChildVolumes(parent, own VolumePolicyValue, ownDeclares bool) VolumePolicyValue {
	if !parent.Active {
		return own
	}
	if !ownDeclares {
		return parent
	}
	return NarrowVolumes(parent, own)
}

// NarrowVolumes computes a sub-agent's volume binding set as the NARROW-ONLY
// intersection (RFC AH §4): child set = (child-declared) ∩ (parent's active
// bindings). A child can NEVER gain a volume the parent lacks; where both hold
// a volume, ReadOnly resolves to the MORE restrictive of the two (ro wins).
// This mirrors NarrowHosts exactly — the parent's policy is the floor, the
// child can only shrink it.
//
// The child's Default flag is preserved from the child's own declaration (the
// parent might mark a different volume as its default), so an omitted `volume`
// in the child resolves against the child's own designated default among the
// surviving intersection.
//
// Called ONLY with an ACTIVE parent (see ChildVolumes). The result is ALWAYS
// active: an empty intersection means the child shares none of the parent's
// volumes and is confined to NOTHING (every file-tool call refused), NOT
// dropped back to the legacy jail.
func NarrowVolumes(parent, child VolumePolicyValue) VolumePolicyValue {
	parentByName := make(map[string]VolumeBinding, len(parent.Bindings))
	for _, b := range parent.Bindings {
		parentByName[b.Name] = b
	}
	out := make([]VolumeBinding, 0, len(child.Bindings))
	for _, cb := range child.Bindings {
		pb, ok := parentByName[cb.Name]
		if !ok {
			continue // child named a volume the parent lacks — drop it.
		}
		// More-restrictive-wins on the ro/rw axis: ro if EITHER side is ro.
		out = append(out, VolumeBinding{
			Name:     cb.Name,
			Root:     pb.Root, // parent's resolved root is authoritative.
			ReadOnly: cb.ReadOnly || pb.ReadOnly,
			Default:  cb.Default,
			Fence:    pb.Fence, // with the root it fences.
		})
	}
	return VolumePolicyValue{Active: true, Bindings: out}
}
