package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/skills"
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
func (s *SkillTool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	// Two paths land here that look like "no skills available":
	//   1. Set == nil — direct construction without a loader (defensive,
	//      not reachable from main.go which always passes a non-nil set).
	//   2. Set is non-nil but empty — main.go's path when
	//      LOOMCYCLE_SKILLS_ROOT is unset (skills.LoadSet("") returns a
	//      non-nil empty Set so callers can always Get without a guard).
	// Both produce the same operator-facing diagnostic: "set
	// LOOMCYCLE_SKILLS_ROOT to use this tool" — distinguishing them in
	// the error text would only matter to a unit test, never an operator.
	if s.Set == nil || len(s.Set.Names()) == 0 {
		return tools.Result{
			IsError: true,
			Text:    "Skill tool: no skills configured (set LOOMCYCLE_SKILLS_ROOT and populate <root>/<name>/SKILL.md)",
		}, nil
	}

	var in skillInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Result{IsError: true, Text: fmt.Sprintf("invalid input JSON: %s", err)}, nil
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return tools.Result{IsError: true, Text: "missing required field: name"}, nil
	}

	sk, ok := s.Set.Get(in.Name)
	if !ok {
		// Hint with the available names so the model can recover. Cap
		// at a reasonable count so a misconfigured root with hundreds
		// of skills doesn't flood the tool_result.
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
		return tools.Result{IsError: true, Text: fmt.Sprintf("unknown skill %q%s", in.Name, hint)}, nil
	}

	// SECURITY: enforce skill.allowed-tools ⊆ agent.allowed_tools at
	// tool-call time. Mirrors the config-load check in resolveSkills.
	// Agent tools are pulled from ctx (see tools.WithAgentTools) so the
	// SkillTool struct stays per-server, not per-run.
	agentTools := tools.AgentTools(ctx)
	if widening := skillToolsExceedingAgent(sk.AllowedTools, agentTools); len(widening) > 0 {
		return tools.Result{
			IsError: true,
			Text: fmt.Sprintf(
				"skill %q requires tools %v not granted by this agent's allowed_tools — skills cannot widen the agent's tool set",
				in.Name, widening,
			),
		}, nil
	}

	return tools.Result{Text: sk.Body}, nil
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
