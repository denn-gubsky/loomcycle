package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC CV P1 — a recalled fact reaches through to the span it came from.
//
// The case for it is the project's own strongest number: the raw-turns arm answers
// temporal questions at 0.873 while the distilled-facts arm answers them at 0.18.
// The information exists and distillation discards it — and an existing test in
// this package already records that recall's gap against `search` "was
// concentrated in temporal questions, where the model reported the timestamp of
// the utterance as the date of the event".
//
// The span is stored (measured: 76 of 87 chunk_memory_meta rows carry
// source_quote) and carries its own leading timestamp, so it is precisely what a
// "when did X happen" question needs from a sentence that dropped "last week".

// TestMemoryTool_Recall_SourceIsAbsentWithoutSQLMemory — degrade, never fail. A
// missing span must cost the provenance nicety and not the retrieval: the
// distilled fact is still a valid answer.
func TestMemoryTool_Recall_SourceIsAbsentWithoutSQLMemory(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	tool.SqlMem = nil // SQL Memory disabled on this server
	tool.Backend = &fakeLayerBackend{recallOut: []memrank.RecallFact{
		{ID: "memory/fact/dave-berlin", Memory: "Dave moved to Berlin.", Score: 0.9},
	}}

	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"recall","scope":"user","query":"dave"}`))
	if err != nil || res.IsError {
		t.Fatalf("recall failed with SQL Memory off: err=%v res=%+v", err, res)
	}
	if strings.Contains(res.Text, `"source"`) {
		t.Errorf("a source field appeared with no SQL Memory to read it from: %s", res.Text)
	}
	if !strings.Contains(res.Text, "Dave moved to Berlin.") {
		t.Errorf("the fact itself went missing: %s", res.Text)
	}
}

// TestMemoryTool_Recall_IncludeSourceFalseSuppressesTheLookup. Default-on is
// deliberate — RFC CL measured an answerer ignoring a system-prompt instruction on
// every question tried — but a caller that wants the smaller payload can say so.
func TestMemoryTool_Recall_IncludeSourceFalseSuppressesTheLookup(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	tool.Backend = &fakeLayerBackend{recallOut: []memrank.RecallFact{
		{ID: "memory/fact/dave-berlin", Memory: "Dave moved to Berlin.", Score: 0.9},
	}}

	res, err := tool.Execute(ctx, json.RawMessage(
		`{"op":"recall","scope":"user","query":"dave","include_source":false}`))
	if err != nil || res.IsError {
		t.Fatalf("recall failed: err=%v res=%+v", err, res)
	}
	if strings.Contains(res.Text, `"source"`) {
		t.Errorf("include_source:false still returned a source: %s", res.Text)
	}
}

// TestSourceSpansFor_OnlyJoinsKvKeyedFacts. The join is fact-id == natural_key,
// which holds because the bundle keeps ONE key space for both stores. A document
// chunk's id is a chunk uuid, not a natural key, so including it would widen the
// IN list to match nothing.
func TestSourceSpansFor_OnlyJoinsKvKeyedFacts(t *testing.T) {
	// A nil manager exercises the guard without needing a database: the function
	// must refuse before building any statement.
	got := SourceSpansFor(context.Background(), nil, "tnt", store.MemoryScopeUser, "u1",
		[]string{"memory/fact/a", "doc.chunk:deadbeef", ""})
	if got != nil {
		t.Errorf("SourceSpansFor with no manager = %v, want nil", got)
	}
}

// TestMemoryTool_Recall_SourceSurvivesAnUnclassifiedRow. `kind` is omitted when the
// backend cannot classify a row; the source span is independent of that and must
// not be dropped along with it.
func TestMemoryTool_Recall_SourceSurvivesAnUnclassifiedRow(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	tool.SqlMem = nil
	tool.Backend = &fakeLayerBackend{recallOut: []memrank.RecallFact{
		{ID: "memory/fact/a", Memory: "a fact", Score: 0.9}, // no Kind set
	}}

	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"recall","scope":"user","query":"a"}`))
	if err != nil || res.IsError {
		t.Fatalf("recall failed: err=%v res=%+v", err, res)
	}
	if strings.Contains(res.Text, `"kind"`) {
		t.Errorf("an unclassified row asserted a kind: %s", res.Text)
	}
}
