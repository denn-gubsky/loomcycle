package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// mustDocScopeKey resolves the per-scope SQL key the entity tier writes into.
func mustDocScopeKey(t *testing.T, tenant string, scope store.MemoryScope, scopeID string) sqlmem.ScopeKey {
	t.Helper()
	key, ok := docScopeKeyFor(tenant, scope, scopeID)
	if !ok {
		t.Fatalf("no scope key for %s/%s/%s", tenant, scope, scopeID)
	}
	return key
}

// TestListFacts_SourceRunIDIsTheINVERSEOfARecalledFactsPointer.
//
// The forward direction — "where did this fact come from" — is the
// source_run_id that recall reports on each hit. This is the other half:
// given a conversation, what did it teach the store.
//
// Without the inverse an operator holding a transcript can only fetch every
// fact and filter client-side, which is not a relation so much as a field you
// may inspect after paying for everything. The SELECT already returned m.run_id
// before this change; what was missing was the ability to ASK.
//
// Asserted on which facts come back, not on the SQL: a filter that was built
// but never applied to the statement would pass a text check on the query.
func TestListFacts_SourceRunIDIsTheINVERSEOfARecalledFactsPointer(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	// upsert_chunk needs a document to hang the chunk on.
	doc, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"facts"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, _ := doc["document_id"].(string)
	if docID == "" {
		docID, _ = doc["id"].(string)
	}
	if docID == "" {
		t.Fatalf("no document id in %v", doc)
	}

	// Two facts from one conversation, one from another.
	for _, f := range []struct{ key, text, run string }{
		{"memory/fact/berlin", "Dave moved to Berlin in July.", "r-chat-1"},
		{"memory/fact/job", "Dave started a new job in July.", "r-chat-1"},
		{"memory/fact/piano", "Caroline is learning the piano.", "r-chat-2"},
	} {
		body, _ := json.Marshal(map[string]any{
			"op": "upsert_chunk", "scope": "user", "natural_key": f.key, "document_id": docID,
			"title": f.text, "body": f.text, "type": "event", "subject": "Dave",
			"source_quote": f.text,
		})
		if _, res := docExec(t, d, ctx, string(body)); res.IsError {
			t.Fatalf("upsert %s: %s", f.key, res.Text)
		}
		// The run id is not a tool argument — it is stamped from the caller's
		// identity — so set it directly to model two different conversations.
		if _, err := d.SqlMem.Exec(ctx, mustDocScopeKey(t, "tnt", store.MemoryScopeUser, "u1"),
			d.SqlMem.Rebind(`UPDATE chunk_memory_meta SET run_id = ? WHERE natural_key = ?`),
			[]any{f.run, f.key}, 0); err != nil {
			t.Fatalf("stamp run for %s: %v", f.key, err)
		}
	}

	// Unfiltered: everything.
	all, res := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","claims_only":true}`)
	if res.IsError {
		t.Fatalf("list_facts: %s", res.Text)
	}
	if n := len(all["facts"].([]any)); n < 3 {
		t.Fatalf("unfiltered list returned %d facts, want at least 3 — the fixture did not land", n)
	}

	// Filtered to one conversation.
	got, res := docExec(t, d, ctx,
		`{"op":"list_facts","scope":"user","claims_only":true,"source_run_id":"r-chat-1"}`)
	if res.IsError {
		t.Fatalf("list_facts filtered: %s", res.Text)
	}
	blob, _ := json.Marshal(got)
	text := string(blob)
	for _, want := range []string{"memory/fact/berlin", "memory/fact/job"} {
		if !strings.Contains(text, want) {
			t.Errorf("the conversation's own fact %q is missing from its inverse lookup:\n%s", want, text)
		}
	}
	if strings.Contains(text, "memory/fact/piano") {
		t.Errorf("a fact from a DIFFERENT conversation leaked into the filter — the relation is "+
			"not actually keyed on the run:\n%s", text)
	}
}
