package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/skillmatch"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/policy"
)

// SkillTool is the dynamic-discovery counterpart to the static skill
// bundling in `internal/config`. The model invokes it with a skill
// name; the tool returns the skill body as a tool_result.
//
// When to prefer Approach B (this tool) over Approach A (config-load
// bundling):
//
//   - The agent has a long menu of candidate skills and uses only a
//     few per run (pay-as-you-go beats always-bundle).
//   - The skill set evolves at runtime — operators drop new skills
//     into LOOMCYCLE_SKILLS_ROOT without restarting loomcycle.
//   - You want the model to surface "I needed cv-voice-applier here"
//     in transcripts (the tool_call event names the skill explicitly).
//
// When to prefer Approach A:
//
//   - Every referenced skill is referenced unconditionally (the
//     current jobs-search-agent shape). Bundling rides the system-
//     prompt cache; the tool path re-bills the body per call when
//     the conversation prefix doesn't include it yet.
//
// Both approaches share the same `internal/skills` loader and the
// same intersection-check semantics — a skill's `allowed-tools` must
// be a subset of the bundling/calling agent's `tools`.
//
// Wire shape:
//
//	{
//	  "name": "voice-applier"   // a key under LOOMCYCLE_SKILLS_ROOT
//	}
//
// SECURITY: subset enforcement happens HERE, at tool-call time, against
// the agent's effective tools (which the tool receives via the
// AgentTools field at construction). A skill that demands a tool the
// current agent doesn't have is refused with an IsError tool_result —
// matching the config-load behaviour for static bundling.
type SkillTool struct {
	// Set is the loaded skill registry, populated from
	// LOOMCYCLE_SKILLS_ROOT at server boot. Shared with Approach A
	// (config-load bundling) so a skill change is visible to both
	// paths after a SIGHUP/restart.
	//
	// The calling agent's effective tools is NOT a field —
	// it varies per request and is read from ctx via tools.AgentTools.
	// The HTTP server attaches it before invoking the loop.
	Set *skills.Set

	// Store is the persistence backend used by the v0.8.22 SkillDef
	// substrate. When non-nil, Execute consults SkillDefGetActive
	// before falling back to the static Set — so a promoted
	// SkillDef row overrides the on-disk SKILL.md body for the same
	// name. Nil = pre-v0.8.22 behaviour (Set only).
	Store store.Store
}

const skillInputSchema = `{
  "type": "object",
  "properties": {
    "op": {"type": "string", "enum": ["invoke", "list"], "description": "invoke (default) loads a skill body by name; list enumerates the skills this agent may use."},
    "name": {"type": "string", "description": "op=invoke: the skill name to load (supports /-grouped names like doc/redactor)."},
    "pattern": {"type": "string", "description": "op=list: optional /-glob filter (e.g. doc/*, marketing/**)."}
  },
  "additionalProperties": false
}`

const skillDescription = `Discover and load domain-specific instruction sets (skills) on demand. ` +
	`op=list enumerates the skills you may use (optionally filtered by a /-glob pattern); ` +
	`op=invoke (the default when only a name is given) loads a named skill's body as the tool_result. ` +
	`Skills you may access are governed by the agent's skills: allowlist; a skill's tools must be a subset of yours (refusals are surfaced as is_error). ` +
	`Full guide (on-demand model, skills: allowlist, /-grouping, authoring): call Context op=help topic=skills.`

type skillInput struct {
	Op      string `json:"op"`
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

// Name implements tools.Tool.
func (s *SkillTool) Name() string { return "Skill" }

// Description implements tools.Tool.
func (s *SkillTool) Description() string { return skillDescription }

// InputSchema implements tools.Tool.
func (s *SkillTool) InputSchema() json.RawMessage { return json.RawMessage(skillInputSchema) }

// Execute implements tools.Tool.
//
// Resolution order (v0.8.22):
//  1. SkillDefGetActive(name) — DB-promoted active definition wins
//     when the Store is wired. Body + tools come from the
//     DB row.
//  2. Set.Get(name) — fall back to the static filesystem-loaded
//     SKILL.md.
//
// Pre-v0.8.22 behaviour (no Store) collapses to step 2 only.
func (s *SkillTool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var in skillInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Result{IsError: true, Text: fmt.Sprintf("invalid input JSON: %s", err)}, nil
	}
	op := strings.TrimSpace(in.Op)
	if op == "" {
		op = "invoke" // back-compat: {"name": ...} with no op invokes.
	}
	policy := tools.SkillPolicy(ctx)
	switch op {
	case "list":
		return s.execList(ctx, policy, strings.TrimSpace(in.Pattern))
	case "invoke":
		return s.execInvoke(ctx, policy, strings.TrimSpace(in.Name))
	default:
		return tools.Result{IsError: true, Text: fmt.Sprintf("Skill: unknown op %q (want \"invoke\" or \"list\")", op)}, nil
	}
}

// execInvoke loads a named skill's body, gated by the agent's RFC BA `skills:`
// allowlist and the skill-tools ⊆ agent-tools invariant.
func (s *SkillTool) execInvoke(ctx context.Context, policy tools.SkillPolicyValue, name string) (tools.Result, error) {
	if name == "" {
		return tools.Result{IsError: true, Text: "missing required field: name"}, nil
	}
	// RFC BA: the agent's `skills:` allowlist gates WHICH skills it may load.
	if !skillmatch.Allowed(policy.Patterns, name) {
		return tools.Result{IsError: true, Text: fmt.Sprintf("skill %q is not permitted by this agent's `skills:` allowlist", name)}, nil
	}
	body, allowedTools, source, err := s.resolveSkill(ctx, name)
	if err != nil {
		return tools.Result{IsError: true, Text: err.Error()}, nil
	}
	// SECURITY: enforce skill.tools ⊆ agent.tools at tool-call time. Agent
	// tools are read from ctx so the SkillTool struct stays per-server. Prefer
	// the raw patterns (globs intact) over the resolved effective list so a
	// glob-scoped skill matches a glob-scoped agent (see skillToolsExceedingAgent).
	agentTools := tools.AgentTools(ctx)
	agentPatterns := tools.AgentToolPatterns(ctx)
	if widening := skillToolsExceedingAgent(allowedTools, agentTools, agentPatterns); len(widening) > 0 {
		return tools.Result{
			IsError: true,
			Text: fmt.Sprintf(
				"skill %q (%s) requires tools %v not granted by this agent's tools — skills cannot widen the agent's tool set",
				name, source, widening,
			),
		}, nil
	}
	return tools.Result{Text: body}, nil
}

// execList enumerates the skills this agent may use — the assembled catalog
// (static Set + inline skills + the caller's tenant substrate SkillDefs)
// filtered by the agent's `skills:` allowlist and an optional /-glob pattern.
func (s *SkillTool) execList(ctx context.Context, policy tools.SkillPolicyValue, pattern string) (tools.Result, error) {
	catalog := map[string]string{} // name → description
	if s.Set != nil {
		for _, n := range s.Set.Names() {
			desc := ""
			if sk, ok := s.Set.Get(n); ok {
				desc = sk.Description
			}
			catalog[n] = desc
		}
	}
	if s.Store != nil {
		if rows, err := s.Store.SkillDefListNames(ctx); err == nil {
			tid := tools.RunIdentity(ctx).TenantID
			for _, r := range rows {
				if r.TenantID != "" && r.TenantID != tid {
					continue // RFC N: only the caller's tenant + the shared "" base.
				}
				if _, ok := catalog[r.Name]; !ok {
					catalog[r.Name] = ""
				}
			}
		}
	}
	type skillEntry struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	}
	out := make([]skillEntry, 0, len(catalog))
	for n, d := range catalog {
		if !skillmatch.Allowed(policy.Patterns, n) {
			continue
		}
		if pattern != "" && !skillmatch.Allowed([]string{pattern}, n) {
			continue
		}
		out = append(out, skillEntry{Name: n, Description: d})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return okJSON(map[string]any{"skills": out})
}

// resolveSkill looks up a skill by name. Returns (body, tools,
// source, err) where `source` is "skill_def" or "static" for the
// resolution branch — useful for operator diagnostics in widening
// refusals.
func (s *SkillTool) resolveSkill(ctx context.Context, name string) (string, []string, string, error) {
	// 1. DB-promoted active SkillDef wins when Store is wired.
	if s.Store != nil {
		// RFC N: read the active pointer within the agent's own tenant
		// (from the authoritative run identity in ctx; "" = shared).
		row, err := s.Store.SkillDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, name)
		if err == nil {
			var def skillDefOverlay
			if uerr := json.Unmarshal(row.Definition, &def); uerr != nil {
				return "", nil, "", fmt.Errorf("skill %q: corrupt active def %s: %v", name, row.DefID, uerr)
			}
			if strings.TrimSpace(def.Body) == "" {
				// Shouldn't happen — SkillDef.create/fork reject empty
				// bodies — but defend against a hand-mucked DB row
				// rather than silently emitting an empty tool_result.
				return "", nil, "", fmt.Errorf("skill %q: active def %s has empty body", name, row.DefID)
			}
			return def.Body, def.Tools, "skill_def", nil
		}
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			return "", nil, "", fmt.Errorf("skill %q: lookup active def: %v", name, err)
		}
		// Fall through to static lookup.
	}

	// 2. Static filesystem fallback.
	if s.Set == nil || len(s.Set.Names()) == 0 {
		// The static set is unset — common for registry-first operators
		// who deliberately unset LOOMCYCLE_SKILLS_ROOT to force every
		// skill through the substrate. The original error here pushed
		// operators toward the static path even when their actual
		// registry (the substrate) was healthy but missing this name.
		// Differentiate: substrate registered some skills (just not
		// this one) vs neither source configured at all.
		if s.Store != nil {
			names, lerr := s.Store.SkillDefListNames(ctx)
			if lerr == nil && len(names) > 0 {
				cap := 10
				if len(names) < cap {
					cap = len(names)
				}
				avail := make([]string, 0, cap)
				for _, n := range names[:cap] {
					avail = append(avail, n.Name)
				}
				more := ""
				if len(names) > cap {
					more = ", ..."
				}
				return "", nil, "", fmt.Errorf("unknown skill %q (substrate has: %s%s)", name, strings.Join(avail, ", "), more)
			}
		}
		return "", nil, "", fmt.Errorf("Skill tool: no skills configured (push via POST /v1/_skilldef create, or set LOOMCYCLE_SKILLS_ROOT and populate <root>/<name>/SKILL.md for the static-MD path)")
	}
	sk, ok := s.Set.Get(name)
	if !ok {
		// Hint with the available names so the model can recover.
		names := s.Set.Names()
		hint := ""
		if len(names) > 0 {
			cap := 10
			if len(names) < cap {
				cap = len(names)
			}
			hint = " (available: " + strings.Join(names[:cap], ", ")
			if len(names) > cap {
				hint += ", ..."
			}
			hint += ")"
		}
		return "", nil, "", fmt.Errorf("unknown skill %q%s", name, hint)
	}
	return sk.Body, sk.Tools, "static", nil
}

// skillToolsExceedingAgent returns the subset of skill tools the agent does
// NOT grant. skill.tools ⊆ agent.tools — a skill may never widen the agent.
//
// agentPatterns is the agent's RAW tool declaration (globs preserved, e.g.
// "mcp__sandbox__*"); agentTools is the run's EFFECTIVE list (that glob already
// RESOLVED to concrete pool tools like mcp__sandbox__open). When the raw
// patterns are attached we check against them with toolCoveredByRoot — the same
// root-covers logic SkillDef authorship uses — so a skill GLOB requirement
// matches the agent's declared glob (invoke-time and create-time agree). A
// concrete agent list can never cover a broader skill glob, which is why the
// resolved list alone gave a false "not granted" for a glob-scoped skill.
//
// Fallback (no raw patterns attached — legacy callers/tests): match the skill
// tool against the effective set via policy.Matches, preserving prior behavior.
//
// (RFC BA moved subset enforcement to Skill-invoke + SkillDef authorship;
// config-load no longer performs it — a pattern allowlist can't be resolved to
// a concrete skill set there.)
func skillToolsExceedingAgent(skillTools, agentTools, agentPatterns []string) []string {
	if len(skillTools) == 0 {
		return nil
	}
	var agentSet map[string]bool
	if len(agentPatterns) == 0 {
		agentSet = make(map[string]bool, len(agentTools))
		for _, t := range agentTools {
			agentSet[t] = true
		}
	}
	var widening []string
	for _, t := range skillTools {
		if len(agentPatterns) > 0 {
			if !toolCoveredByRoot(t, agentPatterns) {
				widening = append(widening, t)
			}
			continue
		}
		if !policy.Matches(t, agentSet) {
			widening = append(widening, t)
		}
	}
	return widening
}

var _ tools.Tool = (*SkillTool)(nil)
