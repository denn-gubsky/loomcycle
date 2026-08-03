package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

type contextT = context.Context

// entityID resolves a natural key back to its chunk id, through the same indexed
// lookup the tool uses.
func entityID(t *testing.T, d *Document, ctx context.Context, naturalKey string) string {
	t.Helper()
	id, err := d.chunkIDByNaturalKey(ctx, sidecarScope(t, d, ctx), naturalKey)
	if err != nil {
		t.Fatalf("lookup %s: %v", naturalKey, err)
	}
	if id == "" {
		t.Fatalf("no chunk for natural key %s", naturalKey)
	}
	return id
}

func entityFixture(t *testing.T) (*Document, contextT, string, string) {
	t.Helper()
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"entities"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	return d, ctx, asStr(out["document_id"]), asStr(out["root_chunk_id"])
}

// TestUpsertChunk_IsIdempotent is the whole point of the natural key: an entity
// mentioned five times is one chunk, not five. Without it the graph accumulates a
// near-duplicate per mention, which is the failure the consolidator's dedup bands
// exist to fight downstream — better not to create it upstream.
func TestUpsertChunk_IsIdempotent(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	body := `{"op":"upsert_chunk","scope":"user","document_id":"` + docID + `","parent_id":"` + root +
		`","title":"Ada Lovelace","natural_key":"person:ada-lovelace","type":"person"}`

	first, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("first upsert: %s", r.Text)
	}
	if created, _ := first["created"].(bool); !created {
		t.Error("the first upsert should report created=true")
	}
	id1 := asStr(first["id"])

	second, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("second upsert: %s", r.Text)
	}
	if created, _ := second["created"].(bool); created {
		t.Error("the second upsert must NOT create a second chunk")
	}
	if id2 := asStr(second["id"]); id2 != id1 {
		t.Errorf("upsert returned a different chunk (%s vs %s) — the natural key is not the identity", id2, id1)
	}
	if n := sidecarRowsFor(t, d, ctx, id1); n != 1 {
		t.Errorf("want exactly 1 sidecar row for the entity, got %d", n)
	}
}

// TestUpsertChunk_RequiresANaturalKey: without one this op is just create_chunk,
// and silently behaving like it would let a caller believe they had idempotency
// when every call added a chunk.
func TestUpsertChunk_RequiresANaturalKey(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	_, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"x"}`)
	if !r.IsError || !strings.Contains(r.Text, "natural_key") {
		t.Errorf("upsert without a natural_key must be refused: %q", r.Text)
	}
}

// TestUpsertChunk_PartialUpdateDoesNotBlankTheOtherHalf: body and fields share ONE
// blob in the Memory plane, and writeBody replaces both. An upsert carrying only a
// body must not erase fields — the exact mechanism that emptied every chunk in this
// store once.
func TestUpsertChunk_PartialUpdateDoesNotBlankTheOtherHalf(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	base := `{"op":"upsert_chunk","scope":"user","document_id":"` + docID + `","parent_id":"` + root +
		`","title":"Ada","natural_key":"person:ada"`

	if _, r := docExec(t, d, ctx, base+`,"body":"first body","fields":{"born":1815}}`); r.IsError {
		t.Fatalf("seed: %s", r.Text)
	}
	// Body only — fields must survive.
	if _, r := docExec(t, d, ctx, base+`,"body":"second body"}`); r.IsError {
		t.Fatalf("body-only upsert: %s", r.Text)
	}
	got, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+entityID(t, d, ctx, "person:ada")+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	if b, _ := got["body"].(string); b != "second body" {
		t.Errorf("body = %q, want the new one", b)
	}
	if f := asStr(got["fields"]); !strings.Contains(f, "1815") {
		t.Errorf("fields were blanked by a body-only upsert: %q", f)
	}
}

// TestSupersedeChunk_RetiresWithoutDeleting is bi-temporality's point: the old fact
// stays queryable, so a question about an earlier moment still has an answer. A
// delete would make the history unrecoverable and the correction indistinguishable
// from a fact that never existed.
func TestSupersedeChunk_RetiresWithoutDeleting(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	mk := func(key, title string) string {
		out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+root+`","title":"`+title+`","natural_key":"`+key+`"}`)
		if r.IsError {
			t.Fatalf("upsert %s: %s", key, r.Text)
		}
		return asStr(out["id"])
	}
	oldID := mk("fact:indent|is|tabs", "prefers tabs")
	newID := mk("fact:indent|is|spaces", "prefers spaces")

	if _, r := docExec(t, d, ctx, `{"op":"supersede_chunk","scope":"user","id":"`+newID+`","supersedes_id":"`+oldID+`"}`); r.IsError {
		t.Fatalf("supersede: %s", r.Text)
	}

	// The retired chunk still EXISTS and is still readable.
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+oldID+`"}`); r.IsError {
		t.Fatalf("the superseded chunk must remain queryable: %s", r.Text)
	}
	// Both end-timestamps closed.
	res, err := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx),
		d.SqlMem.Rebind(`SELECT invalid_at, expired_at FROM chunk_memory_meta WHERE chunk_id = ?`), []any{oldID})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("no sidecar row for the retired chunk")
	}
	for i, col := range []string{"invalid_at", "expired_at"} {
		if res.Rows[0][i] == nil {
			t.Errorf("%s was not closed — the fact is still current after being superseded", col)
		}
	}
	// And the replacement links to it.
	edges, r := docExec(t, d, ctx, `{"op":"get_edges","scope":"user","document_id":"`+docID+`","id":"`+newID+`"}`)
	if r.IsError {
		t.Fatalf("get_edges: %s", r.Text)
	}
	if !strings.Contains(asStr(edges), "supersedes") {
		t.Errorf("no supersedes edge from the replacement: %v", edges)
	}
}

// TestSupersedeChunk_RefusesNonsense: self-supersession and unknown ids, checked
// BEFORE anything is written. A supersede that closed a real row then failed on a
// missing one would retire a fact with nothing replacing it — which reads as "we
// forgot this" rather than "this was corrected".
func TestSupersedeChunk_RefusesNonsense(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"a","natural_key":"k:a"}`)
	id := asStr(out["id"])

	for _, tc := range []struct{ name, body, want string }{
		{"self", `{"op":"supersede_chunk","scope":"user","id":"` + id + `","supersedes_id":"` + id + `"}`, "itself"},
		{"missing new", `{"op":"supersede_chunk","scope":"user","id":"nope","supersedes_id":"` + id + `"}`, "not found"},
		{"missing old", `{"op":"supersede_chunk","scope":"user","id":"` + id + `","supersedes_id":"nope"}`, "not found"},
		{"no id", `{"op":"supersede_chunk","scope":"user","supersedes_id":"` + id + `"}`, "id"},
		{"no supersedes_id", `{"op":"supersede_chunk","scope":"user","id":"` + id + `"}`, "supersedes_id"},
	} {
		_, r := docExec(t, d, ctx, tc.body)
		if !r.IsError {
			t.Errorf("%s: must be refused", tc.name)
			continue
		}
		if !strings.Contains(r.Text, tc.want) {
			t.Errorf("%s: message should mention %q, got %q", tc.name, tc.want, r.Text)
		}
	}
	// Nothing was retired by any of those refusals.
	res, _ := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx),
		d.SqlMem.Rebind(`SELECT count(*) FROM chunk_memory_meta WHERE invalid_at IS NOT NULL`), nil)
	if n := scanCount(res.Rows); n != 0 {
		t.Errorf("a refused supersede retired %d chunk(s) anyway", n)
	}
}

// TestEntityWrite_OriginIsServerStamped: a caller must not be able to label its own
// write. A forgeable origin would let any agent mark its output machine-distilled,
// and the column has to stay a trustworthy filter for the ones that really are —
// the same rule the consolidation queue's origin follows.
func TestEntityWrite_OriginIsServerStamped(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	// Claim a privileged origin in the payload; it must be ignored.
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"x","natural_key":"k:x","origin":"consolidator"}`)
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	res, err := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx),
		d.SqlMem.Rebind(`SELECT origin FROM chunk_memory_meta WHERE chunk_id = ?`), []any{asStr(out["id"])})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if got := asStr(res.Rows[0][0]); got == "consolidator" {
		t.Errorf("origin was taken from the caller (%q) — it must be stamped from the run", got)
	}
}

// TestUpsertChunk_RejectsUnknownClass: `evidential` exempts a row from age-based
// pruning, so the value is a privilege and the set stays closed.
func TestUpsertChunk_RejectsUnknownClass(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	_, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"x","natural_key":"k:y","class":"permanent"}`)
	if !r.IsError || !strings.Contains(r.Text, "derived") {
		t.Errorf("an unknown class must be refused, naming the valid set: %q", r.Text)
	}
}

// TestEntity_TenantWritesRequireBothGrants is what PR 2 owes in place of a separate
// curator gate.
//
// Document.Execute resolves the scope centrally before dispatching, so the new ops
// inherit P4b's check: tenant scope requires `tenant` in BOTH memory_scopes and
// sql_scopes. An agent holding those IS the operator's designated curator. A second
// entity-specific gate would duplicate that decision and leave two places to get
// wrong — the argument made against exactly this in P4b.
//
// What must be proven is that the new write path cannot BYPASS it, which is why
// this is asserted rather than assumed.
func TestEntity_TenantWritesRequireBothGrants(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mem, sql []string
		allow    bool
	}{
		{"no grants", []string{"user"}, []string{"user"}, false},
		{"memory only", []string{"user", "tenant"}, []string{"user"}, false},
		{"sql only", []string{"user"}, []string{"user", "tenant"}, false},
		{"both", []string{"user", "tenant"}, []string{"user", "tenant"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ctx, _ := documentFixture(t)
			ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: tc.mem})
			ctx = tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{AllowedScopes: tc.sql})

			out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"shared"}`)
			if !tc.allow {
				if !r.IsError {
					t.Fatal("an entity write at tenant scope must be refused without both grants")
				}
				return
			}
			if r.IsError {
				t.Fatalf("both grants present: %s", r.Text)
			}
			// And the new op itself, not just create_document.
			if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"tenant","document_id":"`+
				asStr(out["document_id"])+`","parent_id":"`+asStr(out["root_chunk_id"])+
				`","title":"shared entity","natural_key":"person:shared"}`); r.IsError {
				t.Errorf("upsert_chunk at tenant scope with both grants: %s", r.Text)
			}
		})
	}
}

// TestSupersedeChunk_RefusesAForkedCorrectionChain is the invariant the whole
// supersede mechanism rests on: at any moment ONE fact is current.
//
// Nothing previously stopped two different replacements from each superseding the
// same chunk. Both succeeded, both stayed live, and graph_recall returned both as
// current — two contradictory answers to the same question, which is precisely
// what supersede-not-delete exists to prevent. A correction chain is A -> B -> C;
// to correct B you supersede B, not A.
func TestSupersedeChunk_RefusesAForkedCorrectionChain(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	mk := func(key, title string) string {
		out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+root+`","title":"`+title+`","natural_key":"`+key+`"}`)
		if r.IsError {
			t.Fatalf("upsert %s: %s", key, r.Text)
		}
		return asStr(out["id"])
	}
	old := mk("fact:indent|is|tabs", "prefers tabs")
	b := mk("fact:indent|is|spaces", "prefers spaces")
	c := mk("fact:indent|is|two", "prefers 2-space indent")

	if _, r := docExec(t, d, ctx, `{"op":"supersede_chunk","scope":"user","id":"`+b+`","supersedes_id":"`+old+`"}`); r.IsError {
		t.Fatalf("first supersede: %s", r.Text)
	}
	_, r := docExec(t, d, ctx, `{"op":"supersede_chunk","scope":"user","id":"`+c+`","supersedes_id":"`+old+`"}`)
	if !r.IsError {
		t.Fatalf("a SECOND replacement for the same chunk was accepted — the correction " +
			"chain forked and two contradictory facts are now both current")
	}
	// The refusal must name the existing replacement: that is the id the caller
	// almost certainly meant to supersede.
	if !strings.Contains(r.Text, b) {
		t.Errorf("the refusal does not name the existing replacement %q, so the caller "+
			"cannot tell what to supersede instead: %s", b, r.Text)
	}
	// Exactly one chunk supersedes it.
	res, err := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx),
		d.SqlMem.Rebind(`SELECT from_id FROM chunk_edges WHERE to_id = ? AND kind = 'supersedes'`), []any{old})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 {
		t.Errorf("%d chunks supersede the retired fact, want 1", len(res.Rows))
	}
}

// TestSupersedeChunk_SamePairIsIdempotent — these ops are driven by a background
// consolidator that retries after a partial failure, so re-superseding with the
// SAME replacement must be a clean no-op.
//
// It previously surfaced the raw engine error ("UNIQUE constraint failed:
// chunk_edges.from_id, ...") from the edge insert, which both reads as a hard
// failure to a retrying caller and leaks schema internals into a model-visible
// tool result.
func TestSupersedeChunk_SamePairIsIdempotent(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	mk := func(key, title string) string {
		out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+root+`","title":"`+title+`","natural_key":"`+key+`"}`)
		if r.IsError {
			t.Fatalf("upsert: %s", r.Text)
		}
		return asStr(out["id"])
	}
	old := mk("fact:x", "old")
	newer := mk("fact:y", "newer")

	for i := 1; i <= 2; i++ {
		out, r := docExec(t, d, ctx, `{"op":"supersede_chunk","scope":"user","id":"`+newer+`","supersedes_id":"`+old+`"}`)
		if r.IsError {
			t.Fatalf("supersede attempt %d failed: %s", i, r.Text)
		}
		if i == 2 && out["already"] != true {
			t.Errorf("the repeat did not report already=true, so a retrying consolidator "+
				"cannot distinguish 'done' from 'did it again': %v", out)
		}
	}
}

// TestGraphRecall_AFutureEndDateIsStillCurrent — "has an end date" is not "has
// ended".
//
// A fact may carry a KNOWN FUTURE end ("the contract runs until 2027") and be
// perfectly true right now. The default filter required invalid_at IS NULL, so
// recording an end date acted as immediate deletion: the fact disappeared from
// every default recall the moment it was written.
//
// The decisive check is the second half. The default IS "as_of now", so it must
// agree with the as_of predicate given the current time — and it did not, which
// is what makes this a bug rather than a judgement call.
func TestGraphRecall_AFutureEndDateIsStillCurrent(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	future := time.Now().Add(365 * 24 * time.Hour).UnixNano()
	past := time.Now().Add(-365 * 24 * time.Hour).UnixNano()

	mk := func(key, title string, invalid int64) {
		t.Helper()
		body := fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":"%s","parent_id":"%s",`+
			`"title":"%s","natural_key":"%s","invalid_at":%d}`, docID, root, title, key, invalid)
		if _, r := docExec(t, d, ctx, body); r.IsError {
			t.Fatalf("upsert %s: %s", key, r.Text)
		}
	}
	mk("fact:contract", "contract runs until 2027", future)
	mk("fact:oldjob", "worked at Initech", past)

	// Whole-word title matching means each fact needs its own query term.
	recall := func(term, extra string) string {
		t.Helper()
		out, r := docExec(t, d, ctx,
			`{"op":"graph_recall","scope":"user","query":"`+term+`"`+extra+`}`)
		if r.IsError {
			t.Fatalf("graph_recall(%s%s): %s", term, extra, r.Text)
		}
		return asStr(out)
	}

	got := recall("contract", "")
	if !strings.Contains(got, "contract runs until 2027") {
		t.Errorf("a fact with a FUTURE end date is missing from a default recall — "+
			"recording an end date deleted it: %s", got)
	}
	// A genuinely-ended fact must still be hidden, or the fix has simply disabled
	// the filter.
	if ended := recall("worked", ""); strings.Contains(ended, "worked at Initech") {
		t.Errorf("a fact whose end date has PASSED is being returned as current: %s", ended)
	}
	// The label must not contradict the result it decorates.
	//
	// Matched against the Go map rendering (retired:true, no quotes) that asStr
	// produces — an earlier version of this assertion looked for the JSON form
	// `"retired":true`, which cannot appear in this output and therefore could
	// never fail.
	if strings.Contains(got, "retired:true") {
		t.Errorf("the current fact is labelled retired even though its end date is in "+
			"the future — the label contradicts the result it decorates: %s", got)
	}
	// And the default must agree with as_of=<now>, since that is what it means.
	asOf := recall("contract", fmt.Sprintf(`,"as_of":%d`, time.Now().UnixNano()))
	if strings.Contains(asOf, "contract") != strings.Contains(got, "contract") {
		t.Errorf("the default and as_of=now disagree about the same fact:\n default: %s\n as_of:   %s",
			got, asOf)
	}
}

// TestChunkParentage_RefusesAnUnreachableParent covers create_chunk and
// move_chunk together, because they had the SAME hole and it produces the same
// outcome: a chunk that exists and cannot be reached.
//
// Neither validated a directly-supplied parent. Both returned ok/success. And the
// chunk then vanished from get_document and export_md, because every walk
// descends from the root and nothing led to it — so a caller wrote content, was
// told it worked, and never saw it again. That is worse than an error, and the
// dead-link sweeper does not catch it: it looks for a chunk whose DOCUMENT is
// missing, not one whose PARENT is.
//
// Cross-document parentage is the same corruption from the other side — the
// chunk keeps its own document_id while its parent_id points into another tree.
func TestChunkParentage_RefusesAnUnreachableParent(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	const ghost = "deadbeefdeadbeefdeadbeefdeadbeef"

	other, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"other doc"}`)
	if r.IsError {
		t.Fatalf("create other doc: %s", r.Text)
	}
	otherRoot := asStr(other["root_chunk_id"])

	t.Run("create under a missing parent", func(t *testing.T) {
		_, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+ghost+`","title":"orphan-at-birth"}`)
		if !r.IsError {
			t.Fatal("accepted a non-existent parent_id — the chunk is written and " +
				"immediately unreachable from the document root")
		}
	})
	t.Run("create under another document", func(t *testing.T) {
		_, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+otherRoot+`","title":"cross-doc-child"}`)
		if !r.IsError {
			t.Fatal("accepted a parent in a different document")
		}
	})

	// The document must be unchanged by the refusals.
	md, _ := docExec(t, d, ctx, `{"op":"export_md","scope":"user","id":"`+docID+`"}`)
	for _, ghostTitle := range []string{"orphan-at-birth", "cross-doc-child"} {
		if strings.Contains(asStr(md), ghostTitle) {
			t.Errorf("a refused create still wrote %q", ghostTitle)
		}
	}

	victim, vr := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"movable"}`)
	if vr.IsError {
		t.Fatalf("create movable: %s", vr.Text)
	}
	vid := asStr(victim["id"])

	t.Run("move under a missing parent", func(t *testing.T) {
		_, r := docExec(t, d, ctx, `{"op":"move_chunk","scope":"user","id":"`+vid+
			`","new_parent_id":"`+ghost+`"}`)
		if !r.IsError {
			t.Fatal("accepted a non-existent new_parent_id — the chunk is orphaned")
		}
	})
	t.Run("move under another document", func(t *testing.T) {
		_, r := docExec(t, d, ctx, `{"op":"move_chunk","scope":"user","id":"`+vid+
			`","new_parent_id":"`+otherRoot+`"}`)
		if !r.IsError {
			t.Fatal("accepted a cross-document new_parent_id")
		}
	})

	// The chunk survived both refusals AND is still reachable.
	md2, _ := docExec(t, d, ctx, `{"op":"export_md","scope":"user","id":"`+docID+`"}`)
	if !strings.Contains(asStr(md2), "movable") {
		t.Errorf("the chunk is no longer reachable after the refused moves: %s", asStr(md2))
	}

	// And moving to the ROOT level is still allowed — the fix must not narrow that.
	if _, r := docExec(t, d, ctx, `{"op":"move_chunk","scope":"user","id":"`+vid+
		`","new_parent_id":""}`); r.IsError {
		t.Errorf("moving to the root level was refused: %s", r.Text)
	}
}
