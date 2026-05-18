package snapshot

import "time"

// MemoryEmbeddingSnapshot is the optional per-memory-row embedding
// payload. In Phase 1 (this package's current shape) every memory
// entry's Embedding field is nil → serialises as JSON null. Phase 2
// (vector ops) reads the embedding from a new store table and
// populates this struct.
//
// The wire shape is locked in doc-internal/rfcs/semantic-memory.md
// § "Snapshot integration":
//
//	{
//	  "provider": "openai",
//	  "model":    "text-embedding-3-large",
//	  "dimension": 1536,
//	  "vector":   "<base64 little-endian float32 array>",
//	  "embed_text": "the source text we embedded",
//	  "created_at": "..."
//	}
//
// Vector is a base64 string rather than a JSON array of floats for
// two reasons: (1) JSON-encoded floats are lossy + verbose; base64
// preserves bit-identical round-trip, (2) the wire size is ~4×
// dimension bytes rather than ~12× when encoded as decimal-string
// floats. The float32 array is packed little-endian (matches pgvector
// + sqlite-vec storage formats).
type MemoryEmbeddingSnapshot struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Dimension int       `json:"dimension"`
	Vector    string    `json:"vector"` // base64 of little-endian float32 packed array
	EmbedText string    `json:"embed_text"`
	CreatedAt time.Time `json:"created_at"`
}

// nilEmbedding is the canonical "no embedding" value used in Phase 1
// by Capture(). Returns nil so json.Marshal emits null.
//
// Phase 2 will replace this with a store-driven lookup: for each
// memory entry, call store.MemoryEmbedGet(scope, scope_id, key) and
// either return nil (no embedding stored) or a populated struct. The
// Phase 1 → Phase 2 transition is transparent at the wire level:
// the JSON shape is the same; only the Phase 1 value is null.
func nilEmbedding() *MemoryEmbeddingSnapshot {
	return nil
}
