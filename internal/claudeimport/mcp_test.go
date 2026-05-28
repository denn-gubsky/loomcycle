package claudeimport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/recipes"
)

func writeMCP(t *testing.T, root, name string, body string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p := filepath.Join(root, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

func TestWalkMCP_StdioWrappedShape(t *testing.T) {
	root := t.TempDir()
	writeMCP(t, root, "mcp.json", `{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}
    }
  }
}`)
	report, err := Walk(root, WalkOptions{EnvAllowlist: map[string]bool{"LOOMCYCLE_GITHUB_TOKEN": true}})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.MCPServers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(report.MCPServers))
	}
	e := report.MCPServers[0]
	if e.Name != "github" || e.Transport != "stdio" {
		t.Errorf("got name=%q transport=%q", e.Name, e.Transport)
	}
	if !strings.Contains(e.YAMLFragment, "transport: stdio") {
		t.Errorf("yaml missing transport: %s", e.YAMLFragment)
	}
	// Verify env-var rewrite recorded.
	if len(e.EnvVarRewrites) != 1 {
		t.Fatalf("expected 1 env rewrite, got %v", e.EnvVarRewrites)
	}
	if !strings.Contains(e.EnvVarRewrites[0], "${GITHUB_TOKEN} → ${LOOMCYCLE_GITHUB_TOKEN}") {
		t.Errorf("rewrite shape wrong: %q", e.EnvVarRewrites[0])
	}
	if strings.Contains(e.EnvVarRewrites[0], "NOT in env allowlist") {
		t.Errorf("LOOMCYCLE_GITHUB_TOKEN IS in allowlist; should not flag: %q", e.EnvVarRewrites[0])
	}
}

func TestWalkMCP_HTTPShape(t *testing.T) {
	root := t.TempDir()
	writeMCP(t, root, "mcp.json", `{
  "mcpServers": {
    "remote": {
      "url": "https://example.com/mcp"
    }
  }
}`)
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	e := report.MCPServers[0]
	if e.Transport != "http" {
		t.Errorf("expected http transport, got %q", e.Transport)
	}
	if !strings.Contains(e.YAMLFragment, "url: https://example.com/mcp") {
		t.Errorf("yaml missing url: %s", e.YAMLFragment)
	}
}

func TestWalkMCP_EnvAllowlistGap(t *testing.T) {
	root := t.TempDir()
	writeMCP(t, root, "mcp.json", `{
  "mcpServers": {
    "obscure": {
      "command": "npx",
      "args": ["-y", "@scope/obscure"],
      "env": {"WEIRD_API": "${WEIRD_API}"}
    }
  }
}`)
	// Pass an allowlist that doesn't include LOOMCYCLE_WEIRD_API.
	report, _ := Walk(root, WalkOptions{EnvAllowlist: map[string]bool{}})
	e := report.MCPServers[0]
	if len(e.EnvVarRewrites) != 1 {
		t.Fatalf("expected 1 rewrite")
	}
	if !strings.Contains(e.EnvVarRewrites[0], "NOT in env allowlist") {
		t.Errorf("expected allowlist-gap flag, got %q", e.EnvVarRewrites[0])
	}
}

func TestWalkMCP_RecipeMatchByPackage(t *testing.T) {
	// Set up a recipe overlay with a custom 'github' recipe.
	overlay := t.TempDir()
	os.WriteFile(filepath.Join(overlay, "github.json"), []byte(`{
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-github"],
  "env": {"GITHUB_PERSONAL_ACCESS_TOKEN": "${LOOMCYCLE_GITHUB_TOKEN}"},
  "_loomcycle": {
    "description": "Custom GitHub override",
    "transport": "stdio",
    "pool_size": 4,
    "env_vars_required": ["LOOMCYCLE_GITHUB_TOKEN"]
  }
}`), 0o644)
	lib, err := recipes.LoadLibrary(overlay)
	if err != nil {
		t.Fatalf("LoadLibrary: %v", err)
	}

	root := t.TempDir()
	writeMCP(t, root, "mcp.json", `{
  "mcpServers": {
    "my-github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}
    }
  }
}`)
	report, err := Walk(root, WalkOptions{Library: lib, EnvAllowlist: map[string]bool{"LOOMCYCLE_GITHUB_TOKEN": true}})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	e := report.MCPServers[0]
	if e.RecipeMatch != "github" {
		t.Errorf("expected recipe match 'github', got %q", e.RecipeMatch)
	}
	if e.RecipeSource != "overlay" {
		t.Errorf("expected source 'overlay', got %q", e.RecipeSource)
	}
	// The operator's chosen name preserves; the recipe's contents
	// supersede.
	if e.Name != "my-github" {
		t.Errorf("operator's name not preserved: got %q", e.Name)
	}
	if !strings.Contains(e.YAMLFragment, "my-github:") {
		t.Errorf("yaml fragment should use operator's name:\n%s", e.YAMLFragment)
	}
	if !strings.Contains(e.YAMLFragment, "pool_size: 4") {
		t.Errorf("yaml fragment should carry recipe's pool_size=4:\n%s", e.YAMLFragment)
	}
	// Verify warning emitted.
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "REWRITE mcp_servers.my-github → C1 recipe \"github\"") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected REWRITE warning: %v", report.Warnings)
	}
}

func TestWalkMCP_NoRecipeMatchOptOut(t *testing.T) {
	overlay := t.TempDir()
	os.WriteFile(filepath.Join(overlay, "github.json"), []byte(`{
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-github"],
  "_loomcycle": {"description": "X", "transport": "stdio", "pool_size": 4}
}`), 0o644)
	lib, _ := recipes.LoadLibrary(overlay)

	root := t.TempDir()
	writeMCP(t, root, "mcp.json", `{
  "mcpServers": {
    "my-github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github", "--my-flag"]
    }
  }
}`)
	report, _ := Walk(root, WalkOptions{Library: lib, NoRecipeMatch: true})
	e := report.MCPServers[0]
	if e.RecipeMatch != "" {
		t.Errorf("NoRecipeMatch should disable rewrite, got match=%q", e.RecipeMatch)
	}
	// Literal-port preserves the operator's custom flag.
	if !strings.Contains(e.YAMLFragment, "--my-flag") {
		t.Errorf("literal port should preserve --my-flag:\n%s", e.YAMLFragment)
	}
}

func TestWalkMCP_RegistriesUnmapped(t *testing.T) {
	root := t.TempDir()
	writeMCP(t, root, "mcp.json", `{
  "mcpServers": {
    "x": {"command": "echo"}
  },
  "registries": {
    "anthropic-public": {"url": "https://registry.example/"}
  }
}`)
	report, _ := Walk(root, WalkOptions{})
	found := false
	for _, u := range report.Unmapped {
		if strings.HasPrefix(u.Field, "registries[") {
			found = true
			if !strings.Contains(u.Hint, "MCPServerDef") {
				t.Errorf("registries hint should mention MCPServerDef: %q", u.Hint)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected registries unmapped field, got %+v", report.Unmapped)
	}
}

func TestWalkMCP_ProjectRootMCPJson(t *testing.T) {
	// .claude/ at <root>/.claude/, with a .mcp.json at <root>/.mcp.json
	// (the per-project convention).
	root := t.TempDir()
	claude := filepath.Join(root, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeMCP(t, root, ".mcp.json", `{
  "mcpServers": {
    "project-server": {"command": "echo"}
  }
}`)
	report, err := Walk(claude, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.MCPServers) != 1 || report.MCPServers[0].Name != "project-server" {
		t.Errorf("expected to find project-server in <root>/.mcp.json, got %+v", report.MCPServers)
	}
}

func TestWalkMCP_BareTopLevelMap(t *testing.T) {
	root := t.TempDir()
	// Some operator-authored .mcp.json files skip the mcpServers
	// wrapper. Accept the bare map shape as well.
	writeMCP(t, root, "mcp.json", `{
  "alpha": {"command": "a"},
  "beta": {"command": "b"}
}`)
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.MCPServers) != 2 {
		t.Fatalf("expected 2 servers from bare map, got %d", len(report.MCPServers))
	}
	if report.MCPServers[0].Name != "alpha" || report.MCPServers[1].Name != "beta" {
		t.Errorf("wrong names: %+v", report.MCPServers)
	}
}

func TestWalkMCP_MissingMCPJson(t *testing.T) {
	root := t.TempDir()
	// .claude/ exists, no mcp.json — no-op.
	report, err := Walk(root, WalkOptions{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(report.MCPServers) != 0 {
		t.Errorf("expected 0 servers, got %d", len(report.MCPServers))
	}
}

func TestWalkMCP_ServerNeitherCommandNorURL(t *testing.T) {
	root := t.TempDir()
	writeMCP(t, root, "mcp.json", `{
  "mcpServers": {
    "broken": {"description": "no command, no url"}
  }
}`)
	report, _ := Walk(root, WalkOptions{})
	if len(report.MCPServers) != 0 {
		t.Errorf("expected 0 servers (broken filtered), got %d", len(report.MCPServers))
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "neither command nor url") {
		t.Errorf("expected warning about missing transport, got %v", report.Warnings)
	}
}

func TestRewriteEnvRefs_LoomcyclePrefixedPassThrough(t *testing.T) {
	// Already-prefixed refs should not be rewritten.
	in := json.RawMessage(`{"key":"${LOOMCYCLE_FOO}"}`)
	out, rewrites := rewriteEnvRefs(in, map[string]bool{"LOOMCYCLE_FOO": true})
	if string(out) != string(in) {
		t.Errorf("expected pass-through, got %s", out)
	}
	if len(rewrites) != 0 {
		t.Errorf("expected no rewrites for already-prefixed, got %v", rewrites)
	}
}
