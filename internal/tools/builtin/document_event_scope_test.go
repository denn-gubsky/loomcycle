package builtin

import (
	"encoding/json"
	"testing"
)

// The Channel tool reads a channel at the scope the deployment DECLARES, while the
// runtime publishes at the document's own scope. When the two differ, a consumer
// reads another scope's queue and looks every document up in the wrong store, so
// each payload names the scope it was published at: a consumer can refuse a queue
// that is not its own rather than consume it as "missing".

// TestPublishIngested_PayloadNamesItsScope: documents/ingested carries the scope the
// document lives in, and the message is written at that same scope.
func TestPublishIngested_PayloadNamesItsScope(t *testing.T) {
	for _, scope := range []string{"user", "tenant"} {
		t.Run(scope, func(t *testing.T) {
			d, ctx, _ := documentFixture(t)
			w := withRecorder(d)
			out, r := docExec(t, d, grantedTenantCtx(ctx), `{"op":"create_document","scope":"`+scope+`","title":"Notes","path":"/kb/notes"}`)
			if r.IsError {
				t.Fatalf("create_document: %s", r.Text)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			var got []map[string]any
			for _, m := range w.msgs {
				if m.Channel != "documents/ingested" {
					continue
				}
				if string(m.Scope) != scope {
					t.Errorf("published at scope %q, want %q", m.Scope, scope)
				}
				var ev map[string]any
				if err := json.Unmarshal(m.Payload, &ev); err != nil {
					t.Fatal(err)
				}
				got = append(got, ev)
			}
			if len(got) != 1 {
				t.Fatalf("events = %v, want one created", got)
			}
			if ev := got[0]; ev["scope"] != scope || ev["document_id"] != out["document_id"] || ev["reason"] != "created" {
				t.Errorf("event = %v, want scope %q for document %v", ev, scope, out["document_id"])
			}
		})
	}
}

// TestSubjectEvent_PayloadNamesItsScope: memory/subjects carries its scope too, for
// the same reason; nothing reads it yet.
func TestSubjectEvent_PayloadNamesItsScope(t *testing.T) {
	for _, scope := range []string{"user", "tenant"} {
		t.Run(scope, func(t *testing.T) {
			d, ctx, _ := documentFixture(t)
			w := withRecorder(d)
			gctx := grantedTenantCtx(ctx)
			docID, _ := newSubject(t, d, gctx, scope, "Ada", "person", "person:ada")
			if _, r := docExec(t, d, gctx, `{"op":"delete_document","scope":"`+scope+`","id":"`+docID+`"}`); r.IsError {
				t.Fatalf("delete subject: %s", r.Text)
			}
			evs := w.subjectEvents(t)
			if len(evs) != 1 {
				t.Fatalf("events = %v, want one removed", evs)
			}
			if ev := evs[0]; ev["op"] != "removed" || ev["scope"] != scope {
				t.Errorf("event = %v, want removed at scope %q", ev, scope)
			}
		})
	}
}
