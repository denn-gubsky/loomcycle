package otel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

// AttrTenant is the tenant whose work a span records. Every span the recorder
// helpers open carries it, not just the run span: a child span does not
// inherit its parent's attributes, and a backend query, a per-tenant sampler or
// a per-tenant export route sees one span at a time. Absent on substrate work
// that belongs to no tenant.
const AttrTenant = "loomcycle.tenant"

// tenantKey carries the tenant on ctx for the helpers below. It is this
// package's own key, not the run identity's: internal/otel is a leaf module
// that imports nothing from the root module (and internal/providers, whose
// drivers open spans, cannot import the root either), so the run identity is
// out of reach here. RecordRunStart sets it, which every run passes through, so
// the spans of a run's iterations, model calls, tools and memory work inherit
// it through ctx with no call site of their own.
type tenantKey struct{}

// WithTenant returns ctx naming tenant as the owner of the spans opened under
// it. An empty tenant leaves ctx unchanged, so a caller that does not know the
// tenant never clears one an outer run set.
func WithTenant(ctx context.Context, tenant string) context.Context {
	if tenant == "" {
		return ctx
	}
	return context.WithValue(ctx, tenantKey{}, tenant)
}

// TenantFromContext returns the tenant WithTenant (or RecordRunStart) put on
// ctx, "" when none.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantKey{}).(string)
	return v
}

// withTenantAttr prepends ctx's tenant to kvs, so it is a START attribute of
// the span about to open (a sampler sees start attributes only).
func withTenantAttr(ctx context.Context, kvs []attribute.KeyValue) []attribute.KeyValue {
	if t := TenantFromContext(ctx); t != "" {
		return append([]attribute.KeyValue{attribute.String(AttrTenant, t)}, kvs...)
	}
	return kvs
}
