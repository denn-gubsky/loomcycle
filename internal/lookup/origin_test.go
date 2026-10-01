package lookup_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The Origin of a resolved remote def gates whether its own host is trusted on
// a private network, so every case below pins where that trust comes from:
// the operator's static config (directly, or a bootstrapped copy that still
// matches it), and never a row a runtime author wrote. Each case also pins the
// tenant whose layer the def was read in: a credential the def names by
// reference resolves there, so a shared def must report "" whatever the
// calling tenant.

const (
	originStaticURL = "http://peer.internal:8787"
	originOtherURL  = "http://169.254.169.254"
)

func TestMemoryBackend_OriginIsOperatorOnlyForWhatTheOperatorDeclared(t *testing.T) {
	static := &config.Config{MemoryBackends: map[string]config.MemoryBackend{
		"peer": {Kind: "remote", Config: config.MemoryBackendConfig{BaseURL: originStaticURL}},
	}}
	row := func(bootstrapped bool, baseURL string) store.MemoryBackendDefRow {
		return store.MemoryBackendDefRow{DefID: "mb_1", Name: "peer", TenantID: "acme", BootstrappedFromStatic: bootstrapped,
			Definition: json.RawMessage(`{"kind":"remote","config":{"base_url":"` + baseURL + `"}}`)}
	}
	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		rows   map[string]store.MemoryBackendDefRow
		tenant string
		want   lookup.Origin
		owner  string
	}{
		{"static yaml def", static, nil, "", lookup.OriginOperator, ""},
		{"static yaml def resolved by a tenant run", static, nil, "acme", lookup.OriginOperator, ""},
		{"runtime-authored shared row", &config.Config{}, map[string]store.MemoryBackendDefRow{"peer": row(false, originOtherURL)}, "", lookup.OriginRuntime, ""},
		{"tenant fork shadowing the static def", static, map[string]store.MemoryBackendDefRow{"peer": row(false, originStaticURL)}, "acme", lookup.OriginRuntime, "acme"},
		{"bootstrapped row matching the static def", static, map[string]store.MemoryBackendDefRow{"peer": row(true, originStaticURL)}, "acme", lookup.OriginOperator, "acme"},
		{"bootstrapped row the operator no longer declares", &config.Config{}, map[string]store.MemoryBackendDefRow{"peer": row(true, originStaticURL)}, "acme", lookup.OriginRuntime, "acme"},
		{"bootstrapped row diverged from the static def", static, map[string]store.MemoryBackendDefRow{"peer": row(true, originOtherURL)}, "acme", lookup.OriginRuntime, "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, ok := lookup.MemoryBackend(context.Background(), &stubMemoryBackendStore{defs: tc.rows}, tc.cfg, tc.tenant, "peer")
			if !ok {
				t.Fatal("resolver returned !ok")
			}
			if got.Origin != tc.want {
				t.Errorf("origin = %v, want %v", got.Origin, tc.want)
			}
			if got.TenantID != tc.owner {
				t.Errorf("owning tenant = %q, want %q", got.TenantID, tc.owner)
			}
		})
	}
}

func TestDocumentSource_OriginIsOperatorOnlyForWhatTheOperatorDeclared(t *testing.T) {
	static := &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peer": {Config: config.DocumentSourceConfig{BaseURL: originStaticURL}},
	}}
	row := func(bootstrapped bool, baseURL string) store.DocumentSourceDefRow {
		return store.DocumentSourceDefRow{DefID: "ds_1", Name: "peer", TenantID: "acme", BootstrappedFromStatic: bootstrapped,
			Definition: json.RawMessage(`{"config":{"base_url":"` + baseURL + `"}}`)}
	}
	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		rows   map[string]store.DocumentSourceDefRow
		tenant string
		want   lookup.Origin
		owner  string
	}{
		{"static yaml def", static, nil, "", lookup.OriginOperator, ""},
		{"static yaml def resolved by a tenant run", static, nil, "acme", lookup.OriginOperator, ""},
		{"runtime-authored shared row", &config.Config{}, map[string]store.DocumentSourceDefRow{"peer": row(false, originOtherURL)}, "", lookup.OriginRuntime, ""},
		{"tenant fork shadowing the static def", static, map[string]store.DocumentSourceDefRow{"peer": row(false, originStaticURL)}, "acme", lookup.OriginRuntime, "acme"},
		{"bootstrapped row matching the static def", static, map[string]store.DocumentSourceDefRow{"peer": row(true, originStaticURL)}, "acme", lookup.OriginOperator, "acme"},
		{"bootstrapped row the operator no longer declares", &config.Config{}, map[string]store.DocumentSourceDefRow{"peer": row(true, originStaticURL)}, "acme", lookup.OriginRuntime, "acme"},
		{"bootstrapped row diverged from the static def", static, map[string]store.DocumentSourceDefRow{"peer": row(true, originOtherURL)}, "acme", lookup.OriginRuntime, "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, ok := lookup.DocumentSource(context.Background(), &stubDocumentSourceStore{defs: tc.rows}, tc.cfg, tc.tenant, "peer")
			if !ok {
				t.Fatal("resolver returned !ok")
			}
			if got.Origin != tc.want {
				t.Errorf("origin = %v, want %v", got.Origin, tc.want)
			}
			if got.TenantID != tc.owner {
				t.Errorf("owning tenant = %q, want %q", got.TenantID, tc.owner)
			}
		})
	}
}
