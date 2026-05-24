package anthropic_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
)

// makeVoyageEmbeddingResponse builds a synthetic /v1/embeddings
// response matching Voyage AI's wire shape. Each input text gets a
// deterministic vector encoded from its length so assertions can
// compare vectors without floating-point details.
func makeVoyageEmbeddingResponse(texts []string, dim int) []byte {
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

func TestVoyageEmbedder_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected path %q, want /v1/embeddings", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing Authorization header")
		}
		var req struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "voyage-3" {
			t.Errorf("request model = %q, want voyage-3", req.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeVoyageEmbeddingResponse(req.Input, 1024))
	}))
	defer server.Close()

	e, err := anthropic.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "test-voyage-key",
		BaseURL:    server.URL,
		Model:      "voyage-3",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Provider() != "anthropic" {
		t.Errorf("provider = %q, want anthropic (Voyage lives under the anthropic slot)", e.Provider())
	}
	vecs, err := e.Embed(context.Background(), []string{"hello", "world!"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	if len(vecs[0]) != 1024 || len(vecs[1]) != 1024 {
		t.Errorf("got dims %d / %d, want 1024", len(vecs[0]), len(vecs[1]))
	}
	if vecs[0][0] != 5 || vecs[1][0] != 6 {
		t.Errorf("unexpected vector content: vecs[0][0]=%v vecs[1][0]=%v", vecs[0][0], vecs[1][0])
	}
}

func TestVoyageEmbedder_BatchesAcrossCalls(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeVoyageEmbeddingResponse(req.Input, 4))
	}))
	defer server.Close()

	e, err := anthropic.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "test-only-model", // not in voyageEmbeddingDims; skips dim check
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

func TestVoyageEmbedder_PreservesIndexOrderingOnReorderedResponse(t *testing.T) {
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

	e, err := anthropic.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "test-only-model",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	vecs, _ := e.Embed(context.Background(), []string{"x", "yyy", "zzzzz"})
	if vecs[0][0] != 1 || vecs[1][0] != 3 || vecs[2][0] != 5 {
		t.Errorf("reorder failed: %v %v %v", vecs[0][0], vecs[1][0], vecs[2][0])
	}
}

func TestVoyageEmbedder_DimensionMismatchInResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Return 512-dim instead of the expected 1024.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeVoyageEmbeddingResponse(req.Input, 512))
	}))
	defer server.Close()

	e, _ := anthropic.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "voyage-3", // expects 1024
		HTTPClient: server.Client(),
	})
	_, err := e.Embed(context.Background(), []string{"hi"})
	if err == nil || !strings.Contains(err.Error(), "1024") {
		t.Errorf("expected dim-mismatch error mentioning 1024, got %v", err)
	}
}

func TestVoyageEmbedder_HTTPErrorSurfacesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"detail":"internal"}`))
	}))
	defer server.Close()

	e, _ := anthropic.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "voyage-3",
		HTTPClient: server.Client(),
	})
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error with status 500, got %v", err)
	}
}

func TestVoyageEmbedder_MissingModelRefuses(t *testing.T) {
	_, err := anthropic.NewEmbedder(providers.EmbedderOptions{
		APIKey: "k",
	})
	if err == nil || !strings.Contains(err.Error(), "Model") {
		t.Errorf("expected error mentioning Model, got %v", err)
	}
}

// TestVoyageEmbedder_RegistrationViaInit pins the load-bearing claim:
// `provider: anthropic` in operator yaml routes to this Voyage driver
// after the v0.10.2 swap. Mirrors the analogous openai test.
func TestVoyageEmbedder_RegistrationViaInit(t *testing.T) {
	e, err := providers.NewEmbedder("anthropic", providers.EmbedderOptions{
		Model:   "voyage-3",
		APIKey:  "test",
		BaseURL: "http://invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Provider() != "anthropic" {
		t.Errorf("provider %q, want anthropic", e.Provider())
	}
	if e.Dimension() != 1024 {
		t.Errorf("dim %d, want 1024 (voyage-3 default)", e.Dimension())
	}
}
