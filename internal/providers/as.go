package providers

// A registry provider is not the driver itself: it is the driver inside a
// wrapper that takes a call lane around every Call (internal/lanegate). A
// wrapper hides whatever the driver implements beyond Provider, so nothing may
// type-assert a Provider value directly — As looks through the wrappers.
// TestOptionalInterfaces_ReachedOnlyThroughAs holds the tree to that.

// Unwrapper is implemented by a Provider that wraps another one.
type Unwrapper interface {
	Unwrap() Provider
}

// As returns the first value in p's wrapper chain — p itself, then whatever it
// unwraps to — that is a T. T is an optional interface (KeyedProvider,
// ThinkingDowngrader, …) or a concrete driver type.
func As[T any](p Provider) (T, bool) {
	for p != nil {
		if t, ok := p.(T); ok {
			return t, true
		}
		u, ok := p.(Unwrapper)
		if !ok {
			break
		}
		p = u.Unwrap()
	}
	var zero T
	return zero, false
}

// ModelIdentifier is implemented by a driver for which the model a host has
// loaded is more than the request's model name. ModelIdentity returns that
// identity for req; the call scheduler counts distinct identities against a
// provider's max_models_in_flight. Every other driver's identity is req.Model.
type ModelIdentifier interface {
	ModelIdentity(req Request) string
}

// ModelIdentity returns the model identity of req on p (see ModelIdentifier).
func ModelIdentity(p Provider, req Request) string {
	if mi, ok := As[ModelIdentifier](p); ok {
		return mi.ModelIdentity(req)
	}
	return req.Model
}

// RunEnder is implemented by a Provider that keeps state per run. The loop
// calls RunDone once, when the run whose calls carried runID (RunMeta.RunID)
// has ended.
type RunEnder interface {
	RunDone(runID string)
}

// RunDone tells p that the run runID has ended. A no-op for a provider that
// keeps nothing per run, and for an empty runID.
func RunDone(p Provider, runID string) {
	if runID == "" {
		return
	}
	if e, ok := As[RunEnder](p); ok {
		e.RunDone(runID)
	}
}
