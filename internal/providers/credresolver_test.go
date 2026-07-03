package providers

import (
	"context"
	"testing"
)

func TestResolveCredential_NoResolver(t *testing.T) {
	// A bare ctx has no resolver → callers fall back to the host key.
	if v, ok := ResolveCredential(context.Background(), "ANTHROPIC_API_KEY"); ok || v != "" {
		t.Fatalf("ResolveCredential on bare ctx = (%q, %v), want (\"\", false)", v, ok)
	}
}

func TestResolveCredential_Stamped(t *testing.T) {
	ctx := WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		if name == "ANTHROPIC_API_KEY" {
			return "sk-tenant", true
		}
		return "", false
	})
	if v, ok := ResolveCredential(ctx, "ANTHROPIC_API_KEY"); !ok || v != "sk-tenant" {
		t.Errorf("hit = (%q, %v), want (sk-tenant, true)", v, ok)
	}
	// A name the resolver doesn't know → no override.
	if v, ok := ResolveCredential(ctx, "OPENAI_API_KEY"); ok || v != "" {
		t.Errorf("miss = (%q, %v), want (\"\", false)", v, ok)
	}
}

func TestResolveCredential_EmptyValueIsNotAnOverride(t *testing.T) {
	// A resolver that returns ("", true) must NOT override — an empty override
	// would blank the host key and break auth. Treated as a miss.
	ctx := WithCredentialResolver(context.Background(), func(context.Context, string) (string, bool) {
		return "", true
	})
	if v, ok := ResolveCredential(ctx, "ANTHROPIC_API_KEY"); ok || v != "" {
		t.Errorf("empty override = (%q, %v), want (\"\", false)", v, ok)
	}
}

func TestWithCredentialResolver_NilIsNoop(t *testing.T) {
	// Stamping a nil resolver leaves the ctx clean (open-mode / no KEK path).
	ctx := WithCredentialResolver(context.Background(), nil)
	if _, ok := ResolveCredential(ctx, "ANTHROPIC_API_KEY"); ok {
		t.Errorf("nil resolver should not resolve anything")
	}
}
