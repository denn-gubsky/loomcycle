package gemini

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func TestCallKey_OverridesHostKey(t *testing.T) {
	d := &Driver{apiKey: "host-key"}
	if got := d.callKey(context.Background()); got != "host-key" {
		t.Errorf("no resolver: callKey = %q, want host-key", got)
	}
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		return "tenant-gemini", name == "GEMINI_API_KEY"
	})
	if got := d.callKey(ctx); got != "tenant-gemini" {
		t.Errorf("with override: callKey = %q, want tenant-gemini", got)
	}
}
