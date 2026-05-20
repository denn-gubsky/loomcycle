package gemini_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/gemini"
)

// makeGeminiResponse builds a synthetic :batchEmbedContents response.
// Vectors are deterministic from input lengths for cheap assertions.
func makeGeminiResponse(texts []string, dim int) []byte {
	type emb struct {
		Values []float32 `json:"values"`
	}
	doc := struct {
		Embeddings []emb `json:"embeddings"`
	}{}
	for _, t := range texts {
		vec := make([]float32, dim)
		for j := 0; j < dim; j++ {
			vec[j] = float32(len(t) + j)
		}
		doc.Embeddings = append(doc.Embeddings, emb{Values: vec})
	}
	b, _ := json.Marshal(doc)
	return b
}

func TestGeminiEmbedder_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":batchEmbedContents") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") == "" {
			t.Errorf("missing x-goog-api-key header")
		}
		var req struct {
			Requests []struct {
				Model   string `json:"model"`
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Each request must restate the model id with `models/` prefix.
		for i, sr := range req.Requests {
			if !strings.HasPrefix(sr.Model, "models/") {
				t.Errorf("request[%d].model = %q, want prefix 'models/'", i, sr.Model)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// Build response from the parts the client sent.
		texts := make([]string, len(req.Requests))
		for i, sr := range req.Requests {
			if len(sr.Content.Parts) > 0 {
				texts[i] = sr.Content.Parts[0].Text
			}
		}
		_, _ = w.Write(makeGeminiResponse(texts, 768))
	}))
	defer server.Close()

	e, err := gemini.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "test-key",
		BaseURL:    server.URL,
		Model:      "text-embedding-004",
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
	if len(vecs[0]) != 768 {
		t.Errorf("dim %d, want 768", len(vecs[0]))
	}
	// hello (5 chars) → vecs[0][0] = 5; world! (6 chars) → vecs[1][0] = 6.
	if vecs[0][0] != 5 || vecs[1][0] != 6 {
		t.Errorf("unexpected vector content: %v / %v", vecs[0][0], vecs[1][0])
	}
}

func TestGeminiEmbedder_BatchesAcrossCalls(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct {
			Requests []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		texts := make([]string, len(req.Requests))
		for i, sr := range req.Requests {
			if len(sr.Content.Parts) > 0 {
				texts[i] = sr.Content.Parts[0].Text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeGeminiResponse(texts, 4))
	}))
	defer server.Close()

	e, _ := gemini.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "test-only-model", // skips dim check
		BatchSize:  3,
		HTTPClient: server.Client(),
	})
	texts := []string{"a", "b", "c", "d", "e", "f", "g"}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 7 {
		t.Fatalf("got %d vectors, want 7", len(vecs))
	}
	// 7 texts, batch_size=3 → 3 calls (3 + 3 + 1).
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("got %d HTTP calls, want 3", got)
	}
}

func TestGeminiEmbedder_DimensionMismatchInResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return 512-dim vectors against a model expecting 768.
		var req struct {
			Requests []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		texts := make([]string, len(req.Requests))
		for i, sr := range req.Requests {
			if len(sr.Content.Parts) > 0 {
				texts[i] = sr.Content.Parts[0].Text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeGeminiResponse(texts, 512))
	}))
	defer server.Close()

	e, _ := gemini.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-004", // expects 768
		HTTPClient: server.Client(),
	})
	_, err := e.Embed(context.Background(), []string{"hi"})
	if err == nil || !strings.Contains(err.Error(), "768") {
		t.Errorf("expected dim-mismatch error mentioning 768, got %v", err)
	}
}

func TestGeminiEmbedder_HTTPErrorSurfacesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"PERMISSION_DENIED"}}`))
	}))
	defer server.Close()
	e, _ := gemini.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-004",
		HTTPClient: server.Client(),
	})
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected status 403 in error, got %v", err)
	}
}

func TestGeminiEmbedder_TimeoutRespected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = w.Write(makeGeminiResponse([]string{"x"}, 768))
	}))
	defer server.Close()
	e, _ := gemini.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-004",
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

func TestGeminiEmbedder_EmptyTextsReturnsEmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server was called for empty texts")
	}))
	defer server.Close()
	e, _ := gemini.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-004",
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

func TestGeminiEmbedder_MissingModelRefuses(t *testing.T) {
	_, err := gemini.NewEmbedder(providers.EmbedderOptions{APIKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "Model") {
		t.Errorf("expected error mentioning Model, got %v", err)
	}
}

func TestGeminiEmbedder_RegistrationViaInit(t *testing.T) {
	e, err := providers.NewEmbedder("gemini", providers.EmbedderOptions{
		Model:   "text-embedding-004",
		APIKey:  "k",
		BaseURL: "http://invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Provider() != "gemini" {
		t.Errorf("provider %q, want gemini", e.Provider())
	}
	if e.Dimension() != 768 {
		t.Errorf("dim %d, want 768", e.Dimension())
	}
}

func TestGeminiEmbedder_DimensionForKnownModels(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"text-embedding-004", 768},
		{"gemini-embedding-001", 3072},
		{"unknown-model-xyz", 0},
	}
	for _, c := range cases {
		e, err := gemini.NewEmbedder(providers.EmbedderOptions{APIKey: "k", Model: c.model})
		if err != nil {
			t.Fatalf("%s: %v", c.model, err)
		}
		if got := e.Dimension(); got != c.want {
			t.Errorf("model %s: dim %d, want %d", c.model, got, c.want)
		}
	}
}

// Sanity check the URL path includes `:batchEmbedContents` AND the
// model name (Gemini's path-based action routing).
func TestGeminiEmbedder_PathIncludesModelAction(t *testing.T) {
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		var req struct {
			Requests []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		texts := []string{}
		for _, sr := range req.Requests {
			if len(sr.Content.Parts) > 0 {
				texts = append(texts, sr.Content.Parts[0].Text)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(makeGeminiResponse(texts, 768))
	}))
	defer server.Close()
	e, _ := gemini.NewEmbedder(providers.EmbedderOptions{
		APIKey:     "k",
		BaseURL:    server.URL,
		Model:      "text-embedding-004",
		HTTPClient: server.Client(),
	})
	_, _ = e.Embed(context.Background(), []string{"x"})
	if !strings.Contains(seenPath, "text-embedding-004:batchEmbedContents") {
		t.Errorf("path %q must include model:batchEmbedContents action", seenPath)
	}
}
