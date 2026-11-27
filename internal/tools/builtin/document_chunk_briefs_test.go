package builtin

import (
	"context"
	"fmt"
	"testing"
)

// RFC HA item 3: query_chunks — the explorer's chunk list — takes with_summary
// and adds each chunk's OWN annotation brief.

func chunkSummaries(t *testing.T, d *Document, ctx context.Context, docID string, with bool) (map[string]any, string) {
	t.Helper()
	q := fmt.Sprintf(`{"op":"query_chunks","scope":"tenant","document_id":%q,"limit":1000`, docID)
	if with {
		q += `,"with_summary":true`
	}
	out, r := docExec(t, d, ctx, q+`}`)
	if r.IsError {
		t.Fatalf("query_chunks: %s", r.Text)
	}
	got := map[string]any{}
	for _, row := range out["chunks"].([]any) {
		m := row.(map[string]any)
		got[m["id"].(string)] = m["summary"]
	}
	return got, r.Text
}

func TestWithSummary_QueryChunksCarriesEachChunksOwnBrief(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		ctx := annOperator(base)
		c := seedAnnCorpus(t, d, ctx)
		_, before := chunkSummaries(t, d, ctx, c.doc, false)
		annWrite(t, d, ctx, idTarget(c.setup), "Setting up the widget")
		annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")

		// Without with_summary the list is the same bytes as before any annotation.
		if _, after := chunkSummaries(t, d, ctx, c.doc, false); after != before {
			t.Errorf("query_chunks changed without with_summary:\n%s\n%s", before, after)
		}

		got, _ := chunkSummaries(t, d, ctx, c.doc, true)
		setup, _ := got[c.setup].(map[string]any)
		if setup["brief"] != "Setting up the widget" || setup["level"] != "branch" {
			t.Errorf("Setup summary = %v", got[c.setup])
		}
		root, _ := got[c.root].(map[string]any)
		if root["brief"] != "A guide to the widget" || root["level"] != "document" {
			t.Errorf("root summary = %v", got[c.root])
		}
		for name, sm := range map[string]any{"Setup": setup, "root": root} {
			m := sm.(map[string]any)
			if _, ok := m["abstract"]; ok {
				t.Errorf("%s carries the abstract: %v", name, m)
			}
			if _, ok := m["stale"]; ok {
				t.Errorf("%s is fresh yet marked stale: %v", name, m)
			}
		}
		// A chunk with no record of its own has no summary — not its section's,
		// not the document's.
		for _, id := range []string{c.step1, c.step2, c.notes} {
			if got[id] != nil {
				t.Errorf("chunk %s has no annotation, yet summary = %v", id, got[id])
			}
		}

		// The text under Setup changes: its brief is shown, marked stale.
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"body":"Changed.","revision":1}`, c.step1)); r.IsError {
			t.Fatal(r.Text)
		}
		got, _ = chunkSummaries(t, d, ctx, c.doc, true)
		if sm, _ := got[c.setup].(map[string]any); sm["stale"] != true || sm["brief"] != "Setting up the widget" {
			t.Errorf("stale Setup summary = %v, want the brief marked stale", got[c.setup])
		}
	})
}
