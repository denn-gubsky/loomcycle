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

// TestChatBundle_PromptsPointAtGraphRecall pins the guidance that makes the entity
// graph reachable in practice.
//
// The tool grant is not what makes a capability usable. `Document` has been in
// these agents' tools since the bundle shipped, and `graph_recall` has existed
// since v1.42.0 — but nothing told a model when to prefer it over `Memory recall`,
// so it went unused. That is the same "capability with no caller" shape as the
// entity tier having no producer, one layer up: reachable in principle, invisible
// in practice.
//
// The distinction the guidance has to carry is the load-bearing one: word matching
// versus following relations. Two facts about Acme recorded in different words are
// found by graph_recall and missed by recall, which is precisely when a model
// should reach for it.
func TestChatBundle_PromptsPointAtGraphRecall(t *testing.T) {
	cfg := chatBundleConfig(t)
	found := 0
	for name, agent := range cfg.Agents {
		if !strings.HasPrefix(name, "chat/") {
			continue
		}
		found++
		if !strings.Contains(agent.SystemPrompt, "graph_recall") {
			t.Errorf("%s never mentions graph_recall — the entity graph is granted but unreachable in practice", name)
			continue
		}
		// Naming the op is not enough; the prompt must say WHEN, or it reads as
		// one more entry in a list the model already has from Context.tools.
		if !strings.Contains(agent.SystemPrompt, "relation") {
			t.Errorf("%s names graph_recall but not what distinguishes it (following relations vs matching words)", name)
		}
		// And the tool it needs must actually be granted.
		if !hasToolPreset(agent.Tools, "Document") {
			t.Errorf("%s is told to use graph_recall but does not grant Document; tools=%v", name, agent.Tools)
		}
	}
	if found == 0 {
		t.Fatal("no chat/* agents found — the bundle was renamed and this test now checks nothing")
	}
}

// TestChatBundle_PromptsInjectTenantContext pins the CONSUMER of
// {{memory:tenant_info}}.
//
// The variant existed and validated for four phases while rendering nothing,
// because the document behind it was never built — and once built it would have
// been just as inert if no shipped prompt referenced it. That is the failure this
// phase exists to fix and the reason the plan says: wire a placeholder in the same
// PR that adds it, or do not ship it.
//
// The chat agents are the right consumer: they are the interactive surface, they
// already read the per-user root, and deployment vocabulary is exactly the context
// they lack. They stay `memory_scopes: [user]` — the read stamps the tenant grants
// server-side, so consuming it costs no widening.
func TestChatBundle_PromptsInjectTenantContext(t *testing.T) {
	cfg := chatBundleConfig(t)
	found := 0
	for name, agent := range cfg.Agents {
		if !strings.HasPrefix(name, "chat/") {
			continue
		}
		found++
		if !strings.Contains(agent.SystemPrompt, "{{memory:tenant_info}}") {
			t.Errorf("%s does not inject {{memory:tenant_info}} — the deployment-context document would ship with no reader", name)
		}
		// Consuming it must NOT have required widening the agent to tenant scope.
		for _, scope := range agent.MemoryScopes {
			if scope == "tenant" {
				t.Errorf("%s was widened to tenant memory scope; tenant_info is read with server-stamped grants and needs no such widening", name)
			}
		}
	}
	if found == 0 {
		t.Fatal("no chat/* agents found — the bundle was renamed and this test now checks nothing")
	}
}
