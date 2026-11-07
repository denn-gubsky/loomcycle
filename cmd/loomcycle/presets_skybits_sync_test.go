package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
)

func skybitsConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "") // inline skills only
	cfg, err := config.LoadLayers(layersFor(t, "base", "skybits-sync")...)
	if err != nil {
		t.Fatalf("base + skybits-sync must load + validate cleanly: %v", err)
	}
	return cfg
}

// skybitsReadTools is every Skybits tool any agent of the bundle may hold, and
// skybitsWriteTools every Skybits tool that changes the account. The sync only
// reads the account: a write tool on any list would let a bug, or text in a
// document, change the user's Skybits workspace.
var (
	skybitsSyncTools   = []string{"Agent", "Document", "Memory", "Path", "mcp__skybits__find_documents", "mcp__skybits__list_jobs", "mcp__skybits__list_tags"}
	skybitsImportTools = []string{"Agent", "Document", "File", "HTTP", "Memory", "Path", "mcp__skybits__document_history", "mcp__skybits__get_file_url", "mcp__skybits__get_job", "mcp__skybits__list_jobs", "mcp__skybits__outline_document", "mcp__skybits__start_export"}
	skybitsWriteTools  = []string{"edit_document", "edit_cells", "edit_conditional_formats", "edit_named_ranges", "create_doc", "create_canvas",
		"create_spreadsheet", "create_file", "create_tag", "create_comment_thread", "copy_document", "trash_documents", "untrash_documents",
		"restore_document", "share", "assign_tag", "unassign_tag", "update_tag", "delete_tag", "update_document_title", "update_document_theme",
		"update_file_settings", "upload_file_version", "upload_image", "restructure_spreadsheet", "set_template", "link_documents",
		"unlink_documents", "reply_to_comment_thread", "set_comment_thread_status", "cancel_job", "report_problem", "render_report",
		"claim_sync_job", "close_sync_claim", "create_sync_set", "update_sync_set", "delete_sync_set", "finalize_sync_nodes", "update_sync_edges"}
)

func sortedTools(tools []string) string {
	got := append([]string(nil), tools...)
	sort.Strings(got)
	return strings.Join(got, ",")
}

// TestSkybitsBundle_ToolCeilingsAreExactlyTheReadSet pins each agent's tools to
// the list its body needs. Adding a Skybits write tool, a glob or a network tool
// to any of the three fails here by name.
func TestSkybitsBundle_ToolCeilingsAreExactlyTheReadSet(t *testing.T) {
	cfg := skybitsConfig(t)
	want := map[string][]string{
		"skybits/sync":      skybitsSyncTools,
		"skybits/import":    skybitsImportTools,
		"skybits/structure": {},
	}
	for name, tools := range want {
		a, ok := cfg.Agents[name]
		if !ok {
			t.Fatalf("%s not registered (agents: %v)", name, agentNames(cfg))
		}
		if got := sortedTools(a.Tools); got != strings.Join(tools, ",") {
			t.Errorf("%s tools = %v, want exactly %v", name, a.Tools, tools)
		}
		for _, tool := range a.Tools {
			if strings.ContainsAny(tool, "*?[") {
				t.Errorf("%s grants the pattern %q — every tool is named", name, tool)
			}
			if strings.HasPrefix(tool, "mcp__") && !strings.HasPrefix(tool, "mcp__skybits__") {
				t.Errorf("%s grants %q, a tool of another MCP server", name, tool)
			}
		}
		for _, w := range skybitsWriteTools {
			if hasToolPreset(a.Tools, "mcp__skybits__"+w) {
				t.Errorf("%s grants the Skybits write tool %s", name, w)
			}
		}
		for _, denied := range []string{"Bash", "Bashbox", "WebFetch", "WebSearch", "Read", "Write", "Edit", "Channel", "Skill", "Context"} {
			if hasToolPreset(a.Tools, denied) {
				t.Errorf("%s grants %q", name, denied)
			}
		}
		if !containsString(a.Skills, "-*") || !a.DisableContext || !a.Internal {
			t.Errorf("%s must declare skills [-*], disable_context and internal (got %v, %v, %v)", name, a.Skills, a.DisableContext, a.Internal)
		}
		// A tenant Document grant is a tenant WRITE grant: an operator overlay
		// gives it, the bundle does not.
		if len(a.MemoryScopes) > 0 || len(a.SqlScopes) > 0 {
			t.Errorf("%s declares scopes (memory=%v sql=%v) — they belong in the deployment's overlay", name, a.MemoryScopes, a.SqlScopes)
		}
	}
}

// TestSkybitsBundle_BodiesCallOnlyTheirTools: a call in a body to a Skybits tool
// its agent does not hold would only fail at run time, on a real account.
func TestSkybitsBundle_BodiesCallOnlyTheirTools(t *testing.T) {
	cfg := skybitsConfig(t)
	call := regexp.MustCompile(`sky\("([a-z_]+)"`)
	for _, name := range []string{"skybits/sync", "skybits/import"} {
		a := cfg.Agents[name]
		if a.Provider != "code-js" || strings.TrimSpace(a.Code) == "" {
			t.Fatalf("%s must be a code-js agent with a body", name)
		}
		if _, err := codejs.Validate(a.Code); err != nil {
			t.Fatalf("the shipped %s body does not parse: %v", name, err)
		}
		seen := 0
		for _, m := range call.FindAllStringSubmatch(a.Code, -1) {
			seen++
			if !hasToolPreset(a.Tools, "mcp__skybits__"+m[1]) {
				t.Errorf("%s calls %s but does not hold mcp__skybits__%s", name, m[1], m[1])
			}
		}
		if seen == 0 {
			t.Errorf("%s calls no Skybits tool through sky() — the scan found nothing to check", name)
		}
		if strings.TrimSpace(a.SystemPrompt) != "" {
			t.Errorf("%s carries a system_prompt — a code agent never reads one", name)
		}
	}
	if s := cfg.Agents["skybits/sync"]; strings.Join(s.StatementMethods, ",") != "rule" || s.RunTimeoutSeconds != 7200 {
		t.Errorf("skybits/sync statement_methods=%v run_timeout_seconds=%d, want [rule] and 7200", s.StatementMethods, s.RunTimeoutSeconds)
	}
	imp := cfg.Agents["skybits/import"]
	if len(imp.StatementMethods) != 0 {
		t.Errorf("skybits/import holds statement_methods %v — it marks and proposes nothing", imp.StatementMethods)
	}
	if imp.Transcription == nil || imp.Transcription.OCR != "always" || imp.Transcription.STT != "on" || imp.Transcription.MaxMinutesPerRun != 120 {
		t.Errorf("skybits/import transcription = %+v, want ocr always, stt on, 120 minutes", imp.Transcription)
	}
	st := cfg.Agents["skybits/structure"]
	if st.Provider != "" || st.Model != "" || st.Tier != "middle" {
		t.Errorf("skybits/structure provider=%q model=%q tier=%q — it takes the operator's middle tier, never a pinned model", st.Provider, st.Model, st.Tier)
	}
	if st.OutputFormat == nil || st.OutputFormat.Schema["type"] != "object" {
		t.Errorf("skybits/structure must answer through an output_format schema, got %+v", st.OutputFormat)
	}
}

// TestSkybitsBundle_NoDeploymentOrDesignTextShips: the bundle is generic. It
// names no tenant, account or corpus, and model-visible text cites no design
// document.
func TestSkybitsBundle_NoDeploymentOrDesignTextShips(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "bundles", "skybits-sync", "loomcycle.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"skybits-dev", "loomcycle.cloud", "RFC", "§", "#adb5bd"} {
		if bytes.Contains(src, []byte(bad)) {
			t.Errorf("the bundle mentions %q", bad)
		}
	}
	cfg := skybitsConfig(t)
	if p := cfg.Agents["skybits/structure"].SystemPrompt; strings.Contains(p, "RFC") || strings.Contains(p, "§") {
		t.Error("the structurer's prompt cites a design document")
	}
}

func TestSkybitsBundle_SourceMatchesEmbedded(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "bundles", "skybits-sync", "loomcycle.yaml"))
	if err != nil {
		t.Fatalf("read bundle source: %v", err)
	}
	embeddedCopy, err := os.ReadFile(filepath.Join("..", "..", "internal", "embedded", "bundles", "skybits-sync.yaml"))
	if err != nil {
		t.Fatalf("read embedded bundle: %v", err)
	}
	if !bytes.Equal(src, embeddedCopy) {
		t.Error("bundles/skybits-sync/loomcycle.yaml and internal/embedded/bundles/skybits-sync.yaml differ — only the embedded copy ships, so copy the source over it")
	}
}

// TestSkybitsBundle_ComposesWithTheDefaultStack: a deployment selects the bundle
// beside the shipped stack and the knowledge base; a name collision only shows
// in that combination.
func TestSkybitsBundle_ComposesWithTheDefaultStack(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "document-agent", "chat", "agent-teams", "team-examples", "memory", "knowledge-base", "skybits-sync")...)
	if err != nil {
		t.Fatalf("the default stack plus knowledge-base and skybits-sync must load + validate cleanly: %v", err)
	}
	for _, name := range []string{"skybits/sync", "skybits/import", "skybits/structure", "kb/linker"} {
		if _, ok := cfg.Agents[name]; !ok {
			t.Errorf("%s missing from the full stack", name)
		}
	}
}
