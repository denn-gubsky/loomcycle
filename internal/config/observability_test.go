package config

import (
	"strings"
	"testing"
)

func TestObservability_TenantSamplerRatiosLoadAndValidate(t *testing.T) {
	cfg, err := loadYAMLString(t, "observability:\n  tracing:\n    tenant_sampler_ratios:\n      acme: 1\n      beta: 0.1\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Observability.Tracing.TenantSamplerRatios; got["acme"] != 1 || got["beta"] != 0.1 {
		t.Fatalf("ratios = %v", got)
	}
	for body, want := range map[string]string{
		"observability:\n  tracing:\n    tenant_sampler_ratios:\n      acme: 2\n":    "from 0 to 1",
		"observability:\n  tracing:\n    tenant_sampler_ratios:\n      \"a/b\": 1\n": "not a tenant id",
	} {
		if _, err := loadYAMLString(t, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("load(%q) = %v, want an error containing %q", body, err, want)
		}
	}
}
