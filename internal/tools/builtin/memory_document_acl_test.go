package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
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

// A consolidated fact can be homed in a chunk of a document: its body row is a
// doc.chunk: row WITH provenance, so it classifies as a fact, not as prose.
// Document op=search hides it like any other chunk of a denied document, so a
// Memory search must too — which hits are checked is decided by the key, never by
// the row's class.
func TestMemorySearch_ACLDeniedDocumentHidesItsFactChunks(t *testing.T) {
	m, open, limited, s := memACLCorpus(t)
	// The real stores read each hit's origin with it; the shared fake does not,
	// and without it every chunk row would classify as prose and this test could
	// not tell a class-based check from a key-based one.
	m.Store = &originStore{Store: m.Store}
	v, _ := json.Marshal("savepoint nesting secret fact")
	if err := m.Store.MemorySetProvenance(context.Background(), "tnt", store.MemoryScopeTenant, "", chunkBodyKey(s.secretChunk), v, 0,
		store.MemoryProvenance{Origin: "consolidator", Class: "decision"}); err != nil {
		t.Fatal(err)
	}
	calls := []string{
		`{"op":"search","scope":"tenant","query":"savepoint nesting","top_k":50}`,
		`{"op":"search","scope":"tenant","query":"savepoint nesting","top_k":50,"sources":["facts"]}`,
		`{"op":"recall","scope":"tenant","query":"savepoint nesting","top_k":50}`,
	}
	for _, body := range calls {
		if got := memHitKeys(t, m, limited, body); got[chunkBodyKey(s.secretChunk)] {
			t.Errorf("%s: a fact homed in a read-denied document came back: %v", body, got)
		}
		if got := memHitKeys(t, m, open, body); !got[chunkBodyKey(s.secretChunk)] {
			t.Errorf("%s: an unrestricted caller lost the fact: %v", body, got)
		}
	}
}

// originStore fills each vector hit's Origin from its provenance row, as the real
// stores' search legs do.
type originStore struct{ store.Store }

func (o *originStore) MemoryEmbedSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string,
	f store.MemorySearchFilter, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	rows, err := o.Store.MemoryEmbedSearch(ctx, tenantID, scope, scopeID, f, query, topK)
	for i := range rows {
		if prov, perr := o.MemoryProvenanceGet(ctx, tenantID, scope, scopeID, rows[i].Key); perr == nil {
			rows[i].Origin = prov.Origin
		}
	}
	return rows, err
}
