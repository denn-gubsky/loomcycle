package providers

import "context"

// eventSinkKey carries the run's event sink on a ctx.
type eventSinkKey struct{}

// WithEventSink attaches the run's event sink to ctx: the function that
// records an event on the run's transcript and stream. Set where a run hands
// its emitter to its tools (tools.WithEventEmitter), so every ctx a run's tools
// work under carries it.
//
// It exists for model calls made on a run's behalf off the loop's own call
// path — a decision or an embedding a tool makes, a summary — which have no
// Request.OnEvent: the call lanes report a wait on them through it. fn must be
// safe to call from any goroutine, as a run's recording emit is.
func WithEventSink(ctx context.Context, fn func(Event)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, eventSinkKey{}, fn)
}

// EventSink returns the run's event sink on ctx, nil when ctx belongs to no
// run.
func EventSink(ctx context.Context) func(Event) {
	fn, _ := ctx.Value(eventSinkKey{}).(func(Event))
	return fn
}
