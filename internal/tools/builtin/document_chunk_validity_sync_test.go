package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
