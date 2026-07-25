package ollama_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ollama"
)

// embedRequest is the /api/embed request body as this driver sends it.
// `Input` is decoded as []string because the driver always uses the array
// form (Ollama also accepts a bare string; we never send that shape).
type embedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Truncate   *bool    `json:"truncate"`
	Dimensions *int     `json:"dimensions"`
}

// newEmbedder is the common construction: point the driver at `srv` and reuse
// its client so nothing escapes to a real Ollama.
func newEmbedder(t *testing.T, srv *httptest.Server, opts providers.EmbedderOptions) providers.Embedder {
	t.Helper()
	opts.BaseURL = srv.URL
	opts.HTTPClient = srv.Client()
	if opts.Model == "" {
		opts.Model = "nomic-embed-text"
	}
	e, err := ollama.NewEmbedder(opts)
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}
	return e
}

// vectorsJSON renders an /api/embed success body with the given vectors.
func vectorsJSON(model string, vecs [][]float32) string {
	b, _ := json.Marshal(map[string]any{
		"model":             model,
		"embeddings":        vecs,
		"total_duration":    2900000000,
		"load_duration":     2800000000,
		"prompt_eval_count": 8,
	})
	return string(b)
}

// TestOllamaEmbedder_BatchesInOneCall — N texts must cost ONE HTTP call
// carrying the array form of `input`, not N calls. Ollama's /api/embed is
// batch-capable and a per-text call would pay the model-load/queue cost N
// times.
func TestOllamaEmbedder_BatchesInOneCall(t *testing.T) {
	var calls int
	var got embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %q, want /api/embed", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vectorsJSON("nomic-embed-text", [][]float32{{1, 0}, {0, 1}, {1, 1}})))
	}))
	defer srv.Close()

	e := newEmbedder(t, srv, providers.EmbedderOptions{})
	out, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if calls != 1 {
		t.Errorf("HTTP calls = %d, want 1 (the batch must go in a single /api/embed request)", calls)
	}
	if len(out) != 3 {
		t.Fatalf("got %d vectors, want 3", len(out))
	}
	if len(got.Input) != 3 || got.Input[0] != "a" || got.Input[2] != "c" {
		t.Errorf("request input = %#v, want the 3-element array form", got.Input)
	}
	if got.Model != "nomic-embed-text" {
		t.Errorf("request model = %q, want nomic-embed-text", got.Model)
	}
	if got.Truncate == nil || !*got.Truncate {
		t.Errorf("request truncate = %v, want an explicit true", got.Truncate)
	}
}

// TestOllamaEmbedder_ParsesEmbeddingsInOrder — the response has no per-item
// index, so position is the ONLY alignment. Two distinguishable vectors must
// come back paired with the text at the same index.
func TestOllamaEmbedder_ParsesEmbeddingsInOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vectorsJSON("nomic-embed-text", [][]float32{{1, 2, 3}, {9, 8, 7}})))
	}))
	defer srv.Close()

	e := newEmbedder(t, srv, providers.EmbedderOptions{})
	out, err := e.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d vectors, want 2", len(out))
	}
	if out[0][0] != 1 || out[0][2] != 3 {
		t.Errorf("out[0] = %v, want [1 2 3] (first text's vector)", out[0])
	}
	if out[1][0] != 9 || out[1][2] != 7 {
		t.Errorf("out[1] = %v, want [9 8 7] (second text's vector)", out[1])
	}
	if e.Dimension() != 3 {
		t.Errorf("Dimension() = %d, want 3 learned from the first response", e.Dimension())
	}
}

// TestOllamaEmbedder_CountMismatchIsAnError — a response with fewer
// embeddings than inputs must error. Truncating silently would pair every
// subsequent text with the wrong vector, corrupting the store irrecoverably.
func TestOllamaEmbedder_CountMismatchIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vectorsJSON("nomic-embed-text", [][]float32{{1, 0}})))
	}))
	defer srv.Close()

	e := newEmbedder(t, srv, providers.EmbedderOptions{})
	out, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatalf("expected an error for 1 embedding vs 3 inputs, got %d vectors", len(out))
	}
	if !strings.Contains(err.Error(), "1 embeddings for 3 inputs") {
		t.Errorf("error should state the counts, got: %v", err)
	}
}

// TestOllamaEmbedder_MissingModelSurfacesTheModelName — a stock Ollama ships
// NO embedding model, so a 404 is the most likely first-run failure. The
// error must name the model (and the pull remedy) instead of flattening to a
// generic status.
func TestOllamaEmbedder_MissingModelSurfacesTheModelName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"model \"nomic-embed-text\" not found, try pulling it first"}`))
	}))
	defer srv.Close()

	e := newEmbedder(t, srv, providers.EmbedderOptions{})
	_, err := e.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected a 404 error")
	}
	if !strings.Contains(err.Error(), "nomic-embed-text") {
		t.Errorf("error must name the missing model, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ollama pull") {
		t.Errorf("error should point at the remedy, got: %v", err)
	}
}

// TestOllamaEmbedder_KeylessByDefault — local Ollama has no auth. An
// Authorization header appears only when the operator supplied a key (a
// proxied / hosted endpoint).
func TestOllamaEmbedder_KeylessByDefault(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vectorsJSON("nomic-embed-text", [][]float32{{1, 0}})))
	}))
	defer srv.Close()

	keyless := newEmbedder(t, srv, providers.EmbedderOptions{})
	if _, err := keyless.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if auth != "" {
		t.Errorf("Authorization = %q, want no header when no APIKey is configured", auth)
	}

	keyed := newEmbedder(t, srv, providers.EmbedderOptions{APIKey: "unit-test-placeholder"})
	if _, err := keyed.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if auth != "Bearer unit-test-placeholder" {
		t.Errorf("Authorization = %q, want the Bearer header when an APIKey is configured", auth)
	}
}

// TestOllamaEmbedder_OmitsDimensionsWhenUnset — an unset `dimensions` must be
// ABSENT from the body. Sending a literal 0 would ask Ollama for zero-width
// vectors instead of the model's native size.
func TestOllamaEmbedder_OmitsDimensionsWhenUnset(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vectorsJSON("nomic-embed-text", [][]float32{{1, 0}})))
	}))
	defer srv.Close()

	e := newEmbedder(t, srv, providers.EmbedderOptions{})
	if _, err := e.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if v, ok := raw["dimensions"]; ok {
		t.Errorf("request carried dimensions=%v; the key must be absent when unset", v)
	}
}

// TestOllamaEmbedder_SendsDimensionsWhenSet — Matryoshka truncation. An
// operator capping a 4096-dim model at 1024 (so a pgvector HNSW index, capped
// at 2000 dims, stays possible later) must actually reach the wire.
func TestOllamaEmbedder_SendsDimensionsWhenSet(t *testing.T) {
	var got embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vectorsJSON("qwen3-embedding", [][]float32{{1, 0}})))
	}))
	defer srv.Close()

	e := newEmbedder(t, srv, providers.EmbedderOptions{Model: "qwen3-embedding", Dimensions: 1024})
	if _, err := e.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got.Dimensions == nil || *got.Dimensions != 1024 {
		t.Errorf("request dimensions = %v, want 1024", got.Dimensions)
	}
}

// TestOllamaEmbedder_BatchSizeChunksRequests — a batch_size smaller than the
// input count splits into that many calls, still in order.
func TestOllamaEmbedder_BatchSizeChunksRequests(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		calls++
		vecs := make([][]float32, len(req.Input))
		for i, in := range req.Input {
			vecs[i] = []float32{float32(in[0])}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vectorsJSON("nomic-embed-text", vecs)))
	}))
	defer srv.Close()

	e := newEmbedder(t, srv, providers.EmbedderOptions{BatchSize: 2})
	out, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if calls != 2 {
		t.Errorf("HTTP calls = %d, want 2 for 3 inputs at batch_size 2", calls)
	}
	if len(out) != 3 || out[0][0] != float32('a') || out[2][0] != float32('c') {
		t.Errorf("chunked results lost order: %v", out)
	}
}

// TestOllamaEmbedder_RegisteredUnderBothIds — the embedder mirrors the chat
// side's naming: `ollama-local` is the self-hosted runtime, `ollama` is Ollama
// Cloud. Provider() must return the registration id verbatim, because the
// store compares a stored embedding row's provider against the configured
// embedder's Provider() when deciding what needs re-embedding — and because
// the consolidation dispatcher reads "runs on the operator's own hardware"
// off the `-local` suffix.
func TestOllamaEmbedder_RegisteredUnderBothIds(t *testing.T) {
	for _, id := range []string{"ollama-local", "ollama"} {
		e, err := providers.NewEmbedder(id, providers.EmbedderOptions{
			Model:   "nomic-embed-text",
			BaseURL: "http://127.0.0.1:1",
		})
		if err != nil {
			t.Fatalf("NewEmbedder(%s): %v", id, err)
		}
		if e.Provider() != id {
			t.Errorf("Provider() = %q, want %q", e.Provider(), id)
		}
		if e.Dimension() != 0 {
			t.Errorf("%s: Dimension() = %d before any call, want 0 (unknown until observed)", id, e.Dimension())
		}
	}
}
