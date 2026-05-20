package anthropic

import (
	"context"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Embedder is a v0.9.0 placeholder for the Anthropic embedder driver.
//
// As of v0.9.0 Anthropic has no native embeddings API — they direct
// users to Voyage AI (https://docs.voyageai.com/). The v0.9.1
// roadmap covers a Voyage AI proxy under this driver name so an
// operator yaml `memory.embedder.provider: anthropic` becomes the
// "ergonomic choice" once that ships. Until then, this stub
// refuses every Embed() call with store.ErrEmbedderNotImplemented
// so operators selecting it see a clear "use openai or gemini for
// now" message instead of a silent failure.
//
// Construction succeeds — failing in NewEmbedder would prevent
// loomcycle from booting when an operator yaml carries
// `provider: anthropic` for a future-self-only configuration. The
// refusal happens on first Embed() call where the typed error
// flows out to the agent.
type Embedder struct {
	model string
}

func init() {
	providers.RegisterEmbedder("anthropic", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return NewEmbedder(opts)
	})
}

// NewEmbedder constructs the placeholder. Empty Model still
// refuses — same operator-yaml validation as the other drivers, so
// the cross-driver shape is consistent.
func NewEmbedder(opts providers.EmbedderOptions) (*Embedder, error) {
	if opts.Model == "" {
		return nil, errors.New("anthropic embedder: opts.Model is required")
	}
	return &Embedder{model: opts.Model}, nil
}

func (e *Embedder) Model() string    { return e.model }
func (e *Embedder) Provider() string { return "anthropic" }

// Dimension returns 0 — operators can't depend on a stable dimension
// for the placeholder. PR 3's Memory tool surfaces a clear error
// before any Embed() call happens by checking Embedder against
// ErrEmbedderNotImplemented via Embed() at boot or first use.
func (e *Embedder) Dimension() int { return 0 }

// Embed returns store.ErrEmbedderNotImplemented unconditionally.
// Drives the "this provider isn't ready yet" message back to the
// agent via the Memory tool layer (PR 3).
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, store.ErrEmbedderNotImplemented
}
