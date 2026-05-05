package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// loadSetWithSkills builds a skills.Set from a temp directory of
// (name, frontmatter-tools, body) tuples. Helper for the tests below.
func loadSetWithSkills(t *testing.T, defs []struct {
	Name         string
	AllowedTools []string
	Body         string
}) *skills.Set {
	t.Helper()
	root := t.TempDir()
	for _, d := range defs {
		dir := filepath.Join(root, d.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		fm := "---\nname: " + d.Name + "\n"
		if len(d.AllowedTools) > 0 {
			fm += "allowed-tools:\n"
			for _, tn := range d.AllowedTools {
				fm += "  - " + tn + "\n"
			}
		}
		fm += "---\n" + d.Body
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(fm), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set, err := skills.LoadSet(root)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	return set
}

// Happy path: agent's tools cover the skill's needs; tool returns the body.
func TestSkillTool_HappyPath(t *testing.T) {
	set := loadSetWithSkills(t, []struct {
		Name         string
		AllowedTools []string
		Body         string
	}{
		{Name: "voice-applier", AllowedTools: []string{"Read", "Skill"}, Body: "VOICE BODY"},
	})
	tool := &SkillTool{Set: set}
	ctx := tools.WithAgentTools(context.Background(), []string{"Read", "Skill", "HTTP"})

	res, err := tool.Execute(ctx, json.RawMessage(`{"name":"voice-applier"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Errorf("expected success, got IsError: %s", res.Text)
	}
	if !strings.Contains(res.Text, "VOICE BODY") {
		t.Errorf("Text missing skill body: %q", res.Text)
	}
}

// Subset check: skill needs Edit, agent doesn't grant it. Refused.
// EMPIRICAL: removing the subset check makes this test fail (the body
// would be returned successfully despite the missing tool).
func TestSkillTool_RefusesWideningSkill(t *testing.T) {
	set := loadSetWithSkills(t, []struct {
		Name         string
		AllowedTools []string
		Body         string
	}{
		{Name: "writer-skill", AllowedTools: []string{"Read", "Write", "Edit"}, Body: "X"},
	})
	tool := &SkillTool{Set: set}
	// Agent only has Read.
	ctx := tools.WithAgentTools(context.Background(), []string{"Read"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"name":"writer-skill"}`))
	if !res.IsError {
		t.Error("expected IsError when skill widens agent's tool set")
	}
	if !strings.Contains(res.Text, "Write") || !strings.Contains(res.Text, "Edit") {
		t.Errorf("error should name the missing tools: %q", res.Text)
	}
	if strings.Contains(res.Text, "Read") {
		t.Errorf("error should NOT mention Read (agent already has it): %q", res.Text)
	}
}

// Glob composition (literal-vs-glob): skill literal `mcp__brave__search`
// covered by agent glob `mcp__brave__*` → allowed. Mirrors the static-
// path check in resolveSkills, ensuring both code paths agree.
func TestSkillTool_GlobCoversSkillLiteral(t *testing.T) {
	set := loadSetWithSkills(t, []struct {
		Name         string
		AllowedTools []string
		Body         string
	}{
		{Name: "search-skill", AllowedTools: []string{"mcp__brave__search"}, Body: "OK"},
	})
	tool := &SkillTool{Set: set}
	ctx := tools.WithAgentTools(context.Background(), []string{"mcp__brave__*"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"name":"search-skill"}`))
	if res.IsError {
		t.Errorf("agent glob should cover skill literal: %s", res.Text)
	}
}

// Skill broader than agent: skill `mcp__brave__*` glob, agent has only
// the literal `mcp__brave__search`. The skill demands wider access; refused.
func TestSkillTool_RefusesBroaderGlob(t *testing.T) {
	set := loadSetWithSkills(t, []struct {
		Name         string
		AllowedTools []string
		Body         string
	}{
		{Name: "broad-skill", AllowedTools: []string{"mcp__brave__*"}, Body: "X"},
	})
	tool := &SkillTool{Set: set}
	ctx := tools.WithAgentTools(context.Background(), []string{"mcp__brave__search"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"name":"broad-skill"}`))
	if !res.IsError {
		t.Error("agent literal should NOT cover skill's broader glob")
	}
}

// A skill with empty allowed-tools (pure prose-guidance) attaches
// regardless of agent's tool set.
func TestSkillTool_EmptyAllowedToolsAlwaysAllowed(t *testing.T) {
	set := loadSetWithSkills(t, []struct {
		Name         string
		AllowedTools []string
		Body         string
	}{
		{Name: "guidance", AllowedTools: nil, Body: "GUIDANCE"},
	})
	tool := &SkillTool{Set: set}
	// Even an agent with NO tools at all should see this skill.
	ctx := tools.WithAgentTools(context.Background(), nil)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"name":"guidance"}`))
	if res.IsError {
		t.Errorf("zero-tool skill should attach to any agent: %s", res.Text)
	}
	if !strings.Contains(res.Text, "GUIDANCE") {
		t.Errorf("body missing: %q", res.Text)
	}
}

// Unknown skill: hint with available names so the model can recover.
func TestSkillTool_UnknownSkillHints(t *testing.T) {
	set := loadSetWithSkills(t, []struct {
		Name         string
		AllowedTools []string
		Body         string
	}{
		{Name: "alpha", AllowedTools: nil, Body: "x"},
		{Name: "beta", AllowedTools: nil, Body: "y"},
	})
	tool := &SkillTool{Set: set}
	ctx := tools.WithAgentTools(context.Background(), nil)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"name":"does-not-exist"}`))
	if !res.IsError {
		t.Error("expected IsError for unknown skill")
	}
	if !strings.Contains(res.Text, "alpha") || !strings.Contains(res.Text, "beta") {
		t.Errorf("hint should list available skills: %q", res.Text)
	}
}

// Nil Set means LOOMCYCLE_SKILLS_ROOT is unset (or direct test
// construction); refuse with a clear runtime-misconfiguration message.
func TestSkillTool_NilSetReturnsConfigError(t *testing.T) {
	tool := &SkillTool{Set: nil}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"name":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "LOOMCYCLE_SKILLS_ROOT") {
		t.Errorf("expected config error, got %+v", res)
	}
}

// Empty Set is what main.go actually constructs when
// LOOMCYCLE_SKILLS_ROOT is unset (skills.LoadSet("") returns a non-nil
// empty Set). Without a fast-path here, the operator would see
// "unknown skill 'foo'" — true but unhelpful — instead of the
// LOOMCYCLE_SKILLS_ROOT hint.
func TestSkillTool_EmptySetReturnsConfigError(t *testing.T) {
	emptySet, err := skills.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	tool := &SkillTool{Set: emptySet}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"name":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "LOOMCYCLE_SKILLS_ROOT") {
		t.Errorf("expected config error, got %+v", res)
	}
}

// Missing/whitespace name surfaces as IsError with a hint.
func TestSkillTool_MissingName(t *testing.T) {
	tool := &SkillTool{Set: loadSetWithSkills(t, []struct {
		Name         string
		AllowedTools []string
		Body         string
	}{{Name: "x", Body: "y"}})}
	ctx := tools.WithAgentTools(context.Background(), nil)

	res, _ := tool.Execute(ctx, json.RawMessage(`{}`))
	if !res.IsError || !strings.Contains(res.Text, "name") {
		t.Errorf("expected missing-name error, got %+v", res)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"name":"   "}`))
	if !res.IsError {
		t.Error("whitespace name should be treated as missing")
	}
}

// Malformed JSON is recoverable — IsError, not a Go error.
func TestSkillTool_MalformedJSON(t *testing.T) {
	tool := &SkillTool{Set: loadSetWithSkills(t, []struct {
		Name         string
		AllowedTools []string
		Body         string
	}{{Name: "x", Body: "y"}})}

	res, err := tool.Execute(context.Background(), json.RawMessage(`{not json`))
	if err != nil {
		t.Fatalf("hard error from malformed JSON: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError on malformed JSON")
	}
}
