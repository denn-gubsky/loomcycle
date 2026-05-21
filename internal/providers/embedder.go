package providers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Embedder is the v0.9.0 Vector Memory abstraction over each provider's
// embeddings API. Drivers live alongside their chat-completion siblings
// (internal/providers/{anthropic,openai,gemini}/embedder.go) and register
// themselves at init() time.
//
// Embedder is intentionally NOT part of the Provider interface — the
// embeddings API is non-streaming, runs on a different endpoint, and is
// only consumed by the Memory tool. Bolting it onto Provider would force
// every driver to think about embedding shape even when an operator's
// config never asks for vectors.
//
// Returned vectors are float32 (not float64) to match what every
// embedding API returns on the wire AND what pgvector / sqlite-vec
// accept natively. Operators don't see a precision loss here — the
// providers don't expose higher precision in the first place.
type Embedder interface {
	// Embed turns N texts into N embedding vectors. The driver batches
	// according to its EmbedderOptions.BatchSize; the returned slice is
	// the same length and in the same order as `texts`. Returns a typed
	// error (store.ErrEmbedderNotImplemented, context errors, or a
	// driver-specific HTTP error) on failure.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Model returns the wire model id this driver was constructed for
	// (e.g. "text-embedding-3-large"). The Store records this on each
	// MemoryEmbedSet so dimension-mismatch detection at search time can
	// pinpoint which model produced which row.
	Model() string

	// Provider returns the canonical provider name ("openai" / "gemini"
	// / "anthropic"). Mirrors Provider.ID() — same string the operator
	// uses in `memory.embedder.provider` yaml.
	Provider() string

	// Dimension returns the expected output vector dimension. Drivers
	// hardcode this per (provider, model) pair since the embedding APIs
	// don't dynamically negotiate dimension. Used by the Memory tool's
	// pre-flight validation — refuse with ErrDimensionMismatch when an
	// agent's `search` query lands on a config whose Dimension doesn't
	// match the stored rows.
	Dimension() int
}

// EmbedderOptions is the constructor input for every embedder driver.
// Static configuration the embedder needs to make HTTP calls — operator
// yaml + env vars get translated into this shape in
// cmd/loomcycle/main.go and passed to NewEmbedder.
type EmbedderOptions struct {
	// APIKey is the bearer/key for the provider. Sourced from the
	// existing provider env vars (OPENAI_API_KEY etc.) — same value
	// the chat-completion driver uses.
	APIKey string

	// BaseURL overrides the default endpoint. Empty = driver default.
	// Useful for Azure OpenAI, OpenAI-compatible local servers, etc.
	BaseURL string

	// Model selects the wire model name. Required — drivers refuse
	// construction with empty Model.
	Model string

	// Timeout caps a single HTTP call. Zero = no per-call timeout;
	// rely on outer ctx. Matches LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS.
	Timeout time.Duration

	// BatchSize is the max number of texts per single API call.
	// Provider-specific caps still apply on top. Zero = batch
	// everything in one call (provider's hard cap will surface).
	// Matches LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE.
	BatchSize int

	// HTTPClient is optional; nil falls back to a fresh
	// http.Client{Timeout: opts.Timeout}. Tests inject an httptest
	// server's client via this field.
	HTTPClient *http.Client
}

// HTTPClientOrDefault returns opts.HTTPClient when set, otherwise a
// fresh client whose Timeout is opts.Timeout. Centralised here so
// every embedder driver gets the same fallback policy.
func (opts EmbedderOptions) HTTPClientOrDefault() *http.Client {
	if opts.HTTPClient != nil {
		return opts.HTTPClient
	}
	return &http.Client{Timeout: opts.Timeout}
}

// EmbedderConstructor builds one embedder driver instance from opts.
// Errors here are surfaced to the operator at boot — typical failure
// modes are "missing API key", "unsupported model", "bad base URL".
type EmbedderConstructor func(opts EmbedderOptions) (Embedder, error)

var (
	embedderRegistryMu sync.RWMutex
	embedderRegistry   = map[string]EmbedderConstructor{}
)

// RegisterEmbedder records a provider-name → constructor mapping.
// Drivers call this from their init() so the consumer side never
// needs to know which providers are compiled in. Re-registering is
// allowed (later wins) — tests use this to inject fakes without
// touching production code.
func RegisterEmbedder(provider string, ctor EmbedderConstructor) {
	embedderRegistryMu.Lock()
	defer embedderRegistryMu.Unlock()
	embedderRegistry[provider] = ctor
}

// NewEmbedder looks up the registered constructor for `provider` and
// returns a configured driver. Unknown provider → error with the
// full set of known names in the message (operators with a typo see
// what's available).
func NewEmbedder(provider string, opts EmbedderOptions) (Embedder, error) {
	embedderRegistryMu.RLock()
	ctor, ok := embedderRegistry[provider]
	embedderRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown embedder provider %q (known: %v)", provider, RegisteredEmbedders())
	}
	return ctor(opts)
}

// RegisteredEmbedders returns the sorted list of provider names with
// a registered constructor. Used in error messages + the v0.9.0 PR 4
// admin endpoint so operators see "you have openai and gemini wired
// in" without grep-ing the binary.
func RegisteredEmbedders() []string {
	embedderRegistryMu.RLock()
	defer embedderRegistryMu.RUnlock()
	out := make([]string, 0, len(embedderRegistry))
	for name := range embedderRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
