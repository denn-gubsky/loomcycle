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

// TestChatBundle_PromptsTeachMemoryRecall pins the guidance that makes remembered
// facts reachable in practice.
//
// The tool grant is not what makes a capability usable — `Memory` and `Document`
// have been in these agents' tools since the bundle shipped. What decides whether a
// model can answer "which medicine do I take" is whether the prompt names the OP and
// its required arguments.
//
// THIS TEST PREVIOUSLY ASSERTED THE OPPOSITE EMPHASIS, and a live transcript
// disproved it. It required the prompt to point at `Document graph_recall` for
// "what do we know about X", on the reasoning that following relations beats matching
// words. In practice graph_recall walks out from facts carrying entity metadata, so
// on a store without those it returns `seeds: 0` — and an agent sent there first
// concluded there was nothing remembered, then spent four calls guessing argument
// names ("missing required field: op", "missing required field: scope", "missing
// required field: query") before stumbling onto `Memory op=recall`. The relation
// advantage is real but it is a SECOND step, not the entry point.
//
// So the invariants are now: teach `Memory op=recall` with scope AND query, and keep
// graph_recall explained but positioned after it.
func TestChatBundle_PromptsTeachMemoryRecall(t *testing.T) {
	cfg := chatBundleConfig(t)
	found := 0
	for name, agent := range cfg.Agents {
		if !strings.HasPrefix(name, "chat/") {
			continue
		}
		found++
		prompt := agent.SystemPrompt
		low := strings.ToLower(prompt)

		// The entry point, named as an invocation rather than a capability. An agent
		// that has to discover `op`/`scope`/`query` from error messages burns its turn
		// budget before it retrieves anything.
		if !strings.Contains(prompt, "op=recall") {
			t.Errorf("%s never names `Memory op=recall` — the only semantic search over "+
				"remembered facts, and the op an agent otherwise has to guess", name)
		}
		for _, arg := range []string{"scope", "query"} {
			if !strings.Contains(low, arg) {
				t.Errorf("%s does not mention recall's required %q argument", name, arg)
			}
		}

		// graph_recall stays explained — the relation advantage is real — but must not
		// be the first thing reached for.
		if !strings.Contains(prompt, "graph_recall") {
			t.Errorf("%s never mentions graph_recall — the entity graph is granted but "+
				"unreachable in practice", name)
			continue
		}
		if !strings.Contains(low, "relation") {
			t.Errorf("%s names graph_recall but not what distinguishes it (following "+
				"relations rather than matching words)", name)
		}
		if !hasToolPreset(agent.Tools, "Document") {
			t.Errorf("%s is told about graph_recall but does not grant Document; tools=%v",
				name, agent.Tools)
		}
	}
	if found == 0 {
		t.Fatal("no chat/* agents found — the bundle was renamed and this test now checks nothing")
	}
}

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
