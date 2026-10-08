package tools

import "context"

// MeteredOffRunCallValue is who a model call made OUTSIDE any run is charged
// to: the caller's tenant and subject, taken from its authenticated principal
// by the one server path that admits such calls.
type MeteredOffRunCallValue struct {
	TenantID string
	UserID   string
}

type ctxKeyMeteredOffRunCall struct{}

// WithMeteredOffRunCall marks ctx as a model call made outside a run that has
// been admitted and will be metered: its caller established, the operator-key
// restriction stamped from that caller, the budget checked.
//
// ⚠️ It has exactly ONE caller, the server's run-less decision path. A run's
// context is what normally holds a model call to its key restriction and its
// bill; this marker is the claim that the same was done by hand, so a second
// caller that sets it without doing all three opens the hole the marker exists
// to close. TestMeteredOffRunCall_HasOneStampingSite pins the count.
func WithMeteredOffRunCall(ctx context.Context, v MeteredOffRunCallValue) context.Context {
	return context.WithValue(ctx, ctxKeyMeteredOffRunCall{}, v)
}

// MeteredOffRunCall reports whether ctx was prepared by WithMeteredOffRunCall,
// and who the call is charged to.
func MeteredOffRunCall(ctx context.Context) (MeteredOffRunCallValue, bool) {
	v, ok := ctx.Value(ctxKeyMeteredOffRunCall{}).(MeteredOffRunCallValue)
	return v, ok
}
