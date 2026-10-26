package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A document's prose reaches a Memory search or recall because chunk bodies share
// the memory keyspace. Document op=search leaves out what the caller's
// access-control set denies it reading; a Memory search over the same rows must
// leave out the same documents, or it is a second door to them.

// memACLCorpus is aclCorpus with a Memory tool over the same store.
func memACLCorpus(t *testing.T) (m *Memory, open, limited context.Context, s aclDocs) {
	t.Helper()
	d, spy, open, limited, s := aclCorpus(t)
	return &Memory{Store: spy, Embedder: d.Embedder, SqlMem: d.SqlMem, DocumentFilters: d}, open, limited, s
}

// memHitKeys runs a search or recall that must succeed and returns its hit keys.
func memHitKeys(t *testing.T, m *Memory, ctx context.Context, body string) map[string]bool {
	t.Helper()
	r := memRun(t, m, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(r.Text), &out); err != nil {
		t.Fatalf("decode %s: %v", r.Text, err)
	}
	got := map[string]bool{}
	for _, list := range []string{"entries", "memories"} {
		rows, _ := out[list].([]any)
		for _, row := range rows {
			e := row.(map[string]any)
			k, _ := e["key"].(string)
			if k == "" {
				k, _ = e["id"].(string)
			}
			got[k] = true
		}
	}
	return got
}

var memACLCalls = []string{
	`{"op":"search","scope":"tenant","query":"savepoint nesting","top_k":50}`,
	`{"op":"search","scope":"tenant","query":"savepoint nesting","top_k":50,"sources":["documents"]}`,
	`{"op":"recall","scope":"tenant","query":"savepoint nesting","top_k":50,"sources":["documents"]}`,
}

func TestMemorySearch_ACLDeniedDocumentIsAbsent(t *testing.T) {
	m, _, limited, s := memACLCorpus(t)
	for _, body := range memACLCalls {
		got := memHitKeys(t, m, limited, body)
		if got[chunkBodyKey(s.secretChunk)] {
			t.Errorf("%s: a read-denied document's chunk came back: %v", body, got)
		}
		if !got[chunkBodyKey(s.publicChunk)] {
			t.Errorf("%s: a readable document's chunk is missing: %v", body, got)
		}
	}
}

func TestMemorySearch_UnrestrictedCallerSeesEveryDocument(t *testing.T) {
	m, open, _, s := memACLCorpus(t)
	// A set that names no document rule limits nothing either.
	unlimited := tools.UpdateRunPolicy(open, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:*"}})
	})
	for _, ctx := range []context.Context{open, unlimited} {
		for _, body := range memACLCalls {
			got := memHitKeys(t, m, ctx, body)
			for _, k := range []string{chunkBodyKey(s.secretChunk), chunkBodyKey(s.publicChunk)} {
				if !got[k] {
					t.Errorf("%s: %s is missing for a caller nothing denies: %v", body, k, got)
				}
			}
		}
	}
}

// With no Document tool to ask, a caller whose set limits reading documents gets
// none: which document a chunk belongs to cannot be established, so none is
// shown to be readable.
func TestMemorySearch_ACLLimitedWithoutDocumentsConfiguredReturnsNoDocument(t *testing.T) {
	m, open, limited, s := memACLCorpus(t)
	m.DocumentFilters = nil
	for _, body := range memACLCalls {
		if got := memHitKeys(t, m, limited, body); got[chunkBodyKey(s.secretChunk)] || got[chunkBodyKey(s.publicChunk)] {
			t.Errorf("%s: a document came back with no way to check it: %v", body, got)
		}
		if got := memHitKeys(t, m, open, body); !got[chunkBodyKey(s.secretChunk)] {
			t.Errorf("%s: an unrestricted caller lost a document: %v", body, got)
		}
	}
}
