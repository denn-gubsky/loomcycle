package otel

import (
	"encoding/binary"
	"fmt"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Sample at the maximum, thin per destination.
//
// One head decision is made per trace, and two destinations may want it at
// different ratios: the operator at, say, 0.1 and a tenant debugging at 1.
// While tenant export is on, a root span of a tenant with a destination is
// sampled when the operator's sampler keeps it OR its trace id falls under the
// tenant's ratio (and so, below a span the operator keeps, is a child span the
// operator's sampler alone would drop, which only a sampler that is not
// parent-based does). A span kept only for the tenant is marked in its trace
// state (tenantOnlyKey), which every span below it inherits; the operator's
// pipeline drops a marked span before counting it, so the operator exports
// exactly the spans its own sampler keeps. The tenant's pipeline keeps a span
// only when its trace id falls under the tenant's ratio (traceIDKeeps).
//
// Both tests are functions of the trace alone, never of the span, so each
// destination receives whole traces. The marker is used in place of a second
// ratio test on the operator's side because the operator's decision is not
// always a ratio of the trace id (always_on, always_off, a caller's sampled
// traceparent): the marker records exactly "the operator's sampler would have
// dropped this span and everything below it", whatever the sampler.

// tenantOnlyKey is the trace-state key marking a trace sampled only because a
// tenant's export ratio asked for it. A caller's trace state never brings it
// in: it is removed from every span with a remote parent.
const tenantOnlyKey = "loomcycle-tenant-only"

// traceIDKeeps reports whether a trace id falls under ratio, by the test
// sdktrace.TraceIDRatioBased makes: the trace id's low 8 bytes, shifted right
// by one, below ratio × 2^63. A ratio of 1 or more keeps every trace; 0 or
// less (or NaN) none.
func traceIDKeeps(id trace.TraceID, ratio float64) bool {
	switch {
	case ratio >= 1:
		return true
	case !(ratio > 0):
		return false
	}
	return binary.BigEndian.Uint64(id[8:16])>>1 < uint64(ratio*(1<<63))
}

// tenantOnly reports whether sc's trace was sampled only for a tenant's
// export. Meaningful only while tenant routing is on: with it off nothing
// removes a caller's copy of the key, so nothing may act on it.
func tenantOnly(sc trace.SpanContext) bool {
	return tenantRouting.Load() && sc.TraceState().Get(tenantOnlyKey) != ""
}

// tenantExportSampler is the operator's sampler with the tenants' export
// ratios laid over its root decisions.
type tenantExportSampler struct {
	// op is the operator's sampler (NewTenantSampler over the base): what it
	// decides is what the operator exports.
	op sdktrace.Sampler
	r  *TenantRouter
}

// newTenantExportSampler wraps op, the operator's sampler, with r's tenants'
// export ratios.
func newTenantExportSampler(op sdktrace.Sampler, r *TenantRouter) sdktrace.Sampler {
	return &tenantExportSampler{op: op, r: r}
}

func (s *tenantExportSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	psc := trace.SpanContextFromContext(p.ParentContext)
	if psc.IsValid() && !psc.IsRemote() && psc.TraceState().Get(tenantOnlyKey) != "" {
		// The rest of a trace kept for its tenant: kept whole whatever the
		// operator's sampler would decide for a child span, since the
		// operator exports none of it.
		return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample, Tracestate: psc.TraceState()}
	}
	res := s.op.ShouldSample(p)
	if psc.IsRemote() {
		// A caller cannot hide its trace from the operator by sending the
		// marker.
		res.Tracestate = res.Tracestate.Delete(tenantOnlyKey)
	}
	if res.Decision == sdktrace.RecordAndSample {
		return res
	}
	// Raised: a span that starts a trace, or one under a span the operator
	// keeps (a sampler that is not parent-based decides each span on its own,
	// and drops every span below one it drops, its trace id being the same).
	// Never one under a caller's span or an unsampled one: the trace above
	// it is missing, and the operator's sampler carries the caller's decision.
	if psc.IsValid() && (psc.IsRemote() || !psc.IsSampled()) {
		return res
	}
	tenant, noTrace := startRoute(p.Attributes)
	// A no-trace run is never routed to its tenant: raising it would record
	// spans nobody receives.
	if tenant == "" || noTrace {
		return res
	}
	ratio, ok := s.r.exportRatio(tenant)
	if !ok || !traceIDKeeps(p.TraceID, ratio) {
		return res
	}
	ts, err := res.Tracestate.Insert(tenantOnlyKey, "1")
	if err != nil {
		// A full trace state: the operator's decision stands.
		return res
	}
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample, Attributes: res.Attributes, Tracestate: ts}
}

func (s *tenantExportSampler) Description() string {
	return fmt.Sprintf("LoomcycleTenantExportSampler{%s}", s.op.Description())
}
