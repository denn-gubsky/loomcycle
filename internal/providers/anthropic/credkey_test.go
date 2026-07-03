package anthropic

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A run whose tenant/user stored ANTHROPIC_API_KEY authenticates with THAT key
// (and the resolved scope is reported for usage attribution); a run without one
// uses the operator host key with source "operator" (RFC AR / RFC AV).
func TestResolveKey_OverridesHostKey(t *testing.T) {
	d := &Driver{apiKey: "host-key"}

	if key, source, _ := d.resolveKey(context.Background()); key != "host-key" || source != "operator" {
		t.Errorf("no resolver: (%q, %q), want (host-key, operator)", key, source)
	}

	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		if name == "ANTHROPIC_API_KEY" {
			return providers.CredentialResolution{Value: "tenant-key", Scope: "tenant"}, true
		}
		return providers.CredentialResolution{}, false
	})
	if key, source, _ := d.resolveKey(ctx); key != "tenant-key" || source != "tenant" {
		t.Errorf("with override: (%q, %q), want (tenant-key, tenant)", key, source)
	}

	// A resolver that only knows a DIFFERENT provider's key leaves us on the host key.
	ctxOther := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "openai-key"}, name == "OPENAI_API_KEY"
	})
	if key, source, _ := d.resolveKey(ctxOther); key != "host-key" || source != "operator" {
		t.Errorf("unrelated override: (%q, %q), want (host-key, operator)", key, source)
	}
}
