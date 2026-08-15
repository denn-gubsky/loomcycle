package builtin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// stubDocPeer is a minimal /v1/_document peer serving one remote document with
// two KEYED chunks (nk1, nk2) plus one unkeyed chunk (three total). Bodies are
// mutable so a test can drive the diverged-chunk path across syncs.
type stubDocPeer struct {
	mu     sync.Mutex
	bodies map[string]string // chunk id -> body
}

func newStubDocPeer() *stubDocPeer {
	return &stubDocPeer{bodies: map[string]string{"c1": "body-1", "c2": "body-2"}}
}

func (p *stubDocPeer) body(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bodies[id]
}

func (p *stubDocPeer) setBody(id, b string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bodies[id] = b
}

func (p *stubDocPeer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Op string `json:"op"`
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		switch in.Op {
		case "get_document":
			_ = json.NewEncoder(w).Encode(map[string]any{"document_id": "remoteDoc", "root_chunk_id": "r"})
		case "list_facts":
			_ = json.NewEncoder(w).Encode(map[string]any{"facts": []map[string]any{
				{"id": "c1", "title": "Alpha", "type": "note", "entity": map[string]any{"natural_key": "nk1"}},
				{"id": "c2", "title": "Beta", "type": "note", "entity": map[string]any{"natural_key": "nk2"}},
			}})
		case "query_chunks":
			// The real peer returns EVERY chunk incl. the document root ("r"): two
			// keyed (c1,c2) + one unkeyed (c3) + the root → excluded_unkeyed = 1
			// (root is structural, keyed chunks are reconciled).
			_ = json.NewEncoder(w).Encode(map[string]any{"chunks": []map[string]any{{"id": "r"}, {"id": "c1"}, {"id": "c2"}, {"id": "c3"}}})
		case "get_chunk":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": in.ID, "body": p.body(in.ID)})
		default:
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "tool": "Document", "error": "unknown op"})
		}
	})
}

func TestDocumentSetRemote_BindsAndValidates(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{"peerA": {Config: config.DocumentSourceConfig{BaseURL: "https://peer"}}}}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)

	// unknown source → refused
	_, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"nope","remote_ref":"/x"}`, docID))
	if !res.IsError {
		t.Errorf("set_remote with an unknown source should refuse")
	}
	// happy path binds
	bindOut, bres := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peerA","remote_ref":"/docs/remote"}`, docID))
	if bres.IsError {
		t.Fatalf("set_remote: %s", bres.Text)
	}
	if bindOut["bound"] != true || bindOut["source"] != "peerA" {
		t.Errorf("bind result = %+v", bindOut)
	}
	// sync on a fresh (unbound) doc → refused with the set_remote hint
	out2, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Unbound"}`)
	_, sres := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, out2["document_id"].(string)))
	if !sres.IsError {
		t.Errorf("sync on an unbound doc should refuse")
	}
}

func TestDocumentSync_PullCreatesUpdatesExcludes(t *testing.T) {
	peer := newStubDocPeer()
	srv := httptest.NewServer(peer.handler())
	t.Cleanup(srv.Close)
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: srv.URL}},
	}}

	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peerA","remote_ref":"/docs/remote"}`, docID))

	// First sync: both keyed chunks created, the unkeyed one excluded.
	s1, r1 := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if r1.IsError {
		t.Fatalf("sync: %s", r1.Text)
	}
	if s1["created"].(float64) != 2 || s1["excluded_unkeyed"].(float64) != 1 {
		t.Errorf("first sync = %+v, want created=2 excluded_unkeyed=1", s1)
	}
	// The two keyed chunks landed locally with their natural_keys.
	facts, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"list_facts","scope":"user","document_id":%q}`, docID))
	if n := len(facts["facts"].([]any)); n != 2 {
		t.Errorf("local facts = %d, want 2", n)
	}

	// Second sync (unchanged bodies): idempotent.
	s2, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if s2["created"].(float64) != 0 || s2["updated"].(float64) != 0 || s2["unchanged"].(float64) != 2 {
		t.Errorf("second sync = %+v, want created=0 updated=0 unchanged=2", s2)
	}

	// Diverge one chunk on the peer → third sync updates exactly that one.
	peer.setBody("c1", "body-1-EDITED")
	s3, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if s3["updated"].(float64) != 1 || s3["unchanged"].(float64) != 1 {
		t.Errorf("third sync = %+v, want updated=1 unchanged=1", s3)
	}

	// The prior body is preserved in history (retire-not-delete): the updated
	// chunk now has >= 2 body revisions.
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	localID, err := d.chunkIDByNaturalKey(ctx, key, "nk1")
	if err != nil || localID == "" {
		t.Fatalf("resolve nk1: %v (%q)", err, localID)
	}
	hist, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"history","scope":"user","id":%q}`, localID))
	revs, ok := hist["revisions"].([]any)
	if !ok || len(revs) < 2 {
		t.Errorf("nk1 history = %+v, want >= 2 revisions (old body preserved)", hist["revisions"])
	}
}
