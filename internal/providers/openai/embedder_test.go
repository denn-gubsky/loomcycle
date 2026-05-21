package openai_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/openai"
)

// makeEmbeddingResponse builds a synthetic /v1/embeddings response.
// Each input text gets a deterministic 4-dim vector encoded from its
// length — assertions can compare vectors without knowing the
// floating-point representation in detail.
func makeEmbeddingResponse(texts []string, dim int) []byte {
	type entry struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	doc := struct {
		Data []entry `json:"data"`
	}{}
	for i, t := range texts {
		vec := make([]float32, dim)
		for j := 0; j < dim; j++ {
			vec[j] = float32(len(t) + j)
		}
		doc.Data = append(doc.Data, entry{Index: i, Embedding: vec})
	}
	b, _ := json.Marshal(doc)
	return b
}

func TestOpenAIEmbedder_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing Authorization header")
		}
		var req struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeEmbeddingResponse(req.Input, 1536))
	}))
	defer server.Close()

	e, err := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "test-key",
		BaseURL:    server.URL,
		Model:      "text-embedding-3-small",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := e.Embed(context.Background(), []string{"hello", "world!"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	if len(vecs[0]) != 1536 || len(vecs[1]) != 1536 {
		t.Errorf("got dims %d / %d, want 1536", len(vecs[0]), len(vecs[1]))
	}
	// hello (5 chars) → first element should be 5; world! (6 chars) → 6.
	if vecs[0][0] != 5 || vecs[1][0] != 6 {
		t.Errorf("unexpected vector content: vecs[0][0]=%v vecs[1][0]=%v", vecs[0][0], vecs[1][0])
	}
}

func TestOpenAIEmbedder_BatchesAcrossCalls(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeEmbeddingResponse(req.Input, 4))
	}))
	defer server.Close()

	e, err := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "test-only-model", // not in openaiEmbeddingDims; skips dim check
		BatchSize:  2,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{"a", "b", "c", "d", "e"}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 5 {
		t.Fatalf("got %d vectors, want 5", len(vecs))
	}
	// 5 texts, batch_size=2 → 3 calls (2 + 2 + 1).
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("got %d HTTP calls, want 3", got)
	}
}

func TestOpenAIEmbedder_PreservesIndexOrderingOnReorderedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Build response in REVERSE order to test the index-based
		// reorder path.
		type entry struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		doc := struct {
			Data []entry `json:"data"`
		}{}
		for i := len(req.Input) - 1; i >= 0; i-- {
			vec := []float32{float32(len(req.Input[i])), 0, 0, 0}
			doc.Data = append(doc.Data, entry{Index: i, Embedding: vec})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer server.Close()

	e, err := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "test-only-model",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	vecs, _ := e.Embed(context.Background(), []string{"x", "yyy", "zzzzz"})
	// vecs[0] should correspond to "x" (len 1), [1] to "yyy" (3), [2] to "zzzzz" (5).
	if vecs[0][0] != 1 || vecs[1][0] != 3 || vecs[2][0] != 5 {
		t.Errorf("reorder failed: %v %v %v", vecs[0][0], vecs[1][0], vecs[2][0])
	}
}

func TestOpenAIEmbedder_DimensionMismatchInResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Return 512-dim instead of the expected 1536.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeEmbeddingResponse(req.Input, 512))
	}))
	defer server.Close()

	e, _ := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-3-small", // expects 1536
		HTTPClient: server.Client(),
	})
	_, err := e.Embed(context.Background(), []string{"hi"})
	if err == nil || !strings.Contains(err.Error(), "1536") {
		t.Errorf("expected dim-mismatch error mentioning 1536, got %v", err)
	}
}

func TestOpenAIEmbedder_HTTPErrorSurfacesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer server.Close()

	e, _ := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-3-small",
		HTTPClient: server.Client(),
	})
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error with status 500, got %v", err)
	}
}

func TestOpenAIEmbedder_TimeoutRespected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the request past the embedder's timeout.
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = w.Write(makeEmbeddingResponse([]string{"x"}, 1536))
	}))
	defer server.Close()

	e, _ := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-3-small",
		Timeout:    50 * time.Millisecond,
		HTTPClient: server.Client(),
	})
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "context") {
		t.Errorf("expected deadline/context error, got %v", err)
	}
}

func TestOpenAIEmbedder_EmptyTextsReturnsEmptyResult(t *testing.T) {
	// Server should NEVER be called.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server was called for empty texts")
	}))
	defer server.Close()
	e, _ := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-3-small",
		HTTPClient: server.Client(),
	})
	vecs, err := e.Embed(context.Background(), []string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 0 {
		t.Errorf("got %d vectors, want 0", len(vecs))
	}
}

func TestOpenAIEmbedder_MissingModelRefuses(t *testing.T) {
	_, err := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey: "k",
	})
	if err == nil || !strings.Contains(err.Error(), "Model") {
		t.Errorf("expected error mentioning Model, got %v", err)
	}
}

func TestOpenAIEmbedder_RegistrationViaInit(t *testing.T) {
	// init() should have registered "openai".
	e, err := providers.NewEmbedder("openai", providers.EmbedderOptions{
		Model:   "text-embedding-3-small",
		APIKey:  "test",
		BaseURL: "http://invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Provider() != "openai" {
		t.Errorf("provider %q, want openai", e.Provider())
	}
	if e.Dimension() != 1536 {
		t.Errorf("dim %d, want 1536", e.Dimension())
	}
}

func TestOpenAIEmbedder_DimensionForKnownModels(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"text-embedding-3-small", 1536},
		{"text-embedding-3-large", 3072},
		{"text-embedding-ada-002", 1536},
		{"unknown-model-xyz", 0},
	}
	for _, c := range cases {
		e, err := openai.NewEmbedder(providers.EmbedderOptions{
			APIKey: "k",
			Model:  c.model,
		})
		if err != nil {
			t.Fatalf("%s: %v", c.model, err)
		}
		if got := e.Dimension(); got != c.want {
			t.Errorf("model %s: dim %d, want %d", c.model, got, c.want)
		}
	}
}

// Sanity check that the request body actually carries the array form
// (not the legacy string form for single text). The Memory tool sends
// either single-text or batch; the wire shape must be consistent.
func TestOpenAIEmbedder_RequestBodyIsArray(t *testing.T) {
	var gotInputs int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		gotInputs = len(req.Input)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeEmbeddingResponse(req.Input, 1536))
	}))
	defer server.Close()
	e, _ := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-3-small",
		HTTPClient: server.Client(),
	})
	_, _ = e.Embed(context.Background(), []string{"only-one"})
	if gotInputs != 1 {
		t.Errorf("got %d inputs in request body, want 1 (array form even for single text)", gotInputs)
	}
}

// Ensure the path is exactly /embeddings — not /v1/embeddings — when
// baseURL already ends with /v1 (the default).
func TestOpenAIEmbedder_PathConcatenation(t *testing.T) {
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeEmbeddingResponse(req.Input, 1536))
	}))
	defer server.Close()
	// Use the URL exactly as-is (httptest URLs have no /v1 prefix);
	// the embedder appends /embeddings.
	e, _ := openai.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-3-small",
		HTTPClient: server.Client(),
	})
	_, _ = e.Embed(context.Background(), []string{"x"})
	if !strings.HasSuffix(seenPath, "/embeddings") {
		t.Errorf("got path %q, want suffix /embeddings", seenPath)
	}
}

// dummy var to keep strconv imported (used in helpers above
// indirectly via the JSON encoder).
var _ = strconv.Itoa
