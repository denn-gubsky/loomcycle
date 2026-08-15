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
		case "get_edges":
			_ = json.NewEncoder(w).Encode(map[string]any{"edges": []any{}})
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
		case "query_chunks":
			chunks := []map[string]any{{"id": p.rootID}}
			for id := range p.byID {
				chunks = append(chunks, map[string]any{"id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"chunks": chunks})
		case "get_edges":
			_ = json.NewEncoder(w).Encode(map[string]any{"edges": []any{}})
		case "move_chunk":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": in.ID})
		case "link_chunks":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
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

// diffStubPeer serves a fixed remote document with three KEYED chunks (nk1 body
// "same-body", nk2 body "remote-2", nkR body "remote-only") plus one unkeyed
// chunk — enough to exercise every diff bucket against a crafted local doc.
func diffStubPeer() http.Handler {
	bodies := map[string]string{"d1": "same-body", "d2": "remote-2", "dR": "remote-only"}
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
				{"id": "d1", "title": "One", "type": "note", "entity": map[string]any{"natural_key": "nk1"}},
				{"id": "d2", "title": "Two", "type": "note", "entity": map[string]any{"natural_key": "nk2"}},
				{"id": "dR", "title": "RemoteOnly", "type": "note", "entity": map[string]any{"natural_key": "nkR"}},
			}})
		case "query_chunks":
			// root + 3 keyed + 1 unkeyed → excluded_unkeyed_remote = 1
			_ = json.NewEncoder(w).Encode(map[string]any{"chunks": []map[string]any{{"id": "r"}, {"id": "d1"}, {"id": "d2"}, {"id": "dR"}, {"id": "dU"}}})
		case "get_chunk":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": in.ID, "body": bodies[in.ID]})
		case "get_edges":
			_ = json.NewEncoder(w).Encode(map[string]any{"edges": []any{}})
		default:
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": "unknown op"})
		}
	})
}

func TestDocumentDiffRemote_ClassifiesEveryBucket(t *testing.T) {
	srv := httptest.NewServer(diffStubPeer())
	t.Cleanup(srv.Close)
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: srv.URL}},
	}}

	// Local: nk1 matches the peer, nk2 diverges, nkL is local-only, plus 1 unkeyed.
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"nk1","title":"One","type":"note","body":"same-body"}`, docID))
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"nk2","title":"Two","type":"note","body":"local-2"}`, docID))
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"nkL","title":"LocalOnly","type":"note","body":"local-only"}`, docID))
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Plain","body":"unkeyed"}`, docID))
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peerA","remote_ref":"/docs/remote"}`, docID))

	diff, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"diff_remote","scope":"user","id":%q}`, docID))
	if r.IsError {
		t.Fatalf("diff_remote: %s", r.Text)
	}
	nks := func(field string) []string {
		var out []string
		for _, e := range diff[field].([]any) {
			out = append(out, e.(map[string]any)["natural_key"].(string))
		}
		return out
	}
	if got := nks("only_local"); len(got) != 1 || got[0] != "nkL" {
		t.Errorf("only_local = %v, want [nkL]", got)
	}
	if got := nks("only_remote"); len(got) != 1 || got[0] != "nkR" {
		t.Errorf("only_remote = %v, want [nkR]", got)
	}
	if got := nks("diverged"); len(got) != 1 || got[0] != "nk2" {
		t.Errorf("diverged = %v, want [nk2]", got)
	}
	if diff["same"].(float64) != 1 {
		t.Errorf("same = %v, want 1 (nk1)", diff["same"])
	}
	if diff["excluded_unkeyed_local"].(float64) != 1 || diff["excluded_unkeyed_remote"].(float64) != 1 {
		t.Errorf("excluded = local %v / remote %v, want 1 / 1", diff["excluded_unkeyed_local"], diff["excluded_unkeyed_remote"])
	}

	// A doc that isn't bound refuses.
	out2, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Unbound"}`)
	_, r2 := docExec(t, d, ctx, fmt.Sprintf(`{"op":"diff_remote","scope":"user","id":%q}`, out2["document_id"].(string)))
	if !r2.IsError {
		t.Errorf("diff_remote on an unbound doc should refuse")
	}
}

// treeStubPeer serves a remote document with a keyed HIERARCHY (nkP is the
// parent of nkC, plus nkS), a tagged parent, and one manual edge nkC->nkS — so
// a pull test can assert hierarchy + tags + edges reconcile, not just content.
func treeStubPeer() http.Handler {
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
				{"id": "p", "title": "Parent", "type": "note", "entity": map[string]any{"natural_key": "nkP"}},
				{"id": "c", "title": "Child", "type": "note", "entity": map[string]any{"natural_key": "nkC"}},
				{"id": "s", "title": "Sibling", "type": "note", "entity": map[string]any{"natural_key": "nkS"}},
			}})
		case "get_chunk":
			switch in.ID {
			case "p":
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "p", "body": "P", "tags": []string{"alpha", "beta"}})
			case "c":
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "c", "body": "C", "parent_id": "p", "position": 0})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"id": in.ID, "body": "S"})
			}
		case "query_chunks":
			_ = json.NewEncoder(w).Encode(map[string]any{"chunks": []map[string]any{{"id": "r"}, {"id": "p"}, {"id": "c"}, {"id": "s"}}})
		case "get_edges":
			_ = json.NewEncoder(w).Encode(map[string]any{"edges": []map[string]any{
				{"from_id": "c", "to_id": "s", "kind": "relates", "auto": false},
			}})
		default:
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": "unknown op"})
		}
	})
}

func TestDocumentSyncPull_ReconcilesHierarchyTagsEdges(t *testing.T) {
	srv := httptest.NewServer(treeStubPeer())
	t.Cleanup(srv.Close)
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: srv.URL}},
	}}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peerA","remote_ref":"/docs/tree"}`, docID))

	s, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if r.IsError {
		t.Fatalf("sync: %s", r.Text)
	}
	if s["created"].(float64) != 3 || s["edges_added"].(float64) != 1 || s["reparented"].(float64) < 1 {
		t.Fatalf("pull report = %+v, want created=3 edges_added=1 reparented>=1", s)
	}

	// Resolve local ids by natural_key.
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	localP, _ := d.chunkIDByNaturalKey(ctx, key, "nkP")
	localC, _ := d.chunkIDByNaturalKey(ctx, key, "nkC")
	localS, _ := d.chunkIDByNaturalKey(ctx, key, "nkS")

	// Hierarchy: nkC's parent is nkP.
	childChunk, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, localC))
	if childChunk["parent_id"] != localP {
		t.Errorf("nkC parent = %v, want nkP (%s)", childChunk["parent_id"], localP)
	}
	// Tags: nkP carries alpha,beta.
	parentChunk, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, localP))
	tags := parentChunk["tags"]
	tj, _ := json.Marshal(tags)
	if string(tj) != `["alpha","beta"]` {
		t.Errorf("nkP tags = %s, want [alpha beta]", tj)
	}
	// Edge: nkC --relates--> nkS present (manual).
	edges, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID))
	found := false
	for _, e := range edges["edges"].([]any) {
		em := e.(map[string]any)
		if em["from_id"] == localC && em["to_id"] == localS && em["kind"] == "relates" {
			found = true
		}
	}
	if !found {
		t.Errorf("edge nkC->nkS not found in %+v", edges["edges"])
	}

	// Idempotent: a second pull moves nothing.
	s2, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if s2["created"].(float64) != 0 || s2["reparented"].(float64) != 0 || s2["edges_added"].(float64) != 0 || s2["unchanged"].(float64) != 3 {
		t.Errorf("second pull = %+v, want created=0 reparented=0 edges_added=0 unchanged=3", s2)
	}
}

// oneFactStubPeer serves a remote document with a single visible keyed chunk
// (nk1) whose title + body are configurable — used by the convergence (#1) and
// title-propagation (#3) regression tests.
func oneFactStubPeer(title, body string) http.Handler {
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
				{"id": "c1", "title": title, "type": "note", "entity": map[string]any{"natural_key": "nk1", "withheld": false}},
			}})
		case "get_chunk":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": in.ID, "body": body})
		case "query_chunks":
			_ = json.NewEncoder(w).Encode(map[string]any{"chunks": []map[string]any{{"id": "r"}, {"id": "c1"}}})
		case "get_edges":
			_ = json.NewEncoder(w).Encode(map[string]any{"edges": []any{}})
		default:
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": "unknown op"})
		}
	})
}

// TestDocumentSync_ConvergesOnWithheldLocalChunk is the #1 regression: a keyed
// chunk that is REFUTED locally (confidence below the withhold floor) but
// visible on the peer must NOT be re-"created" on every pull. Before the fix,
// localKeyedChunks excluded the refuted chunk from the target snapshot, so pass
// 1 took the create branch every run and churned the chunk's revision.
func TestDocumentSync_ConvergesOnWithheldLocalChunk(t *testing.T) {
	srv := httptest.NewServer(oneFactStubPeer("One", "shared-body"))
	t.Cleanup(srv.Close)
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: srv.URL}},
	}}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	// nk1 exists locally but is REFUTED (confidence 0.1 < 0.25 floor) → withheld.
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"nk1","title":"One","type":"note","body":"shared-body","confidence":0.1}`, docID))
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peerA","remote_ref":"/docs/x"}`, docID))

	s1, r1 := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if r1.IsError {
		t.Fatalf("sync: %s", r1.Text)
	}
	// The refuted local chunk EXISTS, so the pull must NOT create it (the churn bug).
	if s1["created"].(float64) != 0 {
		t.Errorf("first pull created = %v, want 0 (the withheld local chunk already exists — must not be re-created)", s1["created"])
	}
	// Second pull is idempotent (the churn bug re-created every run).
	s2, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if s2["created"].(float64) != 0 {
		t.Errorf("second pull created = %v, want 0 (sync must converge on a withheld chunk)", s2["created"])
	}
}

// TestDocumentSync_PropagatesTitleChange is the #3 regression: a source chunk
// whose ONLY difference is title (body + tags identical) must still propagate,
// and diff_remote must report it as diverged. Before the fix, pass 1's change
// detection was body/tags only, so a rename was silently dropped.
func TestDocumentSync_PropagatesTitleChange(t *testing.T) {
	srv := httptest.NewServer(oneFactStubPeer("RemoteTitle", "same-body"))
	t.Cleanup(srv.Close)
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: srv.URL}},
	}}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	// nk1 locally: same body as the peer, but a DIFFERENT title.
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"nk1","title":"LocalTitle","type":"note","body":"same-body"}`, docID))
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peerA","remote_ref":"/docs/x"}`, docID))

	// diff_remote must SEE the title divergence (not report "same").
	diff, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"diff_remote","scope":"user","id":%q}`, docID))
	if n := len(diff["diverged"].([]any)); n != 1 {
		t.Errorf("diff diverged = %d, want 1 (a title-only change must show as diverged)", n)
	}

	s, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
	if r.IsError {
		t.Fatalf("sync: %s", r.Text)
	}
	if s["updated"].(float64) != 1 {
		t.Errorf("pull updated = %v, want 1 (a title-only change must propagate)", s["updated"])
	}
	key, _, _ := d.resolveScope(ctx, "user")
	localID, _ := d.chunkIDByNaturalKey(ctx, key, "nk1")
	got, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, localID))
	if got["title"] != "RemoteTitle" {
		t.Errorf("local title = %v, want RemoteTitle (title change must land)", got["title"])
	}
}
