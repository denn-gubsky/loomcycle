package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

type ingestedReasons struct {
	mu      sync.Mutex
	reasons []string
}

func (w *ingestedReasons) Write(_ context.Context, req channels.WriteRequest) (channels.WriteResult, error) {
	var ev struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(req.Payload, &ev)
	w.mu.Lock()
	defer w.mu.Unlock()
	if req.Channel == "documents/ingested" {
		w.reasons = append(w.reasons, ev.Reason)
	}
	return channels.WriteResult{}, nil
}

// File op=import of a new edition reconciles onto the held work, as import_md's
// reimport does, and announces it as reimported.
func TestFileImport_ReimportReconcilesOntoTheHeldWork(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, _ context.Context) {
		f, ctx, _, _ := fileDocRun(t)
		f.Docs = d
		w := &ingestedReasons{}
		d.Writer = w
		capture := func(name, md string) string {
			return inbox.Capture(ctx, inbox.Item{Name: name, MediaType: "text/markdown", Origin: "mcp:ms365/get-attachment"}, []byte(md)).Record.Ref
		}
		const wk = "law:ua/amend-5"
		first, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": capture("act-1.md", amendEd1),
			"reimport": true, "work_key": wk, "edition_date": 1_000_000})
		if res.IsError {
			t.Fatal(res.Text)
		}
		docID := asStr(first["document_id"])
		v1 := currentPartsOf(t, d, ctx, docID, 1_000_001)
		amendID, amendRoot := amendingDoc(t, d, ctx)

		out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": capture("act-2.md", amendEd2),
			"reimport": true, "work_key": wk, "edition_date": 2_000_000, "amending_document_id": amendID})
		if res.IsError {
			t.Fatal(res.Text)
		}
		if asStr(out["document_id"]) != docID {
			t.Fatalf("the edition made a new document %v, want the held work %s", out["document_id"], docID)
		}
		rep, _ := out["reimport"].(map[string]any)
		for k, v := range map[string]float64{"unchanged": 1, "moved": 2, "superseded": 1, "created": 1, "amends_written": 2} {
			if rep[k] != v {
				t.Errorf("%s = %v, want %v (result %v)", k, rep[k], v, out)
			}
		}
		if v2 := currentPartsOf(t, d, ctx, docID, 2_000_001); v2["article-3"].id != v1["article-2"].id {
			t.Errorf("the renumbered article lost its part: v1=%+v v2=%+v", v1, v2)
		}
		for _, r := range amendsRows(t, d, ctx) {
			if r.from != amendRoot {
				t.Errorf("amends from %s, want the amending root %s", r.from, amendRoot)
			}
		}
		w.mu.Lock()
		reasons := slices.Clone(w.reasons)
		w.mu.Unlock()
		if !slices.Contains(reasons, "reimported") {
			t.Errorf("documents/ingested reasons = %v, want one reimported", reasons)
		}
		if fmt.Sprint(out["work_key"]) != wk {
			t.Errorf("the result does not name the work: %v", out)
		}
	})
}

// Document's hooks judge a File import as the import_md it makes: a reimport's
// work_key, edition_date and amending_document_id reach the hook, so a hook that
// refuses one cannot be stepped around by importing through File.
func TestFileImport_DocumentHookSeesTheReimportFields(t *testing.T) {
	f, ctx, _, _ := fileDocRun(t)
	var asked []string
	hooked := tools.WithHookPreCheck(ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		if name != "Document" {
			return tools.Result{}, true
		}
		asked = append(asked, string(input))
		if strings.Contains(string(input), `"work_key":"law:ua/held"`) {
			return tools.Result{Text: "that work is frozen", IsError: true}, false
		}
		return tools.Result{}, true
	})
	ref := inbox.Capture(ctx, inbox.Item{Name: "act.md", MediaType: "text/markdown", Origin: "o"}, []byte(amendEd1)).Record.Ref
	_, res := fileOp(t, f, hooked, map[string]any{"op": "import", "path": ref,
		"reimport": true, "work_key": "law:ua/held", "edition_date": 1_000_000, "amending_document_id": "amend-1"})
	if !res.IsError || !strings.Contains(res.Text, "that work is frozen") {
		t.Fatalf("a hook refusing the work key let the import through: %q", res.Text)
	}
	if len(asked) != 1 {
		t.Fatalf("the Document hook was asked %d times: %v", len(asked), asked)
	}
	for _, want := range []string{`"op":"import_md"`, `"via":"File"`, `"reimport":true`, `"edition_date":1000000`, `"amending_document_id":"amend-1"`} {
		if !strings.Contains(asked[0], want) {
			t.Errorf("the hook was asked %s, without %s", asked[0], want)
		}
	}
	if strings.Contains(asked[0], `"markdown"`) {
		t.Errorf("the hook was handed the file's text: %s", asked[0])
	}
}
