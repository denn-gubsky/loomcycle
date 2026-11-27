package claudeimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// writeAgent is a test helper: drops a .claude/agents/<name>.md file
// under the given root and returns the absolute path.
func writeAgent(t *testing.T, root, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p := filepath.Join(dir, name+".md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

func TestWalkAgents_VanillaAgent(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "coder", `---
name: coder
description: Writes code from a spec
model: claude-sonnet-4-6
tools: "Read, Write, Edit"
---
You write code.
`)
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(report.Agents))
	}
	a := report.Agents[0]
	if a.Name != "coder" {
		t.Errorf("name: got %q want %q", a.Name, "coder")
	}
	if len(a.V0_12_7_Heuristics) != 0 {
		t.Errorf("vanilla agent should fire no heuristics, got %v", a.V0_12_7_Heuristics)
	}
	frag := a.YAMLFragment
	if !strings.Contains(frag, "# description: Writes code from a spec") {
		t.Errorf("expected description comment in fragment:\n%s", frag)
	}
	if !strings.Contains(frag, "model: claude-sonnet-4-6") {
		t.Errorf("expected model in fragment:\n%s", frag)
	}
	// Verify the body comes through verbatim.
	if !strings.Contains(frag, "You write code.") {
		t.Errorf("expected body in system_prompt block:\n%s", frag)
	}
	// Verify the emitted yaml is well-formed.
	var probe map[string]any
	if err := yaml.Unmarshal([]byte(frag), &probe); err != nil {
		t.Errorf("emitted yaml is invalid: %v\n%s", err, frag)
	}
}

func TestWalkAgents_CredentialsComment(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "jobs-searcher", `---
name: jobs-searcher
tools:
  - Read
  - mcp__jobs__getAgentContext
  - mcp__jobs__patchApplication
  - mcp__slack__postMessage
---
Body.
`)
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.Agents) != 1 {
		t.Fatalf("expected 1 agent")
	}
	a := report.Agents[0]
	if !strings.Contains(a.YAMLFragment, "# credentials: jobs, slack") {
		t.Errorf("expected credentials comment listing 'jobs, slack' in fragment:\n%s", a.YAMLFragment)
	}
	found := false
	for _, h := range a.V0_12_7_Heuristics {
		if strings.Contains(h, "credentials") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected credentials heuristic in V0_12_7_Heuristics, got %v", a.V0_12_7_Heuristics)
	}
}

func TestWalkAgents_SchedulerScopeStub(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "daily-scheduler", `---
name: daily-scheduler
model: claude-sonnet-4-6
tools: Read
---
Schedules things.
`)
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	a := report.Agents[0]
	if !strings.Contains(a.YAMLFragment, "schedule_def_scopes: [\"any\"]") {
		t.Errorf("expected schedule_def_scopes stub:\n%s", a.YAMLFragment)
	}
	if !strings.Contains(a.YAMLFragment, "scheduled-agent-runs.md") {
		t.Errorf("expected RFC E pointer comment:\n%s", a.YAMLFragment)
	}
}

func TestWalkAgents_EvolverScopeStub(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "agent-evolver", `---
name: agent-evolver
model: claude-sonnet-4-6
tools: Read
---
Evolves agents.
`)
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	a := report.Agents[0]
	if !strings.Contains(a.YAMLFragment, "agent_def_scopes: [\"self\"]") {
		t.Errorf("expected agent_def_scopes stub:\n%s", a.YAMLFragment)
	}
}

func TestWalkAgents_BothHeuristics(t *testing.T) {
	// An agent name that matches BOTH patterns (synthesized) plus
	// has mcp__ tools — exercises the triple-emission path.
	root := t.TempDir()
	writeAgent(t, root, "meta-orchestrator-agent", `---
name: meta-orchestrator-agent
model: claude-sonnet-4-6
tools:
  - Read
  - mcp__github__getRepo
---
Triple body.
`)
	report, _ := Walk(root, WalkOptions{})
	a := report.Agents[0]
	frag := a.YAMLFragment
	// All three substrate emissions present.
	if !strings.Contains(frag, "# credentials: github") {
		t.Errorf("missing credentials comment:\n%s", frag)
	}
	if !strings.Contains(frag, "schedule_def_scopes:") {
		t.Errorf("missing schedule_def_scopes:\n%s", frag)
	}
	if !strings.Contains(frag, "agent_def_scopes:") {
		t.Errorf("missing agent_def_scopes:\n%s", frag)
	}
	if len(a.V0_12_7_Heuristics) != 3 {
		t.Errorf("expected 3 heuristics, got %d: %v", len(a.V0_12_7_Heuristics), a.V0_12_7_Heuristics)
	}
}

func TestWalkAgents_UnmappedFields(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "weirdo", `---
name: weirdo
model: claude-sonnet-4-6
tools: Read
hooks:
  - on_start
output_style: learning
temperature: 0.7
some_made_up_key: 42
---
Body.
`)
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.Unmapped) != 4 {
		t.Errorf("expected 4 unmapped fields, got %d: %+v", len(report.Unmapped), report.Unmapped)
	}
	// Verify field-specific hints fire.
	hintByField := map[string]string{}
	for _, u := range report.Unmapped {
		hintByField[u.Field] = u.Hint
	}
	if !strings.Contains(hintByField["hooks"], "Claude Code-side hooks") {
		t.Errorf("hooks hint missing or wrong: %q", hintByField["hooks"])
	}
	if !strings.Contains(hintByField["output_style"], "Not part of loomcycle") {
		t.Errorf("output_style hint missing or wrong: %q", hintByField["output_style"])
	}
	if !strings.Contains(hintByField["some_made_up_key"], "no loomcycle equivalent") {
		t.Errorf("generic hint missing: %q", hintByField["some_made_up_key"])
	}
}

func TestWalkAgents_TollsAsArray(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "arrayagent", `---
name: arrayagent
tools:
  - Read
  - Write
  - "Edit"
---
Body.
`)
	report, _ := Walk(root, WalkOptions{})
	a := report.Agents[0]
	for _, tool := range []string{"Read", "Write", "Edit"} {
		if !strings.Contains(a.YAMLFragment, "- "+tool) {
			t.Errorf("expected tool %q in fragment:\n%s", tool, a.YAMLFragment)
		}
	}
}

// TestWalkAgents_ToolsCommaStringForm: the single canonical `tools` key
// also accepts Claude Code's comma-string form and coerces it into the
// emitted tools list (the array form is covered above).
func TestWalkAgents_ToolsCommaStringForm(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "comma", `---
name: comma
tools: Read, Write, Edit
---
Body.
`)
	report, _ := Walk(root, WalkOptions{})
	a := report.Agents[0]
	for _, tool := range []string{"Read", "Write", "Edit"} {
		if !strings.Contains(a.YAMLFragment, "- "+tool) {
			t.Errorf("expected tool %q in fragment:\n%s", tool, a.YAMLFragment)
		}
	}
}

func TestWalkAgents_MalformedSurvivesAsWarning(t *testing.T) {
	root := t.TempDir()
	// Frontmatter with no closing fence — buildAgentEntry returns
	// error, walker records as warning + skips.
	writeAgent(t, root, "broken", `---
name: broken
no closing fence here at all
`)
	writeAgent(t, root, "good", `---
name: good
model: claude-sonnet-4-6
---
Body.
`)
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk should not abort on per-file parse error: %v", err)
	}
	if len(report.Agents) != 1 || report.Agents[0].Name != "good" {
		t.Errorf("expected only the good agent, got %+v", report.Agents)
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(report.Warnings), report.Warnings)
	}
	if !strings.Contains(report.Warnings[0], "broken") {
		t.Errorf("warning should name the broken file: %q", report.Warnings[0])
	}
}

func TestExtractMCPCredentialServers(t *testing.T) {
	got := extractMCPCredentialServers([]string{
		"Read", "Write",
		"mcp__github__getRepo",
		"mcp__github__createIssue", // dedup
		"mcp__slack__postMessage",
		"mcp__jobs", // bare server name (no tool suffix)
	})
	want := []string{"github", "jobs", "slack"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("at %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestMatchesScheduler(t *testing.T) {
	for _, name := range []string{"daily-scheduler", "FOO-Orchestrator", "weekly-scheduling"} {
		if !matchesScheduler(name) {
			t.Errorf("expected match for %q", name)
		}
	}
	for _, name := range []string{"coder", "search-agent", "scheduler"} {
		// "scheduler" alone doesn't have the suffix shape
		// (matchesScheduler requires `-scheduler` or `scheduler-`).
		if matchesScheduler(name) && name == "coder" {
			t.Errorf("unexpected match for %q", name)
		}
	}
}

func TestMatchesEvolver(t *testing.T) {
	for _, name := range []string{"agent-evolver", "code-author", "meta-prompt", "x-meta-y"} {
		if !matchesEvolver(name) {
			t.Errorf("expected match for %q", name)
		}
	}
	if matchesEvolver("coder") {
		t.Errorf("unexpected match for coder")
	}
}

// RFC GW P15 (ruling Q6): Bash was removed from loomcycle, so an
// imported agent must not carry it — neither the bare name nor Claude
// Code's permission form Bash(<pattern>) — and the report must say so
// once per agent. It is not mapped to Workspace: that tool is inert
// without a workspace: grant, and guessing a grant would be authority
// the operator did not choose.
func TestImportAgent_DropsBashAndReportsIt(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "builder", `---
name: builder
tools: "Read, Bash, Bash(npm run test:*), Edit"
---
Body.
`)
	writeAgent(t, root, "reader", `---
name: reader
tools: "Read"
---
Body.
`)
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var builder *AgentEntry
	for _, a := range report.Agents {
		if a.Name == "builder" {
			builder = a
		}
	}
	if builder == nil {
		t.Fatalf("builder agent missing from report: %+v", report.Agents)
	}
	var probe map[string]struct {
		Tools []string `yaml:"tools"`
	}
	if err := yaml.Unmarshal([]byte(builder.YAMLFragment), &probe); err != nil {
		t.Fatalf("emitted yaml is invalid: %v\n%s", err, builder.YAMLFragment)
	}
	got := probe["builder"].Tools
	if strings.Join(got, ",") != "Read,Edit" {
		t.Errorf("tools: got %v, want [Read Edit]\n%s", got, builder.YAMLFragment)
	}
	if strings.Contains(builder.YAMLFragment, "Bash") {
		t.Errorf("fragment still names Bash:\n%s", builder.YAMLFragment)
	}

	var lines []string
	for _, w := range report.Warnings {
		if strings.Contains(w, "Bash") {
			lines = append(lines, w)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want exactly one Bash report line (for builder only), got %d: %v", len(lines), report.Warnings)
	}
	for _, want := range []string{"builder", "`Bash` was not imported", "Workspace", "workspace:"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("report line %q lacks %q", lines[0], want)
		}
	}
}
