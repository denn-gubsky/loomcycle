package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// chatBundleConfig loads base + the chat bundle the way an operator selects it
// (LOOMCYCLE_PRESETS=base,chat). LoadLayers runs validate(), so a placeholder
// naming a non-allowlisted tool fails right here.
func chatBundleConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "") // inline skills only
	cfg, err := config.LoadLayers(layersFor(t, "base", "chat")...)
	if err != nil {
		t.Fatalf("base + chat must load + validate cleanly: %v", err)
	}
	return cfg
}

// TestChatBundle_SourceMatchesEmbedded: the bundle exists twice — the readable
// source tree and the flat file the binary go:embeds. Only the EMBEDDED copy
// ships, so editing the source alone changes nothing at runtime while looking,
// in a diff, like it changed everything.
func TestChatBundle_SourceMatchesEmbedded(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "bundles", "chat", "loomcycle.yaml"))
	if err != nil {
		t.Fatalf("read bundle source: %v", err)
	}
	embeddedCopy, err := os.ReadFile(filepath.Join("embedded", "bundles", "chat.yaml"))
	if err != nil {
		t.Fatalf("read embedded bundle: %v", err)
	}
	if !bytes.Equal(src, embeddedCopy) {
		t.Error("bundles/chat/loomcycle.yaml and cmd/loomcycle/embedded/bundles/chat.yaml differ — only the embedded copy ships, so copy the source over it")
	}
}

// TestChatBundle_PromptsCarryTheToolInventoryPlaceholder pins the fix for the
// reported symptom: these agents behaved as though they had no tools until asked
// to check.
//
// The prompts used to hand-write their own tool narrative, and it had DRIFTED —
// all three declared 17 tools while naming 12, silently omitting Agent, HTTP,
// Interruption, Skill and SkillDef. That is not a cosmetic gap: an operator
// observed chat/local answer "Good catch — I do have the Skill tool" only after
// being told, and Skill was one of the five its own prompt never mentioned.
//
// A hand-maintained list in a system prompt cannot be kept in sync with the
// `tools:` list beside it, so the placeholder generates it instead.
func TestChatBundle_PromptsCarryTheToolInventoryPlaceholder(t *testing.T) {
	cfg := chatBundleConfig(t)
	found := 0
	for name, agent := range cfg.Agents {
		if !strings.HasPrefix(name, "chat/") {
			continue
		}
		found++
		if !strings.Contains(agent.SystemPrompt, "{{tool:Context.tools}}") {
			t.Errorf("agent %q: system prompt does not request the generated tool inventory", name)
		}
	}
	if found == 0 {
		t.Fatal("no chat/* agents in the bundle — the test is asserting nothing")
	}
}

// TestChatBundle_PromptsDoNotHandWriteAToolList is the anti-regression half: a
// future edit that re-adds a literal per-tool narrative would reintroduce exactly
// the drift the placeholder removes. Checks the tools most recently missing, in
// the "- Name" list shape the old narrative used, so ordinary prose mentioning a
// tool by name in guidance is still fine.
func TestChatBundle_PromptsDoNotHandWriteAToolList(t *testing.T) {
	cfg := chatBundleConfig(t)
	for name, agent := range cfg.Agents {
		if !strings.HasPrefix(name, "chat/") {
			continue
		}
		for _, tool := range []string{"Read", "Write", "Edit", "Grep", "Glob", "WebSearch", "Bashbox"} {
			if strings.Contains(agent.SystemPrompt, "- "+tool+":") {
				t.Errorf("agent %q: prompt hand-writes a %q list entry; let {{tool:Context.tools}} generate the inventory and keep only the preference guidance", name, tool)
			}
		}
	}
}
