package anthropic

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

// Embedder implements providers.Embedder against Voyage AI's
// /v1/embeddings endpoint. Lives in the `anthropic` package + is
// registered under provider name "anthropic" because Anthropic has
// no native embedding API and explicitly recommends Voyage AI
// (https://docs.anthropic.com/en/docs/build-with-claude/embeddings).
// Operators get the ergonomic `provider: anthropic` yaml without
// learning about Voyage as a separate concept — the routing is
// internal.
//
// The wire shape:
//
//	POST https://api.voyageai.com/v1/embeddings
//	Authorization: Bearer <VOYAGE_API_KEY>
//	{ "input": ["text 1", "text 2"], "model": "voyage-3" }
//	→ { "data": [{"index": 0, "embedding": [0.1, ...]},
//	             {"index": 1, "embedding": [0.2, ...]}],
//	    "model": "voyage-3", "usage": {...} }
//
// Voyage's response shape mirrors OpenAI's almost exactly (data[]
// with index + embedding fields), so the decoding logic is the same.
//
// Models supported via Dimension(): voyage-3, voyage-3-large,
// voyage-code-3, voyage-finance-2, voyage-multilingual-2 — all default
// to 1024 dimensions. voyage-3-large supports 256/512/1024/2048 via
// an output_dimension request param; we don't pass it so the model
// default applies.
type Embedder struct {
	apiKey    string
	baseURL   string
	model     string
	batchSize int
	timeout   time.Duration
	http      *http.Client
}

// defaultEmbedderBaseURL is Voyage AI's public endpoint. Operators
// with a self-hosted proxy or staging environment override via
// EmbedderOptions.BaseURL. Distinct from the package-level
// defaultBaseURL in driver.go which is the Anthropic chat endpoint.
const defaultEmbedderBaseURL = "https://api.voyageai.com"

// voyageEmbeddingDims maps known Voyage AI models to their default
// output dimensions. The voyage-4 family is current as of 2026-05;
// voyage-3 family kept for back-compat with operators on older
// configs. Unknown models construct successfully — Dimension()
// returns 0 + the in-response sanity check at search-time gets
// skipped (the `ok` branch of the map lookup), so the dimension
// mismatch falls through to the store layer's check instead. Keep
// this map current with Voyage's published menu so the embedder-
// side first-line-of-defense catches misconfigurations early.
//
// voyage-4-large supports 256/512/1024/2048 via an output_dimension
// request param; we don't pass it so the 1024 default applies. The
// other -large variants are similar.
var voyageEmbeddingDims = map[string]int{
	// Current (voyage-4 family, 2026-05+).
	"voyage-4":       1024,
	"voyage-4-large": 1024,
	"voyage-4-lite":  1024,
	"voyage-4-nano":  1024,
	// Code + domain-specific.
	"voyage-code-3":    1024,
	"voyage-finance-2": 1024,
	"voyage-law-2":     1024,
	// Back-compat (voyage-3 + voyage-multilingual-2 kept accessible by
	// Voyage; operators on older configs shouldn't break on upgrade).
	"voyage-3":              1024,
	"voyage-3-large":        1024,
	"voyage-multilingual-2": 1024,
}

func init() {
	providers.RegisterEmbedder("anthropic", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return NewEmbedder(opts)
	})
}

// NewEmbedder constructs a configured Embedder. Required: opts.Model
// AND opts.APIKey (a Voyage AI key — Voyage's canonical env var name
// is VOYAGE_API_KEY; main.go's buildEmbedder wires
// cfg.Env.VoyageAPIKey here for the "anthropic" provider slot).
// Empty Model returns an error so a misconfigured operator yaml
// surfaces loudly at boot instead of at first agent run. Empty API
// key is permitted at construction so embedder tests can build the
// driver without secrets; the actual HTTP call will 401 with a clear
// "VOYAGE_API_KEY required" message.
func NewEmbedder(opts providers.EmbedderOptions) (*Embedder, error) {
	if opts.Model == "" {
		return nil, errors.New("anthropic embedder (Voyage AI): opts.Model is required")
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = defaultEmbedderBaseURL
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
func (e *Embedder) Provider() string { return "anthropic" }
func (e *Embedder) Dimension() int   { return voyageEmbeddingDims[e.model] }

// Embed batches `texts` into chunks of at most e.batchSize and
// concatenates the per-batch responses. Voyage's hard cap per request
// is 128 inputs for the voyage-3 family (1000 for voyage-large-2 and
// older); operators should set EmbedderOptions.BatchSize accordingly.
// Default batchSize=0 sends the full slice in one call (works when
// total inputs ≤ 128).
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
			return nil, fmt.Errorf("anthropic embedder (Voyage AI): batch returned %d vectors for %d inputs", len(got), len(batch))
		}
		out = append(out, got...)
	}
	return out, nil
}

// embedOnce sends one /v1/embeddings POST and decodes the response.
// Wraps the call in ratelimit.Do so 429s honour the standard
// Retry-After header. Voyage AI follows the same RFC-7231 semantics
// as OpenAI here, so we reuse the OpenAIRetryAfter parser rather
// than introducing a Voyage-specific one.
func (e *Embedder) embedOnce(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{
		"input": texts,
		"model": e.model,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Per-attempt timeout. Wraps each attempt INSIDE the closure so
	// retries get a fresh deadline — wrapping at the outer call would
	// share a single ticking context across retries, and a Retry-After
	// sleep longer than e.timeout would silently neuter the retry
	// (the second attempt would fire against an already-expired ctx).
	// The outer ctx still applies as the absolute ceiling for the
	// whole batch.
	resp, err := ratelimit.Do(ctx, ratelimit.Config{
		Provider:    "anthropic-voyage",
		ParseHeader: ratelimit.OpenAIRetryAfter,
	}, func(c context.Context) (*http.Response, error) {
		attemptCtx := c
		if e.timeout > 0 {
			var cancel context.CancelFunc
			attemptCtx, cancel = context.WithTimeout(c, e.timeout)
			defer cancel()
		}
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, e.baseURL+"/v1/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
		req.Header.Set("Content-Type", "application/json")
		return e.http.Do(req)
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic embedder (Voyage AI) /v1/embeddings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("anthropic embedder (Voyage AI) /v1/embeddings: status %d: %s", resp.StatusCode, string(preview))
	}

	var doc struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("anthropic embedder (Voyage AI) /v1/embeddings decode: %w", err)
	}
	if len(doc.Data) != len(texts) {
		return nil, fmt.Errorf("anthropic embedder (Voyage AI) /v1/embeddings: got %d embeddings for %d inputs", len(doc.Data), len(texts))
	}
	// Reorder by `index` field — Voyage's docs say in-order today, but
	// the wire contract permits reordering (same as OpenAI), so we
	// honour it.
	out := make([][]float32, len(texts))
	for _, d := range doc.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("anthropic embedder (Voyage AI) /v1/embeddings: response index %d out of range [0,%d)", d.Index, len(out))
		}
		out[d.Index] = d.Embedding
	}
	// Dimension sanity check on the first vector. The Memory tool also
	// validates at search-time against stored rows; doing it here gives
	// a useful error message when the embedder is misconfigured.
	if want, ok := voyageEmbeddingDims[e.model]; ok && len(out) > 0 && len(out[0]) != want {
		return nil, fmt.Errorf("anthropic embedder (Voyage AI) /v1/embeddings: model %q returned %d-dim vectors, expected %d", e.model, len(out[0]), want)
	}
	return out, nil
}
