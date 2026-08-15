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

// statefulPeer is a WRITABLE /v1/_document peer: it accepts upsert_chunk /
// update_chunk and reflects them in get_chunk / list_facts, so a push test can
// assert what actually landed on the peer. The peer document starts with only a
// root chunk (no keyed content) — push must create it.
type statefulPeer struct {
	mu     sync.Mutex
	docID  string
	rootID string
	byNK   map[string]*peerChunk // natural_key -> chunk
	byID   map[string]*peerChunk // id -> chunk
	seq    int
}

type peerChunk struct {
	id, nk, title, ctype, status, body string
	revision                           int
}

func newStatefulPeer() *statefulPeer {
	return &statefulPeer{docID: "remoteDoc", rootID: "r", byNK: map[string]*peerChunk{}, byID: map[string]*peerChunk{}}
}

// facts returns the peer's current keyed chunks (sorted by natural_key so a
// test can assert deterministically).
func (p *statefulPeer) keyedBodies() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]string{}
	for nk, c := range p.byNK {
		out[nk] = c.body
	}
	return out
}

func (p *statefulPeer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Op         string `json:"op"`
			ID         string `json:"id"`
			DocumentID string `json:"document_id"`
			NaturalKey string `json:"natural_key"`
			Title      string `json:"title"`
			Type       string `json:"type"`
			Status     string `json:"status"`
			Body       string `json:"body"`
			Revision   int    `json:"revision"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		p.mu.Lock()
		defer p.mu.Unlock()
		switch in.Op {
		case "get_document":
			_ = json.NewEncoder(w).Encode(map[string]any{"document_id": p.docID, "root_chunk_id": p.rootID})
		case "list_facts":
			facts := []map[string]any{}
			for _, c := range p.byNK {
				facts = append(facts, map[string]any{"id": c.id, "title": c.title, "type": c.ctype, "status": c.status, "entity": map[string]any{"natural_key": c.nk}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"facts": facts})
		case "get_chunk":
			if c, ok := p.byID[in.ID]; ok {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": c.id, "body": c.body, "revision": c.revision})
				return
			}
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": "no such chunk"})
		case "upsert_chunk":
			if c, ok := p.byNK[in.NaturalKey]; ok { // upsert-in-place
				c.body, c.title, c.ctype, c.status = in.Body, in.Title, in.Type, in.Status
				c.revision++
			} else {
				p.seq++
				c := &peerChunk{id: fmt.Sprintf("peer-%d", p.seq), nk: in.NaturalKey, title: in.Title, ctype: in.Type, status: in.Status, body: in.Body, revision: 1}
				p.byNK[in.NaturalKey], p.byID[c.id] = c, c
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"natural_key": in.NaturalKey, "created": true})
		case "update_chunk":
			c, ok := p.byID[in.ID]
			if !ok {
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": "no such chunk"})
				return
			}
			if in.Revision != c.revision { // optimistic-concurrency, like the real peer
				w.WriteHeader(422)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": "stale revision"})
				return
			}
			c.body, c.title, c.ctype, c.status = in.Body, in.Title, in.Type, in.Status
			c.revision++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": c.id, "revision": c.revision})
		default:
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": "unknown op"})
		}
	})
}

func TestDocumentSyncPush_CreatesUpdatesExcludes(t *testing.T) {
	peer := newStatefulPeer()
	srv := httptest.NewServer(peer.handler())
	t.Cleanup(srv.Close)
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: srv.URL}},
	}}

	// A local document with 2 keyed chunks + 1 unkeyed.
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"nk1","title":"Alpha","type":"note","body":"local-1"}`, docID))
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"nk2","title":"Beta","type":"note","body":"local-2"}`, docID))
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Plain","body":"unkeyed"}`, docID))
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peerA","remote_ref":"/docs/remote"}`, docID))

	// First push: both keyed chunks created on the peer, the unkeyed one excluded.
	p1, r1 := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q,"direction":"push"}`, docID))
	if r1.IsError {
		t.Fatalf("push: %s", r1.Text)
	}
	if p1["direction"] != "push" || p1["created"].(float64) != 2 || p1["excluded_unkeyed"].(float64) != 1 {
		t.Errorf("first push = %+v, want direction=push created=2 excluded_unkeyed=1", p1)
	}
	if bodies := peer.keyedBodies(); bodies["nk1"] != "local-1" || bodies["nk2"] != "local-2" {
		t.Errorf("peer bodies after push = %+v, want nk1=local-1 nk2=local-2", bodies)
	}

	// Second push (unchanged): idempotent.
	p2, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q,"direction":"push"}`, docID))
	if p2["created"].(float64) != 0 || p2["updated"].(float64) != 0 || p2["unchanged"].(float64) != 2 {
		t.Errorf("second push = %+v, want created=0 updated=0 unchanged=2", p2)
	}

	// Diverge nk1 LOCALLY, push → updates exactly that one on the peer.
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	localNK1, err := d.chunkIDByNaturalKey(ctx, key, "nk1")
	if err != nil || localNK1 == "" {
		t.Fatalf("resolve local nk1: %v (%q)", err, localNK1)
	}
	rev, err := d.chunkRevision(ctx, key, localNK1)
	if err != nil {
		t.Fatalf("chunkRevision: %v", err)
	}
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%d,"body":"local-1-EDITED"}`, localNK1, rev))
	p3, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q,"direction":"push"}`, docID))
	if p3["updated"].(float64) != 1 || p3["unchanged"].(float64) != 1 {
		t.Errorf("third push = %+v, want updated=1 unchanged=1", p3)
	}
	if peer.keyedBodies()["nk1"] != "local-1-EDITED" {
		t.Errorf("peer nk1 body = %q, want local-1-EDITED", peer.keyedBodies()["nk1"])
	}
}

func TestDocumentSync_RejectsBadDirection(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{"peerA": {Config: config.DocumentSourceConfig{BaseURL: "https://peer"}}}}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	_, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q,"direction":"sideways"}`, docID))
	if !res.IsError {
		t.Errorf("sync with a bad direction should refuse")
	}
}
