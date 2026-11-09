package otel

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

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

// ParseTenantSamplerRatios parses LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS:
// `tenant=ratio` pairs separated by commas, e.g. `acme=1,beta=0.1`.
func ParseTenantSamplerRatios(v string) (map[string]float64, error) {
	const name = "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS"
	out := map[string]float64{}
	for _, pair := range strings.Split(v, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		t, r, ok := strings.Cut(pair, "=")
		t = strings.TrimSpace(t)
		f, err := strconv.ParseFloat(strings.TrimSpace(r), 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("%s: %q: want tenant=ratio", name, pair)
		}
		if _, dup := out[t]; dup {
			return nil, fmt.Errorf("%s: tenant %q is listed twice", name, t)
		}
		out[t] = f
	}
	if err := ValidateTenantSamplerRatios(name, out); err != nil {
		return nil, err
	}
	return out, nil
}

// MergeTenantSamplerRatios returns yaml's ratios with env's laid over them,
// tenant by tenant: the environment wins for a tenant both name, as it does
// for every other setting it overrides.
func MergeTenantSamplerRatios(yaml, env map[string]float64) map[string]float64 {
	if len(yaml)+len(env) == 0 {
		return nil
	}
	out := maps.Clone(yaml)
	if out == nil {
		out = map[string]float64{}
	}
	maps.Copy(out, env)
	return out
}

// NewTenantSampler wraps base with per-tenant ratios for root spans. A span
// with no valid parent that carries loomcycle.tenant as a start attribute
// (the run span) is sampled at its tenant's ratio; every other span — one
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
