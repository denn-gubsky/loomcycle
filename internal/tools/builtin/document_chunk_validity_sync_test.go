package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// realPeer serves /v1/_document from a second Document tool at this version, so
// sync is exercised against a peer that knows chunk validity.
func realPeer(t *testing.T) (*Document, context.Context, string) {
	t.Helper()
	pd, pctx, _ := documentFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		res, err := pd.Execute(pctx, raw)
		w.Header().Set("Content-Type", "application/json")
		if err != nil || res.IsError {
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": res.Text})
			return
		}
		_, _ = io.WriteString(w, res.Text)
	}))
	t.Cleanup(srv.Close)
	return pd, pctx, srv.URL
}

// boundPair is a local document bound to the peer's /docs/remote.
func boundPair(t *testing.T) (d *Document, ctx context.Context, localDoc string, pd *Document, pctx context.Context, remoteDoc string) {
	t.Helper()
	pd, pctx, url := realPeer(t)
	remoteDoc = newValidityDoc(t, pd, pctx, "/docs/remote")
	d, ctx, _ = documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: url}},
	}}
	localDoc = newValidityDoc(t, d, ctx, "/docs/local")
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","id":%q,"source":"peerA","remote_ref":"/docs/remote"}`, localDoc))
	return
}

// keyedChunk returns a keyed chunk's get_chunk response on one side.
func keyedChunkJSON(t *testing.T, d *Document, ctx context.Context, nk string) string {
	t.Helper()
	key, _, _ := d.resolveScope(ctx, "user")
	id, err := d.chunkIDByNaturalKey(ctx, key, nk)
	if err != nil || id == "" {
		t.Fatalf("no chunk for %s: %v", nk, err)
	}
	return vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, id))
}

func syncCounts(t *testing.T, d *Document, ctx context.Context, op, docID, direction string) map[string]any {
	t.Helper()
	var out map[string]any
	_ = json.Unmarshal([]byte(vExec(t, d, ctx, fmt.Sprintf(`{"op":%q,"id":%q,"direction":%q}`, op, docID, direction))), &out)
	return out
}

func TestDocumentSync_PullCarriesChunkValidity(t *testing.T) {
	d, ctx, localDoc, pd, pctx, remoteDoc := boundPair(t)
	id := resultField(toolResult(vExec(t, pd, pctx, fmt.Sprintf(
		`{"op":"upsert_chunk","document_id":%q,"natural_key":"nk1","title":"Clause","body":"text"}`, remoteDoc))), "id")
	vExec(t, pd, pctx, fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":1,"valid_at":%d,"invalid_at":%d}`, id, ns2024, ns2026))

	s := syncCounts(t, d, ctx, "sync", localDoc, "pull")
	if s["created"].(float64) != 1 {
		t.Fatalf("pull = %v, want created 1", s)
	}
	got := keyedChunkJSON(t, d, ctx, "nk1")
	if v, _ := chunkInt64(t, got, "valid_at"); v != ns2024 {
		t.Errorf("pulled valid_at = %d, want %d: %s", v, ns2024, got)
	}
	if v, _ := chunkInt64(t, got, "invalid_at"); v != ns2026 {
		t.Errorf("pulled invalid_at = %d, want %d", v, ns2026)
	}
	// Converged: nothing to do on the next pull, and the diff says the same.
	if s := syncCounts(t, d, ctx, "sync", localDoc, "pull"); s["updated"].(float64) != 0 || s["unchanged"].(float64) != 1 {
		t.Errorf("second pull = %v, want unchanged 1", s)
	}
	if diff := syncCounts(t, d, ctx, "diff_remote", localDoc, ""); len(diff["diverged"].([]any)) != 0 {
		t.Errorf("diff after a converged pull = %v", diff)
	}

	// A validity-only change on the peer is a divergence, and a pull carries it.
	vExec(t, pd, pctx, fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":2,"invalid_at":%d}`, id, ns2025))
	if diff := syncCounts(t, d, ctx, "diff_remote", localDoc, ""); len(diff["diverged"].([]any)) != 1 {
		t.Errorf("diff after a validity-only change = %v, want 1 diverged", diff)
	}
	if s := syncCounts(t, d, ctx, "sync", localDoc, "pull"); s["updated"].(float64) != 1 {
		t.Errorf("pull after a validity-only change = %v, want updated 1", s)
	}
	if v, _ := chunkInt64(t, keyedChunkJSON(t, d, ctx, "nk1"), "invalid_at"); v != ns2025 {
		t.Errorf("local invalid_at after pull = %d, want %d", v, ns2025)
	}
	// A clear on the peer clears locally.
	vExec(t, pd, pctx, fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":3,"valid_at":null,"invalid_at":null}`, id))
	syncCounts(t, d, ctx, "sync", localDoc, "pull")
	if _, ok := chunkInt64(t, keyedChunkJSON(t, d, ctx, "nk1"), "valid_at"); ok {
		t.Errorf("a cleared interval on the peer survived the pull")
	}
}

func TestDocumentSyncPush_CarriesChunkValidity(t *testing.T) {
	d, ctx, localDoc, pd, pctx, _ := boundPair(t)
	id := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","document_id":%q,"natural_key":"nk1","title":"Clause","body":"text"}`, localDoc))), "id")
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":1,"valid_at":%d}`, id, ns2024))

	if s := syncCounts(t, d, ctx, "sync", localDoc, "push"); s["created"].(float64) != 1 {
		t.Fatalf("push = %v, want created 1", s)
	}
	got := keyedChunkJSON(t, pd, pctx, "nk1")
	if v, ok := chunkInt64(t, got, "valid_at"); !ok || v != ns2024 {
		t.Errorf("peer valid_at = %d (present %v), want %d: %s", v, ok, ns2024, got)
	}
	// The chunk interval must not have been written as the peer fact's sidecar interval.
	var gc struct {
		Entity map[string]any `json:"entity"`
	}
	_ = json.Unmarshal([]byte(got), &gc)
	if gc.Entity["valid_at"] != nil {
		t.Errorf("push wrote the chunk interval onto the peer's fact sidecar: %s", got)
	}
	if s := syncCounts(t, d, ctx, "sync", localDoc, "push"); s["updated"].(float64) != 0 || s["unchanged"].(float64) != 1 {
		t.Errorf("second push = %v, want unchanged 1", s)
	}
}

// validityStubPeer serves one keyed chunk (nk1, "Clause"/"text") whose get_chunk
// response carries exactly the extra keys given — none, to stand in for a peer
// that predates chunk validity.
func validityStubPeer(extra map[string]any) http.Handler {
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
				{"id": "c1", "title": "Clause", "type": "note", "entity": map[string]any{"natural_key": "nk1"}},
			}})
		case "get_chunk":
			out := map[string]any{"id": in.ID, "body": "text", "revision": 1}
			for k, v := range extra {
				out[k] = v
			}
			_ = json.NewEncoder(w).Encode(out)
		case "query_chunks":
			_ = json.NewEncoder(w).Encode(map[string]any{"chunks": []map[string]any{{"id": "r"}, {"id": "c1"}}})
		case "get_edges":
			_ = json.NewEncoder(w).Encode(map[string]any{"edges": []any{}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
}

// localDatedNK1 binds a local document holding nk1 — the same content as the
// stub peer's — dated [2024, 2026), and returns its document id.
func localDatedNK1(t *testing.T, peer http.Handler) (*Document, context.Context, string) {
	t.Helper()
	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: srv.URL}},
	}}
	doc := newValidityDoc(t, d, ctx, "/docs/local")
	id := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","document_id":%q,"natural_key":"nk1","title":"Clause","type":"note","body":"text"}`, doc))), "id")
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":1,"valid_at":%d,"invalid_at":%d}`, id, ns2024, ns2026))
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","id":%q,"source":"peerA","remote_ref":"/docs/x"}`, doc))
	return d, ctx, doc
}

// A peer that does not know the keys says nothing about them: a pull keeps the
// local dates, and diff_remote does not call the chunk diverged.
func TestDocumentSync_PullFromAPeerWithoutTheKeysKeepsLocalDates(t *testing.T) {
	d, ctx, doc := localDatedNK1(t, validityStubPeer(nil))
	if diff := syncCounts(t, d, ctx, "diff_remote", doc, ""); len(diff["diverged"].([]any)) != 0 || diff["same"].(float64) != 1 {
		t.Errorf("diff against a peer without the keys = %v, want same 1", diff)
	}
	if s := syncCounts(t, d, ctx, "sync", doc, "pull"); s["updated"].(float64) != 0 {
		t.Errorf("pull = %v, want nothing updated", s)
	}
	got := keyedChunkJSON(t, d, ctx, "nk1")
	if v, _ := chunkInt64(t, got, "valid_at"); v != ns2024 {
		t.Errorf("a pull from a peer without the keys cleared valid_at: %s", got)
	}
	if v, _ := chunkInt64(t, got, "invalid_at"); v != ns2026 {
		t.Errorf("a pull from a peer without the keys cleared invalid_at: %s", got)
	}
}

// An explicit null from the peer is a statement: the chunk is undated there.
func TestDocumentSync_PullOfAnExplicitNullClearsLocalDates(t *testing.T) {
	d, ctx, doc := localDatedNK1(t, validityStubPeer(map[string]any{"valid_at": nil, "invalid_at": nil}))
	if diff := syncCounts(t, d, ctx, "diff_remote", doc, ""); len(diff["diverged"].([]any)) != 1 {
		t.Errorf("diff against a peer sending null = %v, want 1 diverged", diff)
	}
	if s := syncCounts(t, d, ctx, "sync", doc, "pull"); s["updated"].(float64) != 1 {
		t.Errorf("pull = %v, want updated 1", s)
	}
	got := keyedChunkJSON(t, d, ctx, "nk1")
	if _, ok := chunkInt64(t, got, "valid_at"); ok {
		t.Errorf("an explicit null from the peer did not clear valid_at: %s", got)
	}
}

// A keyed chunk's get_chunk always names both keys, null when unset, so a peer
// reading it can tell "undated" from "does not know".
func TestDocumentGetChunk_KeyedChunkAlwaysNamesItsValidity(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newValidityDoc(t, d, ctx, "/facts")
	id := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","document_id":%q,"natural_key":"nk1","title":"F","body":"b"}`, doc))), "id")
	got := vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, id))
	if !strings.Contains(got, `"valid_at":null`) || !strings.Contains(got, `"invalid_at":null`) {
		t.Errorf("an undated keyed chunk does not name its validity: %s", got)
	}
	plain := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","document_id":%q,"title":"P"}`, doc))), "id")
	if got := vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, plain)); strings.Contains(got, "valid_at") {
		t.Errorf("an unkeyed undated chunk names validity keys: %s", got)
	}
}
