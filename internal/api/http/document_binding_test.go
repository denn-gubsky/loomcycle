package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A {{document:...}} binding is only worth anything if it actually resolves at
// prompt-assembly time. These tests drive applyMemoryInjection — the real
// assembly path — against a real document written through the real tool, so a
// scope-resolution regression cannot hide behind a placeholder that renders
// empty and a run that succeeds anyway.

func bindingFixture(t *testing.T) (*Server, *builtin.Document, context.Context) {
	t.Helper()
	cfg := &config.Config{
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	srv := New(cfg, nil, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	srv.SetResolver(resolve.NewResolver(nil, nil))
	srv.sqlMem = mgr

	// The identity a RUN carries: a user id and an empty tenant. This is the
	// shape docToolCtx reconstructs, so the fixture exercises the same scope
	// resolution prompt assembly does.
	ctx := tools.WithAgentName(context.Background(), "reader")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1"})
	return srv, &builtin.Document{Store: st, SqlMem: mgr}, ctx
}

// seedDoc writes a two-section document through the Document tool and names it
// in the Path tree, exactly as an operator or agent would.
func seedDoc(t *testing.T, doc *builtin.Document, ctx context.Context, path string) string {
	t.Helper()
	md := "# Launch plan\n\n## Goals\n\nShip the thing.\n\n## Risks\n\nThe build is flaky.\n"
	req, _ := json.Marshal(map[string]any{
		"op": "import_md", "scope": "user", "title": "Launch plan", "markdown": md,
	})
	res, err := doc.Execute(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("import_md: %v %s", err, res.Text)
	}
	var out struct {
		DocumentID string `json:"document_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("import_md payload: %v", err)
	}
	req, _ = json.Marshal(map[string]any{
		"op": "set_path", "scope": "user", "id": out.DocumentID, "path": path,
	})
	if res, err := doc.Execute(ctx, req); err != nil || res.IsError {
		t.Fatalf("set_path: %v %s", err, res.Text)
	}
	return out.DocumentID
}

func injected(t *testing.T, srv *Server, ctx context.Context, prompt string) string {
	t.Helper()
	def := config.AgentDef{SystemPrompt: prompt}
	out, _ := srv.applyMemoryInjection(ctx, def, memInject{UserID: "u1", AgentName: "reader"})
	return out.SystemPrompt
}

// THE REGRESSION. A section ref must inline the section's text. It rendered
// EMPTY against a live instance while the identical Document op succeeded over
// HTTP — a binding that silently resolves to nothing is worse than one that was
// never wired, because the run succeeds and the agent simply answers without
// the material it was promised.
func TestDocumentBinding_SectionRefInlinesTheSection(t *testing.T) {
	srv, doc, ctx := bindingFixture(t)
	seedDoc(t, doc, ctx, "/specs/launch")

	got := injected(t, srv, ctx, "Spec:\n{{document:/specs/launch#Risks}}")

	if !strings.Contains(got, "The build is flaky.") {
		t.Fatalf("the section did not resolve at prompt assembly:\n%s", got)
	}
	if strings.Contains(got, "Ship the thing.") {
		t.Errorf("the ref named ONE section but the whole document came through:\n%s", got)
	}
}

// A chunk id is the most precise form and must resolve the same way.
func TestDocumentBinding_ChunkIDRefInlinesTheChunk(t *testing.T) {
	srv, doc, ctx := bindingFixture(t)
	docID := seedDoc(t, doc, ctx, "/specs/launch2")

	req, _ := json.Marshal(map[string]any{
		"op": "export_md", "scope": "user", "id": docID, "include_metadata": true,
	})
	res, err := doc.Execute(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("export_md: %v %s", err, res.Text)
	}
	var ex struct {
		Markdown string `json:"markdown"`
	}
	_ = json.Unmarshal([]byte(res.Text), &ex)
	id := chunkIDForTitle(t, ex.Markdown, "Risks")

	got := injected(t, srv, ctx, "Section:\n{{document:"+id+"}}")
	if !strings.Contains(got, "The build is flaky.") {
		t.Fatalf("a chunk-id ref did not resolve at prompt assembly:\n%s", got)
	}
}

// A whole-document ref renders an INSTRUCTION and reads nothing, so it must
// resolve even with no document, no store and no scope — the property that
// makes it immune to the failure the two tests above guard.
func TestDocumentBinding_WholeDocumentRefNeedsNoStore(t *testing.T) {
	srv, _, ctx := bindingFixture(t)

	got := injected(t, srv, ctx, "Spec:\n{{document:/specs/never-created}}")

	if !strings.Contains(got, "Document op=export_md path=/specs/never-created") {
		t.Errorf("a whole-document ref must render its instruction regardless of the store:\n%s", got)
	}
}

// chunkIDForTitle pulls a chunk id out of export_md's round-trip metadata.
func chunkIDForTitle(t *testing.T, md, title string) string {
	t.Helper()
	lines := strings.Split(md, "\n")
	for i, ln := range lines {
		if !strings.Contains(ln, "## "+title) {
			continue
		}
		// The loom metadata comment sits immediately AFTER its heading.
		for j := i; j < len(lines) && j < i+4; j++ {
			if k := strings.Index(lines[j], `"id"`); k >= 0 {
				rest := lines[j][k+len(`"id"`):]
				if a := strings.Index(rest, `"`); a >= 0 {
					if b := strings.Index(rest[a+1:], `"`); b >= 0 {
						return rest[a+1 : a+1+b]
					}
				}
			}
		}
	}
	t.Fatalf("no chunk id found for %q in:\n%s", title, md)
	return ""
}
