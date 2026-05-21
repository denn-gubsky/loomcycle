package providers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// fakeEmbedder is a deterministic embedder for the registry tests.
type fakeEmbedder struct{ model, provider string }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}
func (f *fakeEmbedder) Model() string    { return f.model }
func (f *fakeEmbedder) Provider() string { return f.provider }
func (f *fakeEmbedder) Dimension() int   { return 4 }

func TestRegisterAndNewEmbedder(t *testing.T) {
	providers.RegisterEmbedder("fake-for-test", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return &fakeEmbedder{model: opts.Model, provider: "fake-for-test"}, nil
	})
	e, err := providers.NewEmbedder("fake-for-test", providers.EmbedderOptions{Model: "fm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Model() != "fm-1" {
		t.Errorf("model %q, want fm-1", e.Model())
	}
	if e.Provider() != "fake-for-test" {
		t.Errorf("provider %q, want fake-for-test", e.Provider())
	}
	if e.Dimension() != 4 {
		t.Errorf("dim %d, want 4", e.Dimension())
	}
}

func TestNewEmbedder_UnknownProviderListsKnown(t *testing.T) {
	// Register one known so the error message shows it.
	providers.RegisterEmbedder("known-for-listing", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return &fakeEmbedder{model: opts.Model}, nil
	})
	_, err := providers.NewEmbedder("does-not-exist", providers.EmbedderOptions{Model: "m"})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the unknown provider: %v", err)
	}
	if !strings.Contains(err.Error(), "known-for-listing") {
		t.Errorf("error should list known providers: %v", err)
	}
}

func TestRegisteredEmbeddersReturnsSorted(t *testing.T) {
	// Self-register a couple of names; assert RegisteredEmbedders returns
	// them in sorted order alongside whatever else is registered.
	providers.RegisterEmbedder("zzz-for-sort", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return &fakeEmbedder{}, nil
	})
	providers.RegisterEmbedder("aaa-for-sort", func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return &fakeEmbedder{}, nil
	})
	names := providers.RegisteredEmbedders()
	// Find our pair and verify a < z by index.
	var iA, iZ int = -1, -1
	for i, n := range names {
		if n == "aaa-for-sort" {
			iA = i
		}
		if n == "zzz-for-sort" {
			iZ = i
		}
	}
	if iA < 0 || iZ < 0 {
		t.Fatalf("registered names not found: %v", names)
	}
	if iA >= iZ {
		t.Errorf("expected aaa before zzz, got %v", names)
	}
}
