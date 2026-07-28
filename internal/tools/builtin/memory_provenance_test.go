package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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
// output and poison the "machine-distilled facts" filter.
//
// The Go struct has no Origin field, so the refusal is unconditional whatever the
// wire says; the schema's additionalProperties:false on the provenance block
// (asserted separately below) is the belt to that braces, and matters for
// providers that validate function-call arguments strictly.
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

// TestMemory_ProvenanceSchemaIsClosed: the provenance block must declare
// additionalProperties:false. Two reasons. The comment on
// TestMemorySet_OriginIsNotModelSupplied claimed it already did and it did not, and
// a comment that describes a guard which is absent is worse than no comment. And an
// open sub-object invites a model to send `origin` — silently ignored by the Go
// decoder, so the model believes it set the writer identity — while providers that
// validate function-call arguments strictly can now reject it at the boundary
// instead.
func TestMemory_ProvenanceSchemaIsClosed(t *testing.T) {
	var schema struct {
		Properties struct {
			Provenance struct {
				AdditionalProperties *bool               `json:"additionalProperties"`
				Properties           map[string]struct{} `json:"properties"`
			} `json:"provenance"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(memoryInputSchema), &schema); err != nil {
		t.Fatalf("decode memoryInputSchema: %v", err)
	}
	prov := schema.Properties.Provenance
	if prov.AdditionalProperties == nil || *prov.AdditionalProperties {
		t.Error("the provenance block must set additionalProperties:false — an open block invites a model to send `origin`, which the server then silently drops")
	}
	if _, ok := prov.Properties["origin"]; ok {
		t.Error("the provenance schema declares `origin` — the writer identity is server-stamped and must never be offered on the wire")
	}
	for _, want := range []string{"class", "source_session_id", "source_run_id"} {
		if _, ok := prov.Properties[want]; !ok {
			t.Errorf("closing the block dropped the legitimate field %q", want)
		}
	}
}

// TestMemorySet_ConsolidatorOriginRequiresARun: the stamp means "a background pass
// distilled this from a transcript". The OPERATOR planes (MCP operatorCtx, HTTP
// substrate-admin, gRPC substrate) all hand out Consolidation: true alongside
// their wildcard memory scopes, and none of them is a run — so keying the stamp on
// the grant alone meant a plain `Memory op=set` from any authenticated MCP session
// landed origin=consolidator with nothing having distilled it. That hollows out the
// only thing the column is for: a trustworthy filter for machine-distilled facts.
//
// Both directions, because either half alone is the bug: grant + run ⇒ stamped;
// grant + NO run (the operator-plane shape) ⇒ not stamped, while the descriptive
// fields still record normally.
//
// Fails-before if provenanceForSet checks only MemoryPolicy(ctx).Consolidation.
func TestMemorySet_ConsolidatorOriginRequiresARun(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	// An operator-plane context: the grant and open scopes, but no run id.
	operatorCtx := tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user"},
		Consolidation: true,
	})
	in := `{"op":"set","scope":"agent","key":"fact/from-operator","value":"x","provenance":{"class":"fact"}}`
	if res, _ := tool.Execute(operatorCtx, json.RawMessage(in)); res.IsError {
		t.Fatalf("operator-plane set: %s", res.Text)
	}
	got := provenanceOf(t, tool, "fact/from-operator")
	if got.Origin != "" {
		t.Errorf("origin = %q for a grant-holding context with no run, want empty — nothing distilled this", got.Origin)
	}
	if got.Class != "fact" {
		t.Errorf("descriptive provenance = %+v, want class=fact recorded as usual", got)
	}

	// The same grant inside a real run IS a consolidation pass.
	runCtx := grantedConsolidationCtx(ctx)
	inRun := `{"op":"set","scope":"agent","key":"fact/from-pass","value":"x","provenance":{"class":"fact"}}`
	if res, _ := tool.Execute(runCtx, json.RawMessage(inRun)); res.IsError {
		t.Fatalf("in-run set: %s", res.Text)
	}
	if got := provenanceOf(t, tool, "fact/from-pass"); got.Origin != "consolidator" {
		t.Errorf("origin = %q for a granted RUN, want consolidator — the stamp must still work where it belongs", got.Origin)
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
	leaseTarget(t, tool, gctx, "agent") // supersede is lease-gated
	if res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"supersede","scope":"agent","key":"stale"}`)); res.IsError {
		t.Fatalf("supersede: %s", res.Text)
	}
	if _, err := tool.Store.MemoryProvenanceGet(context.Background(), "", store.MemoryScopeAgent, "qa-agent", "stale"); err == nil {
		t.Error("MemoryProvenanceGet returned a superseded row; want ErrNotFound")
	}
}

// enqueuePending seeds a pending-queue row for the fixture's target
// (tenant "", scope agent, scope_id "qa-agent") with a server-set origin.
func enqueuePending(t *testing.T, tool *Memory, id, origin, sessionID, runID string) {
	t.Helper()
	if err := tool.Store.MemoryPendingEnqueue(context.Background(), store.MemoryPendingRow{
		ID: id, TenantID: "", Scope: store.MemoryScopeAgent, ScopeID: "qa-agent",
		Payload: json.RawMessage(`{"messages":[]}`),
		Origin:  origin, SourceSessionID: sessionID, SourceRunID: runID,
	}); err != nil {
		t.Fatalf("enqueue pending %s: %v", id, err)
	}
}

// TestMemorySet_FromPendingResolvesProvenanceServerSide is the RFC BL P3 write
// path: the model supplies a POINTER to a row it drained, and the SERVER supplies
// the values. That is what lets `origin` stay unforgeable while still recording
// that a fact came from a compaction flush rather than a scheduled pass —
// provenanceForSet alone would stamp `consolidator` for both.
//
// Fails-before: without resolveFromPending the row lands origin=consolidator and
// no source ids, indistinguishable from any other consolidated fact.
func TestMemorySet_FromPendingResolvesProvenanceServerSide(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	enqueuePending(t, tool, "mp_1", store.PendingOriginCompaction, "sess-compact", "run-compact")

	in := `{"op":"set","scope":"agent","key":"fact/from-compaction","value":"user runs ROCm",
	        "from_pending":"mp_1"}`
	if res, _ := tool.Execute(gctx, json.RawMessage(in)); res.IsError {
		t.Fatalf("set from_pending: %s", res.Text)
	}

	got := provenanceOf(t, tool, "fact/from-compaction")
	want := store.MemoryProvenance{
		Origin:          store.PendingOriginCompaction,
		SourceSessionID: "sess-compact",
		SourceRunID:     "run-compact",
	}
	if got != want {
		t.Errorf("stored provenance = %+v, want %+v — the pending row's server-recorded origin must win over the writer-identity stamp", got, want)
	}
}

// TestMemorySet_FromPendingUnownedIsIgnoredAndIndistinguishable is the forgery
// arm. A pending id the caller does not own must neither attribute the write nor
// refuse it — and crucially, an unowned id and a nonexistent one must be
// INDISTINGUISHABLE, or `from_pending` becomes a probe for which ids exist.
//
// Ignoring rather than refusing is deliberate: a stale reference must not fail an
// otherwise-correct write, and the field grants nothing anyway — the caller was
// already authorized to write this key.
func TestMemorySet_FromPendingUnownedIsIgnoredAndIndistinguishable(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	// A real row belonging to a DIFFERENT target (another scope_id in the same
	// tenant) — the id exists, but not for this caller.
	if err := tool.Store.MemoryPendingEnqueue(context.Background(), store.MemoryPendingRow{
		ID: "mp_foreign", TenantID: "", Scope: store.MemoryScopeAgent, ScopeID: "someone-else",
		Payload: json.RawMessage(`{"messages":[]}`),
		Origin:  store.PendingOriginCompaction, SourceRunID: "run-theirs",
	}); err != nil {
		t.Fatal(err)
	}

	foreign, _ := tool.Execute(gctx, json.RawMessage(
		`{"op":"set","scope":"agent","key":"fact/a","value":"x","from_pending":"mp_foreign"}`))
	unknown, _ := tool.Execute(gctx, json.RawMessage(
		`{"op":"set","scope":"agent","key":"fact/b","value":"x","from_pending":"mp_nope"}`))

	if foreign.IsError || unknown.IsError {
		t.Fatalf("an unresolvable from_pending must not fail the write: foreign=%q unknown=%q", foreign.Text, unknown.Text)
	}
	if foreign.Text != unknown.Text {
		t.Errorf("an unowned id and an unknown id produced DIFFERENT responses:\n foreign=%s\n unknown=%s\nthat makes from_pending an existence probe", foreign.Text, unknown.Text)
	}
	// Neither write borrowed the foreign row's provenance.
	for _, key := range []string{"fact/a", "fact/b"} {
		got := provenanceOf(t, tool, key)
		if got.SourceRunID == "run-theirs" || got.Origin == store.PendingOriginCompaction {
			t.Errorf("%s borrowed provenance from a row the caller does not own: %+v", key, got)
		}
		// It still gets the ordinary identity-derived stamp — the field is not a
		// gate, so the write proceeds exactly as it would without it.
		if got.Origin != "consolidator" {
			t.Errorf("%s origin = %q, want the ordinary consolidator stamp", key, got.Origin)
		}
	}
}

// TestMemoryGet_OriginAvailableFalseWhenChatDeleted is the Decision 4 invariant,
// and the one most likely to be implemented wrongly.
//
// RFC BM retention deletes chats on a clock, so a fact's origin link is EXPECTED
// to go dead. When it does, the fact must still be returned, must still carry its
// recorded ids verbatim, and must simply report origin_available=false. Anything
// that pruned or blanked the fact here would let retention silently destroy
// memory — deleting a 3-day-old chat would delete what was learned from it.
func TestMemoryGet_OriginAvailableFalseWhenChatDeleted(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	// A real session in the fixture's tenant (""), so the link resolves at first.
	sess, err := tool.Store.CreateSession(context.Background(), "", "qa-agent", "u1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessID := sess.ID
	enqueuePending(t, tool, "mp_live", store.PendingOriginCompaction, sessID, "")
	if res, _ := tool.Execute(gctx, json.RawMessage(
		`{"op":"set","scope":"agent","key":"fact/sourced","value":"v","from_pending":"mp_live"}`)); res.IsError {
		t.Fatalf("set: %s", res.Text)
	}

	read := func() map[string]any {
		res, _ := tool.Execute(gctx, json.RawMessage(
			`{"op":"get","scope":"agent","key":"fact/sourced","include_provenance":true}`))
		if res.IsError {
			t.Fatalf("get: %s", res.Text)
		}
		out := decodeResult(t, res.Text)
		prov, ok := out["provenance"].(map[string]any)
		if !ok {
			t.Fatalf("get returned no provenance block: %v", out)
		}
		return prov
	}

	prov := read()
	if prov["origin_available"] != true {
		t.Errorf("origin_available = %v with the session present, want true", prov["origin_available"])
	}
	if prov["source_session_id"] != sessID || prov["origin"] != store.PendingOriginCompaction {
		t.Errorf("provenance = %v, want session %q + compaction origin", prov, sessID)
	}

	// Retention deletes the chat.
	if err := tool.Store.DeleteSessionCascade(context.Background(), sessID); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	prov = read()
	if prov["origin_available"] != false {
		t.Errorf("origin_available = %v after the chat was deleted, want false", prov["origin_available"])
	}
	// The ids are NOT rewritten or cleared — a dead citation is still the honest
	// record of where the fact came from, and it is what an audit needs.
	if prov["source_session_id"] != sessID {
		t.Errorf("source_session_id = %v, want the id retained verbatim (%q)", prov["source_session_id"], sessID)
	}
	// And the fact itself survives.
	res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"get","scope":"agent","key":"fact/sourced"}`))
	if res.IsError {
		t.Fatalf("the fact did not survive its origin being deleted: %s", res.Text)
	}
	if v, _ := decodeResult(t, res.Text)["value"]; v == nil {
		t.Error("the fact's value was blanked when its origin went away")
	}
}

// TestMemoryAdd_StampsAgentExplicitOrigin covers the real `add` path, which the
// other tests in this file bypass by seeding pending rows directly.
//
// That gap was found by fail-before verification: removing the stamp from
// inprocess.Add broke NOTHING, because every other test enqueued through the
// store. An origin the server sets is only trustworthy if something asserts the
// server actually sets it on the path an agent uses.
func TestMemoryAdd_StampsAgentExplicitOrigin(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	in := `{"op":"add","scope":"agent","infer":true,
	        "messages":[{"role":"user","content":"I use ROCm on an AMD card"}]}`
	if res, _ := tool.Execute(gctx, json.RawMessage(in)); res.IsError {
		t.Fatalf("add: %s", res.Text)
	}

	rows, err := tool.Store.MemoryPendingDrain(context.Background(), "", store.MemoryScopeAgent, "qa-agent", 10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("drain returned %d rows, want 1", len(rows))
	}
	if rows[0].Origin != store.PendingOriginAgentExplicit {
		t.Errorf("enqueued origin = %q, want %q — an agent reaching `add` IS an explicit agent write, and the value must come from the server rather than the tool input",
			rows[0].Origin, store.PendingOriginAgentExplicit)
	}
}
