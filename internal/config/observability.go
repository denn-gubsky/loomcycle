package config

import (
	"fmt"
	"strings"
)

// ObservabilityConfig is the `observability:` block.
type ObservabilityConfig struct {
	Tracing TracingConfig `yaml:"tracing,omitempty"`
	// TenantTelemetry holds the operator's limits on tenant trace export. The
	// feature itself is switched on in the environment
	// (LOOMCYCLE_TENANT_TELEMETRY).
	TenantTelemetry TenantTelemetryConfig `yaml:"tenant_telemetry,omitempty"`
}

// TenantTelemetryConfig is `observability.tenant_telemetry`.
type TenantTelemetryConfig struct {
	// AllowedHosts, when set, are the only hosts a tenant's collector
	// endpoint may name: each entry a host name, matched exactly, or a
	// "*.suffix". Empty allows any host the dial-time guard lets through.
	AllowedHosts []string `yaml:"allowed_hosts,omitempty"`
}

// applyTenantTelemetry lower-cases and checks the allowed hosts. An entry the
// matcher could not mean stops the load: skipped, it would leave the list
// shorter than written, and an emptied list allows every host.
func applyTenantTelemetry(cfg *Config) error {
	hosts := cfg.Observability.TenantTelemetry.AllowedHosts
	for i, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || (strings.Contains(h, "*") && (!strings.HasPrefix(h, "*.") || strings.Count(h, "*") > 1 || len(h) < 3)) {
			return fmt.Errorf("observability.tenant_telemetry.allowed_hosts: %q: an entry is a host name or *.suffix", hosts[i])
		}
		hosts[i] = h
	}
	return nil
}

// TracingConfig is `observability.tracing`.
type TracingConfig struct {
	// TenantSamplerRatios samples a trace a tenant's work starts (a run, a
	// consolidation pass, an off-run tool call, a gateway model call) at a
	// ratio per tenant (0 to 1), in place of the OTEL_TRACES_SAMPLER ratio. A
	// span with a parent follows its trace's decision, and a tenant not
	// listed uses the global sampler. Read at startup; a reload does not
	// change the sampler.
	TenantSamplerRatios map[string]float64 `yaml:"tenant_sampler_ratios,omitempty"`
}
