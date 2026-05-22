package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
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

// TestSkillTool_ResolvesDBActiveOverStatic verifies the v0.8.22
// DB-first resolution behaviour. A promoted SkillDef row must
// override the same-named static SKILL.md body.
func TestSkillTool_ResolvesDBActiveOverStatic(t *testing.T) {
	// Static set carries one entry.
	set := loadSetWithSkills(t, []struct {
		Name         string
		AllowedTools []string
		Body         string
	}{
		{Name: "shared-skill", AllowedTools: []string{"Read"}, Body: "STATIC BODY"},
	})
	// Store contains a promoted SkillDef row for the same name
	// with a DIFFERENT body.
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := tools.WithAgentTools(context.Background(), []string{"Read"})

	skillDefTool := &SkillDef{Store: s, Set: set}
	dctx := tools.WithSkillDefPolicy(ctx, tools.SkillDefPolicyValue{Scopes: []string{"any"}})
	dctx = tools.WithRunIdentity(dctx, tools.RunIdentityValue{AgentID: "a_seed"})
	res, _ := skillDefTool.Execute(dctx, json.RawMessage(`{"op":"fork","name":"shared-skill","overlay":{"body":"DB BODY"},"promote":true}`))
	if res.IsError {
		t.Fatalf("seed fork+promote: %s", res.Text)
	}

	// Skill tool with Store wired: should return DB body.
	skillTool := &SkillTool{Set: set, Store: s}
	res, _ = skillTool.Execute(ctx, json.RawMessage(`{"name":"shared-skill"}`))
	if res.IsError {
		t.Fatalf("Skill lookup: %s", res.Text)
	}
	if res.Text != "DB BODY" {
		t.Errorf("body = %q, want DB BODY (DB-active should override static)", res.Text)
	}

	// Skill tool without Store: should fall back to static body.
	staticOnly := &SkillTool{Set: set}
	res, _ = staticOnly.Execute(ctx, json.RawMessage(`{"name":"shared-skill"}`))
	if res.IsError {
		t.Fatalf("Skill lookup (static-only): %s", res.Text)
	}
	if res.Text != "STATIC BODY" {
		t.Errorf("static-only body = %q, want STATIC BODY", res.Text)
	}
}

// TestSkillTool_SubstrateOnlyHintsAtAvailable confirms the registry-first
// deployment story: when LOOMCYCLE_SKILLS_ROOT is unset (Set==nil or
// empty) BUT the substrate has skills registered, asking for an unknown
// name returns an error that points the operator at the substrate names
// — NOT at "set LOOMCYCLE_SKILLS_ROOT" which would be wrong guidance.
// Mirrors the JobEmber deployment where every skill ships via
// /v1/_skilldef create at boot.
func TestSkillTool_SubstrateOnlyHintsAtAvailable(t *testing.T) {
	emptySet, err := skills.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer store.Close()

	// Seed two skills via SkillDef create + promote. Mirrors the
	// substrate write path JobEmber uses on first boot.
	ctx := tools.WithAgentTools(context.Background(), []string{"Read"})
	dctx := tools.WithSkillDefPolicy(ctx, tools.SkillDefPolicyValue{Scopes: []string{"any"}})
	dctx = tools.WithRunIdentity(dctx, tools.RunIdentityValue{AgentID: "a_seed"})
	skillDefTool := &SkillDef{Store: store, Set: emptySet}
	for _, name := range []string{"position-relevance-filtering", "voice-applier"} {
		body := `{"op":"create","name":"` + name + `","overlay":{"body":"body of ` + name + `"},"promote":true}`
		res, _ := skillDefTool.Execute(dctx, json.RawMessage(body))
		if res.IsError {
			t.Fatalf("seed %s: %s", name, res.Text)
		}
	}

	// Skill tool: substrate wired, no static skills. Ask for a name
	// that ISN'T in the substrate.
	skillTool := &SkillTool{Set: emptySet, Store: store}
	res, _ := skillTool.Execute(ctx, json.RawMessage(`{"name":"does-not-exist"}`))
	if !res.IsError {
		t.Fatalf("expected IsError for unknown skill; got %+v", res)
	}
	// Must NOT mention LOOMCYCLE_SKILLS_ROOT — that would mislead a
	// registry-first operator into reverting their deployment model.
	if strings.Contains(res.Text, "LOOMCYCLE_SKILLS_ROOT") {
		t.Errorf("error text leaks misleading LOOMCYCLE_SKILLS_ROOT guidance for registry-first deployment: %s", res.Text)
	}
	// Must surface the substrate names so the model can recover.
	if !strings.Contains(res.Text, "position-relevance-filtering") || !strings.Contains(res.Text, "voice-applier") {
		t.Errorf("error text should list substrate-registered skills, got: %s", res.Text)
	}
	if !strings.Contains(res.Text, "substrate") {
		t.Errorf("error text should mention the substrate source, got: %s", res.Text)
	}
}

// TestSkillTool_NoSourcesConfigured exercises the path where neither
// the substrate nor the static set has any skills. The error message
// should point at BOTH paths — substrate first (the modern default)
// and the static path second (legacy).
func TestSkillTool_NoSourcesConfigured(t *testing.T) {
	emptySet, err := skills.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer store.Close()

	tool := &SkillTool{Set: emptySet, Store: store}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"name":"anything"}`))
	if !res.IsError {
		t.Fatalf("expected IsError; got %+v", res)
	}
	if !strings.Contains(res.Text, "/v1/_skilldef") {
		t.Errorf("error should point at the substrate path, got: %s", res.Text)
	}
	if !strings.Contains(res.Text, "LOOMCYCLE_SKILLS_ROOT") {
		t.Errorf("error should also keep the static-path hint for legacy operators, got: %s", res.Text)
	}
}
