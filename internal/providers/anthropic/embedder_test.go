package anthropic_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestAnthropicEmbedder_RefusesWithTypedError(t *testing.T) {
	e, err := anthropic.NewEmbedder(providers.EmbedderOptions{
		APIKey: "k",
		Model:  "claude-embed-future",
	})
	if err != nil {
		t.Fatalf("construction should succeed: %v", err)
	}
	_, err = e.Embed(context.Background(), []string{"hello"})
	if !errors.Is(err, store.ErrEmbedderNotImplemented) {
		t.Errorf("got %v, want store.ErrEmbedderNotImplemented", err)
	}
	// Surface the operator-facing migration hint.
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("error should mention openai/gemini alternative: %v", err)
	}
}

func TestAnthropicEmbedder_RegistrationViaInit(t *testing.T) {
	e, err := providers.NewEmbedder("anthropic", providers.EmbedderOptions{
		Model: "claude-embed-future",
	})
	if err != nil {
		t.Fatalf("registry construction: %v", err)
	}
	if e.Provider() != "anthropic" {
		t.Errorf("provider %q, want anthropic", e.Provider())
	}
	// Dimension is 0 for the placeholder — operators can't depend
	// on it. PR 3's Memory tool will surface the refusal via the
	// Embed() error instead.
	if e.Dimension() != 0 {
		t.Errorf("dim %d, want 0 (placeholder)", e.Dimension())
	}
}

func TestAnthropicEmbedder_MissingModelRefuses(t *testing.T) {
	_, err := anthropic.NewEmbedder(providers.EmbedderOptions{APIKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "Model") {
		t.Errorf("expected error mentioning Model, got %v", err)
	}
}
