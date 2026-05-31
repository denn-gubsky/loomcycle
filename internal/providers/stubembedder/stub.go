// Package stubembedder is a DETERMINISTIC, dependency-free embedder for
// runtime tests of the Memory feature (search + dedup). It is NOT a real
// embedder and must never be used in production: it produces a hashed
// bag-of-words vector, not a semantic embedding.
//
// Registration is GATED on LOOMCYCLE_EMBEDDER_STUB=1 (mirroring the mock
// provider's LOOMCYCLE_MOCK_ENABLED posture), so selecting
// `memory.embedder.provider: stub` in a config that did not set the env
// var fails with the normal "unknown embedder provider" error — the stub
// is invisible to a production binary.
//
// The vector is a signed hashed bag-of-words, L2-normalised, so:
//   - the same text always yields the same vector (deterministic CI);
//   - near-duplicate text (one shares most tokens with the other) yields a
//     near-identical vector → cosine ≥ the dedup threshold (0.92), so the
//     search-time dedup collapses them — exactly the property a dedup
//     runtime test needs to assert deterministically;
//   - clearly-distinct text yields a low-cosine vector → kept.
package stubembedder

import (
	"context"
	"hash/fnv"
	"math"
	"os"
	"strings"
	"unicode"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// stubDim is the fixed output dimension. 256 is ample to keep token-hash
// collisions rare for the short texts a dedup test uses, while staying
// cheap to store/compare.
const stubDim = 256

func init() {
	// Only available when explicitly enabled — a production binary that
	// never sets the flag cannot select a fake embedder by config typo.
	if os.Getenv("LOOMCYCLE_EMBEDDER_STUB") != "1" {
		return
	}
	providers.RegisterEmbedder("stub", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		model := opts.Model
		if model == "" {
			model = "stub-deterministic-v1"
		}
		return &Embedder{model: model}, nil
	})
}

// Embedder implements providers.Embedder deterministically.
type Embedder struct{ model string }

func (e *Embedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = embedOne(t)
	}
	return out, nil
}

func (e *Embedder) Model() string    { return e.model }
func (e *Embedder) Provider() string { return "stub" }
func (e *Embedder) Dimension() int   { return stubDim }

var _ providers.Embedder = (*Embedder)(nil)

// embedOne builds the signed hashed bag-of-words vector for one text.
func embedOne(text string) []float32 {
	v := make([]float32, stubDim)
	for _, tok := range tokenize(text) {
		idx := hash32(tok) % uint32(stubDim)
		// A second, independent hash decides the sign so two distinct
		// tokens that collide on the same bucket tend to cancel rather
		// than spuriously reinforce similarity.
		if hash32(tok+"\x00sign")&1 == 1 {
			v[idx] += 1
		} else {
			v[idx] -= 1
		}
	}
	// L2-normalise so cosine == dot product. An empty/token-less text maps
	// to a fixed unit vector (deterministic + valid, never all-zero).
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		v[0] = 1
		return v
	}
	inv := 1.0 / math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) * inv)
	}
	return v
}

// tokenize lowercases and splits on any non-alphanumeric rune.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func hash32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}
