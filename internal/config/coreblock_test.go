package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlaceholder_UnknownVariantBootError pins RFC BL P1: a {{memory:<variant>}}
// placeholder naming a variant that is not in the closed set fails config Load
// at boot (a typo caught early, not a silent empty render at run time). Fails on
// pre-change code, which has no such validation and loads the agent fine.
func TestPlaceholder_UnknownVariantBootError(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "c.yaml")
	os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  bad:
    model: claude-sonnet-4-6
    system_prompt: "You are helpful. {{memory:core_block}} done."
`), 0o600)
	_, err := Load(yamlPath)
	if err == nil {
		t.Fatal("expected error for unknown {{memory:...}} variant")
	}
	if !strings.Contains(err.Error(), "core_block") {
		t.Errorf("error should name the offending variant: %v", err)
	}
}

// TestPlaceholder_KnownVariantLoads confirms a recognised variant (and an
// escaped placeholder) load cleanly.
func TestPlaceholder_KnownVariantLoads(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "c.yaml")
	os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  ok:
    model: claude-sonnet-4-6
    tools: [Memory]
    memory_scopes: [user]
    system_prompt: "Helpful. {{memory:core_blocks}} and {{ memory : user_info }} and \\{{memory:literal}}"
`), 0o600)
	if _, err := Load(yamlPath); err != nil {
		t.Fatalf("Load: known + escaped variants should load: %v", err)
	}
}

// TestCoreBlocks_RejectsBadScope pins the core-block scope validation.
func TestCoreBlocks_RejectsBadScope(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "c.yaml")
	os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  bad:
    model: claude-sonnet-4-6
    tools: [Memory]
    memory_scopes: [agent]
    core_blocks:
      - { label: notes, scope: session }
`), 0o600)
	_, err := Load(yamlPath)
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("expected a scope error, got: %v", err)
	}
}

// TestCoreBlocks_RejectsSlashLabel pins that a label must be a single segment
// (it becomes the Memory key core/<label>).
func TestCoreBlocks_RejectsSlashLabel(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "c.yaml")
	os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  bad:
    model: claude-sonnet-4-6
    tools: [Memory]
    memory_scopes: [user]
    core_blocks:
      - { label: "a/b", scope: user }
`), 0o600)
	_, err := Load(yamlPath)
	if err == nil || !strings.Contains(err.Error(), "single segment") {
		t.Fatalf("expected a single-segment label error, got: %v", err)
	}
}

// TestCoreBlocks_AcceptsAndRoundTrips confirms a valid core-blocks list loads
// and carries through to the AgentDef.
func TestCoreBlocks_AcceptsAndRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "c.yaml")
	os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  ok:
    model: claude-sonnet-4-6
    tools: [Memory]
    memory_scopes: [agent, user]
    inherit_core_blocks: true
    memory_inject_max_tokens: 512
    memory_protocol: true
    memory_consolidation: true
    core_blocks:
      - { label: human, scope: user, read_only: true }
      - { label: notes, scope: agent, limit_bytes: 2048 }
`), 0o600)
	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def := cfg.Agents["ok"]
	if len(def.CoreBlocks) != 2 {
		t.Fatalf("CoreBlocks len = %d, want 2", len(def.CoreBlocks))
	}
	if !def.InheritCoreBlocks || def.MemoryInjectMaxTokens != 512 || !def.MemoryProtocol {
		t.Errorf("scalar core-block fields didn't round-trip: %+v", def)
	}
	if !def.MemoryConsolidation {
		t.Errorf("memory_consolidation didn't round-trip from yaml: %+v", def)
	}
	if def.CoreBlocks[0].Label != "human" || !def.CoreBlocks[0].ReadOnly {
		t.Errorf("block[0] round-trip: %+v", def.CoreBlocks[0])
	}
	if def.CoreBlocks[1].LimitBytes != 2048 {
		t.Errorf("block[1] limit_bytes: %+v", def.CoreBlocks[1])
	}
}

// TestMergeAgentDef_MemoryConsolidationOrsIn pins the RFC BL P2 grant's overlay
// semantics: an override enables it, and it OR-s in (never disables a base grant
// that the override leaves unset) — mirroring memory_protocol / interruption.
func TestMergeAgentDef_MemoryConsolidationOrsIn(t *testing.T) {
	// Override enables the grant on a base that lacked it.
	got := mergeAgentDef(AgentDef{}, AgentDef{MemoryConsolidation: true})
	if !got.MemoryConsolidation {
		t.Error("override memory_consolidation=true did not OR into the merged def")
	}
	// An override that leaves it unset never disables a base grant.
	got = mergeAgentDef(AgentDef{MemoryConsolidation: true}, AgentDef{})
	if !got.MemoryConsolidation {
		t.Error("base memory_consolidation was dropped by an override that didn't set it")
	}
}
