package otel

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// tenantNameRe is the tenant id charset (internal/dynvol.ValidTenant, which
// this leaf module cannot import). A ratio keyed by anything else could never
// match a run, so it is refused instead of silently ignored.
var tenantNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ValidateTenantSamplerRatios checks a tenant → ratio map: each tenant a valid
// tenant id, each ratio from 0 to 1. what names the source in the error.
func ValidateTenantSamplerRatios(what string, ratios map[string]float64) error {
	for _, t := range slices.Sorted(maps.Keys(ratios)) {
		if !tenantNameRe.MatchString(t) {
			return fmt.Errorf("%s: tenant %q is not a tenant id (^[a-zA-Z0-9_-]{1,64}$)", what, t)
		}
		if r := ratios[t]; !(r >= 0 && r <= 1) {
			return fmt.Errorf("%s: tenant %q ratio %v: want a number from 0 to 1", what, t, r)
		}
	}
	return nil
}

// NewTenantSampler wraps base with per-tenant ratios for root spans. A span
// with no valid parent that carries loomcycle.tenant as a start attribute (a
// run span, a consolidation pass, an off-run tool call, a gateway model call)
// is sampled at its tenant's ratio; every other span — one
// with a parent, local or remote, or of a tenant with no ratio — is decided
// by base, so a parent's decision still carries down a trace exactly as base
// would carry it. No ratios returns base itself.
func NewTenantSampler(base sdktrace.Sampler, ratios map[string]float64) sdktrace.Sampler {
	if len(ratios) == 0 {
		return base
	}
	ts := &tenantSampler{base: base, byTenant: make(map[string]sdktrace.Sampler, len(ratios))}
	for t, r := range ratios {
		ts.byTenant[t] = sdktrace.TraceIDRatioBased(r)
	}
	return ts
}

type tenantSampler struct {
	base     sdktrace.Sampler
	byTenant map[string]sdktrace.Sampler
}

func (s *tenantSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if trace.SpanContextFromContext(p.ParentContext).IsValid() {
		return s.base.ShouldSample(p)
	}
	for _, kv := range p.Attributes {
		if string(kv.Key) != AttrTenant {
			continue
		}
		if ts, ok := s.byTenant[kv.Value.AsString()]; ok {
			return ts.ShouldSample(p)
		}
		break
	}
	return s.base.ShouldSample(p)
}

func (s *tenantSampler) Description() string {
	return fmt.Sprintf("LoomcycleTenantSampler{tenants:%d,base:%s}", len(s.byTenant), s.base.Description())
}
