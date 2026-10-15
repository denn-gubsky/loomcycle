// services.go — the lanes of the model calls that are not Provider.Call.
//
// A decision model and an embedder are reached over protocols of their own, so
// the provider wrapper never sees them. Left alone they are exactly the calls a
// provider's caps do not count: a host that serves one request at a time gets an
// embedding and a decision beside its chat call. Each has a wrapper here that
// takes its kind's lane on the provider it is served by, through the same
// acquire as a chat call.
package lanegate

import (
	"context"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// decisionDriver is a decision.Driver whose calls take the decision lane.
type decisionDriver struct {
	inner decision.Driver
	id    string
	sched *concurrency.CallScheduler
	// floor bounds the driver's calls at FloorCap while no decision lane is
	// configured for its provider.
	floor *concurrency.Lane
}

// WrapDecision returns inner with every Decide inside the decision lane of the
// provider id it is served by. The lane is looked up per call, so a cap a
// config reload adds or removes applies to the next call. While the provider
// has no decision lane, the driver's calls are bounded at FloorCap in flight —
// a nil sched included.
func WrapDecision(id string, inner decision.Driver, sched *concurrency.CallScheduler) decision.Driver {
	return &decisionDriver{inner: inner, id: id, sched: sched, floor: newFloor()}
}

func (d *decisionDriver) Limits(model string) decision.Limits { return d.inner.Limits(model) }

// Decide waits for a slot, asks, and gives the slot back when the driver
// returns.
//
// The wait is OUTSIDE the driver's own timeout: that bounds how long the model
// may take to answer, and a decision queued behind other work has not been
// asked yet. Only ctx ends the wait. A decision a tool asks for a run reports
// its wait on the run's stream (runReporter).
func (d *decisionDriver) Decide(ctx context.Context, req decision.Request) (*decision.Response, error) {
	// A request the driver would refuse unasked must not queue for a slot first.
	if err := decision.Validate(req, d.inner.Limits(req.Model)); err != nil {
		return nil, err
	}
	release, err := acquire(ctx, d.sched, d.floor, d.id, config.ModelKindDecision, req.Model, runReporter(ctx), nil)
	if err != nil {
		code := decision.CodeCallFailed
		if errors.Is(err, context.DeadlineExceeded) {
			code = decision.CodeTimeout
		}
		return nil, &decision.Error{Code: code, Message: "waiting for a decision slot on provider " + d.id, Err: err}
	}
	defer release()
	return d.inner.Decide(ctx, req)
}

// Embedder is a providers.Embedder whose Embed calls take the embedder lane of
// the provider it is served by. Build one with WrapEmbedder.
type Embedder struct {
	inner providers.Embedder
	id    string
	sched *concurrency.CallScheduler
}

var _ providers.Embedder = (*Embedder)(nil)

// WrapEmbedder returns inner with every Embed inside the embedder lane of
// provider id. A nil inner returns nil, so a caller's "no embedder configured"
// check (`if embedder == nil`) still fires; a nil sched gates nothing.
//
// The health observer (providers.ObserveEmbedder) goes AROUND this wrapper, not
// inside it: the observer is what a reader asserts on, and it must stay the
// outermost value.
func WrapEmbedder(id string, inner providers.Embedder, sched *concurrency.CallScheduler) providers.Embedder {
	if inner == nil {
		return nil
	}
	return &Embedder{inner: inner, id: id, sched: sched}
}

// Unwrap returns the wrapped embedder.
func (e *Embedder) Unwrap() providers.Embedder { return e.inner }

// HoldEmbedder is HoldLane for the embedder e: it takes e's embedder-lane slot
// for ONE Embed the caller is about to make and returns the ctx to make it
// under. It finds the lane on e itself (through whatever wraps it, such as the
// health observer), so the slot held is exactly the one that Embed would have
// taken. An embedder that is not gated returns ctx as is and a release that
// does nothing.
func HoldEmbedder(ctx context.Context, e providers.Embedder) (context.Context, func(), error) {
	for cur := e; cur != nil; {
		if le, ok := cur.(*Embedder); ok {
			return HoldLane(ctx, le.sched, le.id, config.ModelKindEmbedder, le.inner.Model())
		}
		u, ok := cur.(interface{ Unwrap() providers.Embedder })
		if !ok {
			break
		}
		cur = u.Unwrap()
	}
	return ctx, func() {}, nil
}

func (e *Embedder) Model() string    { return e.inner.Model() }
func (e *Embedder) Provider() string { return e.inner.Provider() }
func (e *Embedder) Dimension() int   { return e.inner.Dimension() }

// Embed holds one slot for the whole call, however many requests the driver
// batches texts into: the host is busy with it until it returns. An embedding
// made for a run reports its wait on the run's stream (runReporter).
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if !e.sched.Gated(e.id, config.ModelKindEmbedder) {
		return e.inner.Embed(ctx, texts)
	}
	release, err := acquire(ctx, e.sched, nil, e.id, config.ModelKindEmbedder, e.inner.Model(), runReporter(ctx), nil)
	if err != nil {
		return nil, err
	}
	defer release()
	return e.inner.Embed(ctx, texts)
}
