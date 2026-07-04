package gemini

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func TestCallKey_OverridesHostKey(t *testing.T) {
	d := &Driver{apiKey: "host-key"}
	if got, _, _, err := d.resolveKey(context.Background()); err != nil || got != "host-key" {
		t.Errorf("no resolver: resolveKey = (%q, %v), want (host-key, nil)", got, err)
	}
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		return providers.CredentialResolution{Value: "tenant-gemini"}, name == "GEMINI_API_KEY"
	})
	if got, _, _, err := d.resolveKey(ctx); err != nil || got != "tenant-gemini" {
		t.Errorf("with override: resolveKey = (%q, %v), want (tenant-gemini, nil)", got, err)
	}
}

// RFC AX: a restricted run with no override refuses with ErrOperatorKeyForbidden.
func TestResolveKey_RestrictedRefusesHostKey(t *testing.T) {
	d := &Driver{apiKey: "host-key"}
	restricted := providers.WithOperatorKeyAllowed(context.Background(), false)
	if got, _, _, err := d.resolveKey(restricted); !errors.Is(err, providers.ErrOperatorKeyForbidden) || got != "" {
		t.Errorf("restricted, no override: (%q, %v), want (\"\", ErrOperatorKeyForbidden)", got, err)
	}
}
