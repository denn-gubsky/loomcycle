package recall

import "context"

type ctxKey struct{}

// NewContext stamps the run-scoped index on ctx so the loop's harvest hook and
// the Recall tool both reach it. A nil index is a no-op (recall disabled) — the
// Recall tool then finds no run-index and falls back to durable memory only.
func NewContext(ctx context.Context, ix *Index) context.Context {
	if ix == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, ix)
}

// FromContext returns the run-scoped index, or nil when recall is not enabled for
// this run.
func FromContext(ctx context.Context) *Index {
	ix, _ := ctx.Value(ctxKey{}).(*Index)
	return ix
}
