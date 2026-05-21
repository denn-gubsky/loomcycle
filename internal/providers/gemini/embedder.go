package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Embedder implements providers.Embedder against Gemini's
// :batchEmbedContents endpoint. Wire shape:
//
//	POST /v1beta/models/{model}:batchEmbedContents
//	{ "requests": [
//	    { "model": "models/text-embedding-004",
//	      "content": { "parts": [{"text": "hello"}] } },
//	    ...
//	  ] }
//	→ { "embeddings": [
//	     {"values": [0.1, ...]},
//	     ...
//	  ] }
//
// Important: unlike OpenAI's flat /v1/embeddings, Gemini's batch
// endpoint takes a `requests` array where EACH request restates the
// model name. The wire model id is `models/<name>` (e.g.
// `models/text-embedding-004`). The URL path is the same shape but
// with `:batchEmbedContents` action suffix.
//
// Known models + dimensions:
//   text-embedding-004      → 768
//   gemini-embedding-001    → 3072 (when released)
//
// We don't ship a `dimensions` param; Gemini's API allows it on some
// models but the matrix is small and the default is the typical
// operator pick.
type Embedder struct {
	apiKey    string
	baseURL   string
	model     string
	batchSize int
	timeout   time.Duration
	http      *http.Client
}

var geminiEmbeddingDims = map[string]int{
	"text-embedding-004":   768,
	"gemini-embedding-001": 3072,
}

func init() {
	providers.RegisterEmbedder("gemini", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return NewEmbedder(opts)
	})
}

// NewEmbedder constructs a configured Gemini embedder.
func NewEmbedder(opts providers.EmbedderOptions) (*Embedder, error) {
	if opts.Model == "" {
		return nil, errors.New("gemini embedder: opts.Model is required")
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &Embedder{
		apiKey:    opts.APIKey,
		baseURL:   baseURL,
		model:     opts.Model,
		batchSize: opts.BatchSize,
		timeout:   opts.Timeout,
		http:      opts.HTTPClientOrDefault(),
	}, nil
}

func (e *Embedder) Model() string    { return e.model }
func (e *Embedder) Provider() string { return "gemini" }
func (e *Embedder) Dimension() int   { return geminiEmbeddingDims[e.model] }

// Embed batches texts into chunks of at most e.batchSize and
// concatenates per-batch responses preserving order.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	batchSize := e.batchSize
	if batchSize <= 0 || batchSize > len(texts) {
		batchSize = len(texts)
	}
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		got, err := e.embedOnce(ctx, texts[i:end])
		if err != nil {
			return nil, err
		}
		if len(got) != end-i {
			return nil, fmt.Errorf("gemini embedder: batch returned %d vectors for %d inputs", len(got), end-i)
		}
		out = append(out, got...)
	}
	return out, nil
}

// embedOnce sends one :batchEmbedContents POST and decodes the
// response. Gemini's HTTP error shape carries a JSON body with
// status / message — we surface the full body in the error.
func (e *Embedder) embedOnce(ctx context.Context, texts []string) ([][]float32, error) {
	// Build the requests array. Each request restates the model id
	// in the `model` field — `models/<name>`. The Gemini API requires
	// this even though the URL also names the model.
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	type singleRequest struct {
		Model   string  `json:"model"`
		Content content `json:"content"`
	}
	wireModel := "models/" + e.model
	reqs := make([]singleRequest, len(texts))
	for i, t := range texts {
		reqs[i] = singleRequest{
			Model:   wireModel,
			Content: content{Parts: []part{{Text: t}}},
		}
	}
	body, err := json.Marshal(map[string]any{"requests": reqs})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	attemptCtx := ctx
	if e.timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}

	url := e.baseURL + "/models/" + e.model + ":batchEmbedContents"
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("x-goog-api-key", e.apiKey)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini :batchEmbedContents: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("gemini :batchEmbedContents: status %d: %s", resp.StatusCode, string(preview))
	}

	var doc struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("gemini :batchEmbedContents decode: %w", err)
	}
	if len(doc.Embeddings) != len(texts) {
		return nil, fmt.Errorf("gemini :batchEmbedContents: got %d embeddings for %d inputs", len(doc.Embeddings), len(texts))
	}
	out := make([][]float32, len(texts))
	for i, em := range doc.Embeddings {
		out[i] = em.Values
	}
	if want, ok := geminiEmbeddingDims[e.model]; ok && len(out) > 0 && len(out[0]) != want {
		return nil, fmt.Errorf("gemini :batchEmbedContents: model %q returned %d-dim vectors, expected %d", e.model, len(out[0]), want)
	}
	return out, nil
}
