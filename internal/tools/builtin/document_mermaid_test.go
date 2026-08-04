package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestMermaidEmbedText_KeepsLanguageDropsSyntax asserts the property that matters
// for retrieval, per dialect: every human-authored word survives, and no mermaid
// syntax token does.
//
// The `want` lists are the CONTENT of each diagram — an assertion that this
// extractor loses nothing. That is the axis a regex extractor actually fails on: a
// first draft here used shape regexes only and silently dropped erDiagram entity
// names, gantt task names and mindmap nodes, all of which pass a "no syntax
// leaked" check because what leaked was nothing at all.
func TestMermaidEmbedText_KeepsLanguageDropsSyntax(t *testing.T) {
	cases := []struct {
		name string
		src  string
		kind string   // expected diagram-kind prefix
		want []string // words that MUST survive
		deny []string // tokens that must NOT appear
	}{
		{
			name: "flowchart",
			src:  "graph TD\n  A[User] -->|reads| B[(Memory)]",
			kind: "flowchart",
			want: []string{"User", "reads", "Memory"},
			deny: []string{"-->", "[", "|", "TD", "graph"},
		},
		{
			name: "flowchart_decision",
			src:  "flowchart LR\n  Agent{Decision} -- writes --> Store[[Store]]",
			kind: "flowchart",
			want: []string{"Agent", "Decision", "writes", "Store"},
			deny: []string{"{", "}", "-->", "--", "LR"},
		},
		{
			name: "sequence",
			src:  "sequenceDiagram\n  participant A as Agent\n  participant S as Store\n  A->>S: MemorySet(body)\n  S-->>A: ok",
			kind: "sequence diagram",
			want: []string{"Agent", "Store", "MemorySet", "body", "ok"},
			deny: []string{"->>", "-->>", "participant", " as "},
		},
		{
			name: "class",
			src:  "classDiagram\n  class Document {\n    +writeBody()\n  }\n  Document --> Chunk",
			kind: "class diagram",
			want: []string{"Document", "writeBody", "Chunk"},
			// classDiagram is the kind keyword; it must not ALSO appear as a label.
			deny: []string{"classDiagram", "+", "-->"},
		},
		{
			name: "state",
			src:  "stateDiagram-v2\n  [*] --> Pending\n  Pending --> Embedded: embedder succeeded",
			kind: "state diagram",
			want: []string{"Pending", "Embedded", "embedder succeeded"},
			deny: []string{"[*]", "-->"},
		},
		{
			// Entity names here are BARE identifiers, not bracketed — the case a
			// label-shape-only extractor loses entirely.
			name: "er_bare_entities",
			src:  "erDiagram\n  CHUNK ||--o{ EMBEDDING : has",
			kind: "entity relationship diagram",
			want: []string{"CHUNK", "EMBEDDING", "has"},
			deny: []string{"||--o{", "{"},
		},
		{
			// gantt task names sit BEFORE the colon while metadata sits after it.
			name: "gantt_task_names",
			src:  "gantt\n  title Backfill plan\n  section Phase 1\n  Embed on write :a1, 2026-08-01, 3d",
			kind: "gantt chart",
			want: []string{"Backfill plan", "Phase", "Embed on write"},
			deny: []string{"title", "section"},
		},
		{
			name: "pie",
			src:  "pie title Embedding coverage\n  \"Embedded\" : 70\n  \"Missing\" : 30",
			kind: "pie chart",
			want: []string{"Embedding coverage", "Embedded", "Missing"},
			deny: []string{"\"", "title"},
		},
		{
			// mindmap nodes are bare INDENTED words with no delimiter at all.
			name: "mindmap_bare_nodes",
			src:  "mindmap\n  root((Memory))\n    Facts\n    Documents",
			kind: "mindmap",
			want: []string{"Memory", "Facts", "Documents"},
			deny: []string{"((", "root"},
		},
		{
			// An unrecognised kind must DEGRADE, not vanish: mermaid adds diagram
			// types, and a new one must still index its words.
			name: "unknown_kind_degrades",
			src:  "someNewDiagram\n  Alpha --> Beta",
			kind: "",
			want: []string{"Alpha", "Beta"},
			deny: []string{"-->"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mermaidEmbedText(tc.src)
			if tc.kind != "" && !strings.HasPrefix(got, tc.kind+": ") {
				t.Errorf("kind prefix: want %q, got %q", tc.kind+": ", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("LOST content %q\n  got: %q", w, got)
				}
			}
			for _, d := range tc.deny {
				// The kind prefix legitimately contains its own name, so compare
				// against the body only.
				body := got
				if i := strings.Index(got, ": "); tc.kind != "" && i >= 0 {
					body = got[i+2:]
				}
				if strings.Contains(body, d) {
					t.Errorf("syntax %q leaked into labels\n  got: %q", d, got)
				}
			}
		})
	}
}

// TestMermaidEmbedText_EmptyWhenNothingToIndex — a diagram with no human language
// must yield "", so embedBody skips it. Embedding punctuation would store a vector
// that can only ever produce false matches, which is worse than being unsearchable.
func TestMermaidEmbedText_EmptyWhenNothingToIndex(t *testing.T) {
	for _, src := range []string{
		"",
		"   \n  \n",
		"%% just a comment\n%% another",
		"graph TD",
	} {
		if got := mermaidEmbedText(src); got != "" {
			t.Errorf("mermaidEmbedText(%q) = %q, want \"\"", src, got)
		}
	}
}

// TestMermaidEmbedText_DoesNotRepeatPhraseWords — a phrase and its own words must
// not both appear. Triple-counting whatever happens to be bracketed skews the
// vector toward it.
func TestMermaidEmbedText_DoesNotRepeatPhraseWords(t *testing.T) {
	got := mermaidEmbedText("stateDiagram-v2\n  Pending --> Embedded: embedder succeeded")
	if n := strings.Count(got, "embedder"); n != 1 {
		t.Errorf("word 'embedder' appears %d times, want 1: %q", n, got)
	}
	if n := strings.Count(got, "succeeded"); n != 1 {
		t.Errorf("word 'succeeded' appears %d times, want 1: %q", n, got)
	}
}

// mermaidDocFixture wires a Document tool to the capturing vector store + fake
// embedder, so a test can read back the exact text that was embedded.
func mermaidDocFixture(t *testing.T, vocab ...string) (*Document, *vectorStore, context.Context) {
	t.Helper()
	base, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = base.Close() })
	vs := newVectorStore(base)
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	d := &Document{Store: vs, SqlMem: mgr, Bus: channels.NewBus(),
		Embedder: newFakeEmbedder("fake", "m1", vocab...)}
	return d, vs, ctx
}

// embeddedTextFor returns the text that was embedded for a chunk, or "" if none.
func embeddedTextFor(t *testing.T, vs *vectorStore, chunkID string) string {
	t.Helper()
	e, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "u1", chunkBodyKey(chunkID))
	if err != nil {
		return ""
	}
	return e.EmbedText
}

// TestEmbedBody_MermaidChunkEmbedsLabelsNotSource is the REGRESSION test for the
// bug this phase found: embedBody classified with classifyMediaBody, which
// recognises only the ```mermaid FENCED form — but a mermaid chunk STORES its bare
// source. So a chunk created with type=mermaid was embedded as raw diagram source
// (`graph`, `TD`, `-->` and all), while the identical diagram arriving through
// import_md was skipped. The fix threads the authoritative chunk type in.
//
// FAIL-BEFORE: reverting embedBody to `if typ,_,_,_ := classifyMediaBody(body);
// typ != "" { return }` makes this fail with the raw source as embed_text.
func TestEmbedBody_MermaidChunkEmbedsLabelsNotSource(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "user", "reads", "memory", "flowchart")

	res, err := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Diagrams"}`))
	if err != nil || res.IsError {
		t.Fatalf("create_document: %v %s", err, res.Text)
	}
	docID := resultField(res, "document_id")

	src := "graph TD\n  A[User] -->|reads| B[(Memory)]"
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID, "title": "Flow",
		"type": "mermaid", "body": src,
	})
	res, err = d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	chunkID := resultField(res, "id")
	if chunkID == "" {
		t.Fatalf("no chunk id in %s", res.Text)
	}

	got := embeddedTextFor(t, vs, chunkID)
	if got == "" {
		t.Fatal("mermaid chunk was not embedded at all")
	}
	// The words, without the syntax.
	for _, w := range []string{"User", "reads", "Memory", "flowchart"} {
		if !strings.Contains(got, w) {
			t.Errorf("embed text lost %q: %q", w, got)
		}
	}
	if strings.Contains(got, "-->") || strings.Contains(got, "TD") {
		t.Errorf("embed text is raw diagram source, not labels: %q", got)
	}
	// And the BODY is untouched — only the embedded text differs.
	stored, rerr := d.readBody(ctx, store.MemoryScopeUser, "u1", chunkID)
	if rerr != nil {
		t.Fatalf("readBody: %v", rerr)
	}
	if stored.Body != src {
		t.Errorf("stored body was rewritten:\n want %q\n  got %q", src, stored.Body)
	}
}

// TestEmbedBody_ImageChunkNotEmbedded — an image body is a caption or a data URL;
// until phase 4 generates a description there is no text worth indexing, and
// embedding base64 would match nothing while consuming the scope's vector quota.
func TestEmbedBody_ImageChunkNotEmbedded(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "diagram", "screenshot")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Shots"}`))
	docID := resultField(res, "document_id")
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID, "title": "Shot",
		"type": "image", "body": "a screenshot of the diagram",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	if got := embeddedTextFor(t, vs, resultField(res, "id")); got != "" {
		t.Errorf("image chunk was embedded (%q); phase 4 owns image text", got)
	}
}

// TestEmbedBody_TypeChangeToMermaidReembedsAsLabels — turning a prose chunk into a
// diagram in the same update that sets its body must use the NEW type. Reading the
// pre-update row's type would embed diagram source as prose.
func TestEmbedBody_TypeChangeToMermaidReembedsAsLabels(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "user", "reads", "memory", "flowchart", "draft")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Evolving"}`))
	docID := resultField(res, "document_id")
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID, "title": "Later a diagram",
		"body": "draft prose",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	chunkID := resultField(res, "id")
	if got := embeddedTextFor(t, vs, chunkID); got != "draft prose" {
		t.Fatalf("prose precondition: want %q, got %q", "draft prose", got)
	}

	upd, _ := json.Marshal(map[string]any{
		"op": "update_chunk", "id": chunkID, "revision": 1,
		"type": "mermaid", "body": "graph TD\n  A[User] -->|reads| B[(Memory)]",
	})
	res, err = d.Execute(ctx, upd)
	if err != nil || res.IsError {
		t.Fatalf("update_chunk: %v %s", err, res.Text)
	}
	got := embeddedTextFor(t, vs, chunkID)
	if strings.Contains(got, "-->") {
		t.Errorf("embedded as prose using the PRE-update type: %q", got)
	}
	if !strings.Contains(got, "User") || !strings.Contains(got, "flowchart") {
		t.Errorf("not embedded as a diagram: %q", got)
	}
}
