package config

import (
	"testing"
)

// hasTool reports whether tools contains name (case-insensitive, matching
// addSkillToolDefaults' dedupe).
func hasTool(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

// RFC BA: skills are on-demand — config-load no longer bakes skill bodies into
// the prompt. Instead addSkillToolDefaults auto-adds the `Skill` tool to every
// agent that may use ANY skill, so on-demand access is the default. It is
// skipped ONLY for `skills: [-*]` (deny-all). These tests replace the old
// resolveSkills bundling suite.
func TestAddSkillToolDefaults_AutoAddsForAllowingAllowlists(t *testing.T) {
	cases := []struct {
		name   string
		skills []string
		want   bool // want the Skill tool auto-added
	}{
		{"absent (nil) = allow all", nil, true},
		{"empty = allow all", []string{}, true},
		{"whitelist", []string{"doc/*"}, true},
		{"blacklist (only negatives)", []string{"-secret/*"}, true},
		{"deny-all -*", []string{"-*"}, false},
		{"deny-all -** ", []string{"-**"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Agents: map[string]AgentDef{
				"a": {Tools: []string{"Read"}, Skills: tc.skills},
			}}
			addSkillToolDefaults(cfg)
			got := hasTool(cfg.Agents["a"].Tools, "Skill")
			if got != tc.want {
				t.Errorf("Skill auto-added = %v, want %v (tools=%v)", got, tc.want, cfg.Agents["a"].Tools)
			}
		})
	}
}

// A deny-all agent (`skills: [-*]`) gets NO skill access, so the Skill tool must
// NOT be auto-added — the one case the auto-add skips. Reverting the
// skillmatch.DeniesAll guard in addSkillToolDefaults breaks this.
func TestAddSkillToolDefaults_SkipsDenyAll(t *testing.T) {
	cfg := &Config{Agents: map[string]AgentDef{
		"locked": {Tools: []string{"Read"}, Skills: []string{"-*"}},
	}}
	addSkillToolDefaults(cfg)
	if hasTool(cfg.Agents["locked"].Tools, "Skill") {
		t.Errorf("`skills: [-*]` must not get the Skill tool; tools=%v", cfg.Agents["locked"].Tools)
	}
}

// No double-add when the agent already lists Skill (case-insensitive dedupe so a
// lowercase typo doesn't add a second confusing entry).
func TestAddSkillToolDefaults_NoDoubleAdd(t *testing.T) {
	cfg := &Config{Agents: map[string]AgentDef{
		"a": {Tools: []string{"Read", "Skill"}},
		"b": {Tools: []string{"Read", "skill"}}, // lowercase typo
	}}
	addSkillToolDefaults(cfg)
	if n := countTool(cfg.Agents["a"].Tools, "Skill"); n != 1 {
		t.Errorf("agent a: Skill count = %d, want 1 (no double-add); tools=%v", n, cfg.Agents["a"].Tools)
	}
	// The lowercase variant is treated as already-present → no capital-S add.
	if hasTool(cfg.Agents["b"].Tools, "Skill") {
		t.Errorf("agent b: a lowercase `skill` must suppress the auto-add; tools=%v", cfg.Agents["b"].Tools)
	}
}

func countTool(tools []string, name string) int {
	n := 0
	for _, t := range tools {
		if t == name {
			n++
		}
	}
	return n
}

// Inline `skills:` map bodies survive config-load into cfg.Skills (they are the
// on-demand catalog now — loaded via the Skill tool, not baked into the prompt).
// A whitelisting agent additionally gets the Skill tool.
func TestLoadLayers_InlineSkillsSurviveAndAgentGetsSkillTool(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	layer := Layer{Name: "test", Data: []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  writer:
    model: claude-sonnet-4-6
    tools: [Read]
    skills: [chunker]
skills:
  chunker:
    description: split prose
    tools: [Read]
    body: CHUNK GUIDANCE
`)}
	cfg, err := LoadLayers(layer)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	sk, ok := cfg.Skills["chunker"]
	if !ok {
		t.Fatalf("inline skill `chunker` dropped from cfg.Skills")
	}
	if sk.Body != "CHUNK GUIDANCE" {
		t.Errorf("chunker body = %q, want CHUNK GUIDANCE", sk.Body)
	}
	w := cfg.Agents["writer"]
	if !hasTool(w.Tools, "Skill") {
		t.Errorf("whitelisting agent should get the auto-added Skill tool; tools=%v", w.Tools)
	}
	// On-demand: the body is NOT baked into the config-load system prompt.
	if w.SystemPrompt != "" {
		t.Errorf("skill body/note must not be baked at config-load; SystemPrompt=%q", w.SystemPrompt)
	}
}

// A malformed inline skill name (RFC BA `/`-grammar) fails config-load loudly
// rather than silently mis-registering.
func TestLoadLayers_RejectsBadInlineSkillName(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	layer := Layer{Name: "test", Data: []byte(`
skills:
  "../escape":
    body: X
`)}
	if _, err := LoadLayers(layer); err == nil {
		t.Fatalf("a `..` inline skill name should fail config-load")
	}
}
