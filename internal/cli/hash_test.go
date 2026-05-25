package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/skills"
)

func TestRunHashAgent_PrintsSameHashAsInProcessSign(t *testing.T) {
	dir := t.TempDir()
	md := filepath.Join(dir, "researcher.md")
	body := `---
name: researcher
description: thorough investigator
allowed_tools: [Read, WebFetch]
max_tokens: 8192
max_iterations: 32
---
be thorough
`
	if err := os.WriteFile(md, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunHash([]string{"agent", md}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("RunHash exit %d; stderr=%s", code, stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(got, "sha256:") || len(got) != 71 {
		t.Errorf("malformed hash %q", got)
	}

	// Recompute the hash via the same Go code the server uses on the
	// inbound substrate path. Equality here is the load-bearing
	// guarantee — CLI hash MUST equal server hash for matching content.
	set, err := agents.LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	a, _ := set.Get("researcher")
	want := agents.Sign(agents.FromYAMLAgent(a))
	if got != want {
		t.Errorf("CLI hash %s != in-process hash %s — drift between code paths", got, want)
	}
}

func TestRunHashSkill_AcceptsSKILLMdPath(t *testing.T) {
	skillRoot := t.TempDir()
	skillDir := filepath.Join(skillRoot, "summariser")
	if err := os.Mkdir(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := filepath.Join(skillDir, "SKILL.md")
	body := `---
name: summariser
allowed-tools: [Read]
---
Summarise the input concisely.
`
	if err := os.WriteFile(md, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunHash([]string{"skill", md}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("RunHash skill exit %d; stderr=%s", code, stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("malformed: %q", got)
	}
	// Equality with in-process sign.
	set, _ := skills.LoadSet(skillRoot)
	sk, _ := set.Get("summariser")
	want := skills.Sign(skills.FromSkill(sk))
	if got != want {
		t.Errorf("CLI vs in-process drift: %s vs %s", got, want)
	}
}

func TestRunHashSkill_AcceptsSkillDirPath(t *testing.T) {
	skillRoot := t.TempDir()
	skillDir := filepath.Join(skillRoot, "voice-applier")
	_ = os.Mkdir(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: voice-applier\n---\nbe terse\n"), 0o600)

	var stdout, stderr bytes.Buffer
	code := RunHash([]string{"skill", skillDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("RunHash skill exit %d; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); !strings.HasPrefix(got, "sha256:") {
		t.Errorf("expected hash, got %q", got)
	}
}

func TestRunHash_UnknownVerb(t *testing.T) {
	var stderr bytes.Buffer
	code := RunHash([]string{"foo"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRunHash_NoArgs(t *testing.T) {
	var stderr bytes.Buffer
	code := RunHash(nil, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRunHashAgent_MissingFile(t *testing.T) {
	var stderr bytes.Buffer
	code := RunHash([]string{"agent", "/nonexistent/whatever.md"}, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit for missing file")
	}
}

// TestRunHashAgent_ConfigModeMatchesInProcessSign exercises the
// v0.11.12 `--config <yaml> <name>` path. Hash MUST equal the same
// `agents.Sign(agents.FromYAMLAgent)` chain applied to a fully-
// resolved (config.Load-mutated) agent struct — that's the contract
// the doc comment promises operators can rely on for CI drift checks
// against the deployed substrate.
func TestRunHashAgent_ConfigModeMatchesInProcessSign(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "loomcycle.yaml")
	body := `
defaults:
  provider: anthropic
  model: claude-sonnet-4-6
agents:
  researcher:
    provider: anthropic
    model: claude-sonnet-4-6
    allowed_tools: [Read, WebFetch]
    max_tokens: 8192
    max_iterations: 32
    system_prompt: be thorough
`
	if err := os.WriteFile(yamlPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunHash([]string{"agent", "--config", yamlPath, "researcher"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("RunHash exit %d; stderr=%s", code, stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(got, "sha256:") || len(got) != 71 {
		t.Fatalf("malformed hash %q", got)
	}

	// Two invocations on identical input MUST produce the same digest.
	// Catches accidental non-determinism (e.g., map iteration order).
	var stdout2 bytes.Buffer
	if c := RunHash([]string{"agent", "--config", yamlPath, "researcher"}, &stdout2, &bytes.Buffer{}); c != 0 {
		t.Fatalf("second invocation exit %d", c)
	}
	if got2 := strings.TrimSpace(stdout2.String()); got2 != got {
		t.Errorf("non-deterministic hash: %s vs %s", got, got2)
	}
}

func TestRunHashAgent_ConfigModeMissingAgentListsAvailable(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "loomcycle.yaml")
	body := `
defaults:
  provider: anthropic
  model: claude-sonnet-4-6
agents:
  alpha:
    allowed_tools: [Read]
  beta:
    allowed_tools: [Read]
`
	if err := os.WriteFile(yamlPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunHash([]string{"agent", "--config", yamlPath, "missing"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit for unknown agent; stdout=%s", stdout.String())
	}
	msg := stderr.String()
	if !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
		t.Errorf("error message should list available agents, got: %s", msg)
	}
	// Sorted (alpha before beta) so operators get stable output.
	if i, j := strings.Index(msg, "alpha"), strings.Index(msg, "beta"); i > j {
		t.Errorf("available agents should be sorted; got: %s", msg)
	}
}

func TestRunHashAgent_ConfigModeProducesDistinctHashes(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "loomcycle.yaml")
	body := `
defaults:
  provider: anthropic
  model: claude-sonnet-4-6
agents:
  alpha:
    allowed_tools: [Read]
    system_prompt: alpha prompt
  beta:
    allowed_tools: [Read, WebFetch]
    system_prompt: beta prompt
`
	if err := os.WriteFile(yamlPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var alphaOut, betaOut bytes.Buffer
	if c := RunHash([]string{"agent", "--config", yamlPath, "alpha"}, &alphaOut, &bytes.Buffer{}); c != 0 {
		t.Fatalf("alpha hash exit %d", c)
	}
	if c := RunHash([]string{"agent", "--config", yamlPath, "beta"}, &betaOut, &bytes.Buffer{}); c != 0 {
		t.Fatalf("beta hash exit %d", c)
	}
	a := strings.TrimSpace(alphaOut.String())
	b := strings.TrimSpace(betaOut.String())
	if a == b {
		t.Errorf("distinct agents must hash differently, both got %s", a)
	}
}
