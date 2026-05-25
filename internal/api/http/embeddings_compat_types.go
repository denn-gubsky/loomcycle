package http

import "encoding/json"

// embeddings_compat_types.go — v0.11.4 OpenAI Embeddings-compatible
// wire shapes for POST /v1/embeddings. Mirrors the OpenAI Embeddings
// API closely enough that consumers using the OpenAI SDK can point
// at loomcycle by changing only the base URL.
//
// The shim translates these shapes into a `providers.Embedder.Embed`
// call against the single configured embedder. No resolver path, no
// tier overlay, no streaming — embeddings are synchronous and
// loomcycle has one embedder per instance per the v0.9.0 RFC.

// openaiEmbeddingsRequest is the POST /v1/embeddings body. The
// `input` field is intentionally json.RawMessage so the handler can
// reject pre-tokenized inputs (number arrays — Voyage / Anthropic
// don't accept tokens) with a clear error rather than silently
// mis-routing.
type openaiEmbeddingsRequest struct {
	// Model is the consumer's requested model id (e.g.
	// "text-embedding-3-small"). Loomcycle's resolver doesn't
	// switch embedders based on this field — there's only one
	// configured embedder per instance — but the value is echoed
	// in the response so SDK consumers see the model they asked
	// for (drop-in compatibility) and the audit log records both
	// requested + served so operators can spot drift.
	Model string `json:"model"`

	// Input is `string | []string | []int | [][]int` per OpenAI.
	// v1 handles the string + []string forms. Tokenized forms
	// (number arrays) are refused with a clear error pointing
	// at "send text strings".
	Input json.RawMessage `json:"input"`

	// EncodingFormat is "float" (default) or "base64". Base64
	// packs each float32 little-endian + base64-encodes per
	// OpenAI spec — saves ~25% wire bytes on 1536-dim vectors.
	EncodingFormat string `json:"encoding_format,omitempty"`

	// Dimensions is OpenAI's post-hoc dimension reduction
	// parameter (text-embedding-3-* models support it). v1
	// accepts-but-ignores — the providers.Embedder interface
	// doesn't take a dimension parameter today. When the
	// substrate grows it, the shim picks it up automatically.
	Dimensions int `json:"dimensions,omitempty"`

	// User is OpenAI's opaque end-user identifier. Maps onto
	// loomcycle's user_id for per-user quota tracking + audit.
	User string `json:"user,omitempty"`
}

// openaiEmbeddingsResponse is the response shape.
type openaiEmbeddingsResponse struct {
	Object string                `json:"object"` // always "list"
	Data   []openaiEmbeddingItem `json:"data"`
	Model  string                `json:"model"`
	Usage  openaiEmbeddingsUsage `json:"usage"`
}

// openaiEmbeddingItem is one embedded vector entry in the response
// `data` array.
//
// `Embedding` is `json.RawMessage` because it serialises differently
// based on the request's `encoding_format`:
//   - "float" (default) → JSON array of numbers: `[0.1, 0.2, ...]`
//   - "base64" → JSON string: `"AABEAAEgwgI..."` (LE float32 packed
//     then base64)
//
// Using RawMessage on the struct lets the handler marshal the
// appropriate shape per request without two response types.
type openaiEmbeddingItem struct {
	Object    string          `json:"object"` // always "embedding"
	Embedding json.RawMessage `json:"embedding"`
	Index     int             `json:"index"`
}

// openaiEmbeddingsUsage is the token-accounting payload. OpenAI's
// shape uses prompt_tokens + total_tokens (NOT completion_tokens —
// embeddings have no completion). Loomcycle's embedder interface
// doesn't return token counts today; v1 leaves both fields at 0
// (operator-visible signal that the shim doesn't have native
// per-call token accounting yet).
//
// When the providers.Embedder interface gains usage reporting (an
// `EmbedWithUsage` overload, say), this struct gets populated; no
// wire-shape change needed.
type openaiEmbeddingsUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}
