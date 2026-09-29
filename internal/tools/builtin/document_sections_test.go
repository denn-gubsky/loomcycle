package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// importAt writes markdown as a new document through the tool itself and
// names it at path.
func importAt(t *testing.T, d *Document, ctx context.Context, path, md string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md, "path": path})
	out, res := docExec(t, d, ctx, string(body))
	if res.IsError {
		t.Fatalf("import_md: %s", res.Text)
	}
	return out["document_id"].(string)
}

const sectionsSpec = "# Spec\n\nIntro text.\n\n" +
	"## Goals\n\nShip it.\n\n### Stretch\n\nShip it early.\n\n" +
	"## Risks\n\nThe build is flaky.\n\n" +
	"## Plan\n\nOne step at a time.\n"

// The sections are the root's DIRECT children, in document order, and each
// carries its whole subtree rendered as a clean export — nested headings at the
// level they have in the document, and nothing of a sibling.
func TestDocumentSections_TopLevelSectionsCarryTheirSubtree(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := importAt(t, d, ctx, "/specs/acme", sectionsSpec)

	secs, err := d.Sections(ctx, "user", "/specs/acme")
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	var titles []string
	for _, s := range secs {
		titles = append(titles, s.Title)
		if s.DocumentID != docID || s.ChunkID == "" {
			t.Errorf("section %q ids = (%q, %q), want the document's id and a chunk id", s.Title, s.DocumentID, s.ChunkID)
		}
	}
	if strings.Join(titles, ",") != "Goals,Risks,Plan" {
		t.Fatalf("sections = %v, want [Goals Risks Plan] in document order", titles)
	}
	goals := secs[0].Markdown
	for _, want := range []string{"## Goals", "Ship it.", "### Stretch", "Ship it early."} {
		if !strings.Contains(goals, want) {
			t.Errorf("Goals markdown lacks %q:\n%s", want, goals)
		}
	}
	for _, not := range []string{"## Risks", "Intro text.", "# Spec", "<!-- loom"} {
		if strings.Contains(goals, not) {
			t.Errorf("Goals markdown carries %q, which is not its subtree:\n%s", not, goals)
		}
	}
	if secs[1].Markdown != "## Risks\n\nThe build is flaky." {
		t.Errorf("Risks markdown = %q", secs[1].Markdown)
	}
}

// A path in another user's tree reads exactly like a path that does not
// exist: dirents are per scope, and the read runs as the caller.
func TestDocumentSections_AnotherUsersPathIsNotFound(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	bob := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "bob", TenantID: "tnt"})
	importAt(t, d, bob, "/specs/bobs", sectionsSpec)

	if secs, err := d.Sections(bob, "user", "/specs/bobs"); err != nil || len(secs) != 3 {
		t.Fatalf("bob reads his own document: %d sections, %v", len(secs), err)
	}
	secs, err := d.Sections(ctx, "user", "/specs/bobs") // ctx runs as u1
	if err == nil || !strings.Contains(err.Error(), "no such path") {
		t.Fatalf("u1 read bob's document: %d sections, err = %v; want not found", len(secs), err)
	}
}

// A document with a root and nothing under it has no sections — an empty
// list, which the Starter turns into a walk error.
func TestDocumentSections_RootOnlyDocumentHasNone(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	importAt(t, d, ctx, "/specs/empty", "# Just a title\n\nNo sections here.\n")

	secs, err := d.Sections(ctx, "user", "/specs/empty")
	if err != nil || len(secs) != 0 {
		t.Fatalf("Sections = %d, %v; want none and no error", len(secs), err)
	}
}

// Tenant scope needs the same two grants a tenant Document read needs, and a
// walk has no agent tree to read.
func TestDocumentSections_ScopeIsGated(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	if _, err := d.Sections(ctx, "tenant", "/specs/acme"); err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Errorf("tenant without grants: err = %v, want a refusal", err)
	}
	if _, err := d.Sections(ctx, "agent", "/specs/acme"); err == nil || !strings.Contains(err.Error(), "unsupported scope") {
		t.Errorf("agent scope: err = %v, want a refusal", err)
	}
}
