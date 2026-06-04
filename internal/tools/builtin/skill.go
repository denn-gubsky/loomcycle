package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
// be a subset of the bundling/calling agent's `allowed_tools`.
//
// Wire shape:
//
//	{
//	  "name": "voice-applier"   // a key under LOOMCYCLE_SKILLS_ROOT
//	}
//
// SECURITY: subset enforcement happens HERE, at tool-call time, against
// the agent's effective allowed_tools (which the tool receives via the
// AgentTools field at construction). A skill that demands a tool the
// current agent doesn't have is refused with an IsError tool_result —
// matching the config-load behaviour for static bundling.
type SkillTool struct {
	// Set is the loaded skill registry, populated from
	// LOOMCYCLE_SKILLS_ROOT at server boot. Shared with Approach A
	// (config-load bundling) so a skill change is visible to both
	// paths after a SIGHUP/restart.
	//
	// The calling agent's effective allowed_tools is NOT a field —
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
    "name": {"type": "string", "description": "Skill name (a directory under LOOMCYCLE_SKILLS_ROOT containing SKILL.md)."}
  },
  "required": ["name"],
  "additionalProperties": false
}`

const skillDescription = `Load a named skill's body and return it as the tool_result. ` +
	`Skills are domain-specific instruction sets stored under LOOMCYCLE_SKILLS_ROOT (frontmatter + markdown). ` +
	`Use when the model needs a specific extension that wasn't pre-bundled into its system prompt. ` +
	`The skill's allowed-tools must be a subset of the calling agent's allowed_tools — refusals are surfaced as is_error.`

type skillInput struct {
	Name string `json:"name"`
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
//     when the Store is wired. Body + allowed_tools come from the
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
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return tools.Result{IsError: true, Text: "missing required field: name"}, nil
	}

	body, allowedTools, source, err := s.resolveSkill(ctx, in.Name)
	if err != nil {
		return tools.Result{IsError: true, Text: err.Error()}, nil
	}

	// SECURITY: enforce skill.allowed-tools ⊆ agent.allowed_tools at
	// tool-call time. Mirrors the config-load check in resolveSkills.
	// Agent tools are pulled from ctx (see tools.WithAgentTools) so
	// the SkillTool struct stays per-server, not per-run.
	agentTools := tools.AgentTools(ctx)
	if widening := skillToolsExceedingAgent(allowedTools, agentTools); len(widening) > 0 {
		return tools.Result{
			IsError: true,
			Text: fmt.Sprintf(
				"skill %q (%s) requires tools %v not granted by this agent's allowed_tools — skills cannot widen the agent's tool set",
				in.Name, source, widening,
			),
		}, nil
	}

	return tools.Result{Text: body}, nil
}

// resolveSkill looks up a skill by name. Returns (body, allowed_tools,
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
			return def.Body, def.AllowedTools, "skill_def", nil
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
	return sk.Body, sk.AllowedTools, "static", nil
}

// skillToolsExceedingAgent returns the subset of skill tools that the
// agent does NOT grant (literal + glob composition via policy.Matches).
// Mirrors resolveSkills' check in internal/config so the static and
// dynamic skill paths apply the same security rule.
func skillToolsExceedingAgent(skillTools, agentTools []string) []string {
	if len(skillTools) == 0 {
		return nil
	}
	agentSet := make(map[string]bool, len(agentTools))
	for _, t := range agentTools {
		agentSet[t] = true
	}
	var widening []string
	for _, t := range skillTools {
		if !policy.Matches(t, agentSet) {
			widening = append(widening, t)
		}
	}
	return widening
}

var _ tools.Tool = (*SkillTool)(nil)
