package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// buildToolsStepOrder is the order buildTools runs its steps in. It is the
// agent-available catalog's order, and several steps read what an earlier
// one built (see the comment above buildTools).
var buildToolsStepOrder = []string{
	"http", "skills_help", "catalog_logs", "core", "memory", "channels",
	"defs", "files", "document", "history", "evaluation", "interruption",
	"introspection",
}

// buildToolsCatalog is the tool catalog buildTools leaves in b.allTools, in
// order.
var buildToolsCatalog = []string{
	"Read", "Write", "Edit", "Grep", "Glob", "NotebookEdit", "HTTP", "WebFetch",
	"WebSearch", "Bash", "Bashbox", "Skill", "Memory", "Recall", "Channel",
	"AgentDef", "SkillDef", "TeamDef", "VolumeDef", "File", "Git",
	"CredentialDef", "Crypto", "Path", "Document", "History", "Evaluation",
	"Interruption", "Context", "Identity",
}

// WHY: buildTools' step order is the catalog order agents see, and the later
// steps wire tools into ones built earlier (Document into Crypto, File and
// Memory; the TeamDef board and escalation into Document and Interruption).
// A swap of two steps compiles and passes every other test.
func TestBuildTools_StepsRunInCatalogOrder(t *testing.T) {
	b := &boot{cfg: &config.Config{}, pr: &providerResolver{}, secrets: &secretSink{}}
	var got []string
	b.toolStepHook = func(step string) { got = append(got, step) }
	b.buildTools()

	if !slices.Equal(got, buildToolsStepOrder) {
		t.Errorf("buildTools step order\n got: %s\nwant: %s", strings.Join(got, " > "), strings.Join(buildToolsStepOrder, " > "))
	}
	var names []string
	for _, tool := range b.allTools {
		names = append(names, tool.Name())
	}
	if !slices.Equal(names, buildToolsCatalog) {
		t.Errorf("tool catalog order\n got: %s\nwant: %s", strings.Join(names, " > "), strings.Join(buildToolsCatalog, " > "))
	}
	if b.teamDefTool.Board != b.documentTool || b.teamDefTool.AskHuman == nil {
		t.Errorf("TeamDef board/escalation not wired: board=%v askHuman=%v", b.teamDefTool.Board, b.teamDefTool.AskHuman != nil)
	}
	if b.cryptoTool.Docs != b.documentTool || b.fileTool.Docs != b.documentTool || b.memoryTool.DocumentFilters != b.documentTool || b.gitTool.Crypto != b.cryptoTool {
		t.Error("Document/Crypto cross-wiring missing")
	}
}
