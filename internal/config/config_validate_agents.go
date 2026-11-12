package config

// The blocks validate checks, in its order; see validate.

import (
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/promptinject"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/skillmatch"
)

// validateAgentRouting checks one agent's name, routing, model settings and
// provider, search and tier overrides. Part of validate's agents loop.
func validateAgentRouting(c *Config, name string, agent AgentDef, known, enabledSearch map[string]bool) error {
	// Agent names share the RFC BA `/`-grouped grammar with skill names
	// (segments of [A-Za-z0-9_-] joined by `/`; no glob/`..`/leading slash),
	// so a bundled/preset agent like `doc/manager` is valid but a malformed
	// key (glob char, `..`) fails loud at load rather than at first spawn.
	if err := agents.ValidateName(name); err != nil {
		return fmt.Errorf("agents: %w", err)
	}
	// Tier-based resolution and explicit pin are mutually
	// exclusive — pinning a model and asking for a tier is
	// ambiguous, and silently picking one would surprise the
	// next reader of the yaml.
	hasPin := agent.Provider != "" || agent.Model != ""
	hasTier := agent.Tier != ""
	if err := ValidateRoutingMode(agent.Provider, agent.Model, agent.Tier); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := ValidateAgentRouting(agent.Routing, agent.Provider, agent.Model); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := c.CheckModelKind(agent.Model, ModelKindChat); err != nil {
		return fmt.Errorf("agent %q: model: %w", name, err)
	}
	if !hasPin && !hasTier {
		// Back-compat path: agents without either fall back
		// to defaults.model — same as v0.5.x behaviour.
		if c.Defaults.Model == "" {
			// An agent that declares ONLY capability grants is almost
			// never a new agent — it is an operator overlay meant to
			// widen a bundled one, layered onto a stack that does not
			// contain the bundle. Merging then produces this half-built
			// agent, and the bare size/model complaint sends the reader
			// looking at their overlay instead of at LOOMCYCLE_PRESETS.
			if looksLikeAnOverrideOnly(agent) {
				return fmt.Errorf("agent %q: no model, no tier, and no defaults.model — this declares only capability grants, "+
					"so it reads as an override of an agent that no earlier config layer defines. Check that the bundle providing "+
					"%q is in LOOMCYCLE_PRESETS / LOOMCYCLE_CONFIG_DIR, and that the layer carrying the override comes after it", name, name)
			}
			return fmt.Errorf("agent %q: no model, no tier, and no defaults.model", name)
		}
	}
	if !validEffortLevels[agent.Effort] {
		return fmt.Errorf("agent %q: invalid effort %q (want one of low/medium/high or empty)", name, agent.Effort)
	}
	if err := agent.Sampling.Validate(); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := agent.MemoryRerank.Validate(); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := c.CheckAgentDecision(agent.Decision); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := agent.ToolChoice.Validate(); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := agent.OutputFormat.Validate(); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := agent.Compaction.Validate(); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := c.CheckAgentTranscription(agent.Transcription); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := c.CheckAgentSpeech(agent.Speech); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := agent.Context.Validate(); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if hasTier && !validTierNames[agent.Tier] {
		return fmt.Errorf("agent %q: invalid tier %q (want one of low/middle/high)", name, agent.Tier)
	}
	// Per-agent provider override.
	for i, p := range agent.Providers {
		if !known[p] {
			return fmt.Errorf("agent %q: providers[%d]: unknown provider %q", name, i, p)
		}
	}
	// Per-agent web-search fallback list (RFC BB) — every entry must be an
	// enabled top-level search_providers key.
	for i, id := range agent.SearchProviders {
		if !enabledSearch[id] {
			return fmt.Errorf("agent %q: search_providers[%d]: %q is not an enabled search_providers entry", name, i, id)
		}
	}
	// Per-agent tier-candidate override.
	for tierName, candidates := range agent.Models {
		if !validTierNames[tierName] {
			return fmt.Errorf("agent %q: models.%s: unknown tier", name, tierName)
		}
		for i, cand := range candidates {
			if err := validateTierCandidate(cand, c.Models, known); err != nil {
				return fmt.Errorf("agent %q: models.%s[%d]: %v", name, tierName, i, err)
			}
		}
	}
	return nil
}

// validateAgentMemory checks one agent's memory scopes, core blocks, memory
// bounds, prompt placeholders, SQL scopes and volumes. Part of validate's
// agents loop.
func validateAgentMemory(c *Config, name string, agent AgentDef) error {
	// Memory tool: validate memory_scopes are known scope strings. An
	// unset list is not an error: it resolves at policy time to the
	// caller's own data (tools.EffectiveMemoryScopes). Non-empty must be a
	// subset of validMemoryScopes; the message is generated from that set.
	for i, sc := range agent.MemoryScopes {
		if !validMemoryScopes[sc] {
			return fmt.Errorf("agent %q: memory_scopes[%d]: unknown scope %q (want one of: %s)", name, i, sc, memoryScopeNames())
		}
	}
	// RFC BL P1: validate core_blocks. Each block backs a Memory key
	// `core/<label>`, so the label must be a clean single segment and the
	// scope must be known. Duplicate (scope,label) pairs are rejected — two
	// blocks fighting over one key is always a config bug.
	seenBlock := make(map[string]bool, len(agent.CoreBlocks))
	for i, b := range agent.CoreBlocks {
		label := strings.TrimSpace(b.Label)
		if label == "" {
			return fmt.Errorf("agent %q: core_blocks[%d]: label is required", name, i)
		}
		if strings.ContainsAny(label, "/ \t\n") {
			return fmt.Errorf("agent %q: core_blocks[%d]: label %q must be a single segment (no slashes or whitespace)", name, i, b.Label)
		}
		if b.Scope == "" || !validCoreBlockScopes[b.Scope] {
			return fmt.Errorf("agent %q: core_blocks[%d]: scope %q invalid (want one of: agent, user, tenant)", name, i, b.Scope)
		}
		if b.LimitBytes < 0 {
			return fmt.Errorf("agent %q: core_blocks[%d]: limit_bytes must be >= 0", name, i)
		}
		key := b.Scope + "/" + label
		if seenBlock[key] {
			return fmt.Errorf("agent %q: core_blocks: duplicate (scope=%s, label=%s)", name, b.Scope, label)
		}
		seenBlock[key] = true
	}
	if agent.MemoryInjectMaxTokens < 0 {
		return fmt.Errorf("agent %q: memory_inject_max_tokens must be >= 0", name)
	}
	if agent.MaxContextTokens < 0 {
		return fmt.Errorf("agent %q: max_context_tokens must be >= 0", name)
	}
	if agent.MemoryIndexMaxBytes < 0 {
		return fmt.Errorf("agent %q: memory_index_max_bytes must be >= 0", name)
	}
	switch agent.MemoryRoots {
	case "", "lazy", "force", "suppress":
	default:
		return fmt.Errorf("agent %q: memory_roots must be one of lazy|force|suppress (got %q)", name, agent.MemoryRoots)
	}
	// RFC BL P1: a {{memory:<variant>}} placeholder in the system prompt must
	// reference a recognised variant — a typo is caught here, at boot, rather
	// than silently rendering nothing at run time.
	if unknown := promptinject.UnknownVariants(agent.SystemPrompt); len(unknown) > 0 {
		return fmt.Errorf("agent %q: unknown {{memory:%s}} placeholder in system prompt (recognised: %s)",
			name, unknown[0], strings.Join(promptinject.AllVariants(), ", "))
	}
	// A {{tool:<Tool>.<op>}} placeholder must name a ref on the read-only
	// ALLOWLIST. Rejecting here is the whole reason the allowlist is safe to
	// state as a bound: an operator who writes {{tool:Bash}} is told at boot
	// that it is not callable, instead of shipping a prompt that silently
	// expands to nothing (or, in a design without the allowlist, one that runs
	// a mutating tool on every run-entry, sub-agent spawn and resume).
	if unknown := promptinject.UnknownToolRefs(agent.SystemPrompt); len(unknown) > 0 {
		return fmt.Errorf("agent %q: {{tool:%s}} in system prompt is not an allowlisted read-only tool call (allowed: %s)",
			name, unknown[0], strings.Join(promptinject.AllToolRefs(), ", "))
	}
	// The ARGUMENT form — {{tool:<Tool>:<argument>}} — has its own, smaller
	// set, because it is the one family that can reach the network. Same
	// reason to reject at boot, one step sharper: {{tool:Bash:rm -rf /}}
	// matches the family's pattern on purpose so it is REFUSED here rather
	// than sitting in a prompt that looks wired and is not.
	if unknown := promptinject.UnknownToolCalls(agent.SystemPrompt); len(unknown) > 0 {
		return fmt.Errorf("agent %q: {{tool:%s:...}} in system prompt is not a tool the argument form may name (allowed: %s)",
			name, unknown[0], strings.Join(promptinject.AllToolCalls(), ", "))
	}
	// Same posture for a document ref: the placeholder pattern matches
	// loosely on purpose, so a ref with no path is REFUSED loudly here rather
	// than left silently literal in a prompt the operator believes is wired.
	// Whether the document EXISTS is deliberately not checked — it can be
	// created, moved or removed after config load, so a boot-time check would
	// be a false guarantee; a missing document renders nothing at run time.
	if bad := promptinject.MalformedDocRefs(agent.SystemPrompt); len(bad) > 0 {
		return fmt.Errorf("agent %q: {{document:%s}} in system prompt is not a usable reference "+
			"(want a path or id, optionally #heading — e.g. {{document:/specs/launch#Risks}})", name, bad[0])
	}
	// RFC AA SQL Memory: validate sql_scopes are known scope strings.
	// Empty = no SQL access (default-deny, enforced at runtime, not here).
	// Non-empty must be a subset of validSqlScopes. Keep the message
	// generated FROM that set: it read "agent, user, run" long after
	// `tenant` was added, so a correct config looked rejected.
	for i, sc := range agent.SqlScopes {
		if !validSqlScopes[sc] {
			return fmt.Errorf("agent %q: sql_scopes[%d]: unknown scope %q (want one of: %s)", name, i, sc, sqlScopeNames())
		}
	}
	// RFC AH Phase 1: a per-agent `volumes` binding must reference a
	// declared top-level `volumes:` entry (mirrors how tools
	// validates against registered tools — operator-yaml is the floor,
	// the model can never enlarge its own filesystem access). An agent
	// that declares NO volumes is implicitly bound to [default] and so
	// needs no validation here.
	for i, vn := range agent.Volumes {
		if _, ok := c.Volumes[vn]; !ok {
			return fmt.Errorf("agent %q: volumes[%d]: unknown volume %q (declare it in the top-level volumes: map)", name, i, vn)
		}
	}
	return nil
}

// validateAgentGrants records one agent's advisories and checks its quotas,
// memory backend, channels and the tool grants. Part of validate's agents
// loop.
func validateAgentGrants(c *Config, name string, agent AgentDef) error {
	// Non-fatal: "tool in tools but its capability gate is unset"
	// advisories (Memory/memory_scopes, Evaluation/evaluation_scopes,
	// Channel/channels, Interruption/interruption.enabled). Accumulated and
	// logged once at boot, never fatal — an operator may legitimately list a
	// tool they haven't gated yet.
	c.Warnings = append(c.Warnings, agentGateWarnings(name, agent)...)
	c.Warnings = append(c.Warnings, shellCredentialWarnings(name, agent, c.Env)...)
	c.Warnings = append(c.Warnings, gitRemoteWarnings(name, agent, c.Env)...)
	if !c.Decision.Configured() {
		for _, tool := range agent.Tools {
			if tool == "Decision" {
				c.Warnings = append(c.Warnings, fmt.Sprintf("agent %q: tools includes Decision but no decision: block is declared, so the tool is not offered to the agent; declare decision: (default, and optionally models), or drop the tool", name))
			}
		}
	}
	c.Warnings = append(c.Warnings, sqlMemConfigWarnings(name, agent, c.Storage.SqlMemEnabled)...)
	// Context/compaction settings that cannot take effect for this agent's
	// mode. The failure they catch is the one this advisory channel exists
	// for: a knob that is set, looks right, and is never read.
	c.Warnings = append(c.Warnings, contextCompactionWarnings(name, agent)...)
	if agent.MemoryQuotaBytes < 0 {
		return fmt.Errorf("agent %q: memory_quota_bytes must be >= 0", name)
	}
	if agent.SqlQuotaBytes < 0 {
		return fmt.Errorf("agent %q: sql_quota_bytes must be >= 0", name)
	}
	// Memory backend routing (RFC I MR-3b): a static agent that names
	// a memory_backend must reference a declared memory_backends key
	// OR the built-in "inprocess"/"default" literals. We only validate
	// static-yaml→static-yaml references: an agent may legitimately
	// name a backend that exists only as a dynamic MemoryBackendDef
	// (created at runtime, absent at config load), so an unresolved
	// name is NOT a load error — the Memory tool degrades to the
	// operator-default backend at runtime (see memory.go backend()).
	if agent.MemoryBackend != "" &&
		agent.MemoryBackend != "inprocess" &&
		agent.MemoryBackend != "default" {
		if _, ok := c.MemoryBackends[agent.MemoryBackend]; !ok {
			// Lenient: only fail when the static map is non-empty and
			// the name is clearly a typo against it. When the operator
			// declares no static backends at all, assume the name
			// targets a dynamic Def and let the runtime fallback cover
			// a miss.
			if len(c.MemoryBackends) > 0 {
				return fmt.Errorf("agent %q: memory_backend %q is not a declared memory_backends entry (or \"inprocess\"/\"default\")", name, agent.MemoryBackend)
			}
		}
	}
	if agent.RetryAttempts != nil && *agent.RetryAttempts < 0 {
		return fmt.Errorf("agent %q: retry_attempts must be >= 0 (0 = explicitly disable retries; omit to use user_tier default)", name)
	}
	// Channel tool (v0.8.4): every entry in publish/subscribe must
	// reference a declared channel (exact match) OR be a "<prefix>/*"
	// wildcard whose prefix matches at least one declared channel.
	// Wildcard with no matches at config-load is rejected so an
	// operator typo doesn't silently disable an ACL.
	for i, ch := range agent.Channels.Publish {
		if err := validateAgentChannelEntry(c.Channels, ch); err != nil {
			return fmt.Errorf("agent %q: channels.publish[%d]: %w", name, i, err)
		}
	}
	for i, ch := range agent.Channels.Subscribe {
		if err := validateAgentChannelEntry(c.Channels, ch); err != nil {
			return fmt.Errorf("agent %q: channels.subscribe[%d]: %w", name, i, err)
		}
	}
	// AgentDef tool (v0.8.5): validate agent_def_scopes entries.
	// Closed set: "self" / "named:<name>" / "any".
	for i, sc := range agent.AgentDefScopes {
		if err := validateAgentDefScope(sc); err != nil {
			return fmt.Errorf("agent %q: agent_def_scopes[%d]: %w", name, i, err)
		}
	}
	// The other families' gates take the same vocabulary; only the value
	// that granted more than it said is refused here, so a gate that loads
	// today keeps loading.
	for _, g := range []struct {
		field  string
		scopes []string
	}{
		{"schedule_def_scopes", agent.ScheduleDefScopes},
		{"a2a_server_card_def_scopes", agent.A2AServerCardDefScopes},
		{"a2a_agent_def_scopes", agent.A2AAgentDefScopes},
	} {
		for i, sc := range g.scopes {
			if sc == "descendants" {
				return fmt.Errorf("agent %q: %s[%d]: %s", name, g.field, i, DescendantsUnsupported)
			}
		}
	}
	// RFC BA: `skill_def_scopes` is REMOVED — skill authoring is now
	// governed by the unified `skills:` pattern allowlist. Reject a stale
	// key loudly rather than silently ignoring an operator's intended gate.
	if len(agent.SkillDefScopes) > 0 {
		return fmt.Errorf("agent %q: `skill_def_scopes` was removed (RFC BA) — express skill-authoring limits as `skills:` patterns instead (e.g. skills: [doc/*] to allow authoring only doc/* skills, skills: [-*] to forbid all)", name)
	}
	// RFC BA: validate the `skills:` pattern allowlist (globs + optional
	// +/- sign). A malformed entry (bad char, `..`, empty) fails loud at
	// load rather than silently mis-gating skill access at runtime.
	for i, sc := range agent.Skills {
		if err := skillmatch.ValidatePattern(sc); err != nil {
			return fmt.Errorf("agent %q: skills[%d]: %w", name, i, err)
		}
	}
	if err := ValidateAgentHooks(agent); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if _, err := retentionmode.Parse(agent.Retention); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := ValidateEphemeralVolumes(agent.EphemeralVolumes); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	// VolumeDef tool (RFC AH Phase 2a): validate volume_def_scopes
	// entries. Closed set: "named:<volume-name>" / "any".
	for i, sc := range agent.VolumeDefScopes {
		if err := validateVolumeDefScope(sc); err != nil {
			return fmt.Errorf("agent %q: volume_def_scopes[%d]: %w", name, i, err)
		}
	}
	// Evaluation tool (v0.8.5): validate evaluation_scopes entries.
	// Closed set as documented on AgentDef.EvaluationScopes.
	for i, sc := range agent.EvaluationScopes {
		if !validEvaluationScopes[sc] {
			return fmt.Errorf("agent %q: evaluation_scopes[%d]: unknown scope %q (want one of: submit_self, submit_siblings, submit_descendants, submit_any, read_any)", name, i, sc)
		}
	}
	// Crypto tool: crypto_scopes is a closed set, and an unknown value is
	// refused here rather than dropped at run time, where it would grant
	// nothing while the operator believed it granted something.
	if err := ValidateCryptoScopes(agent.CryptoScopes); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := ValidateShellCredentials(agent.ShellCredentials); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := ValidateGitRemote(agent.GitRemote); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	if err := ValidateStatementMethods(agent.Provider, agent.StatementMethods); err != nil {
		return fmt.Errorf("agent %q: %w", name, err)
	}
	// History tool (RFC BE): validate history_scope entries against the
	// owner-scope vocabulary. "any" is accepted (legacy alias for
	// "global"); the retired "siblings"/"descendants"/"named:<n>" values
	// are rejected with a message pointing at the new vocabulary.
	for i, sc := range agent.HistoryScope {
		if validHistoryScopes[sc] {
			continue
		}
		return fmt.Errorf("agent %q: history_scope[%d]: unknown scope %q (want one of: self, user, tenant, global)", name, i, sc)
	}
	return nil
}
