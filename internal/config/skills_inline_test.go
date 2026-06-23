package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentWithSkills builds a minimal Config for resolveSkills: one agent that
// lists the given skills, with the given allowed_tools + base prompt.
func agentWithSkills(skillsRoot string, inline map[string]SkillSpec, agentAllowed, agentSkills []string) *Config {
	return &Config{
		Skills: inline,
		Agents: map[string]AgentDef{
			"a": {SystemPrompt: "BASE", AllowedTools: agentAllowed, Skills: agentSkills},
		},
		Env: Env{SkillsRoot: skillsRoot},
	}
}

func TestResolveSkills_InlineNoRoot(t *testing.T) {
	// An inline skill with NO LOOMCYCLE_SKILLS_ROOT set must resolve (the
	// fast-fail relaxation) and bake its body onto the agent prompt.
	cfg := agentWithSkills("", map[string]SkillSpec{
		"chunker": {Description: "split prose", AllowedTools: []string{"Read"}, Body: "CHUNK GUIDANCE"},
	}, []string{"Read"}, []string{"chunker"})
	if err := resolveSkills(cfg); err != nil {
		t.Fatalf("resolveSkills: %v", err)
	}
	got := cfg.Agents["a"]
	if got.SystemPromptBase != "BASE" {
		t.Errorf("SystemPromptBase = %q, want BASE (captured pre-bake)", got.SystemPromptBase)
	}
	if !strings.Contains(got.SystemPrompt, "CHUNK GUIDANCE") {
		t.Errorf("skill body not bundled into the prompt: %q", got.SystemPrompt)
	}
}

func TestResolveSkills_InlineWideningRefused(t *testing.T) {
	// A skill may not widen the agent's tool set — even inline.
	cfg := agentWithSkills("", map[string]SkillSpec{
		"writer": {AllowedTools: []string{"Edit"}, Body: "x"},
	}, []string{"Read"}, []string{"writer"})
	if err := resolveSkills(cfg); err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Errorf("err = %v, want a tool-widening refusal", err)
	}
}

func TestResolveSkills_InlineOverlaysRoot(t *testing.T) {
	// A name defined in BOTH the root dir and inline → inline wins.
	root := t.TempDir()
	skillDir := filepath.Join(root, "dup")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: dup\n---\nFROM ROOT"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := agentWithSkills(root, map[string]SkillSpec{
		"dup": {Body: "FROM INLINE"},
	}, nil, []string{"dup"})
	if err := resolveSkills(cfg); err != nil {
		t.Fatalf("resolveSkills: %v", err)
	}
	sp := cfg.Agents["a"].SystemPrompt
	if !strings.Contains(sp, "FROM INLINE") || strings.Contains(sp, "FROM ROOT") {
		t.Errorf("inline must overlay the root; got %q", sp)
	}
}

func TestResolveSkills_UnknownSkillErrors(t *testing.T) {
	// A skill in neither source → a clear error naming both.
	cfg := agentWithSkills("", nil, []string{"Read"}, []string{"nope"})
	err := resolveSkills(cfg)
	if err == nil || !strings.Contains(err.Error(), "unknown skill") {
		t.Fatalf("err = %v, want an unknown-skill error", err)
	}
	if !strings.Contains(err.Error(), "skills:") || !strings.Contains(err.Error(), "LOOMCYCLE_SKILLS_ROOT") {
		t.Errorf("error should name both sources; got %v", err)
	}
}

func TestResolveSkills_RootStillWorks(t *testing.T) {
	// Back-compat: a root-only skill (no inline) still resolves.
	root := t.TempDir()
	skillDir := filepath.Join(root, "rooted")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("ROOT BODY"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := agentWithSkills(root, nil, nil, []string{"rooted"})
	if err := resolveSkills(cfg); err != nil {
		t.Fatalf("resolveSkills: %v", err)
	}
	if !strings.Contains(cfg.Agents["a"].SystemPrompt, "ROOT BODY") {
		t.Errorf("root skill body not bundled: %q", cfg.Agents["a"].SystemPrompt)
	}
}
