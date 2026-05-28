package recipes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadBundled is a test helper that returns the bundled recipe by
// name. Used across yaml-emit tests to avoid restating recipe JSON.
func loadBundled(t *testing.T, name string) *Recipe {
	t.Helper()
	lib, err := LoadLibrary("")
	if err != nil {
		t.Fatalf("LoadLibrary: %v", err)
	}
	rec, _, ok := lib.Get(name)
	if !ok {
		t.Fatalf("bundled recipe %q not found", name)
	}
	return rec
}

func TestAppendToConfig_AppendsToEmptyFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	rec := loadBundled(t, "github")
	out, err := AppendToConfig(rec, target, AppendOptions{})
	if err != nil {
		t.Fatalf("AppendToConfig: %v", err)
	}
	if !strings.Contains(string(out), "mcp_servers:") {
		t.Errorf("output missing mcp_servers: block:\n%s", string(out))
	}
	if !strings.Contains(string(out), "github:") {
		t.Errorf("output missing github: entry:\n%s", string(out))
	}
	if !strings.Contains(string(out), "transport: stdio") {
		t.Errorf("output should declare transport explicitly:\n%s", string(out))
	}
	// Output must be parseable yaml.
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Errorf("output is not valid yaml: %v", err)
	}
}

func TestAppendToConfig_PreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	original := `# Loomcycle operator config
defaults:
  provider: anthropic
  model: claude-haiku-4-5

# Auth + storage
storage:
  driver: sqlite
  path: /var/loomcycle/state.db

mcp_servers:
  existing:
    transport: stdio
    command: existing-server
`
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := loadBundled(t, "slack")
	out, err := AppendToConfig(rec, target, AppendOptions{})
	if err != nil {
		t.Fatalf("AppendToConfig: %v", err)
	}
	s := string(out)
	// Existing top-level keys still present.
	for _, want := range []string{"defaults:", "storage:", "mcp_servers:", "existing:"} {
		if !strings.Contains(s, want) {
			t.Errorf("output should preserve %q\n%s", want, s)
		}
	}
	// New entry appended.
	if !strings.Contains(s, "slack:") {
		t.Errorf("output should contain new slack: entry\n%s", s)
	}
}

func TestAppendToConfig_RefusesCollisionWithoutForce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	original := `mcp_servers:
  github:
    transport: stdio
    command: existing-github
`
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := loadBundled(t, "github")
	_, err := AppendToConfig(rec, target, AppendOptions{Force: false})
	var typed *ErrEntryExists
	if !errors.As(err, &typed) {
		t.Fatalf("expected *ErrEntryExists, got %v (%T)", err, err)
	}
	if typed.Name != "github" {
		t.Errorf("ErrEntryExists.Name = %q, want github", typed.Name)
	}
}

func TestAppendToConfig_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	original := `mcp_servers:
  github:
    transport: stdio
    command: old-cmd
    pool_size: 99
`
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := loadBundled(t, "github")
	out, err := AppendToConfig(rec, target, AppendOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "old-cmd") {
		t.Errorf("force should have removed old command:\n%s", s)
	}
	if !strings.Contains(s, "pool_size: 4") {
		t.Errorf("force should have written github recipe's pool_size=4:\n%s", s)
	}
}

func TestAppendToConfig_RefusesMissingEnvAllowlist(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	rec := loadBundled(t, "github") // requires LOOMCYCLE_GITHUB_TOKEN
	_, err := AppendToConfig(rec, target, AppendOptions{
		EnvAllowlist: map[string]bool{"LOOMCYCLE_OTHER_VAR": true},
	})
	var typed *ErrMissingEnvVars
	if !errors.As(err, &typed) {
		t.Fatalf("expected *ErrMissingEnvVars, got %v (%T)", err, err)
	}
	wantMissing := "LOOMCYCLE_GITHUB_TOKEN"
	found := false
	for _, n := range typed.Names {
		if n == wantMissing {
			found = true
		}
	}
	if !found {
		t.Errorf("missing names = %v, want to include %q", typed.Names, wantMissing)
	}
}

func TestAppendToConfig_AcceptsAllowlistedEnvVars(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	rec := loadBundled(t, "github")
	_, err := AppendToConfig(rec, target, AppendOptions{
		EnvAllowlist: map[string]bool{"LOOMCYCLE_GITHUB_TOKEN": true},
	})
	if err != nil {
		t.Errorf("with allowlisted env vars, should succeed: %v", err)
	}
}

func TestAppendToConfig_RoundTripParseableByYAML(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	// Append a few recipes back-to-back; each output must still parse.
	for _, name := range []string{"github", "slack", "tavily"} {
		rec := loadBundled(t, name)
		out, err := AppendToConfig(rec, target, AppendOptions{})
		if err != nil {
			t.Fatalf("AppendToConfig %s: %v", name, err)
		}
		if err := os.WriteFile(target, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Final content must be valid yaml that boot-parses.
	final, _ := os.ReadFile(target)
	var doc yaml.Node
	if err := yaml.Unmarshal(final, &doc); err != nil {
		t.Errorf("final config is not valid yaml: %v\n%s", err, string(final))
	}
	for _, want := range []string{"github:", "slack:", "tavily:"} {
		if !strings.Contains(string(final), want) {
			t.Errorf("final missing %q\n%s", want, string(final))
		}
	}
}

func TestAppendToConfig_HTTPTransportEmitsURL(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	rec := loadBundled(t, "jobs") // http transport
	out, err := AppendToConfig(rec, target, AppendOptions{
		EnvAllowlist: map[string]bool{"LOOMCYCLE_JOBS_MCP_URL": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "transport: http") {
		t.Errorf("http recipe should emit transport: http\n%s", s)
	}
	if !strings.Contains(s, "url:") {
		t.Errorf("http recipe should emit url field\n%s", s)
	}
	if !strings.Contains(s, "${run.credentials.jobs}") {
		t.Errorf("http recipe should preserve credential reference\n%s", s)
	}
}

func TestAppendToConfig_PreservesComments(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	// Operator-authored config with comments at multiple positions.
	original := `# Top-level comment.
defaults:
  # provider comment
  provider: anthropic
  model: claude-haiku-4-5

# Pre-mcp-servers comment.
mcp_servers:
  # Comment on existing entry.
  existing:
    transport: stdio
    command: x
`
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := loadBundled(t, "slack")
	out, err := AppendToConfig(rec, target, AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"# Top-level comment.",
		"# provider comment",
		"# Pre-mcp-servers comment.",
		"# Comment on existing entry.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output should preserve comment %q\n%s", want, s)
		}
	}
}
