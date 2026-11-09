package config

// ObservabilityConfig is the `observability:` block.
type ObservabilityConfig struct {
	Tracing TracingConfig `yaml:"tracing,omitempty"`
}

// TracingConfig is `observability.tracing`.
type TracingConfig struct {
	// TenantSamplerRatios samples the runs that start a trace at a ratio per
	// tenant (0 to 1), in place of the OTEL_TRACES_SAMPLER ratio. A run that
	// joins a caller's trace follows the caller's decision, and a tenant not
	// listed uses the global sampler. LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS
	// overrides it tenant by tenant. Read at startup; a reload does not
	// change the sampler.
	TenantSamplerRatios map[string]float64 `yaml:"tenant_sampler_ratios,omitempty"`
}
