package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// A pull writes the bound document and nothing else in the scope. A natural key
// is unique across the scope, so a peer chunk whose key a chunk of ANOTHER local
// document holds would, through upsert_chunk, rewrite that chunk and the
// hierarchy pass would then move it into the synced document — reaching a
// document the caller was only asked to sync into. The key is a conflict: the
// other document is left as it was, the conflict is counted and named, and the
// rest of the pull still lands.
func TestSyncPull_NeverWritesOutsideTheBoundDocument(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		localDoc, pd, pctx, remoteDoc := carryPair(t, d, ctx)
		victimDoc := newValidityDoc(t, d, ctx, "/docs/victim")
		victim := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
			`{"op":"upsert_chunk","document_id":%q,"natural_key":"nkV","title":"Alpha","body":"body-1"}`, victimDoc))), "id")

		parent := upsertKeyed(t, pd, pctx, remoteDoc, "nkP", "the peer's parent")
		vExec(t, pd, pctx, fmt.Sprintf(
			`{"op":"upsert_chunk","document_id":%q,"natural_key":"nkV","title":"Overwritten","body":"the peer's text"}`, remoteDoc))
		pkey, _, _ := pd.resolveScope(pctx, "user")
		peerV, _ := pd.chunkIDByNaturalKey(pctx, pkey, "nkV")
		vExec(t, pd, pctx, fmt.Sprintf(`{"op":"move_chunk","id":%q,"new_parent_id":%q,"position":0}`, peerV, parent))

		// The dry run names it before anything is written.
		diff, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"diff_remote","id":%q}`, localDoc))
		if res.IsError {
			t.Fatalf("diff_remote: %s", res.Text)
		}
		if c, _ := diff["conflicts"].([]any); len(c) != 1 || c[0].(map[string]any)["natural_key"] != "nkV" {
			t.Errorf("diff_remote conflicts = %v, want nkV", diff["conflicts"])
		}

		out, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","id":%q,"direction":"pull"}`, localDoc))
		if res.IsError {
			t.Fatalf("a key held by another document failed the whole pull: %s", res.Text)
		}
		if out["conflicts"] != float64(1) {
			t.Errorf("pull report = %v, want conflicts 1", out)
		}
		if keys, _ := out["conflicting_keys"].([]any); len(keys) != 1 || keys[0] != "nkV" {
			t.Errorf("conflicting_keys = %v, want [nkV]", out["conflicting_keys"])
		}
		if out["created"] != float64(1) {
			t.Errorf("pull report = %v, want the rest of the document (nkP) created", out)
		}

		got := map[string]any{}
		_ = json.Unmarshal([]byte(vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, victim))), &got)
		if got["title"] != "Alpha" || got["body"] != "body-1" || got["document_id"] != victimDoc {
			t.Errorf("the other document's chunk changed: title %v, body %v, document %v", got["title"], got["body"], got["document_id"])
		}
	})
}
