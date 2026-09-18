package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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

// TestSourceSpansFor_CarriesTheObservationDate.
//
// recall returned no date at all. The span was believed to supply one — its own
// comment claims it "carries the turn's own leading timestamp" — but a turn split on
// sentence punctuation keeps the stamp on its FIRST sentence and strips it from every
// later one. Measured on a live store: 36 of 111 spans (32%) had a date, against 263
// of 303 facts (87%) carrying observed_at, which was being dropped at this boundary.
//
// So a "when did X happen" question had nothing to answer from on two thirds of hits,
// which is what sent the answerer to a second retrieval for a date already in hand.
func TestSourceSpansFor_CarriesTheObservationDate(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)
	const nk = "memory/fact/when-it-happened"
	const at int64 = 1683554160000000000 // 2023-05-08T13:56:00Z

	if err := d.exec(ctx, key,
		`INSERT INTO chunk_memory_meta (chunk_id, natural_key, source_quote, session_id, observed_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"c-when", nk, "I went to the support group", "s-1", at); err != nil {
		t.Skipf("fixture insert not supported on this tier: %v", err)
	}

	got := SourceSpansFor(ctx, d.SqlMem, tools.RunIdentity(ctx).TenantID,
		store.MemoryScopeUser, tools.RunIdentity(ctx).UserID, []string{nk})
	src, ok := got[nk]
	if !ok {
		t.Fatalf("no source row returned for %q (got %v)", nk, got)
	}
	if src.ObservedAt != at {
		t.Errorf("ObservedAt = %d, want %d — the date is on the row and was being "+
			"discarded at the read boundary", src.ObservedAt, at)
	}
}

// TestSourceSpansFor_ADateAloneIsStillWorthReturning.
//
// The old drop test required a span or a pointer, so a fact whose span was never
// derived but whose observation time WAS recorded got dropped entirely — the exact
// row this change exists to surface.
func TestSourceSpansFor_ADateAloneIsStillWorthReturning(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)
	const nk = "memory/fact/date-only"
	const at int64 = 1683554160000000000

	if err := d.exec(ctx, key,
		`INSERT INTO chunk_memory_meta (chunk_id, natural_key, observed_at) VALUES (?, ?, ?)`,
		"c-dateonly", nk, at); err != nil {
		t.Skipf("fixture insert not supported on this tier: %v", err)
	}
	got := SourceSpansFor(ctx, d.SqlMem, tools.RunIdentity(ctx).TenantID,
		store.MemoryScopeUser, tools.RunIdentity(ctx).UserID, []string{nk})
	if _, ok := got[nk]; !ok {
		t.Errorf("a row carrying only a date was dropped — it is the row a "+
			"\"when did X happen\" question needs most (got %v)", got)
	}
}

// TestSourceSpansFor_CarriesTheEventTimeSeparately.
//
// valid_at was SET-ONLY: the Memory schema accepts it on a write and recall never
// returned it, so the one field that answers "when did it HAPPEN" — as opposed to
// when it was said — was unreachable to the reader that needs it.
//
// "Yesterday I met them in Boston", said on the 4th, is observed_at the 4th and
// valid_at the 3rd. A probe on the live rig showed exactly this failure: handed only
// observed_at, the answerer replied "May 8, 2023" where the gold was 7 May — the day
// the remark was made, not the day of the event.
func TestSourceSpansFor_CarriesTheEventTimeSeparately(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)
	const nk = "memory/fact/said-then-happened-earlier"
	const said int64 = 1683554160000000000     // 2023-05-08
	const happened int64 = 1683467760000000000 // 2023-05-07

	if err := d.exec(ctx, key,
		`INSERT INTO chunk_memory_meta (chunk_id, natural_key, observed_at, valid_at)
		 VALUES (?, ?, ?, ?)`, "c-evt", nk, said, happened); err != nil {
		t.Skipf("fixture insert not supported on this tier: %v", err)
	}
	got := SourceSpansFor(ctx, d.SqlMem, tools.RunIdentity(ctx).TenantID,
		store.MemoryScopeUser, tools.RunIdentity(ctx).UserID, []string{nk})
	src, ok := got[nk]
	if !ok {
		t.Fatalf("no source row for %q", nk)
	}
	if src.ValidAt != happened {
		t.Errorf("ValidAt = %d, want %d — the event time is on the row and was "+
			"unreachable through recall", src.ValidAt, happened)
	}
	if src.ObservedAt != said {
		t.Errorf("ObservedAt = %d, want %d — the two must not be conflated", src.ObservedAt, said)
	}
}

// TestSourceSpansFor_AnEventTimeAloneIsStillWorthReturning. A fact whose span was
// never derived but whose EVENT time was recorded is exactly the row a temporal
// question needs; the row-keeping test must not drop it.
func TestSourceSpansFor_AnEventTimeAloneIsStillWorthReturning(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key := sidecarScope(t, d, ctx)
	const nk = "memory/fact/event-time-only"
	const happened int64 = 1683467760000000000
	if err := d.exec(ctx, key,
		`INSERT INTO chunk_memory_meta (chunk_id, natural_key, valid_at) VALUES (?, ?, ?)`,
		"c-evtonly", nk, happened); err != nil {
		t.Skipf("fixture insert not supported on this tier: %v", err)
	}
	if _, ok := SourceSpansFor(ctx, d.SqlMem, tools.RunIdentity(ctx).TenantID,
		store.MemoryScopeUser, tools.RunIdentity(ctx).UserID, []string{nk})[nk]; !ok {
		t.Error("a row carrying only an event time was dropped")
	}
}
