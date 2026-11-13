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

// observability.tenant_telemetry.allowed_hosts is a list of host names and
// *.suffix entries, kept lower-case; any other entry stops the load.
func TestTenantTelemetryAllowedHosts_YAMLList(t *testing.T) {
	cfg, err := loadYAMLString(t, "agents: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Observability.TenantTelemetry.AllowedHosts; len(got) != 0 {
		t.Errorf("no block: allowed hosts = %v, want none (any host)", got)
	}
	cfg, err = loadYAMLString(t, "observability:\n  tenant_telemetry:\n    allowed_hosts: [\"Grafana.NET\", \" *.otel.example \"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Observability.TenantTelemetry.AllowedHosts; len(got) != 2 || got[0] != "grafana.net" || got[1] != "*.otel.example" {
		t.Errorf("allowed hosts = %q, want [grafana.net *.otel.example]", got)
	}
	for _, bad := range []string{"a*.b", "*", "*.a.*", ""} {
		_, err := loadYAMLString(t, "observability:\n  tenant_telemetry:\n    allowed_hosts: [\""+bad+"\"]\n")
		if err == nil || !strings.Contains(err.Error(), "observability.tenant_telemetry.allowed_hosts") {
			t.Errorf("allowed host %q: err = %v, want a refusal naming the key", bad, err)
		}
	}
}

// The per-tenant sampler ratios come from the yaml only; the variable that
// overrode them tenant by tenant is not read.
func TestTenantSamplerRatios_TheYAMLIsTheOnlySource(t *testing.T) {
	t.Setenv("LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", "acme=1")
	cfg, err := loadYAMLString(t, "observability:\n  tracing:\n    tenant_sampler_ratios:\n      acme: 1\n")
	if err != nil {
		t.Fatal(err)
	}
	requireRetiredAdvisory(t, cfg, "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS")
	if _, err := loadYAMLString(t, "agents: {}\n"); err == nil || !strings.Contains(err.Error(), "observability.tracing.tenant_sampler_ratios") {
		t.Errorf("a retired ratio the yaml does not carry: err = %v, want a refusal naming the yaml key", err)
	}
}
