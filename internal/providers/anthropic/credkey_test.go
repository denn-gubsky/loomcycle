package anthropic

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A run whose tenant/user stored ANTHROPIC_API_KEY authenticates with THAT key;
// a run without one uses the operator host key (RFC AR).
func TestCallKey_OverridesHostKey(t *testing.T) {
	d := &Driver{apiKey: "host-key"}

	if got := d.callKey(context.Background()); got != "host-key" {
		t.Errorf("no resolver: callKey = %q, want host-key", got)
	}

	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		if name == "ANTHROPIC_API_KEY" {
			return "tenant-key", true
		}
		return "", false
	})
	if got := d.callKey(ctx); got != "tenant-key" {
		t.Errorf("with override: callKey = %q, want tenant-key", got)
	}

	// A resolver that only knows a DIFFERENT provider's key leaves us on the host key.
	ctxOther := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		return "openai-key", name == "OPENAI_API_KEY"
	})
	if got := d.callKey(ctxOther); got != "host-key" {
		t.Errorf("unrelated override: callKey = %q, want host-key", got)
	}
}
