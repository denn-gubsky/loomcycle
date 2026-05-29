package claudeimport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWalk_EmptyDirectory exercises the happy path of a .claude/
// directory with no subdirectories at all: the walker should produce
// a report with zero entries and zero error.
func TestWalk_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	report, err := Walk(dir, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got := len(report.Agents) + len(report.Skills) + len(report.MCPServers); got != 0 {
		t.Errorf("expected empty report, got %d entries", got)
	}
	if report.Root == "" {
		t.Error("Root should be set to the absolute path")
	}
	if !filepath.IsAbs(report.Root) {
		t.Errorf("Root should be absolute, got %q", report.Root)
	}
}

func TestWalk_NotADirectory(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Walk(f, WalkOptions{})
	if err == nil {
		t.Fatal("expected error when target is not a directory")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error should say 'not a directory', got %q", err.Error())
	}
}

func TestWalk_MissingPath(t *testing.T) {
	_, err := Walk("/tmp/this-path-does-not-exist-xyz-987", WalkOptions{})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestWalk_CommandsSkipped(t *testing.T) {
	dir := t.TempDir()
	commands := filepath.Join(dir, "commands")
	if err := os.MkdirAll(commands, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, name := range []string{"snapshot.md", "review.md"} {
		if err := os.WriteFile(filepath.Join(commands, name), []byte("body"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	report, err := Walk(dir, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got := len(report.Skipped); got != 2 {
		t.Fatalf("expected 2 skipped, got %d", got)
	}
	for _, s := range report.Skipped {
		if !strings.Contains(s.Reason, "Claude Code slash commands") {
			t.Errorf("skip reason should mention slash commands, got %q", s.Reason)
		}
	}
	// Ordering: snapshot.md should come before review.md alphabetically.
	if !strings.HasSuffix(report.Skipped[0].Path, "review.md") {
		t.Errorf("expected sorted order, got first=%s", report.Skipped[0].Path)
	}
}

func TestWalk_EmitRecipesRequiresOverlayRoot(t *testing.T) {
	dir := t.TempDir()
	_, err := Walk(dir, WalkOptions{EmitRecipes: true})
	if err == nil {
		t.Fatal("expected error: --emit-recipes without OverlayRoot")
	}
	if !strings.Contains(err.Error(), "--emit-recipes") {
		t.Errorf("error should mention --emit-recipes, got %q", err.Error())
	}
}

func TestImportReport_Render_EmptyReport(t *testing.T) {
	r := &ImportReport{Root: "/tmp/sample"}
	out := r.Render()
	for _, want := range []string{"dry-run report", "/tmp/sample", "AGENTS (0)", "SKILLS (0)", "MCP SERVERS (0)"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render missing %q in:\n%s", want, out)
		}
	}
}

func TestImportReport_Summary(t *testing.T) {
	r := &ImportReport{
		Agents:     []*AgentEntry{{Name: "a"}, {Name: "b"}},
		Skills:     []*SkillEntry{{Name: "s"}},
		MCPServers: []*MCPEntry{{Name: "github"}, {Name: "slack"}, {Name: "tavily"}},
		Skipped:    []*SkippedFile{{Path: "foo.md"}},
	}
	got := r.Summary()
	want := "would import 2 agents, 1 skills, 3 mcp servers; 1 files skipped, 0 unmapped fields, 0 warnings"
	if got != want {
		t.Errorf("Summary mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestImportReport_RenderJSON_RoundTrip(t *testing.T) {
	r := &ImportReport{
		Root:   "/abs",
		Agents: []*AgentEntry{{Name: "a", SourcePath: "/abs/agents/a.md"}},
	}
	out, err := r.RenderJSON()
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var back ImportReport
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Root != r.Root || len(back.Agents) != 1 || back.Agents[0].Name != "a" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}
