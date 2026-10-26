package builtin

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestListFacts_AsOfHonoursTheFactChunksOwnInterval: a fact chunk can carry its
// own valid_at/invalid_at beside its fact dates, and list_facts — like search and
// graph_recall — answers "true then?" yes only when both intervals say so.
func TestListFacts_AsOfHonoursTheFactChunksOwnInterval(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"parent_id":%q,`+
		`"title":"Clause 7 applies","natural_key":"fact:clause7","valid_at":1000}`, docID, root))
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	fact := asStr(out["id"])
	rev := resultField(toolResult(vExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+fact+`"}`)), "revision")
	// Its own interval: in force 2024..2026, so ended by now.
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%s,"valid_at":%d,"invalid_at":%d}`,
		fact, rev, ns2024, ns2026))

	listed := func(extra string) bool {
		t.Helper()
		var res struct {
			Facts []struct {
				ID string `json:"id"`
			} `json:"facts"`
		}
		_ = json.Unmarshal([]byte(vExec(t, d, ctx, `{"op":"list_facts","scope":"user","document_id":"`+docID+`"`+extra+`}`)), &res)
		for _, f := range res.Facts {
			if f.ID == fact {
				return true
			}
		}
		return false
	}
	if !listed(fmt.Sprintf(`,"as_of":%d`, ns2025)) {
		t.Error("as_of mid-2025 lies inside both intervals and must list the fact")
	}
	if listed(fmt.Sprintf(`,"as_of":%d`, ns2023)) {
		t.Error("as_of 2023 listed a fact whose own interval starts in 2024")
	}
	// The default stays sidecar-only: document sync enumerates keyed chunks
	// through it, and an ended edition must still sync.
	if !listed("") {
		t.Error("the default left out a fact whose only end is its own chunk date")
	}
	if !listed(`,"include_retired":true`) {
		t.Error("include_retired must list the fact whatever its dates")
	}
}
