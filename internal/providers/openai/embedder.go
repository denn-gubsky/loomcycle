package openai

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
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
)

// Embedder implements providers.Embedder against OpenAI's
// /v1/embeddings endpoint. The wire shape:
//
//	POST /v1/embeddings
//	{ "input": ["text 1", "text 2"], "model": "text-embedding-3-large" }
//	→ { "data": [{"index": 0, "embedding": [0.1, ...]},
//	             {"index": 1, "embedding": [0.2, ...]}],
//	    "model": "text-embedding-3-large", "usage": {...} }
//
// Single endpoint serves both single-text and batch — the `input`
// field accepts a string OR an array. We always send the array form
// so the request shape doesn't fork.
//
// Models supported via Dimension(): text-embedding-3-small (1536),
// text-embedding-3-large (3072), text-embedding-ada-002 (1536).
// Unknown models construct successfully (we don't gatekeep) but
// Dimension() returns the configured value from openaiEmbeddingDims
// or zero — the Memory tool's pre-flight check catches mismatches.
type Embedder struct {
	apiKey    string
	baseURL   string
	model     string
	batchSize int
	timeout   time.Duration
	http      *http.Client
}

// openaiEmbeddingDims maps known OpenAI embedding models to their
// output dimensions. text-embedding-3-* models support a
// `dimensions` request parameter to truncate; we don't pass it so
// the default (full size) applies — callers wanting smaller vectors
// will need a v0.9.x knob (and a re-embed migration).
var openaiEmbeddingDims = map[string]int{
	"text-embedding-3-small": 1536,
	"text-embedding-3-large": 3072,
	"text-embedding-ada-002": 1536,
}

func init() {
	providers.RegisterEmbedder("openai", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return NewEmbedder(opts)
	})
}

// NewEmbedder constructs a configured Embedder. Required: opts.Model.
// Empty Model returns an error so a misconfigured operator yaml
// surfaces loudly at boot instead of at first agent run.
func NewEmbedder(opts providers.EmbedderOptions) (*Embedder, error) {
	if opts.Model == "" {
		return nil, errors.New("openai embedder: opts.Model is required")
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
func (e *Embedder) Provider() string { return "openai" }
func (e *Embedder) Dimension() int   { return openaiEmbeddingDims[e.model] }

// Embed batches `texts` into chunks of at most e.batchSize and
// concatenates the per-batch responses. OpenAI's hard cap per
// request is currently 2048 inputs; e.batchSize defaults via
// providers.EmbedderOptions caller.
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
		batch := texts[i:end]
		got, err := e.embedOnce(ctx, batch)
		if err != nil {
			return nil, err
		}
		if len(got) != len(batch) {
			return nil, fmt.Errorf("openai embedder: batch returned %d vectors for %d inputs", len(got), len(batch))
		}
		out = append(out, got...)
	}
	return out, nil
}

// embedOnce sends one /v1/embeddings POST and decodes the response.
// Wraps the call in ratelimit.Do so 429s honour OpenAI's
// Retry-After (or x-ratelimit-reset-*) header just like Call() does
// for chat completions.
func (e *Embedder) embedOnce(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{
		"input": texts,
		"model": e.model,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Per-call timeout. ratelimit.Do's attempt func gets a derived ctx;
	// we wrap once here so all retries share the same per-call window
	// (the outer ctx caps cumulative wait via ratelimit.MaxTotalWait).
	attemptCtx := ctx
	if e.timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}

	resp, err := ratelimit.Do(attemptCtx, ratelimit.Config{
		Provider:    "openai",
		ParseHeader: ratelimit.OpenAIRetryAfter,
	}, func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
		req.Header.Set("Content-Type", "application/json")
		return e.http.Do(req)
	})
	if err != nil {
		return nil, fmt.Errorf("openai /v1/embeddings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("openai /v1/embeddings: status %d: %s", resp.StatusCode, string(preview))
	}

	var doc struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("openai /v1/embeddings decode: %w", err)
	}
	if len(doc.Data) != len(texts) {
		return nil, fmt.Errorf("openai /v1/embeddings: got %d embeddings for %d inputs", len(doc.Data), len(texts))
	}
	// Reorder by `index` field — the API returns in-order today, but
	// the wire contract permits reordering, so we honour it.
	out := make([][]float32, len(texts))
	for _, d := range doc.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("openai /v1/embeddings: response index %d out of range [0,%d)", d.Index, len(out))
		}
		out[d.Index] = d.Embedding
	}
	// Dimension sanity check on the first vector. The Memory tool
	// also validates at search-time against stored rows; doing it here
	// gives a useful error message when the embedder is misconfigured.
	if want, ok := openaiEmbeddingDims[e.model]; ok && len(out) > 0 && len(out[0]) != want {
		return nil, fmt.Errorf("openai /v1/embeddings: model %q returned %d-dim vectors, expected %d", e.model, len(out[0]), want)
	}
	return out, nil
}
