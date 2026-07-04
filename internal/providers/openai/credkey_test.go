package openai

import (
	"context"
	"errors"
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
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "tenant-openai"}, name == "OPENAI_API_KEY"
	})
	if got, _, _, err := d.resolveKey(ctx); err != nil || got != "tenant-openai" {
		t.Errorf("resolveKey = (%q, %v), want (tenant-openai, nil)", got, err)
	}
}

// RFC AX: a restricted run with no override refuses with ErrOperatorKeyForbidden
// (never the host key); an override still wins under restriction.
func TestResolveKey_RestrictedRefusesHostKey(t *testing.T) {
	d := New("host-key", "", streamhttp.Options{}, nil)

	restricted := providers.WithOperatorKeyAllowed(context.Background(), false)
	if got, _, _, err := d.resolveKey(restricted); !errors.Is(err, providers.ErrOperatorKeyForbidden) || got != "" {
		t.Errorf("restricted, no override: (%q, %v), want (\"\", ErrOperatorKeyForbidden)", got, err)
	}

	withOverride := providers.WithCredentialResolver(restricted, func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "tenant-openai", Scope: "user"}, name == "OPENAI_API_KEY"
	})
	if got, source, _, err := d.resolveKey(withOverride); err != nil || got != "tenant-openai" || source != "user" {
		t.Errorf("restricted, with override: (%q, %q, %v), want (tenant-openai, user, nil)", got, source, err)
	}
}

// SetKeyEnvName (used by the DeepSeek wrapper, which reuses this driver) points
// the override at DEEPSEEK_API_KEY — an OPENAI_API_KEY credential must NOT leak
// into a DeepSeek run, and vice versa.
func TestCallKey_SetKeyEnvName_DeepSeek(t *testing.T) {
	d := New("host-key", "", streamhttp.Options{}, nil)
	d.SetKeyEnvName("DEEPSEEK_API_KEY")

	deepseekOnly := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "tenant-deepseek"}, name == "DEEPSEEK_API_KEY"
	})
	if got, _, _, err := d.resolveKey(deepseekOnly); err != nil || got != "tenant-deepseek" {
		t.Errorf("resolveKey = (%q, %v), want (tenant-deepseek, nil)", got, err)
	}

	// A stored OPENAI_API_KEY is the wrong name for a DeepSeek driver → host key.
	openaiOnly := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "tenant-openai"}, name == "OPENAI_API_KEY"
	})
	if got, _, _, err := d.resolveKey(openaiOnly); err != nil || got != "host-key" {
		t.Errorf("resolveKey = (%q, %v), want (host-key, nil) (OPENAI name must not apply to DeepSeek)", got, err)
	}
}
