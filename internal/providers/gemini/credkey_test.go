package gemini

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func TestCallKey_OverridesHostKey(t *testing.T) {
	d := &Driver{apiKey: "host-key"}
	if got, _, _ := d.resolveKey(context.Background()); got != "host-key" {
		t.Errorf("no resolver: resolveKey = %q, want host-key", got)
	}
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "tenant-gemini"}, name == "GEMINI_API_KEY"
	})
	if got, _, _ := d.resolveKey(ctx); got != "tenant-gemini" {
		t.Errorf("with override: resolveKey = %q, want tenant-gemini", got)
	}
}
