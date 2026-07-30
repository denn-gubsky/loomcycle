package builtin

import (
	"context"
	"strings"
	"testing"

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
