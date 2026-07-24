package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// provenanceOf reads back the stored provenance for an agent-scope key written
// by the fixture tool (target: tenant "", scope agent, scope_id "qa-agent").
func provenanceOf(t *testing.T, tool *Memory, key string) store.MemoryProvenance {
	t.Helper()
	prov, err := tool.Store.MemoryProvenanceGet(context.Background(), "", store.MemoryScopeAgent, "qa-agent", key)
	if err != nil {
		t.Fatalf("MemoryProvenanceGet(%s): %v", key, err)
	}
	return prov
}

// TestMemorySet_StampsConsolidatorOriginUnderTheGrant is the provenance-write
// regression. The RFC BL provenance columns existed but nothing wrote them, so
// a consolidated fact was indistinguishable from one an agent typed by hand —
// no audit trail back to the transcript it came from, and no way to filter
// machine-distilled facts. A write from a run holding the consolidation grant
// must land origin=consolidator plus the model's class + source ids.
//
// Fails-before: without provenanceForSet the row's columns stay NULL and every
// field below reads empty.
func TestMemorySet_StampsConsolidatorOriginUnderTheGrant(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	in := `{"op":"set","scope":"agent","key":"fact/likes-tabs","value":"prefers tabs",
	        "provenance":{"class":"preference","source_session_id":"sess-7","source_run_id":"run-9"}}`
	if res, _ := tool.Execute(gctx, json.RawMessage(in)); res.IsError {
		t.Fatalf("set with provenance: %s", res.Text)
	}

	got := provenanceOf(t, tool, "fact/likes-tabs")
	want := store.MemoryProvenance{
		Origin:          "consolidator",
		Class:           "preference",
		SourceSessionID: "sess-7",
		SourceRunID:     "run-9",
	}
	if got != want {
		t.Errorf("stored provenance = %+v, want %+v", got, want)
	}
}

// TestMemorySet_OriginIsNotModelSupplied: `origin` names the WRITER, so it must
// come from the server's view of the run, never the wire. An ordinary agent
// (no consolidation grant) writing a provenance block gets its class + source
// ids recorded but NO origin — it cannot label its own writes as consolidator
// output and poison the "machine-distilled facts" filter. The schema also
// refuses the field outright (additionalProperties:false inside the block).
//
// Fails-before if provenanceForSet ever reads an origin off memoryInput.
func TestMemorySet_OriginIsNotModelSupplied(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t) // fixture ctx: scopes but NO consolidation grant
	defer cleanup()

	in := `{"op":"set","scope":"agent","key":"fact/self-claimed","value":"x",
	        "provenance":{"class":"fact","source_session_id":"sess-1","origin":"consolidator"}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(in)); res.IsError {
		t.Fatalf("set: %s", res.Text)
	}

	got := provenanceOf(t, tool, "fact/self-claimed")
	if got.Origin != "" {
		t.Errorf("origin = %q for an ungranted agent, want empty — origin must be server-stamped", got.Origin)
	}
	if got.Class != "fact" || got.SourceSessionID != "sess-1" {
		t.Errorf("descriptive provenance = %+v, want class=fact source_session_id=sess-1", got)
	}
}

// TestMemorySet_WithoutProvenanceWritesNoColumns: the zero-provenance path must
// stay byte-identical to the pre-provenance write for every existing caller.
func TestMemorySet_WithoutProvenanceWritesNoColumns(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"plain","value":1}`)); res.IsError {
		t.Fatalf("plain set: %s", res.Text)
	}
	if got := provenanceOf(t, tool, "plain"); !got.IsZero() {
		t.Errorf("plain set stored provenance %+v, want all-empty", got)
	}
}

// TestMemorySet_ClampsOverlongProvenanceFields: the source ids are relayed from
// model output, so an unbounded string would bloat the row. Clamped, not
// refused — losing the audit trail is worse than truncating it.
func TestMemorySet_ClampsOverlongProvenanceFields(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	long := strings.Repeat("s", maxProvenanceFieldBytes*3)
	in := `{"op":"set","scope":"agent","key":"clamped","value":1,"provenance":{"class":"` + long + `"}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(in)); res.IsError {
		t.Fatalf("set with an overlong class: %s", res.Text)
	}
	if got := provenanceOf(t, tool, "clamped"); len(got.Class) != maxProvenanceFieldBytes {
		t.Errorf("stored class length = %d, want the %d-byte clamp", len(got.Class), maxProvenanceFieldBytes)
	}
}

// TestMemoryProvenanceGet_SupersededRowIsOpaque: a soft-archived row must read
// as a miss through the provenance reader too, matching MemoryGet — otherwise
// the provenance surface would leak the existence of superseded facts.
func TestMemoryProvenanceGet_SupersededRowIsOpaque(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	in := `{"op":"set","scope":"agent","key":"stale","value":1,"provenance":{"class":"fact"}}`
	if res, _ := tool.Execute(gctx, json.RawMessage(in)); res.IsError {
		t.Fatalf("set: %s", res.Text)
	}
	if res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"supersede","scope":"agent","key":"stale"}`)); res.IsError {
		t.Fatalf("supersede: %s", res.Text)
	}
	if _, err := tool.Store.MemoryProvenanceGet(context.Background(), "", store.MemoryScopeAgent, "qa-agent", "stale"); err == nil {
		t.Error("MemoryProvenanceGet returned a superseded row; want ErrNotFound")
	}
}
