package providers

import (
	"context"
	"time"
)

// CallTiming is how long ONE model call took, carried on its Usage. All fields
// are milliseconds; 0 means "not measured", never "took no time" — a driver that
// cannot see a phase leaves it 0, and a consumer must not read 0 as fast.
//
// The loop measures the first two around every provider call; the phase fields
// come only from a driver that is told them (Ollama reports load, prompt-eval and
// eval durations on its done frame). Non-secret: durations and nothing else.
type CallTiming struct {
	// DurationMs is wall time from just before Provider.Call to the done event.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// TTFTMs is wall time to the first text, thinking or tool event — what a
	// caller waited before the model produced anything.
	TTFTMs int64 `json:"ttft_ms,omitempty"`

	// LoadMs, PrefillMs and DecodeMs are the server's own account of where the
	// time went: loading the model, evaluating the prompt, generating the reply.
	LoadMs    int64 `json:"load_ms,omitempty"`
	PrefillMs int64 `json:"prefill_ms,omitempty"`
	DecodeMs  int64 `json:"decode_ms,omitempty"`
	// QueueMs is wall time not spent on the call's own work: DurationMs minus
	// the load, prefill and decode phases when the driver reported them, else
	// minus the server's total — waiting behind other calls on a one-model box
	// (inside the server or before it), plus the network. Recorded so contention
	// is visible; a throughput estimate must not learn from it, because it
	// measures load on the box, not the model's speed.
	QueueMs int64 `json:"queue_ms,omitempty"`

	// ServerTotalMs is the server's total for the call (Ollama total_duration).
	// Only an input to QueueMs, used when no phase was reported; not part of the
	// wire shape.
	ServerTotalMs int64 `json:"-"`
}

// CallTimer measures one provider call from the caller's side. Start it just
// before Provider.Call, feed it every event read from the returned channel, and
// Stamp the usage once the stream ends.
//
// It stamps only a call that COMPLETED: a stream that errored or never sent its
// done event has no meaningful duration, and a timing taken from it would read
// as a fast call.
type CallTimer struct {
	now    func() time.Time
	start  time.Time
	first  time.Time
	done   time.Time
	failed bool
}

// StartCallTimer starts timing a call. now is the clock (nil = time.Now); tests
// pass a fake one so durations are exact.
func StartCallTimer(now func() time.Time) *CallTimer {
	if now == nil {
		now = time.Now
	}
	return &CallTimer{now: now, start: now()}
}

// Observe records what one event says about the call's timing.
func (t *CallTimer) Observe(ev Event) {
	if t == nil {
		return
	}
	switch ev.Type {
	case EventText, EventThinking, EventToolCall:
		if t.first.IsZero() {
			t.first = t.now()
		}
	case EventDone:
		if t.done.IsZero() {
			t.done = t.now()
		}
	case EventError:
		t.failed = true
	}
}

// Stamp writes the measured timing onto u, keeping any phase durations the
// driver already reported. A call that failed or never finished is left
// unstamped. Safe on a nil timer or nil usage.
func (t *CallTimer) Stamp(u *Usage) {
	if t == nil || u == nil || t.failed || t.done.IsZero() {
		return
	}
	var tm CallTiming
	if u.Timing != nil {
		tm = *u.Timing
	}
	tm.DurationMs = t.done.Sub(t.start).Milliseconds()
	if !t.first.IsZero() {
		tm.TTFTMs = t.first.Sub(t.start).Milliseconds()
	}
	// Queue time is wall time the server did not spend on THIS call's work.
	// Measure it against the phases when the driver reported them, not against
	// the server's total: Ollama's total_duration starts when the request
	// reaches the server, so a call that waited there for a runner slot behind
	// another call carries the whole wait inside its total — 150 s of it on a
	// one-model box serving two calls — and the wait would read as slowness.
	switch {
	case tm.PrefillMs > 0 || tm.DecodeMs > 0:
		if served := tm.LoadMs + tm.PrefillMs + tm.DecodeMs; tm.DurationMs > served {
			tm.QueueMs = tm.DurationMs - served
		}
	case tm.ServerTotalMs > 0 && tm.DurationMs > tm.ServerTotalMs:
		tm.QueueMs = tm.DurationMs - tm.ServerTotalMs
	}
	u.Timing = &tm
}

// CallObserver receives the usage of a model call that has no usage event of
// its own to ride — a summary, for instance — so its timing is still measured.
type CallObserver func(u *Usage)

type callObserverKey struct{}

// WithCallObserver returns ctx carrying obs. A nil obs returns ctx unchanged.
func WithCallObserver(ctx context.Context, obs CallObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, callObserverKey{}, obs)
}

// ObserveCall hands u to the observer on ctx, if there is one.
func ObserveCall(ctx context.Context, u *Usage) {
	if u == nil {
		return
	}
	if obs, ok := ctx.Value(callObserverKey{}).(CallObserver); ok {
		obs(u)
	}
}
