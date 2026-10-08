package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/skillmatch"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// agentdef_ceiling.go — the capability ceiling for a new agent definition.
//
// The tools ceiling alone left a hole: a definition's other fields are
// authority too. A run builds a spawned child's memory, SQL, history, channel,
// interruption, evaluation, skill, volume and def-authoring policies from the
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
// through the agent's own tools, which the tools ceiling bounds.

// authorIsOperatorPlane reports whether the author of a definition is the
// operator, authoring through an operator surface — who is the root of
// authority and is not narrowed by an agent's policies.
//
// The signal is the one the tools ceiling already relies on: every operator
// plane (the HTTP substrate surface, the gRPC substrate surface, every MCP
// operator session) stamps the wildcard tools ceiling ["*"], and no run does —
// a run stamps the concrete names of the tools it resolved. It is paired with
// "not inside a run" so that a run is always narrowed, whatever reaches its
// ctx. IsSubstrateOperator is not used: only the HTTP plane stamps it, so it
// would narrow an operator authoring over MCP or gRPC.
//
// Anything else — a run, a sub-agent, a ctx no plane stamped — is narrowed by
// the policies on ctx, and a policy that is absent grants nothing, so a
// non-empty capability field fails closed exactly as the tools ceiling does.
func authorIsOperatorPlane(ctx context.Context) bool {
	if tools.RunID(ctx) != "" {
		return false
	}
	for _, t := range tools.AgentTools(ctx) {
		if t == "*" {
			return true
		}
	}
	return false
}

// checkCapabilityCeiling refuses a definition granting a capability its author
// does not hold. base is the stored version a fork is made from (nil at
// create): what it already holds a fork may keep, so a fork can narrow a
// lineage but never widen it beyond what its author or the version it forks
// held.
func (a *AgentDef) checkCapabilityCeiling(ctx context.Context, name string, def mergedDef, base *mergedDef) error {
	if authorIsOperatorPlane(ctx) {
		return nil
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
	pol := tools.VolumePolicy(ctx)
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
	pol := tools.MemoryPolicy(ctx)
	ceiling := pol.AllowedScopes
	consolidation, includeTurns, attachTraces := pol.Consolidation, pol.RecallIncludeTurns, pol.RecallAttachTraces
	if base != nil {
		ceiling = scopeUnion(ceiling, tools.EffectiveMemoryScopes(ctx, base.MemoryScopes))
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
	blockCeiling := ceiling
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
	ceiling := tools.SqlMemPolicy(ctx).AllowedScopes
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
	ceiling := tools.HistoryPolicy(ctx).Scopes
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
	pol := tools.ChannelPolicy(ctx)
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
	pol := tools.InterruptionPolicy(ctx)
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
	ceiling := tools.EvaluationPolicy(ctx).Scopes
	if base != nil {
		ceiling = scopeUnion(ceiling, tools.EffectiveEvaluationScopes(base.EvaluationScopes))
	}
	eff := tools.EffectiveEvaluationScopes(def.EvaluationScopes)
	if v, bad := firstUncovered(eff, func(c string) bool { return evaluationCovered(c, ceiling) }); bad {
		return refuseCapability("evaluation_scopes", v, false, held, ceiling)
	}
	return nil
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
	author := tools.SkillPolicy(ctx).Patterns
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
// ceilingSelf. The vocabulary is the def families' own: "any", "descendants"
// (which grants as "any" does today — see checkScopeForName), "self" and
// "named:<pattern>". "self" is relative to who holds it, so both sides are read
// as the name they stand for. A value outside the vocabulary is covered only by
// the same value: it grants nothing today, and a future one must not pass
// because nothing here knew it.
func defScopeCovered(c, childName string, ceiling []string, ceilingSelf string) bool {
	grantsAll := contains(ceiling, "any") || contains(ceiling, "descendants")
	p, isNamed := strings.CutPrefix(c, "named:")
	if c == "self" && childName != "" {
		p, isNamed = childName, true
	}
	switch {
	case c == "any" || c == "descendants":
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
	ad, sd := tools.AgentDefPolicy(ctx), tools.ScheduleDefPolicy(ctx)
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
		{field: "volume_def_scopes", child: def.VolumeDefScopes, ceil: tools.VolumeDefPolicy(ctx).Scopes, baseVal: b.VolumeDefScopes},
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

// decodeBase reads the stored definition a fork is made from. A definition
// that does not decode contributes nothing, which can only refuse more.
func decodeBase(raw json.RawMessage) *mergedDef {
	var d mergedDef
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &d)
	}
	return &d
}
