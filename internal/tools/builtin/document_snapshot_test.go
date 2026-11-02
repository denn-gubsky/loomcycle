package builtin

import (
	"testing"
	"time"
)

// query_chunks rows carry the chunk's updated_at, so a client holding a snapshot
// can tell which chunks changed since; an edit moves it forward.
func TestQueryChunks_RowsCarryUpdatedAt(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Doc"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID := out["document_id"].(string)
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","title":"Part","body":"one"}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	chunkID := out["id"].(string)

	updatedAt := func() float64 {
		t.Helper()
		out, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","document_id":"`+docID+`"}`)
		if r.IsError {
			t.Fatalf("query_chunks: %s", r.Text)
		}
		var got float64
		for _, c := range out["chunks"].([]any) {
			m := c.(map[string]any)
			v, ok := m["updated_at"].(float64)
			if !ok || v <= 0 {
				t.Fatalf("query_chunks row has no positive updated_at: %v", m)
			}
			if m["id"] == chunkID {
				got = v
			}
		}
		if got == 0 {
			t.Fatalf("query_chunks did not list chunk %s", chunkID)
		}
		return got
	}
	before := updatedAt()
	time.Sleep(2 * time.Millisecond)
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+chunkID+`","body":"two","revision":1}`); r.IsError {
		t.Fatalf("update_chunk: %s", r.Text)
	}
	if after := updatedAt(); after <= before {
		t.Errorf("updated_at after an edit = %v, want later than %v", after, before)
	}
}
