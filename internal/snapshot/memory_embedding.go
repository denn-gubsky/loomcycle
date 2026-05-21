package snapshot

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MemoryEmbeddingSnapshot is the optional per-memory-row embedding
// payload. The wire shape is locked in
// doc-internal/rfcs/semantic-memory.md § "Snapshot integration":
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

// captureEmbedding looks up the embedding row for (scope, scopeID,
// key) and returns the wire shape. Returns (nil, nil) when:
//
//   - the backend doesn't support vectors (refusal stub backends);
//   - no embedding exists for this row (operators wrote it with
//     embed=false, or it was deleted via the admin reembed endpoint
//     after the k/v row landed).
//
// Capture failures (e.g. a DB error) bubble out so the snapshot
// fails loudly rather than silently dropping vectors. Operators
// re-run capture on a healthy DB.
func captureEmbedding(ctx context.Context, s store.Store, scope store.MemoryScope, scopeID, key string) (*MemoryEmbeddingSnapshot, error) {
	if !s.SupportsVectors() {
		return nil, nil
	}
	emb, err := s.MemoryEmbedGet(ctx, scope, scopeID, key)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, nil
		}
		// ErrVectorUnsupported is a guard against backends whose
		// SupportsVectors() lied — treat as "no embedding" so a
		// flapping config doesn't fail the whole snapshot.
		if errors.Is(err, store.ErrVectorUnsupported) {
			return nil, nil
		}
		return nil, fmt.Errorf("capture embedding %s/%s/%s: %w", scope, scopeID, key, err)
	}
	return &MemoryEmbeddingSnapshot{
		Provider:  emb.Provider,
		Model:     emb.Model,
		Dimension: emb.Dimension,
		Vector:    encodeFloat32LEBase64(emb.Vector),
		EmbedText: emb.EmbedText,
		CreatedAt: emb.CreatedAt,
	}, nil
}

// nilEmbedding is preserved for back-compat with the Phase-1
// captureMemory call site. It returns nil → JSON null. New call
// sites should use captureEmbedding directly.
//
// Deprecated: use captureEmbedding for snapshot Phase-2 paths.
func nilEmbedding() *MemoryEmbeddingSnapshot { return nil }

// encodeFloat32LEBase64 packs []float32 into little-endian bytes
// then base64-encodes the result. Matches the on-disk pgvector +
// sqlite-vec wire formats so a JSON snapshot can move bytes
// directly to either backend.
func encodeFloat32LEBase64(vec []float32) string {
	if len(vec) == 0 {
		return ""
	}
	b := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// decodeFloat32LEBase64 reverses encodeFloat32LEBase64. Returns an
// error when the base64 payload doesn't decode OR its byte length
// isn't a multiple of 4 — both are corruption signals.
func decodeFloat32LEBase64(s string) ([]float32, error) {
	if s == "" {
		return []float32{}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	if len(raw)%4 != 0 {
		return nil, fmt.Errorf("vector byte length %d is not a multiple of 4", len(raw))
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out, nil
}
