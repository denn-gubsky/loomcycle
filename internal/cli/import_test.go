package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildSampleClaudeRepo materialises a minimal .claude/ tree under
// tempDir for the import tests to consume. Returns the .claude/ path.
func buildSampleClaudeRepo(t *testing.T, tempDir string) string {
	t.Helper()
	claude := filepath.Join(tempDir, ".claude")
	if err := os.MkdirAll(filepath.Join(claude, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(claude, "skills", "yaml-fence"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(claude, "commands"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// One agent with mcp__ tools → fires credentials heuristic.
	if err := os.WriteFile(filepath.Join(claude, "agents", "coder.md"), []byte(`---
name: coder
model: claude-sonnet-4-6
tools:
  - Read
  - Edit
  - mcp__github__createIssue
---
You are a coder.
`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// One skill (single-file).
	if err := os.WriteFile(filepath.Join(claude, "skills", "yaml-fence", "SKILL.md"), []byte(`---
allowed-tools: Read
---
Skill body.
`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// One mcp.json with a stdio entry + a registries field.
	if err := os.WriteFile(filepath.Join(claude, "mcp.json"), []byte(`{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}
    }
  },
  "registries": {
    "remote": {"url": "https://example/registry"}
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// One slash command → SKIPPED.
	if err := os.WriteFile(filepath.Join(claude, "commands", "snapshot.md"), []byte("body"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	return claude
}

func TestRunImport_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunImport(nil, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("expected exit 2 on no args, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "Usage") {
		t.Errorf("expected usage message: %q", stderr.String())
	}
}

func TestRunImport_UnknownSubverb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"banana"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("expected exit 2, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "unknown import source") {
		t.Errorf("expected unknown-source error: %q", stderr.String())
	}
}

func TestRunImport_MissingFrom(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("expected exit 2, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "--from") {
		t.Errorf("expected --from required error: %q", stderr.String())
	}
}

func TestRunImport_DefaultDryRun(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", rc, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"dry-run report",
		"AGENTS (1)",
		"coder",
		"SKILLS (1)",
		"yaml-fence",
		"MCP SERVERS (1)",
		"github [stdio]",
		"SKIPPED (1)",
		"snapshot.md",
		"would import 1 agents, 1 skills, 1 mcp servers",
		"Re-run with --write",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default-dry-run output missing %q", want)
		}
	}
}

func TestRunImport_ReportOnly(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude, "--report-only"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", rc, stderr.String())
	}
	out := stdout.String()
	// --report-only is one-line; no AGENTS section header.
	if strings.Contains(out, "AGENTS (") {
		t.Errorf("--report-only should not print section headers: %q", out)
	}
	if !strings.Contains(out, "would import 1 agents") {
		t.Errorf("expected summary line: %q", out)
	}
}

func TestRunImport_JSONFormat(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude, "--json"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("--json should produce JSON, got: %.80s", out)
	}
	for _, want := range []string{`"root":`, `"agents":`, `"mcp_servers":`, `"skipped":`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON output missing %q", want)
		}
	}
}

func TestRunImport_DryRunDiff(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	target := filepath.Join(td, "loomcycle.yaml")
	// Write a placeholder target.
	if err := os.WriteFile(target, []byte("# placeholder\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude,
		"--dry-run", "--diff=" + target}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", rc, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Dry-run diff",
		target,
		"mcp_servers: additions",
		"agents: additions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output missing %q", want)
		}
	}
	// Target file should NOT have been modified.
	got, _ := os.ReadFile(target)
	if string(got) != "# placeholder\n" {
		t.Errorf("dry-run modified the target file: %q", got)
	}
}

func TestRunImport_Write(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	target := filepath.Join(td, "loomcycle.yaml")
	skillsDest := filepath.Join(td, "skills")

	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude,
		"--write",
		"--diff=" + target,
		"--skills-dest=" + skillsDest}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", rc, stderr.String())
	}

	// Target yaml should now contain the entries.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	s := string(got)
	for _, want := range []string{"mcp_servers:", "github:", "transport: stdio", "agents:", "coder:"} {
		if !strings.Contains(s, want) {
			t.Errorf("target missing %q:\n%s", want, s)
		}
	}

	// Skill file should have been copied.
	skillPath := filepath.Join(skillsDest, "yaml-fence", "SKILL.md")
	if _, err := os.Stat(skillPath); err != nil {
		t.Errorf("skill not copied to %s: %v", skillPath, err)
	}
}

func TestRunImport_WriteRefusesClobberWithoutForce(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	target := filepath.Join(td, "loomcycle.yaml")

	// First write succeeds.
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude,
		"--write", "--diff=" + target,
		"--skills-dest=" + filepath.Join(td, "skills1")}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("first --write should succeed, got %d (stderr: %s)", rc, stderr.String())
	}

	// Second write without --force should refuse on collision.
	stdout.Reset()
	stderr.Reset()
	rc = RunImport([]string{"claude-code", "--from=" + claude,
		"--write", "--diff=" + target,
		"--skills-dest=" + filepath.Join(td, "skills2")}, &stdout, &stderr)
	if rc != 1 {
		t.Errorf("expected exit 1 (operational fail) on collision, got %d (stderr: %s)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("expected collision error: %q", stderr.String())
	}
}

func TestRunImport_WriteForceAllowsClobber(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	target := filepath.Join(td, "loomcycle.yaml")
	skillsDest := filepath.Join(td, "skills")

	// Initial write.
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude,
		"--write", "--diff=" + target,
		"--skills-dest=" + skillsDest}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("first --write should succeed, got %d", rc)
	}

	// --force re-run should succeed.
	stdout.Reset()
	stderr.Reset()
	rc = RunImport([]string{"claude-code", "--from=" + claude,
		"--write", "--force", "--diff=" + target,
		"--skills-dest=" + skillsDest}, &stdout, &stderr)
	if rc != 0 {
		t.Errorf("--write --force should succeed, got %d (stderr: %s)", rc, stderr.String())
	}
}

func TestRunImport_EmitRecipesRequiresEnv(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	// Ensure env is unset for this test.
	prev := os.Getenv("LOOMCYCLE_MCP_RECIPES_ROOT")
	os.Unsetenv("LOOMCYCLE_MCP_RECIPES_ROOT")
	defer os.Setenv("LOOMCYCLE_MCP_RECIPES_ROOT", prev)

	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude, "--emit-recipes"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("expected exit 2 (user error) on missing env, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "LOOMCYCLE_MCP_RECIPES_ROOT") {
		t.Errorf("expected env-var error: %q", stderr.String())
	}
}

func TestRunImport_EmitRecipesOverlayOnly(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	overlay := filepath.Join(td, "overlay")
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	prev := os.Getenv("LOOMCYCLE_MCP_RECIPES_ROOT")
	os.Setenv("LOOMCYCLE_MCP_RECIPES_ROOT", overlay)
	defer os.Setenv("LOOMCYCLE_MCP_RECIPES_ROOT", prev)

	target := filepath.Join(td, "loomcycle.yaml")
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude,
		"--write", "--diff=" + target,
		"--emit-recipes", "--no-yaml",
		"--skills-dest=" + filepath.Join(td, "skills")}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", rc, stderr.String())
	}

	// Overlay should have github.json.
	if _, err := os.Stat(filepath.Join(overlay, "github.json")); err != nil {
		t.Errorf("github.json not written to overlay: %v", err)
	}
	// Target yaml should NOT have mcp_servers (because --no-yaml).
	got, _ := os.ReadFile(target)
	if strings.Contains(string(got), "mcp_servers:") {
		t.Errorf("--no-yaml should have suppressed mcp_servers: %s", got)
	}
	// Target yaml SHOULD still have agents: (--no-yaml only affects mcp).
	if !strings.Contains(string(got), "agents:") {
		t.Errorf("agents: should still be written under --no-yaml: %s", got)
	}
}

func TestRunImport_NoYAMLWithoutEmitRecipes(t *testing.T) {
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude, "--no-yaml"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("expected exit 2, got %d", rc)
	}
	if !strings.Contains(stderr.String(), "only valid with --emit-recipes") {
		t.Errorf("expected --no-yaml gating error: %q", stderr.String())
	}
}

func TestRunImport_NoRecipeMatch(t *testing.T) {
	// Operator wrote `--my-flag` in their .claude/mcp.json; the
	// recipe-match path would replace with the canonical recipe
	// (losing the flag); --no-recipe-match preserves the literal port.
	td := t.TempDir()
	claude := buildSampleClaudeRepo(t, td)
	// Overwrite mcp.json with a customised entry.
	if err := os.WriteFile(filepath.Join(claude, "mcp.json"), []byte(`{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github", "--my-flag"]
    }
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	target := filepath.Join(td, "loomcycle.yaml")
	var stdout, stderr bytes.Buffer
	rc := RunImport([]string{"claude-code", "--from=" + claude,
		"--write", "--force", "--no-recipe-match",
		"--diff=" + target,
		"--skills-dest=" + filepath.Join(td, "skills")}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", rc, stderr.String())
	}
	got, _ := os.ReadFile(target)
	// The literal port should preserve --my-flag in the args.
	if !strings.Contains(string(got), "--my-flag") {
		t.Errorf("--no-recipe-match should preserve --my-flag:\n%s", got)
	}
}
