package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeFile creates a file under dir with the given name + content. Helper for
// the table-driven tests so the surrounding signal-to-noise stays high.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestLoadSet_HappyPath: two MDs with rich frontmatter resolve cleanly into
// a populated Set with the expected fields and body. Pins the canonical
// frontmatter contract: comma-string `tools` parses, `model` survives,
// `description` stored, body becomes SystemPrompt, name from filename.
func TestLoadSet_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ats-filter.md", `---
name: ats-filter
description: Relevance filter
tools: Read, mcp__jobs__getAgentContext, Skill
model: haiku
tier: low
max_tokens: 24576
max_iterations: 64
skills: [position-relevance-filtering]
memory_scopes: [agent]
---
You are an ATS-filter agent.
`)
	writeFile(t, dir, "cv-adapter.md", `---
name: cv-adapter
description: CV adapter
allowed_tools: [Read, mcp__jobs__patchApplication]
provider: anthropic
model: claude-sonnet-4-6
---
Body for cv-adapter.
`)
	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	names := set.Names()
	want := []string{"ats-filter", "cv-adapter"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("Names() = %v, want %v", names, want)
	}

	a, ok := set.Get("ats-filter")
	if !ok {
		t.Fatal("ats-filter not found")
	}
	if a.Description != "Relevance filter" {
		t.Errorf("Description = %q, want %q", a.Description, "Relevance filter")
	}
	if a.Model != "haiku" {
		t.Errorf("Model = %q, want haiku", a.Model)
	}
	if a.Tier != "low" {
		t.Errorf("Tier = %q, want low", a.Tier)
	}
	if a.MaxTokens != 24576 {
		t.Errorf("MaxTokens = %d, want 24576", a.MaxTokens)
	}
	if a.MaxIterations != 64 {
		t.Errorf("MaxIterations = %d, want 64", a.MaxIterations)
	}
	wantTools := []string{"Read", "mcp__jobs__getAgentContext", "Skill"}
	if !reflect.DeepEqual(a.AllowedTools, wantTools) {
		t.Errorf("AllowedTools = %v, want %v", a.AllowedTools, wantTools)
	}
	if !reflect.DeepEqual(a.Skills, []string{"position-relevance-filtering"}) {
		t.Errorf("Skills = %v, want [position-relevance-filtering]", a.Skills)
	}
	if !reflect.DeepEqual(a.MemoryScopes, []string{"agent"}) {
		t.Errorf("MemoryScopes = %v, want [agent]", a.MemoryScopes)
	}
	if a.SystemPrompt != "You are an ATS-filter agent.\n" {
		t.Errorf("SystemPrompt = %q, want body", a.SystemPrompt)
	}

	b, ok := set.Get("cv-adapter")
	if !ok {
		t.Fatal("cv-adapter not found")
	}
	if b.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", b.Provider)
	}
	if b.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", b.Model)
	}
	wantBT := []string{"Read", "mcp__jobs__patchApplication"}
	if !reflect.DeepEqual(b.AllowedTools, wantBT) {
		t.Errorf("AllowedTools = %v, want %v", b.AllowedTools, wantBT)
	}
}

// TestLoadSet_EmptyRoot: an unset/blank root returns a non-nil empty Set so
// callers can always Get() without nil-checking. Mirrors the skills loader's
// contract for the AGENTS_ROOT-not-set deployment.
func TestLoadSet_EmptyRoot(t *testing.T) {
	set, err := LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet(\"\"): %v", err)
	}
	if set == nil {
		t.Fatal("LoadSet(\"\") returned nil Set")
	}
	if got := len(set.Names()); got != 0 {
		t.Errorf("Names() len = %d, want 0", got)
	}
	if _, ok := set.Get("anything"); ok {
		t.Errorf("Get on empty Set returned ok=true")
	}
}

// TestLoadSet_NonexistentRoot: a missing directory is an error (almost
// certainly an operator misconfiguration of LOOMCYCLE_AGENTS_ROOT).
func TestLoadSet_NonexistentRoot(t *testing.T) {
	_, err := LoadSet("/nonexistent/path/that/should/never/exist/loomcycle-test")
	if err == nil {
		t.Fatal("expected error for nonexistent root, got nil")
	}
}

// TestLoadSet_FrontmatterParseError: malformed YAML in the frontmatter
// surfaces as a wrapped error citing the file path. Operators get a
// pointer to the broken file rather than a generic parse error.
func TestLoadSet_FrontmatterParseError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "broken.md", `---
name: broken
tools: [unclosed list
---
body
`)
	_, err := LoadSet(dir)
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if !strings.Contains(err.Error(), "broken.md") {
		t.Errorf("error %q does not cite path 'broken.md'", err.Error())
	}
}

// TestLoadSet_NoClosingDelimiter: a frontmatter that opens with --- but
// never closes is an error, matching skills-loader behaviour.
func TestLoadSet_NoClosingDelimiter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "incomplete.md", `---
name: incomplete
no closing delim here
`)
	_, err := LoadSet(dir)
	if err == nil {
		t.Fatal("expected error for missing closing ---, got nil")
	}
	if !strings.Contains(err.Error(), "no closing") {
		t.Errorf("error %q does not mention 'no closing'", err.Error())
	}
}

// TestLoadSet_BodyOnly: a file without leading --- is treated as body-only.
// The agent ends up with name from the filename, all other fields zero,
// and the entire content as SystemPrompt. Tolerates ad-hoc MDs that
// haven't been written with frontmatter yet.
func TestLoadSet_BodyOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "simple.md", "Just a plain prompt body, no frontmatter.\n")
	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	a, ok := set.Get("simple")
	if !ok {
		t.Fatal("simple not found")
	}
	if a.Name != "simple" {
		t.Errorf("Name = %q, want simple", a.Name)
	}
	if a.SystemPrompt != "Just a plain prompt body, no frontmatter.\n" {
		t.Errorf("SystemPrompt = %q, want full content", a.SystemPrompt)
	}
	if len(a.AllowedTools) != 0 || a.Model != "" || a.Tier != "" {
		t.Errorf("expected zero values for non-name fields, got tools=%v model=%q tier=%q",
			a.AllowedTools, a.Model, a.Tier)
	}
}

// TestParseToolList_CommaString: the standalone helper that splits Claude
// Code's comma-string tool field. Critical because operators copy MDs
// straight from .claude/agents/ where this format is the norm.
func TestParseToolList_CommaString(t *testing.T) {
	got := ParseToolList("Read, mcp__foo__bar, Skill")
	want := []string{"Read", "mcp__foo__bar", "Skill"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseToolList = %v, want %v", got, want)
	}
	// Whitespace-only input → empty slice (not nil — the merge layer
	// distinguishes those).
	got = ParseToolList("   ,  ,  ")
	if len(got) != 0 {
		t.Errorf("ParseToolList of whitespace-only = %v, want empty", got)
	}
	// Empty input → empty slice.
	got = ParseToolList("")
	if got == nil {
		t.Errorf("ParseToolList(\"\") = nil, want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("ParseToolList(\"\") len = %d, want 0", len(got))
	}
}

// TestLoadSet_AllowedToolsWinsOverTools: when an MD has BOTH the loomcycle
// `allowed_tools:` list AND the Claude Code `tools:` comma-string, the
// list form wins (loomcycle is the consumer that demands precision).
// This pins the documented precedence rule.
func TestLoadSet_AllowedToolsWinsOverTools(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "dual.md", `---
name: dual
tools: A, B, C
allowed_tools: [X, Y]
---
body
`)
	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	a, ok := set.Get("dual")
	if !ok {
		t.Fatal("dual not found")
	}
	want := []string{"X", "Y"}
	if !reflect.DeepEqual(a.AllowedTools, want) {
		t.Errorf("AllowedTools = %v, want %v (allowed_tools should win)", a.AllowedTools, want)
	}
}

// TestLoadSet_FilenameNameMismatch: a frontmatter `name:` that disagrees
// with the filename is operator drift and refusal-to-load is the right
// surface (mirrors skills-loader behaviour). Loud errors here prevent
// "wait, why doesn't agent X load when its file is right there?"
// debugging spirals.
func TestLoadSet_FilenameNameMismatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "actual-name.md", `---
name: different-name
---
body
`)
	_, err := LoadSet(dir)
	if err == nil {
		t.Fatal("expected error for name/filename mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "different-name") || !strings.Contains(err.Error(), "actual-name") {
		t.Errorf("error %q should cite both names", err.Error())
	}
}

// TestLoadSet_SubdirAndNonMDIgnored: subdirectories under root and files
// without a .md extension are skipped silently. Operators stage auxiliary
// content (fixtures, fragments, a `.git` directory) alongside the MDs;
// those should not produce spurious agents or errors.
func TestLoadSet_SubdirAndNonMDIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "real.md", `---
name: real
---
ok
`)
	// A subdirectory with a stray .md inside (we don't recurse).
	if err := os.Mkdir(filepath.Join(dir, "fixtures"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "fixtures"), "buried.md", "ignored")
	// A non-md file at root.
	writeFile(t, dir, "README.txt", "not an agent")
	// A dot-file the loader shouldn't try to parse.
	writeFile(t, dir, ".DS_Store", "macos noise")

	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	names := set.Names()
	if !reflect.DeepEqual(names, []string{"real"}) {
		t.Errorf("Names() = %v, want [real]", names)
	}
}

// TestLoadSet_EmptyBody: a file with frontmatter but no body parses
// cleanly with an empty SystemPrompt. Useful for agents whose prompt
// is fully bundled from a system_prompt_file or from skills.
func TestLoadSet_EmptyBody(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "empty.md", `---
name: empty
tools: Read
---`)
	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	a, ok := set.Get("empty")
	if !ok {
		t.Fatal("empty not found")
	}
	if a.SystemPrompt != "" {
		t.Errorf("SystemPrompt = %q, want empty", a.SystemPrompt)
	}
	if !reflect.DeepEqual(a.AllowedTools, []string{"Read"}) {
		t.Errorf("AllowedTools = %v, want [Read]", a.AllowedTools)
	}
}

// TestParseAgent_ToolsAsYAMLList: when the Claude Code `tools:` field is
// itself a YAML list (some MDs in the wild do this even though the
// canonical Claude Code shape is a comma-string), it should also be
// accepted. Same outcome as if `allowed_tools` had been used — but
// `allowed_tools` still wins if both are present.
func TestParseAgent_ToolsAsYAMLList(t *testing.T) {
	a, err := parseAgent([]byte(`---
name: yaml-list
tools: [A, B, C]
---
body
`))
	if err != nil {
		t.Fatalf("parseAgent: %v", err)
	}
	want := []string{"A", "B", "C"}
	if !reflect.DeepEqual(a.AllowedTools, want) {
		t.Errorf("AllowedTools = %v, want %v", a.AllowedTools, want)
	}
}

// TestParseAgent_ToolsRejectsBadType: a non-string non-list `tools:` value
// (number, map, etc.) is operator-typo territory — surface a clear error
// rather than silently dropping to nil.
func TestParseAgent_ToolsRejectsBadType(t *testing.T) {
	_, err := parseAgent([]byte(`---
name: bad
tools: 42
---
body
`))
	if err == nil {
		t.Fatal("expected error for numeric tools, got nil")
	}
	if !strings.Contains(err.Error(), "tools") {
		t.Errorf("error %q should mention 'tools'", err.Error())
	}
}

// TestParseAgent_ToolsRejectsMapElement: a list containing a YAML map
// (a realistic typo when an operator confuses `models:` block syntax
// with `tools:`) must error rather than silently dropping the bad
// element. The rejection happens in the []any branch of coerceToolsField
// when the element type assertion to string fails.
func TestParseAgent_ToolsRejectsMapElement(t *testing.T) {
	_, err := parseAgent([]byte(`---
name: bad-mixed
tools:
  - { provider: foo, model: bar }
  - "Read"
---
body
`))
	if err == nil {
		t.Fatal("expected error for map element in tools list, got nil")
	}
	if !strings.Contains(err.Error(), "tools") {
		t.Errorf("error %q should mention 'tools'", err.Error())
	}
}
