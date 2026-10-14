package providers

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestResolveCredential_NoResolver(t *testing.T) {
	// A bare ctx has no resolver → callers fall back to the host key.
	if v, ok, _ := ResolveCredential(context.Background(), "ANTHROPIC_API_KEY"); ok || v != "" {
		t.Fatalf("ResolveCredential on bare ctx = (%q, %v), want (\"\", false)", v, ok)
	}
	if _, ok, _ := ResolveCredentialFull(context.Background(), "ANTHROPIC_API_KEY"); ok {
		t.Errorf("ResolveCredentialFull on bare ctx should miss")
	}
}

func TestResolveCredential_Stamped(t *testing.T) {
	ctx := WithCredentialResolver(context.Background(), func(_ context.Context, name string) (CredentialResolution, bool, error) {
		if name == "ANTHROPIC_API_KEY" {
			return CredentialResolution{Value: "sk-tenant", Scope: "tenant"}, true, nil
		}
		return CredentialResolution{}, false, nil
	})
	if v, ok, _ := ResolveCredential(ctx, "ANTHROPIC_API_KEY"); !ok || v != "sk-tenant" {
		t.Errorf("hit = (%q, %v), want (sk-tenant, true)", v, ok)
	}
	// A name the resolver doesn't know → no override.
	if v, ok, _ := ResolveCredential(ctx, "OPENAI_API_KEY"); ok || v != "" {
		t.Errorf("miss = (%q, %v), want (\"\", false)", v, ok)
	}
}

func TestResolveCredentialFull_CarriesScope(t *testing.T) {
	// RFC AV: the owning scope + scope_id ride the resolution for usage tagging.
	ctx := WithCredentialResolver(context.Background(), func(_ context.Context, name string) (CredentialResolution, bool, error) {
		return CredentialResolution{Value: "tok", Scope: "user", ScopeID: "u-42"}, name == "TELEGRAM_TOKEN", nil
	})
	res, ok, _ := ResolveCredentialFull(ctx, "TELEGRAM_TOKEN")
	if !ok || res.Value != "tok" || res.Scope != "user" || res.ScopeID != "u-42" {
		t.Errorf("full = %+v ok=%v, want {tok user u-42} true", res, ok)
	}
}

func TestResolveCredential_EmptyValueIsNotAnOverride(t *testing.T) {
	// A resolver that returns ("", true) must NOT override — an empty override
	// would blank the host key and break auth. Treated as a miss.
	ctx := WithCredentialResolver(context.Background(), func(context.Context, string) (CredentialResolution, bool, error) {
		return CredentialResolution{Value: "", Scope: "tenant"}, true, nil
	})
	if v, ok, _ := ResolveCredential(ctx, "ANTHROPIC_API_KEY"); ok || v != "" {
		t.Errorf("empty override = (%q, %v), want (\"\", false)", v, ok)
	}
	if _, ok, _ := ResolveCredentialFull(ctx, "ANTHROPIC_API_KEY"); ok {
		t.Errorf("empty override (full) should be a miss")
	}
}

func TestWithCredentialResolver_NilIsNoop(t *testing.T) {
	// Stamping a nil resolver leaves the ctx clean (open-mode / no KEK path).
	ctx := WithCredentialResolver(context.Background(), nil)
	if _, ok, _ := ResolveCredential(ctx, "ANTHROPIC_API_KEY"); ok {
		t.Errorf("nil resolver should not resolve anything")
	}
}

// An override the run holds but may not use is an error from the key choke
// point, whether or not the run may use the operator's key: the operator key
// never replaces it. A plain miss still gets the operator key.
func TestResolveKeyOrOperator_AnUnusableOverrideNeverFallsBackToTheOperatorKey(t *testing.T) {
	unusable := func(_ context.Context, name string) (CredentialResolution, bool, error) {
		if name == "ANTHROPIC_API_KEY" {
			return CredentialResolution{}, false, fmt.Errorf("%w: %q expired", ErrCredentialUnusable, name)
		}
		return CredentialResolution{}, false, nil
	}
	for _, allowed := range []bool{true, false} {
		ctx := WithOperatorKeyAllowed(WithCredentialResolver(context.Background(), unusable), allowed)
		key, source, _, err := ResolveKeyOrOperator(ctx, "ANTHROPIC_API_KEY", "sk-operator")
		if !errors.Is(err, ErrCredentialUnusable) || key != "" || source != "" {
			t.Errorf("operator key allowed=%v: key=%q source=%q err=%v; want ErrCredentialUnusable and no key", allowed, key, source, err)
		}
		if got := ClassifyError(err); got != ErrorClassPermanent {
			t.Errorf("ClassifyError = %v, want permanent (no cascade onto another provider's operator key)", got)
		}
	}
	ctx := WithCredentialResolver(context.Background(), unusable)
	if key, source, _, err := ResolveKeyOrOperator(ctx, "OPENAI_API_KEY", "sk-operator"); err != nil || key != "sk-operator" || source != "operator" {
		t.Errorf("a miss: key=%q source=%q err=%v; want the operator key", key, source, err)
	}
}
