package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// countingGenerator writes one claim and one question per chunk, recording which
// chunks (by section text) it was asked about.
type countingGenerator struct {
	mu    sync.Mutex
	texts []string
	fail  map[string]bool
}

func (g *countingGenerator) ModelID() string { return "test-writer" }
func (g *countingGenerator) Generate(_ context.Context, r memrank.UnitRequest) ([]memrank.GeneratedUnit, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.texts = append(g.texts, r.Text)
	if g.fail[r.Text] {
		return nil, fmt.Errorf("the model is down")
	}
	var out []memrank.GeneratedUnit
	for _, k := range r.Kinds {
		switch k {
		case "claims":
			out = append(out, memrank.GeneratedUnit{Kind: memrank.UnitClaim, Text: "claim about " + r.Text})
		case "questions":
			out = append(out, memrank.GeneratedUnit{Kind: memrank.UnitQuestion, Text: "question about " + r.Text})
		case "description":
			out = append(out, memrank.GeneratedUnit{Kind: memrank.UnitDescription, Text: "describes " + r.Text})
		}
	}
	return out, nil
}

func (g *countingGenerator) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.texts)
}

// deriveDoc creates a document at path with prose chunks, and returns its root and
// chunk ids by body.
func deriveDoc(t *testing.T, d *Document, ctx context.Context, path string, indexUnits any, bodies ...string) (string, map[string]string) {
	t.Helper()
	out := docOp(t, d, ctx, map[string]any{"op": "create_document", "title": "Policy", "path": path})
	docID, rootID := out["document_id"].(string), out["root_chunk_id"].(string)
	ids := map[string]string{}
	for i, b := range bodies {
		c := docOp(t, d, ctx, map[string]any{"op": "create_chunk", "document_id": docID, "title": fmt.Sprintf("Section %d", i), "body": b})
		ids[b] = c["id"].(string)
	}
	if indexUnits != nil {
		docOp(t, d, ctx, map[string]any{"op": "update_chunk", "id": rootID, "revision": chunkRevision(t, d, ctx, rootID),
			"fields": map[string]any{"index_units": indexUnits}})
	}
	return docID, ids
}

func derive(t *testing.T, d *Document, ctx context.Context, gen memrank.UnitGenerator, opts DeriveUnitsOptions) DeriveUnitsReport {
	t.Helper()
	rep, err := d.DeriveUnits(ctx, "user", gen, opts)
	if err != nil {
		t.Fatalf("DeriveUnits: %v", err)
	}
	return rep
}

// TestDeriveUnits_ADryRunWritesNothing — a dry run reports what would be generated
// and makes no model call.
func TestDeriveUnits_ADryRunWritesNothing(t *testing.T) {
	d, _, ctx, _ := unitsDocFixture(t)
	_, ids := deriveDoc(t, d, ctx, "/docs/leave", true, "carry over five days", "sick leave needs a note")
	gen := &countingGenerator{}
	rep := derive(t, d, ctx, gen, DeriveUnitsOptions{DryRun: true})
	if rep.Generated != 2 || rep.DocumentsOptedIn != 1 || gen.calls() != 0 {
		t.Errorf("dry run: %+v, %d model calls", rep, gen.calls())
	}
	for _, id := range ids {
		if n := len(unitKeysOf(t, d, ctx, id)); n != 0 {
			t.Errorf("the dry run wrote %d units", n)
		}
	}
}

// TestDeriveUnits_AnEditedBodyRewritesExactlyThatChunk — the RFC's C2 check: after
// a real pass everything is up to date and a second pass costs nothing; editing one
// chunk's body makes exactly its units stale, and the next pass rewrites only those.
func TestDeriveUnits_AnEditedBodyRewritesExactlyThatChunk(t *testing.T) {
	d, _, ctx, _ := unitsDocFixture(t)
	_, ids := deriveDoc(t, d, ctx, "/docs/leave", true, "carry over five days", "sick leave needs a note")
	gen := &countingGenerator{}
	rep := derive(t, d, ctx, gen, DeriveUnitsOptions{})
	if rep.Generated != 2 || rep.UnitsWritten != 6 || rep.Model != "test-writer" {
		t.Fatalf("first pass: %+v", rep)
	}
	us, _ := d.UnitsForChunk(ctx, "user", ids["carry over five days"])
	if len(us) != 3 || us[0].Model != "test-writer" || us[0].BodySHA256 == "" {
		t.Errorf("stored units = %+v", us)
	}

	calls := gen.calls()
	if again := derive(t, d, ctx, gen, DeriveUnitsOptions{}); again.UpToDate != 2 || again.Generated+again.Rewritten != 0 || gen.calls() != calls {
		t.Fatalf("second pass over an unchanged document: %+v, %d new calls", again, gen.calls()-calls)
	}

	id := ids["sick leave needs a note"]
	docOp(t, d, ctx, map[string]any{"op": "update_chunk", "id": id, "revision": chunkRevision(t, d, ctx, id),
		"body": "sick leave needs a doctor's note after three days"})
	rep = derive(t, d, ctx, gen, DeriveUnitsOptions{})
	if rep.Rewritten != 1 || rep.UpToDate != 1 || rep.Generated != 0 {
		t.Errorf("after one edit: %+v", rep)
	}
	if last := gen.texts[len(gen.texts)-1]; gen.calls() != calls+1 || last != "sick leave needs a doctor's note after three days" {
		t.Errorf("the pass called the model %d times (last on %q), want exactly the edited chunk", gen.calls()-calls, last)
	}
}

// TestDeriveUnits_ADocumentWithoutIndexUnitsIsNeverTouched — the RFC's C2 check.
// So is one that opts out explicitly under a marked subtree: its own setting wins.
func TestDeriveUnits_ADocumentWithoutIndexUnitsIsNeverTouched(t *testing.T) {
	d, _, ctx, _ := unitsDocFixture(t)
	_, plain := deriveDoc(t, d, ctx, "/docs/other", nil, "not opted in")
	_, optedOut := deriveDoc(t, d, ctx, "/docs/policies/secret", false, "opted out under a marked subtree")
	_, marked := deriveDoc(t, d, ctx, "/docs/policies/leave", nil, "marked by the subtree")
	gen := &countingGenerator{}
	rep := derive(t, d, ctx, gen, DeriveUnitsOptions{Generator: config.UnitGeneratorConfig{
		Subtrees: []config.UnitSubtree{{Path: "/docs/policies", Kinds: []string{"questions"}}}}})
	if rep.Generated != 1 || !reflect.DeepEqual(gen.texts, []string{"marked by the subtree"}) {
		t.Errorf("pass: %+v, model saw %v", rep, gen.texts)
	}
	for name, ids := range map[string]map[string]string{"plain": plain, "opted out": optedOut} {
		for _, id := range ids {
			if n := len(unitKeysOf(t, d, ctx, id)); n != 0 {
				t.Errorf("%s document got %d units", name, n)
			}
		}
	}
	us, _ := d.UnitsForChunk(ctx, "user", marked["marked by the subtree"])
	if len(us) != 1 || us[0].Kind != memrank.UnitQuestion {
		t.Errorf("the subtree's kinds were not honoured: %+v", us)
	}
}

// TestDeriveUnits_TheMemoryTreesAreRefusedAndReported — a document named under
// /facts, or holding memory entity metadata, never gets units even when it opts in,
// and the refusal is reported, not silent.
func TestDeriveUnits_TheMemoryTreesAreRefusedAndReported(t *testing.T) {
	d, _, ctx, _ := unitsDocFixture(t)
	deriveDoc(t, d, ctx, "/facts/ada", true, "a fact tree")
	_, ids := deriveDoc(t, d, ctx, "/docs/entities", true, "an entity document")
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.exec(ctx, key, `INSERT INTO chunk_memory_meta (chunk_id, natural_key) VALUES (?, ?)`, ids["an entity document"], "person:ada"); err != nil {
		t.Fatal(err)
	}
	gen := &countingGenerator{}
	rep := derive(t, d, ctx, gen, DeriveUnitsOptions{})
	if gen.calls() != 0 || len(rep.SkippedByRule) != 2 {
		t.Fatalf("memory trees: %d model calls, skipped %+v", gen.calls(), rep.SkippedByRule)
	}
	for _, s := range rep.SkippedByRule {
		if s.Reason == "" || s.DocumentID == "" {
			t.Errorf("an unexplained skip: %+v", s)
		}
	}
}

// TestDeriveUnits_ALimitStopsAndTheCursorResumes — limit bounds the model calls one
// request makes; following next_cursor reaches every chunk exactly once.
func TestDeriveUnits_ALimitStopsAndTheCursorResumes(t *testing.T) {
	d, _, ctx, _ := unitsDocFixture(t)
	deriveDoc(t, d, ctx, "/docs/a", true, "a1", "a2", "a3")
	deriveDoc(t, d, ctx, "/docs/b", []string{"claims"}, "b1", "b2")
	gen := &countingGenerator{}
	total, cursor := 0, ""
	for calls := 0; ; calls++ {
		if calls > 10 {
			t.Fatal("the cursor did not terminate")
		}
		rep := derive(t, d, ctx, gen, DeriveUnitsOptions{Limit: 2, After: cursor})
		if rep.Generated > 2 {
			t.Errorf("a call generated %d chunks past its limit of 2", rep.Generated)
		}
		total += rep.Generated
		if !rep.More {
			break
		}
		cursor = rep.NextCursor
	}
	if total != 5 || gen.calls() != 5 {
		t.Errorf("generated %d chunks in %d model calls, want 5 and 5 (each once)", total, gen.calls())
	}
}

// TestDeriveUnits_AFailedChunkKeepsItsUnits — a model fault is no reason to delete
// units that still match their chunk; it is counted and named.
func TestDeriveUnits_AFailedChunkKeepsItsUnits(t *testing.T) {
	d, _, ctx, _ := unitsDocFixture(t)
	_, ids := deriveDoc(t, d, ctx, "/docs/leave", true, "carry over five days")
	id := ids["carry over five days"]
	derive(t, d, ctx, &countingGenerator{}, DeriveUnitsOptions{})
	docOp(t, d, ctx, map[string]any{"op": "update_chunk", "id": id, "revision": chunkRevision(t, d, ctx, id), "body": "carry over ten days"})
	rep := derive(t, d, ctx, &countingGenerator{fail: map[string]bool{"carry over ten days": true}}, DeriveUnitsOptions{})
	if rep.Failed != 1 || len(rep.FailedChunks) != 1 || rep.FirstFailure == "" {
		t.Errorf("report = %+v", rep)
	}
	if n := len(unitKeysOf(t, d, ctx, id)); n != 3 {
		t.Errorf("the chunk's units after a failed rewrite: %d, want its 3 old ones kept", n)
	}
}

func TestParseIndexUnits(t *testing.T) {
	for raw, want := range map[string][]string{
		`true`:                               config.UnitKinds,
		`false`:                              nil,
		`[]`:                                 nil,
		`["questions","claims","questions"]`: {"claims", "questions"},
		`["summary"]`:                        nil,
		`"yes"`:                              nil,
	} {
		if got := parseIndexUnits(json.RawMessage(raw)); !reflect.DeepEqual(got, want) {
			t.Errorf("parseIndexUnits(%s) = %v, want %v", raw, got, want)
		}
	}
}
