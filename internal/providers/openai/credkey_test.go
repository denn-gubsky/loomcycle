package openai

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// The OpenAI driver resolves the override under OPENAI_API_KEY by default.
func TestCallKey_DefaultEnvName(t *testing.T) {
	d := New("host-key", "", streamhttp.Options{}, nil)
	if d.keyEnvName != "OPENAI_API_KEY" {
		t.Fatalf("default keyEnvName = %q, want OPENAI_API_KEY", d.keyEnvName)
	}
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		return "tenant-openai", name == "OPENAI_API_KEY"
	})
	if got := d.callKey(ctx); got != "tenant-openai" {
		t.Errorf("callKey = %q, want tenant-openai", got)
	}
}

// SetKeyEnvName (used by the DeepSeek wrapper, which reuses this driver) points
// the override at DEEPSEEK_API_KEY — an OPENAI_API_KEY credential must NOT leak
// into a DeepSeek run, and vice versa.
func TestCallKey_SetKeyEnvName_DeepSeek(t *testing.T) {
	d := New("host-key", "", streamhttp.Options{}, nil)
	d.SetKeyEnvName("DEEPSEEK_API_KEY")

	deepseekOnly := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		return "tenant-deepseek", name == "DEEPSEEK_API_KEY"
	})
	if got := d.callKey(deepseekOnly); got != "tenant-deepseek" {
		t.Errorf("callKey = %q, want tenant-deepseek", got)
	}

	// A stored OPENAI_API_KEY is the wrong name for a DeepSeek driver → host key.
	openaiOnly := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		return "tenant-openai", name == "OPENAI_API_KEY"
	})
	if got := d.callKey(openaiOnly); got != "host-key" {
		t.Errorf("callKey = %q, want host-key (OPENAI name must not apply to DeepSeek)", got)
	}
}
