package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/skillmatch"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// agentdef_ceiling.go — the capability ceiling for a new agent definition.
//
// The tools ceiling alone left a hole: a definition's other fields are
// authority too. A run builds a spawned child's memory, SQL, history, channel,
// interruption, evaluation, crypto, skill, volume and def-authoring policies from the
// CHILD's own definition, so an agent able to create an agent with fields it
// does not itself hold could mint authority and then spawn it. Every capability
// field is therefore held to the same rule as tools: within what the author
// holds, read from the author's own policies on ctx.
//
// An explicit value is always judged. A field left UNSET is judged at the
// value it resolves to (its EFFECTIVE value), except:
//
//   - memory_scopes and sql_scopes, the two memory facets: unset is the
//     ordinary memory fallback every agent gets (the caller's own data), and
//     an author is not made to spell it out;
//   - evaluation_scopes: unset is submit_self, which reaches only the new
//     agent's own run.
//
// Every other default is reach the author may not hold: unset volumes binds
// the operator's `default` host directory, unset history_scope reads the
// user's chats, unset interruption kinds is `question`, and unset skills is
// every skill.
//
// Entries naming a team's own resources ("./<name>" channels and skills, a
// single well-formed segment) are not judged here: they reach only what the
// team declares, the author holds those by authoring the team, and teamgraph
// refuses one the team does not declare. On a global agent such an entry
// names nothing. Any other "./" spelling is judged like any entry.
//
// Deliberately NOT judged, because they grant no reach over anyone's data:
// resource budgets (max_iterations, unbounded_iterations,
// max_concurrent_children, max_tokens, run_timeout_seconds, the memory/SQL
// quotas, interruption.max_pending — bounded by the token budgets and the
// runtime's own ceilings), routing and tuning (provider, model, tier, effort,
// sampling, providers, models, search_providers, memory_backend, memory_rerank,
// memory_units, decision — which can only narrow the operator's own list —
// compaction, context), prompt shape (system_prompt,
// inject_tool_guide, memory_protocol, memory_roots), and `internal`, which
// nothing reads off a runtime-authored definition (Config.InternalAgentNames
// is static-only). Hooks have their own gate (checkHooks); code_body runs only
// through the agent's own tools, which the tools ceiling bounds, and a fork
// that changes it keeps none of the forked version's grants (forkChangesCode).

// checkCapabilityCeiling refuses a definition granting a capability its author
// does not hold. base is the stored version a fork is made from (nil at
// create): what it already holds a fork may keep, so a fork can narrow a
// lineage but never widen it beyond what its author or the version it forks
// held.
//
// The operator plane is the root of authority and is not narrowed. A member
// on a plane is narrowed by what it may do itself (memberCeilingCtx, and the
// classification beside it); a run by its own policies, where a policy that
// is absent grants nothing, so a non-empty capability field fails closed
// exactly as the tools ceiling does.
func (a *AgentDef) checkCapabilityCeiling(ctx context.Context, name string, def mergedDef, base *mergedDef) error {
	switch {
	case tools.IsOperatorPlane(ctx):
		return nil
	case tools.IsOffRunPlane(ctx):
		if err := memberToolsCeiling(ctx, def); err != nil {
			return err
		}
		ctx = a.memberCeilingCtx(ctx, def)
		// No base: the forked version's grants were its author's, and a
		// member's fork holds only what the member could grant itself.
		base = nil
	}
	held := "this agent's own"
	if base != nil {
		held = "this agent's own, or the forked version's"
	}
	checks := []func() error{
		func() error { return a.ceilVolumes(ctx, def, base, held) },
		func() error { return ceilMemory(ctx, def, base, held) },
		func() error { return ceilSQL(ctx, def, base, held) },
		func() error { return ceilHistory(ctx, def, base, held) },
		func() error { return ceilChannels(ctx, def, base, held) },
		func() error { return ceilInterruption(ctx, def, base, held) },
		func() error { return ceilEvaluation(ctx, def, base, held) },
		func() error { return ceilCrypto(ctx, def, base, held) },
		func() error { return ceilToolModes(ctx, def, base, held) },
		func() error { return ceilShellCreds(ctx, def, base, held) },
		func() error { return ceilGitRemote(ctx, def, base, held) },
		func() error { return ceilWorkspace(ctx, def, base, held) },
		func() error { return ceilStatementMethods(ctx, def, base) },
		func() error { return ceilSkills(ctx, def, base, held) },
		func() error { return ceilDefScopes(ctx, name, def, base, held) },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// refuseCapability is the refusal for one value outside the ceiling. defaulted
// says the field was left unset, so the value came from its default — the
// author has to set the field to narrow it, not remove something it wrote.
func refuseCapability(field, value string, defaulted bool, held string, ceiling []string) error {
	if defaulted {
		none := ""
		if field == "history_scope" {
			none = ` (or ["` + tools.DenyAllScopes + `"] for none)`
		}
		return fmt.Errorf("%s: left unset, the new agent would hold the default %q, which is not within %s %s %s. "+
			"A new agent can only be granted authority its author holds: set %s explicitly to what it needs from that list%s, "+
			"or ask an operator to author this agent",
			field, value, held, field, listOrNone(ceiling), field, none)
	}
	return fmt.Errorf("%s: %q is not within %s %s %s. A new agent can only be granted authority its author holds: "+
		"remove it, or ask an operator to author this agent",
		field, value, held, field, listOrNone(ceiling))
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "(none)"
	}
	return fmt.Sprintf("%v", l)
}

// firstUncovered returns the first entry of child that covered rejects.
func firstUncovered(child []string, covered func(string) bool) (string, bool) {
	for _, c := range child {
		if !covered(c) {
			return c, true
		}
	}
	return "", false
}

// scopeUnion is a then the entries of b it lacks. Entries compare as the
// policy checks compare them (contains: trimmed, exact).
func scopeUnion(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, s := range b {
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// subsetCeiling judges a scope list compared entry by entry (memory, SQL,
// history): every effective entry must be one the ceiling lists.
func subsetCeiling(field string, effective []string, defaulted bool, held string, ceiling []string) error {
	if v, bad := firstUncovered(effective, func(c string) bool { return contains(ceiling, c) }); bad {
		return refuseCapability(field, v, defaulted, held, ceiling)
	}
	return nil
}

// ---- volumes ----

// effectiveVolumes is the volume set an agent binds: its declared names, or
// the operator's `default` volume when it declares none (volumePolicyForAgent).
func (a *AgentDef) effectiveVolumes(d mergedDef) []string {
	if len(d.Volumes) > 0 {
		return d.Volumes
	}
	if _, ok := a.Cfg.Volumes["default"]; ok {
		return []string{"default"}
	}
	return nil
}

func (a *AgentDef) ceilVolumes(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	// Unset binds the operator's `default` volume — a host directory — so it
	// is judged as that name.
	child, defaulted := a.effectiveVolumes(def), len(def.Volumes) == 0
	if len(child) == 0 {
		return nil
	}
	pol := tools.PolicyOf(ctx).Volumes
	bound := map[string]tools.VolumeBinding{}
	var ceiling []string
	if pol.Active {
		for _, b := range pol.Bindings {
			bound[b.Name] = b
			ceiling = append(ceiling, b.Name)
		}
	}
	var fromBase []string
	if base != nil {
		fromBase = a.effectiveVolumes(*base)
		ceiling = scopeUnion(ceiling, fromBase)
	}
	for _, v := range child {
		if contains(fromBase, v) {
			continue
		}
		b, ok := bound[v]
		if !ok {
			return refuseCapability("volumes", v, defaulted, held, ceiling)
		}
		// A binding narrowed to read-only (a sub-agent of a read-only parent)
		// does not make the volume read-only: a new agent naming it binds the
		// volume as it is declared, which may be read-write. A volume that no
		// longer resolves binds nothing, so it is no widening.
		if b.ReadOnly {
			spec, found := lookup.VolumeDef(ctx, a.Cfg, a.Store, tools.RunIdentity(ctx).TenantID, v)
			if found && spec.Mode != "ro" {
				return fmt.Errorf("volumes: %q is read-only for this agent, and a new agent bound to it could write to it. "+
					"A new agent can only be granted authority its author holds: remove it, or ask an operator to author this agent", v)
			}
		}
	}
	return nil
}

// ---- memory / SQL / history ----

func ceilMemory(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	pol := tools.PolicyOf(ctx).Memory
	// memory_scopes grants write, so a scope the author holds read-only (its
	// caller may not write it) is outside the ceiling, as a read-only volume
	// is. A core block only reads its scope, so blocks keep the read reach.
	ceiling := pol.WritableScopes()
	readReach := pol.AllowedScopes
	consolidation, includeTurns, attachTraces := pol.Consolidation, pol.RecallIncludeTurns, pol.RecallAttachTraces
	if base != nil {
		ceiling = scopeUnion(ceiling, tools.EffectiveMemoryScopes(ctx, base.MemoryScopes))
		readReach = scopeUnion(readReach, tools.EffectiveMemoryScopes(ctx, base.MemoryScopes))
		consolidation = consolidation || base.MemoryConsolidation
		includeTurns = includeTurns || base.RecallIncludeTurns
		attachTraces = attachTraces || base.RecallAttachTraces
	}
	// Unset falls back to the ordinary default (EffectiveMemoryScopes), as for
	// any agent. An explicit deny-all is within every ceiling.
	if len(def.MemoryScopes) > 0 {
		if err := subsetCeiling("memory_scopes", tools.EffectiveMemoryScopes(ctx, def.MemoryScopes), false, held, ceiling); err != nil {
			return err
		}
	}
	// A core block is read into the prompt at its own scope whatever the
	// memory_scopes say, so its scope is reach like a memory scope — and the
	// forked version's blocks are its own, memory_scopes or not.
	blockCeiling := readReach
	if base != nil {
		for _, b := range base.CoreBlocks {
			blockCeiling = scopeUnion(blockCeiling, []string{b.Scope})
		}
	}
	for _, b := range def.CoreBlocks {
		if !contains(blockCeiling, b.Scope) {
			return fmt.Errorf("core_blocks: block %q reads the %q memory scope, which is not within %s memory_scopes %s. "+
				"A new agent can only be granted authority its author holds: remove the block, or ask an operator to author this agent",
				b.Label, b.Scope, held, listOrNone(blockCeiling))
		}
	}
	for _, g := range []struct {
		field     string
		want, has bool
	}{
		{"memory_consolidation", def.MemoryConsolidation, consolidation},
		{"recall_include_turns", def.RecallIncludeTurns, includeTurns},
		{"recall_attach_traces", def.RecallAttachTraces, attachTraces},
	} {
		if g.want && !g.has {
			return fmt.Errorf("%s: this agent does not hold the %s grant itself. "+
				"A new agent can only be granted authority its author holds: remove it, or ask an operator to author this agent",
				g.field, g.field)
		}
	}
	return nil
}

func ceilSQL(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	// sql_scopes grants write: a scope held read-only is outside the ceiling.
	ceiling := tools.PolicyOf(ctx).SqlMem.WritableScopes()
	if base != nil {
		ceiling = scopeUnion(ceiling, tools.EffectiveSqlScopes(ctx, base.SqlScopes))
	}
	if len(def.SqlScopes) == 0 {
		return nil // the ordinary default, as for any agent
	}
	return subsetCeiling("sql_scopes", tools.EffectiveSqlScopes(ctx, def.SqlScopes), false, held, ceiling)
}

// historyScopes is a history_scope list as a run resolves it, with the legacy
// "any" read as the "global" it stands for. Whether "global" survives is the
// run's to decide (it is stripped for a non-admin principal); here it is only
// compared, and an author without it cannot grant it.
func historyScopes(ctx context.Context, declared []string) []string {
	eff := tools.EffectiveHistoryScopes(ctx, declared)
	out := make([]string, 0, len(eff))
	for _, s := range eff {
		if s == "any" {
			s = "global"
		}
		out = append(out, s)
	}
	return out
}

func ceilHistory(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	ceiling := tools.PolicyOf(ctx).History.Scopes
	if base != nil {
		ceiling = scopeUnion(ceiling, historyScopes(ctx, base.HistoryScope))
	}
	return subsetCeiling("history_scope", historyScopes(ctx, def.HistoryScope), len(def.HistoryScope) == 0, held, ceiling)
}

// ---- channels ----

// isTeamLocalRef reports whether an ACL or skills entry names one of a team's
// own resources: "./" and ONE well-formed local name. Anything else spelled
// with "./" — "./*", "./a/b", "./../x", "./" — is not a name a team can
// declare, so it is judged like any other entry rather than waved through.
func isTeamLocalRef(entry string) bool {
	name, ok := teamgraph.LocalRef(strings.TrimSpace(entry))
	return ok && teamgraph.ValidateLocalName(name) == nil
}

func ceilChannels(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	pol := tools.PolicyOf(ctx).Channel
	var basePub, baseSub []string
	if base != nil {
		basePub, baseSub = base.Channels.Publish, base.Channels.Subscribe
	}
	for _, side := range []struct {
		name        string
		child, base []string
	}{
		{"publish", def.Channels.Publish, basePub},
		{"subscribe", def.Channels.Subscribe, baseSub},
	} {
		all, allow := pol.GrantsFor(side.name)
		if all {
			continue
		}
		ceiling := scopeUnion(allow, side.base)
		// channelAllowed is the Channel tool's own matcher. Fed an allowlist
		// entry as the name, it is also a sound coverage test: an entry is
		// either exact or a trailing "/*" prefix, so "team/x/*" passes against
		// "team/*" (every name it matches, "team/*" matches) and "team/*"
		// fails against "team/x".
		if v, bad := firstUncovered(side.child, func(c string) bool {
			return isTeamLocalRef(c) || channelAllowed(c, ceiling)
		}); bad {
			return refuseCapability("channels."+side.name, v, false, held, ceiling)
		}
	}
	return nil
}

// ---- interruption ----

// interruptionKinds is a kinds list with its default, as the Interruption tool
// reads it.
func interruptionKinds(kinds []string) []string {
	if len(kinds) == 0 {
		return []string{"question"}
	}
	return kinds
}

func ceilInterruption(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	if !def.Interruption.Enabled && len(def.Interruption.Kinds) == 0 {
		return nil
	}
	pol := tools.PolicyOf(ctx).Interruption.Load()
	var ceiling []string
	if pol.Enabled {
		ceiling = interruptionKinds(pol.Kinds)
	}
	if base != nil && (base.Interruption.Enabled || len(base.Interruption.Kinds) > 0) {
		ceiling = scopeUnion(ceiling, interruptionKinds(base.Interruption.Kinds))
	}
	if len(ceiling) == 0 {
		return fmt.Errorf("interruption: this agent may not raise interruptions itself, so it cannot grant that to a new agent. " +
			"Remove the interruption block, or ask an operator to author this agent")
	}
	return subsetCeiling("interruption.kinds", interruptionKinds(def.Interruption.Kinds), len(def.Interruption.Kinds) == 0, held, ceiling)
}

// ---- evaluation ----

// evaluationCovered reports whether the ceiling grants scope c. submit_any is
// the Evaluation tool's short-circuit for every submit, so it covers each
// narrower submit_* scope; every other scope needs itself.
func evaluationCovered(c string, ceiling []string) bool {
	return contains(ceiling, c) || (strings.HasPrefix(c, "submit_") && contains(ceiling, "submit_any"))
}

func ceilEvaluation(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	// Unset is submit_self, which reaches only the new agent's own run.
	if len(def.EvaluationScopes) == 0 {
		return nil
	}
	// The run's policy is already effective (submit_self when its definition
	// declared none).
	ceiling := tools.PolicyOf(ctx).Evaluation.Scopes
	if base != nil {
		ceiling = scopeUnion(ceiling, tools.EffectiveEvaluationScopes(base.EvaluationScopes))
	}
	eff := tools.EffectiveEvaluationScopes(def.EvaluationScopes)
	if v, bad := firstUncovered(eff, func(c string) bool { return evaluationCovered(c, ceiling) }); bad {
		return refuseCapability("evaluation_scopes", v, false, held, ceiling)
	}
	return nil
}

// ---- crypto ----

// ceilCrypto holds crypto_scopes to the author's own key scopes. There is no
// default to wave through — unset grants no key scope — so an unset or deny-all
// list is within every ceiling, and any scope named must be one the author's
// run holds (or the forked version already had).
func ceilCrypto(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	eff := tools.EffectiveCryptoScopes(def.CryptoScopes)
	if len(eff) == 0 {
		return nil
	}
	// The run's policy is already effective (and, for a sub-agent, already
	// narrowed by its parent's).
	ceiling := tools.PolicyOf(ctx).Crypto.Scopes
	if base != nil {
		ceiling = scopeUnion(ceiling, tools.EffectiveCryptoScopes(base.CryptoScopes))
	}
	return subsetCeiling("crypto_scopes", eff, false, held, ceiling)
}

// ---- shell credentials ----

// ceilShellCreds holds shell_credentials to the author's own narrowing (RFC
// EW). Unset is the widest value here, every credential the operator lists,
// so the rule is the reverse of crypto's: an author whose shell narrows
// nothing may store anything; one narrowed to some names may store only a
// subset of them, plus what the forked version already allowed, and may not
// leave the field unset unless the forked version did.
//
// The ceiling is the UNION of the author's names and the forked version's,
// as for crypto_scopes, and that is deliberate (RFC EW D7, the owner's call
// after the post-merge review): a fork may keep what it forked, and an author
// may add what its own shell holds, so one definition can hold names that
// were each reachable separately. The operator's two env lists stay the hard
// ceiling, and the stricter forms (the author's list alone, or the
// intersection) were considered and declined.
func ceilShellCreds(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	author := tools.PolicyOf(ctx).ShellCreds
	if !author.Set {
		return nil
	}
	forked := tools.ShellCredsPolicyValue{Set: true}
	if base != nil {
		forked = tools.ShellCredsFor(base.ShellCredentials)
	}
	if !forked.Set {
		return nil // the forked version narrowed nothing, and a fork may keep that
	}
	ceiling := scopeUnion(author.Names, forked.Names)
	if def.ShellCredentials == nil {
		return refuseCapability("shell_credentials", "every credential the operator lists", true, held, ceiling)
	}
	return subsetCeiling("shell_credentials", *def.ShellCredentials, false, held, ceiling)
}

// ---- git remote ----

// ceilGitRemote holds git_remote to the Git network operations the author's
// own run may perform (RFC FB). Unset and [] grant nothing, so like crypto
// there is no default to wave through: an empty list is within every
// ceiling, and each operation listed must be one the author's run may
// perform (already narrowed by its parent's, for a sub-agent) or the forked
// version already had. The ceiling is that union, as for crypto_scopes and
// shell_credentials (RFC EW D7). An author that cannot fetch may list
// nothing; one that can fetch but not push may not list push.
func ceilGitRemote(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	if len(def.GitRemote) == 0 {
		return nil
	}
	ceiling := tools.PolicyOf(ctx).GitRemote.Ops()
	if base != nil {
		ceiling = scopeUnion(ceiling, tools.GitRemoteFor(base.GitRemote).Ops())
	}
	return subsetCeiling("git_remote", def.GitRemote, false, held, ceiling)
}

// ---- workspace ----

// ceilWorkspace holds workspace to the author's own run's grant (RFC GW). A
// block that grants nothing is within every ceiling, except for its routes.
// Otherwise each operation, open, share and route must be held by the
// author's run (already narrowed by its parent, for a sub-agent) or by the
// forked version, and the engine requirement may be no weaker than the
// weakest of theirs. The ceiling is that union, as for git_remote; a side
// that grants nothing adds nothing to it, so a forked version without the
// grant cannot loosen the author's engine requirement or lend its routes.
//
// Routes are held even on a block that grants nothing: stored, they would
// otherwise be kept by a later fork that adds ops, judged against a union
// with this version, by an author that never held them.
func ceilWorkspace(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	if def.Workspace == nil {
		return nil
	}
	ceiling := tools.PolicyOf(ctx).Workspace
	if base != nil {
		ceiling = unionWorkspace(ceiling, tools.WorkspaceFor(base.Workspace))
	}
	if err := subsetCeiling("workspace.egress", def.Workspace.Egress, false, held, ceiling.Egress); err != nil {
		return err
	}
	if !def.Workspace.Grants() {
		return nil
	}
	if err := subsetCeiling("workspace.ops", def.Workspace.Ops, false, held, ceiling.Ops()); err != nil {
		return err
	}
	if def.Workspace.Open && !ceiling.Open {
		return fmt.Errorf("workspace.open: true is not within %s workspace grant, which may not open workspaces. "+
			"A new agent can only be granted authority its author holds: remove it, or ask an operator to author this agent", held)
	}
	if def.Workspace.Share && !ceiling.Share {
		return fmt.Errorf("workspace.share: true is not within %s workspace grant, which may not share workspaces. "+
			"A new agent can only be granted authority its author holds: remove it, or ask an operator to author this agent", held)
	}
	if def.Workspace.Expose && !ceiling.Expose {
		return fmt.Errorf("workspace.expose: true is not within %s workspace grant, which may not expose workspace ports. "+
			"A new agent can only be granted authority its author holds: remove it, or ask an operator to author this agent", held)
	}
	if def.Workspace.Templates && !ceiling.Templates {
		return fmt.Errorf("workspace.templates: true is not within %s workspace grant, which may not save or delete templates. "+
			"A new agent can only be granted authority its author holds: remove it, or ask an operator to author this agent", held)
	}
	if !ceiling.Grants() || !ceiling.AcceptsEngineRequirement(def.Workspace.Engine) {
		return fmt.Errorf("workspace.engine: %q accepts a less isolating engine than %s workspace.engine %q. "+
			"A new agent can only be granted authority its author holds: require that engine or a stricter one, or ask an operator to author this agent",
			def.Workspace.Engine, held, ceiling.Engine)
	}
	return nil
}

// unionWorkspace is the wider of two grants, for a ceiling: the operations,
// open, share, templates and routes either holds, and the weaker engine requirement — of
// the sides that grant anything, since a side granting nothing names no
// requirement.
func unionWorkspace(a, b tools.WorkspacePolicyValue) tools.WorkspacePolicyValue {
	switch {
	case !a.Grants():
		return b
	case !b.Grants():
		return a
	}
	out := tools.WorkspacePolicyValue{
		Read: a.Read || b.Read, Write: a.Write || b.Write, Exec: a.Exec || b.Exec,
		Open: a.Open || b.Open, Share: a.Share || b.Share, Expose: a.Expose || b.Expose, Templates: a.Templates || b.Templates, Engine: b.Engine,
		Egress: slices.Clone(a.Egress),
	}
	for _, r := range b.Egress {
		if !slices.Contains(out.Egress, r) {
			out.Egress = append(out.Egress, r)
		}
	}
	if a.AcceptsEngineRequirement(b.Engine) {
		out.Engine = a.Engine
	}
	return out
}

// ---- statement methods ----

// ceilStatementMethods holds statement_methods for a definition authored from
// inside a run. A new one may list only methods the author's own run may
// stamp; a fork only methods the version it forks already held — narrowing a
// lineage, never widening it, even with methods the author itself holds,
// because the grant is what lets a body's writes skip review (auto-assert)
// and the lineage's operator chose which bodies get it. Unset and [] grant
// nothing, so there is no default to wave through.
func ceilStatementMethods(ctx context.Context, def mergedDef, base *mergedDef) error {
	if len(def.StatementMethods) == 0 {
		return nil
	}
	ceiling, held := tools.PolicyOf(ctx).StatementMethods, "this agent's own"
	if base != nil {
		ceiling, held = base.StatementMethods, "the forked version's"
	}
	return subsetCeiling("statement_methods", def.StatementMethods, false, held, ceiling)
}

// ---- skills ----

// skillEntry splits a `skills:` entry into its sign and pattern, the syntax
// skillmatch reads. ok is false for an empty entry.
func skillEntry(entry string) (neg bool, pat string, ok bool) {
	entry = strings.TrimSpace(entry)
	switch {
	case entry == "":
		return false, "", false
	case entry[0] == '-':
		neg, pat = true, entry[1:]
	case entry[0] == '+':
		pat = entry[1:]
	default:
		pat = entry
	}
	return neg, pat, pat != ""
}

func skillMatchAll(pat string) bool { return pat == "*" || pat == "**" }

// skillPatternCovered reports whether every skill name the positive pattern p
// matches, q matches too. It is deliberately conservative — it may refuse a
// narrowing a cleverer test would prove, never pass a widening: q matches
// everything; or they are the same pattern; or p names one skill and q (by the
// skill matcher itself) matches it; or q is "X/**" and p sits under "X/".
func skillPatternCovered(p, q string) bool {
	switch {
	case skillMatchAll(q), p == q:
		return true
	case !strings.ContainsAny(p, "*?["):
		return skillmatch.Allowed([]string{q}, p)
	case strings.HasSuffix(q, "/**"):
		return strings.HasPrefix(p, strings.TrimSuffix(q, "**"))
	}
	return false
}

// skillsWithin reports whether every skill the allowlist child permits, the
// allowlist ceiling permits too (skillmatch semantics: a negative always wins;
// with any positive the list is a whitelist; empty allows all).
func skillsWithin(child, ceiling []string) bool {
	if len(ceiling) == 0 || skillmatch.DeniesAll(child) {
		return true
	}
	if skillmatch.DeniesAll(ceiling) {
		return false
	}
	var childNeg, childPos []string
	for _, e := range child {
		if neg, pat, ok := skillEntry(e); ok {
			if neg {
				childNeg = append(childNeg, pat)
			} else {
				childPos = append(childPos, pat)
			}
		}
	}
	// A whitelist of only the team's own skills permits no skill outside the
	// team, whatever the ceiling denies.
	onlyLocal := len(childPos) > 0
	for _, p := range childPos {
		if !isTeamLocalRef(p) {
			onlyLocal = false
		}
	}
	if onlyLocal {
		return true
	}
	var ceilPos []string
	for _, e := range ceiling {
		neg, pat, ok := skillEntry(e)
		if !ok {
			continue
		}
		// Every skill the ceiling denies, the child must deny too.
		if neg && !contains(childNeg, pat) {
			return false
		}
		if !neg {
			ceilPos = append(ceilPos, pat)
		}
	}
	if len(ceilPos) == 0 {
		return true // the ceiling only denies, and the child denies all of it
	}
	for _, q := range ceilPos {
		if skillMatchAll(q) {
			return true
		}
	}
	// A whitelist ceiling needs a whitelist child, each entry within it.
	if len(childPos) == 0 {
		return false
	}
	for _, p := range childPos {
		// A team's own skill ("./<name>") is judged as a new skill of its own,
		// under this same allowlist, when the team is authored; it names no
		// skill outside the team.
		if isTeamLocalRef(p) {
			continue
		}
		covered := false
		for _, q := range ceilPos {
			if skillPatternCovered(p, q) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func ceilSkills(ctx context.Context, def mergedDef, base *mergedDef, held string) error {
	author := tools.PolicyOf(ctx).Skill.Effective()
	if skillsWithin(def.Skills, author) || (base != nil && skillsWithin(def.Skills, base.Skills)) {
		return nil
	}
	if len(def.Skills) == 0 {
		return fmt.Errorf("skills: left unset, the new agent would be allowed every skill, which is not within %s skills %s. "+
			"A new agent can only be granted authority its author holds: set skills explicitly to patterns within that list, "+
			"or ask an operator to author this agent", held, listOrNone(author))
	}
	return fmt.Errorf("skills: %v allows skills that %s skills %s does not. "+
		"A new agent can only be granted authority its author holds: keep every deny entry of that list and narrow the "+
		"allow entries to patterns within it, or ask an operator to author this agent", def.Skills, held, listOrNone(author))
}

// ---- the *_def_scopes grants ----

// defScopeCovered reports whether a def-authoring grant c, held by a new agent
// named childName, is within the grants ceiling held by an author named
// ceilingSelf. The vocabulary is the def families' own: "any", "self" and
// "named:<pattern>" ("descendants" is refused at write and grants nothing).
// "self" is relative to who holds it, so both sides are read as the name they
// stand for. A value outside the vocabulary is covered only by
// the same value: it grants nothing today, and a future one must not pass
// because nothing here knew it.
func defScopeCovered(c, childName string, ceiling []string, ceilingSelf string) bool {
	grantsAll := contains(ceiling, "any")
	p, isNamed := strings.CutPrefix(c, "named:")
	if c == "self" && childName != "" {
		p, isNamed = childName, true
	}
	switch {
	case c == "any":
		return grantsAll
	case !isNamed:
		return contains(ceiling, c)
	case grantsAll:
		return true
	}
	for _, q := range ceiling {
		qp, ok := strings.CutPrefix(q, "named:")
		if q == "self" && ceilingSelf != "" {
			qp, ok = ceilingSelf, true
		}
		if !ok {
			continue
		}
		switch {
		case qp == p:
			return true
		case !strings.Contains(p, "*"):
			if matchNamedScope(qp, p) {
				return true
			}
		case strings.HasSuffix(qp, "/**"):
			if strings.HasPrefix(p, strings.TrimSuffix(qp, "**")) {
				return true
			}
		}
	}
	return false
}

func ceilDefScopes(ctx context.Context, name string, def mergedDef, base *mergedDef, held string) error {
	ad, sd := tools.PolicyOf(ctx).AgentDef, tools.ScheduleDefPolicy(ctx)
	sc, aa := tools.A2AServerCardDefPolicy(ctx), tools.A2AAgentDefPolicy(ctx)
	var b mergedDef
	if base != nil {
		b = *base
	}
	for _, g := range []struct {
		field                string
		child, ceil, baseVal []string
		selfName             string
	}{
		{field: "agent_def_scopes", child: def.AgentDefScopes, ceil: ad.Scopes, selfName: ad.SelfName, baseVal: b.AgentDefScopes},
		{field: "schedule_def_scopes", child: def.ScheduleDefScopes, ceil: sd.Scopes, selfName: sd.SelfName, baseVal: b.ScheduleDefScopes},
		{field: "a2a_server_card_def_scopes", child: def.A2AServerCardDefScopes, ceil: sc.Scopes, selfName: sc.SelfName, baseVal: b.A2AServerCardDefScopes},
		{field: "a2a_agent_def_scopes", child: def.A2AAgentDefScopes, ceil: aa.Scopes, selfName: aa.SelfName, baseVal: b.A2AAgentDefScopes},
		// Volume grants have no "self": a volume has no agent identity.
		{field: "volume_def_scopes", child: def.VolumeDefScopes, ceil: tools.PolicyOf(ctx).VolumeDef.Scopes, baseVal: b.VolumeDefScopes},
	} {
		childName := name
		if g.field == "volume_def_scopes" {
			childName = ""
		}
		v, bad := firstUncovered(g.child, func(c string) bool {
			// The forked version's own grants were held by that same name, so
			// its "self" means the same agent.
			return defScopeCovered(c, childName, g.ceil, g.selfName) ||
				defScopeCovered(c, childName, g.baseVal, childName)
		})
		if bad {
			return refuseCapability(g.field, v, false, held, scopeUnion(g.ceil, g.baseVal))
		}
	}
	return nil
}

// forkChangesCode reports whether a fork changes the code that runs: any
// byte change of code_body, whatever the provider (fail closed: a body the
// route does not run today may run under a later one), or a switch into or
// out of code-js (out of it, the author's prompt runs where the code did).
// Code-js is decided on the provider the run resolves, not the raw field. A
// model agent's rewritten prompt is not judged here: keeping the lineage's
// grants is the documented fork contract.
func (a *AgentDef) forkChangesCode(def, base mergedDef) bool {
	return def.Code != base.Code || a.isCodeJS(a.resolvedProvider(def)) != a.isCodeJS(a.resolvedProvider(base))
}

// resolvedProvider is the provider a run of d resolves to, the way
// config.ResolveAgentDefModel resolves it: the def's own, else the one its
// `models:` alias names, else the defaults.
func (a *AgentDef) resolvedProvider(d mergedDef) string {
	if a.Cfg == nil {
		return d.Provider
	}
	provider, _, _ := config.ExpandModelAlias(a.Cfg.Models, d.Provider, d.Model)
	if provider == "" {
		provider = a.Cfg.Defaults.Provider
	}
	return provider
}

// isCodeJS reports whether provider executes a code body: the code-js
// provider itself, or a configured provider served by its driver.
func (a *AgentDef) isCodeJS(provider string) bool {
	if provider == config.CodeJSProvider {
		return true
	}
	if a.Cfg == nil || provider == "" {
		return false
	}
	p, ok := a.Cfg.Providers[provider]
	return ok && p.Driver == config.CodeJSProvider
}

// forkerToolsCeiling holds a fork that changes the code to the forker's own
// tools, as create holds a new agent; the lineage root no longer bounds it.
// Inside a run only: the operator plane is not narrowed, and a member's call
// is held to its own tools by checkCapabilityCeiling (memberToolsCeiling).
func forkerToolsCeiling(ctx context.Context, def mergedDef) error {
	if tools.IsOffRunPlane(ctx) || len(def.Tools) == 0 {
		return nil
	}
	callerTools := tools.PolicyOf(ctx).Tools
	if callerTools == nil {
		return fmt.Errorf("caller's effective tools not on ctx (runtime misconfiguration); refuse rather than risk silent widening")
	}
	if err := assertToolsSubset(toolGrants(def.Tools), callerTools); err != nil {
		return fmt.Errorf("%w; a fork that changes the code is held to this agent's own tools — narrow tools, or ask an operator to author this agent", err)
	}
	return nil
}

// decodeBase reads the stored definition a fork is made from. A definition
// that does not decode contributes nothing, which can only refuse more.
func decodeBase(raw json.RawMessage) *mergedDef {
	var d mergedDef
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &d)
	}
	return &d
}

// descendantsRefusal is a def-scope check's refusal when the caller's grant
// names "descendants" and nothing else in it authorised the name. The value
// never walked the lineage it promised and granted every name, as "any" does;
// it is refused at load and at write, and a definition stored before that is
// denied by it rather than widened. Logged, so an operator learns which gate
// a stored definition lost.
func descendantsRefusal(tool, field string, scopes []string) error {
	if !contains(scopes, "descendants") {
		return nil
	}
	slog.Warn("defscope: a stored grant names descendants, which authorises nothing", "tool", tool, "field", field)
	return fmt.Errorf("%s tool: %s: %s", tool, field, config.DescendantsUnsupported)
}

// refuseDescendantsGrant refuses an AgentDef create or fork overlay granting
// "descendants" in any *_def_scopes gate.
func refuseDescendantsGrant(ov mergedDef) error {
	for _, g := range []struct {
		field  string
		scopes []string
	}{
		{"agent_def_scopes", ov.AgentDefScopes},
		{"schedule_def_scopes", ov.ScheduleDefScopes},
		{"a2a_server_card_def_scopes", ov.A2AServerCardDefScopes},
		{"a2a_agent_def_scopes", ov.A2AAgentDefScopes},
	} {
		if contains(g.scopes, "descendants") {
			return fmt.Errorf("AgentDef tool: %s: %s", g.field, config.DescendantsUnsupported)
		}
	}
	return nil
}
