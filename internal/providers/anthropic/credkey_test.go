package anthropic

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A run whose tenant/user stored ANTHROPIC_API_KEY authenticates with THAT key
// (and the resolved scope is reported for usage attribution); a run without one
// uses the operator host key with source "operator" (RFC AR / RFC AV).
func TestResolveKey_OverridesHostKey(t *testing.T) {
	d := &Driver{apiKey: "host-key"}

	if key, source, _, err := d.resolveKey(context.Background()); err != nil || key != "host-key" || source != "operator" {
		t.Errorf("no resolver: (%q, %q, %v), want (host-key, operator, nil)", key, source, err)
	}

	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		if name == "ANTHROPIC_API_KEY" {
			return providers.CredentialResolution{Value: "tenant-key", Scope: "tenant"}, true
		}
		return providers.CredentialResolution{}, false
	})
	if key, source, _, err := d.resolveKey(ctx); err != nil || key != "tenant-key" || source != "tenant" {
		t.Errorf("with override: (%q, %q, %v), want (tenant-key, tenant, nil)", key, source, err)
	}

	// A resolver that only knows a DIFFERENT provider's key leaves us on the host key.
	ctxOther := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "openai-key"}, name == "OPENAI_API_KEY"
	})
	if key, source, _, err := d.resolveKey(ctxOther); err != nil || key != "host-key" || source != "operator" {
		t.Errorf("unrelated override: (%q, %q, %v), want (host-key, operator, nil)", key, source, err)
	}
}

// RFC AX: a RESTRICTED run (operator-key not allowed) with no own override must
// refuse with ErrOperatorKeyForbidden — never leak the operator host key. An
// override still wins regardless of the restriction; an allowed run uses the
// host key as before.
func TestResolveKey_RestrictedRefusesHostKey(t *testing.T) {
	d := &Driver{apiKey: "host-key"}

	restricted := providers.WithOperatorKeyAllowed(context.Background(), false)
	if key, _, _, err := d.resolveKey(restricted); !errors.Is(err, providers.ErrOperatorKeyForbidden) || key != "" {
		t.Errorf("restricted, no override: (%q, %v), want (\"\", ErrOperatorKeyForbidden)", key, err)
	}

	// Override present → used regardless of the restriction (the tenant pays).
	withOverride := providers.WithCredentialResolver(restricted, func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "tenant-key", Scope: "tenant"}, name == "ANTHROPIC_API_KEY"
	})
	if key, source, _, err := d.resolveKey(withOverride); err != nil || key != "tenant-key" || source != "tenant" {
		t.Errorf("restricted, with override: (%q, %q, %v), want (tenant-key, tenant, nil)", key, source, err)
	}

	// Allowed run (default) uses the host key.
	allowed := providers.WithOperatorKeyAllowed(context.Background(), true)
	if key, source, _, err := d.resolveKey(allowed); err != nil || key != "host-key" || source != "operator" {
		t.Errorf("allowed, no override: (%q, %q, %v), want (host-key, operator, nil)", key, source, err)
	}
}
