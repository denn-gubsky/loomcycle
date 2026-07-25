package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Embedder implements providers.Embedder against Ollama's /api/embed
// endpoint — the self-hostable embedder. Wire shape:
//
//	POST {base_url}/api/embed
//	{ "model": "nomic-embed-text",
//	  "input": ["text 1", "text 2"],
//	  "truncate": true }
//	→ { "model": "nomic-embed-text",
//	    "embeddings": [[0.1, ...], [0.2, ...]],
//	    "total_duration": 2900000000, "load_duration": ..., "prompt_eval_count": ... }
//
// `input` accepts a string OR an array; we always send the array form so one
// HTTP call embeds a whole batch and the request shape never forks.
//
// Unlike OpenAI's /v1/embeddings, the response carries NO per-item index —
// order is response order, so the count check below is the only alignment
// guarantee there is. A silent length mismatch would pair every vector with
// the wrong text, so it is a hard error, never a truncation.
//
// PREREQUISITE: a stock Ollama ships no embedding model. The operator must
// `ollama pull nomic-embed-text` (or another embedding model) first; without
// it every call 404s. See the 404 branch in embedOnce.
type Embedder struct {
	apiKey     string
	baseURL    string
	model      string
	batchSize  int
	dimensions int
	timeout    time.Duration
	http       *http.Client

	// dim is the vector width LEARNED from the first successful response.
	// Ollama serves arbitrary operator-pulled models, so unlike the
	// openai/gemini drivers there is no (model → dimension) table that
	// could be anything but wrong. 0 = not yet observed. Atomic because a
	// single Embedder is shared across concurrent runs.
	dim atomic.Int64
}

func init() {
	providers.RegisterEmbedder("ollama", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return NewEmbedder(opts)
	})
}

// NewEmbedder constructs a configured Ollama embedder. Required: opts.Model
// (an embedding model the operator has pulled). opts.APIKey is optional —
// local Ollama is keyless and only a proxied/authenticated deployment needs
// a Bearer token.
func NewEmbedder(opts providers.EmbedderOptions) (*Embedder, error) {
	if opts.Model == "" {
		return nil, errors.New("ollama embedder: opts.Model is required")
	}
	if opts.Dimensions < 0 {
		return nil, fmt.Errorf("ollama embedder: opts.Dimensions must be >= 0 (got %d)", opts.Dimensions)
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
		// Announce the default once, at construction. Inside a container
		// `localhost` is the CONTAINER, not the host running Ollama — the
		// resulting connection-refused is otherwise mystifying, and this
		// line is the operator's pointer at memory.embedder.base_url.
		log.Printf("ollama embedder: no memory.embedder.base_url set — defaulting to %s (in a container, point this at the host's Ollama)", defaultBaseURL)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &Embedder{
		apiKey:     opts.APIKey,
		baseURL:    baseURL,
		model:      opts.Model,
		batchSize:  opts.BatchSize,
		dimensions: opts.Dimensions,
		timeout:    opts.Timeout,
		http:       opts.HTTPClientOrDefault(),
	}, nil
}

func (e *Embedder) Model() string    { return e.model }
func (e *Embedder) Provider() string { return "ollama" }

// Dimension returns 0 until the first successful Embed, then the observed
// vector width. Deliberately NOT a hardcoded table: Ollama serves whatever
// embedding model the operator pulled, and several (qwen3-embedding) support
// Matryoshka truncation via the `dimensions` request field, so the width is a
// runtime fact, not a compile-time one.
//
// Every write path that persists a dimension either reads it AFTER an Embed
// call (cmd/loomcycle bootstrapMemoryEntries, internal/api/http/memory.go) or
// uses len(vector) directly (history, help index, the reembed admin op), so a
// pre-first-call 0 never reaches a stored row. It does show as dim=0 in the
// boot log line and in the /v1/_memory/reembed report until the first embed.
func (e *Embedder) Dimension() int { return int(e.dim.Load()) }

// Embed batches texts into chunks of at most e.batchSize and concatenates the
// per-batch responses preserving order. batchSize 0 = one call for everything.
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
			return nil, fmt.Errorf("ollama embedder: batch returned %d vectors for %d inputs", len(got), len(batch))
		}
		out = append(out, got...)
	}
	return out, nil
}

// embedOnce sends one /api/embed POST and decodes the response.
func (e *Embedder) embedOnce(ctx context.Context, texts []string) ([][]float32, error) {
	payload := map[string]any{
		"model": e.model,
		"input": texts,
		// Ollama's documented default is already true; sending it explicitly
		// pins the behaviour so a server-side default flip can't silently
		// turn over-long inputs into errors (or vice versa).
		"truncate": true,
	}
	// Matryoshka truncation. Omitted entirely when unset — sending 0 would ask
	// for zero-width vectors rather than the model's native size.
	if e.dimensions > 0 {
		payload["dimensions"] = e.dimensions
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	attemptCtx := ctx
	if e.timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, e.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Keyless by default — a local Ollama has no auth. Only a proxied or
	// hosted (ollama.com) endpoint gets a Bearer header.
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama /api/embed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Bounded read: a base_url pointing at the wrong service could stream
		// megabytes into what is only meant to be an error string.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		preview := strings.TrimSpace(string(raw))
		if resp.StatusCode == http.StatusNotFound {
			// The single most likely first-run failure: a stock Ollama has NO
			// embedding model, and its 404 body names the model. Surface the
			// name plus the exact remedy instead of a bare status code.
			return nil, fmt.Errorf("ollama /api/embed: model %q not found — run `ollama pull %s` on the Ollama host: %s", e.model, e.model, preview)
		}
		return nil, fmt.Errorf("ollama /api/embed: model %q: status %d: %s", e.model, resp.StatusCode, preview)
	}

	var doc struct {
		Model      string      `json:"model"`
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("ollama /api/embed decode: %w", err)
	}
	// No per-item index on the wire: position IS the only alignment. A short
	// (or long) response must fail loudly — silently returning fewer vectors
	// would pair every later text with the wrong embedding.
	if len(doc.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama /api/embed: got %d embeddings for %d inputs", len(doc.Embeddings), len(texts))
	}
	if len(doc.Embeddings) > 0 {
		got := len(doc.Embeddings[0])
		if got == 0 {
			return nil, fmt.Errorf("ollama /api/embed: model %q returned an empty vector", e.model)
		}
		// First success wins; afterwards a differing width means the model
		// behind this name changed under us, which would corrupt the store's
		// per-row dimension bookkeeping. Same sanity check the openai/gemini
		// drivers do against their static table, against a learned value.
		if !e.dim.CompareAndSwap(0, int64(got)) {
			if want := int(e.dim.Load()); want != got {
				return nil, fmt.Errorf("ollama /api/embed: model %q returned %d-dim vectors, expected %d", e.model, got, want)
			}
		}
	}
	return doc.Embeddings, nil
}
