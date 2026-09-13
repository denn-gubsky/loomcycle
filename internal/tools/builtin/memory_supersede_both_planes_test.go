package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A FACT IS RETIRED IN BOTH PLANES OR IN NEITHER (RFC CV, decision 4).
//
// Once the chunk became a fact's only home, retirement stopped being one write.
// The body row in the k/v plane is what recall and search read; the sidecar in SQL
// Memory is what list_facts and graph_recall read. `Memory supersede` closed only
// the first and `Document supersede_chunk` only the second, so the consolidator had
// to call both and bridge a natural key to a chunk id itself — and the bridge was
// the bug: recall reports a chunk-homed fact under its NATURAL key, retirement
// handed that name straight to the k/v store where every row is keyed
// `doc.chunk:<hex>`, and MemorySupersede treats a missing key as a no-op returning
// nil. The k/v half stamped nothing and reported success.

// supersedeFixture wires a Memory and a Document tool onto ONE store and ONE SQL
// Memory manager — which is the whole point: the defect only exists where the two
// planes hold halves of the same fact, so a fixture that gives each tool its own
// store cannot see it.
func supersedeFixture(t *testing.T) (*Memory, *Document, context.Context) {
	t.Helper()
	d, ctx, s := documentFixture(t)
	m := &Memory{Store: s, SqlMem: d.SqlMem, MaxValueBytes: 65536}
	ctx = tools.WithRunID(ctx, "run_consolidation_pass")
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user"},
		Consolidation: true,
	})
	return m, d, ctx
}

// writeFact stores one chunk-homed fact and returns its chunk id.
func writeFact(t *testing.T, d *Document, ctx context.Context, doc, key, text string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"`+key+`","title":"`+text+`","body":"`+text+`","type":"fact","subject":"Dave"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk %s: %s", key, r.Text)
	}
	return asStr(out["id"])
}

// TestMemorySupersede_ClosesTheFactInBothPlanes is the regression.
//
// FAILS BEFORE: the k/v stamp was applied to the natural key, which names no row,
// so the body row stayed live and the fact kept answering recalls after being
// retired. The graph assertion passed even then — which is exactly why the split
// went unnoticed.
func TestMemorySupersede_ClosesTheFactInBothPlanes(t *testing.T) {
	m, d, ctx := supersedeFixture(t)
	doc := newEntityDoc(t, d, ctx)

	const staleKey = "memory/fact/dave-works-at-shop"
	const freshKey = "memory/fact/dave-works-at-mill"
	staleID := writeFact(t, d, ctx, doc, staleKey, "Dave works at the shop.")
	freshID := writeFact(t, d, ctx, doc, freshKey, "Dave works at the mill.")

	leaseUser(t, m, ctx)
	res, _ := m.Execute(ctx, json.RawMessage(
		`{"op":"supersede","scope":"user","key":"`+staleKey+`","superseded_by":"`+freshKey+`"}`))
	if res.IsError {
		t.Fatalf("supersede: %s", res.Text)
	}

	// PLANE 1 — the k/v body row, which is what recall and search read. This is the
	// half that silently did nothing.
	bodyKey := memory.DocumentChunkKeyPrefix + staleID
	sk := sidecarScope(t, d, ctx)
	if live := bodyRowIsLive(t, m, ctx, sk.ScopeID, bodyKey); live {
		t.Errorf("the retired fact's body row %s is still live in the k/v plane — "+
			"recall and search will keep returning a fact that was corrected", bodyKey)
	}
	// Its replacement must NOT have been caught by the same stamp.
	if live := bodyRowIsLive(t, m, ctx, sk.ScopeID, memory.DocumentChunkKeyPrefix+freshID); !live {
		t.Errorf("the REPLACEMENT fact's body row was retired too — a correction that " +
			"removes both the old and the new answer is worse than no correction")
	}

	// PLANE 2 — the sidecar, which is what list_facts and graph_recall read.
	if !chunkIsRetired(t, d, ctx, staleID) {
		t.Errorf("chunk %s is still current in the fact graph — list_facts will report "+
			"a retired fact as the current answer", staleID)
	}
	// And the replacement is recorded, so "why did we stop believing this" is answerable.
	if got := supersedesEdgeFrom(t, d, ctx, staleID); got != freshID {
		t.Errorf("supersedes edge points at %q, want the replacement %q", got, freshID)
	}
}

// TestMemorySupersede_DisplacementNeedsNoReplacement: the placement path retires a
// row that was rewritten under the SAME key in another scope. There is no second
// chunk to point at, and asserting an edge to nothing would claim a replacement
// that does not exist — so the row is closed and carries no edge.
func TestMemorySupersede_DisplacementNeedsNoReplacement(t *testing.T) {
	m, d, ctx := supersedeFixture(t)
	doc := newEntityDoc(t, d, ctx)
	const key = "memory/fact/dave-likes-tea"
	id := writeFact(t, d, ctx, doc, key, "Dave likes tea.")

	leaseUser(t, m, ctx)
	if res, _ := m.Execute(ctx, json.RawMessage(
		`{"op":"supersede","scope":"user","key":"`+key+`"}`)); res.IsError {
		t.Fatalf("supersede without a replacement: %s", res.Text)
	}
	if !chunkIsRetired(t, d, ctx, id) {
		t.Error("a displaced fact was not closed in the graph")
	}
	if got := supersedesEdgeFrom(t, d, ctx, id); got != "" {
		t.Errorf("a displacement asserted a replacement edge from %q — there is no "+
			"replacement chunk, so the edge claims something untrue", got)
	}
}

// TestMemorySupersede_IsIdempotent: the consolidator retries after a partial
// failure, and a retry that reports failure for work already done is
// indistinguishable from one that could not do it.
func TestMemorySupersede_IsIdempotent(t *testing.T) {
	m, d, ctx := supersedeFixture(t)
	doc := newEntityDoc(t, d, ctx)
	const stale, fresh = "memory/fact/a", "memory/fact/b"
	staleID := writeFact(t, d, ctx, doc, stale, "A.")
	writeFact(t, d, ctx, doc, fresh, "B.")

	leaseUser(t, m, ctx)
	body := `{"op":"supersede","scope":"user","key":"` + stale + `","superseded_by":"` + fresh + `"}`
	for i := 0; i < 2; i++ {
		if res, _ := m.Execute(ctx, json.RawMessage(body)); res.IsError {
			t.Fatalf("supersede call %d: %s", i+1, res.Text)
		}
	}
	if !chunkIsRetired(t, d, ctx, staleID) {
		t.Error("re-superseding revived the fact")
	}
}

// TestMemorySupersede_RawRowStillWorks: a key with no chunk is the ordinary case
// for a raw memory row. Resolving it as a fact must not refuse the retirement —
// that would break supersede on every deployment not running the fact tier.
func TestMemorySupersede_RawRowStillWorks(t *testing.T) {
	m, _, ctx := supersedeFixture(t)
	if res, _ := m.Execute(ctx, json.RawMessage(
		`{"op":"set","scope":"user","key":"raw-1","value":"one"}`)); res.IsError {
		t.Fatalf("set: %s", res.Text)
	}
	leaseUser(t, m, ctx)
	if res, _ := m.Execute(ctx, json.RawMessage(
		`{"op":"supersede","scope":"user","key":"raw-1"}`)); res.IsError {
		t.Fatalf("supersede of a raw row: %s", res.Text)
	}
	res, _ := m.Execute(ctx, json.RawMessage(`{"op":"get","scope":"user","key":"raw-1"}`))
	if res.IsError || !strings.Contains(res.Text, `"value":null`) {
		t.Errorf("get(superseded raw row) = %q, want value:null", res.Text)
	}
}

func leaseUser(t *testing.T, m *Memory, ctx context.Context) {
	t.Helper()
	res, _ := m.Execute(ctx, json.RawMessage(`{"op":"cursor_lease","scope":"user","lease_ttl_ms":60000}`))
	if res.IsError || !strings.Contains(res.Text, `"acquired":true`) {
		t.Fatalf("cursor_lease(user) = %q, want acquired:true", res.Text)
	}
}

// bodyRowIsLive reads the k/v plane directly. It asserts on the ROW rather than on
// a tool's rendering of it: a superseded row is filtered out of every read path, so
// "get returns null" and "the row is stamped" are the same observation only while
// that filter works.
func bodyRowIsLive(t *testing.T, m *Memory, ctx context.Context, scopeID, key string) bool {
	t.Helper()
	_, err := m.Store.MemoryGet(ctx, tools.RunIdentity(ctx).TenantID, store.MemoryScopeUser, scopeID, key)
	if err == nil {
		return true
	}
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return false // missing, expired, or SUPERSEDED — the visibility rule this asserts
	}
	t.Fatalf("MemoryGet(%s): %v", key, err)
	return false
}

func chunkIsRetired(t *testing.T, d *Document, ctx context.Context, chunkID string) bool {
	t.Helper()
	sk := sidecarScope(t, d, ctx)
	res, err := d.query(ctx, sk, `SELECT expired_at FROM chunk_memory_meta WHERE chunk_id = ?`, chunkID)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatalf("chunk %s has no sidecar row at all", chunkID)
	}
	return res.Rows[0][0] != nil
}

func supersedesEdgeFrom(t *testing.T, d *Document, ctx context.Context, retiredID string) string {
	t.Helper()
	sk := sidecarScope(t, d, ctx)
	res, err := d.query(ctx, sk,
		`SELECT from_id FROM chunk_edges WHERE to_id = ? AND kind = 'supersedes'`, retiredID)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	if len(res.Rows) == 0 {
		return ""
	}
	return asStr(res.Rows[0][0])
}
