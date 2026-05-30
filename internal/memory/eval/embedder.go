package eval

import (
	"context"
	"hash/fnv"
	"math"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// DeterministicEmbedder is a hash-based providers.Embedder for the
// memory-eval harness. It maps text → a fixed-dimension unit vector
// reproducibly (same text always yields the same vector), with no network
// call and no API key — so the eval harness runs in CI without a real
// embedding provider.
//
// IT IS NOT A SEMANTIC EMBEDDER. Two texts that share whitespace tokens
// land in overlapping coordinates (a bag-of-hashed-tokens model), which is
// enough to exercise precision/recall/dedup/latency deterministically, but
// it does NOT capture meaning. A real precision/recall number for a ranker
// change must come from an operator-run eval against a REAL embedder
// (--dataset <file> with the real memory stack); this stub exists so the
// harness's plumbing and metric math are CI-testable.
type DeterministicEmbedder struct {
	dim int
}

// NewDeterministicEmbedder builds the stub embedder with the given output
// dimension. dim <= 0 falls back to 64 — small enough to be fast, large
// enough that distinct token sets rarely fully collide.
func NewDeterministicEmbedder(dim int) *DeterministicEmbedder {
	if dim <= 0 {
		dim = 64
	}
	return &DeterministicEmbedder{dim: dim}
}

// Embed maps each text to a unit vector via a bag-of-hashed-tokens model:
// every whitespace token sets a coordinate (token-hash mod dim) to 1, then
// the vector is L2-normalised. Texts sharing tokens get higher cosine
// similarity — enough structure for the harness's recall/dedup metrics to
// be meaningful and reproducible. A text with no tokens yields the zero
// vector (cosine similarity 0 to everything), which the ranker/dedup
// handle safely.
func (e *DeterministicEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vectorFor(t)
	}
	return out, nil
}

func (e *DeterministicEmbedder) vectorFor(text string) []float32 {
	vec := make([]float32, e.dim)
	for _, tok := range tokenize(text) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		vec[int(h.Sum32())%e.dim] += 1
	}
	// L2-normalise so cosine similarity is bounded and a longer text
	// doesn't artificially dominate by magnitude.
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return vec // all-zero: safe (cosine 0 everywhere)
	}
	inv := float32(1.0 / math.Sqrt(norm))
	for j := range vec {
		vec[j] *= inv
	}
	return vec
}

// tokenize lowercases and splits on non-alphanumeric runes. Deliberately
// simple — the stub's only job is reproducibility, not linguistics.
func tokenize(text string) []string {
	var toks []string
	cur := make([]rune, 0, 16)
	flush := func() {
		if len(cur) > 0 {
			toks = append(toks, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z':
			cur = append(cur, r)
		case r >= 'A' && r <= 'Z':
			cur = append(cur, r+('a'-'A'))
		case r >= '0' && r <= '9':
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return toks
}

func (e *DeterministicEmbedder) Model() string    { return "deterministic-eval-stub" }
func (e *DeterministicEmbedder) Provider() string { return "eval" }
func (e *DeterministicEmbedder) Dimension() int   { return e.dim }

var _ providers.Embedder = (*DeterministicEmbedder)(nil)
