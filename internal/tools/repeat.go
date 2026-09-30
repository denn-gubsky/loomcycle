package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
)

// A run that sends the SAME failed call again and again. Measured live
// (ornith-1.5 behind chat/local): a tenant write refused as not granted was
// re-sent, unchanged, until the run's 10-minute deadline — every repeat a slow
// local model call that could only get the same refusal back.
//
// The dispatcher counts failures per exact call (tool name + canonical
// arguments). Up to repeatFailuresAllowed they run normally: a transient
// failure may well succeed the second time. After that the call is not run;
// the model is told the call cannot succeed as sent. And once it has been
// refused that way repeatStopAfter times, the run is flagged to stop, and the
// loops end it rather than let it spin to its deadline.
const (
	repeatFailuresAllowed = 2
	repeatStopAfter       = 3
)

type repeatTracker struct {
	mu       sync.Mutex
	failures map[string]int
	refused  int
	stopped  string

	// last is the most recent call that ran, lastResult a hash of what it
	// returned, and streak how many calls in a row have been that call with
	// that result, for refuseConsecutive. refusedInRow is how many times it
	// has been refused since, for the refusal's count.
	last         string
	lastResult   [sha256.Size]byte
	streak       int
	refusedInRow int

	// batch is the batch (WithToolBatch) the latest call in the streak ran in,
	// and inBatch how many of the streak ran in it. batches mints batch ids.
	batch   uint64
	inBatch int
	batches uint64
}

// consecutiveCallsAllowed is how many times in a row a run may make the exact
// same call and get the exact same result back. The next such call is refused
// unrun.
//
// A model looping on one call: measured live (ornith-1.5 behind chat/local),
// the same successful tenant save was re-sent 36, 17 and 12 times in a row,
// and one help article re-read 7 times. The failed-call guard never saw it,
// because every call succeeded. This is deliberately narrow: only the SAME
// tool with the SAME arguments, back to back, returning the SAME result. Any
// other call in between, any change to the arguments, or any change in what
// the call returns starts the count over. The last one matters because some
// calls are meant to be repeated as they are: a poll of a sub-agent, a
// subscribe that drains a queue batch by batch, a clock read. Each returns
// something new, and refusing them would end a run that is doing what the
// tool's help tells it to.
const consecutiveCallsAllowed = 2

// refuseConsecutive returns the refusal for a call that has already been made
// consecutiveCallsAllowed times in a row with the same result, or false to run
// it. A refused call leaves the streak as it is, so a model that keeps sending
// it keeps being refused.
//
// Only results the model had seen when it sent the call count. Calls it sent
// together in one turn run concurrently and none of them has seen another's
// result, so three identical calls in one turn all run — three identical
// spawns for a majority vote are a deliberate request, and "use the result you
// already have" would be false for them. If they all come back the same, that
// same call in a LATER turn is refused.
func (d *Dispatcher) refuseConsecutive(ctx context.Context, name string, input json.RawMessage) (Result, bool) {
	d.repeats.mu.Lock()
	defer d.repeats.mu.Unlock()
	t := &d.repeats
	if repeatKey(name, input) != t.last {
		return Result{}, false
	}
	seen := t.streak
	if b := toolBatch(ctx); b != 0 && b == t.batch {
		seen -= t.inBatch
	}
	if seen < consecutiveCallsAllowed {
		return Result{}, false
	}
	n := t.streak + t.refusedInRow
	t.refusedInRow++
	return consecutiveRefusal(name, n), true
}

// recordResult makes this call, with what it returned, the run's latest. The
// same call returning the same thing extends the streak; anything else starts
// a new one.
func (d *Dispatcher) recordResult(ctx context.Context, name string, input json.RawMessage, res Result) {
	k, h, b := repeatKey(name, input), resultHash(res), toolBatch(ctx)
	d.repeats.mu.Lock()
	defer d.repeats.mu.Unlock()
	t := &d.repeats
	if k == t.last && h == t.lastResult {
		t.streak++
	} else {
		t.last, t.lastResult, t.streak, t.refusedInRow = k, h, 1, 0
		t.batch, t.inBatch = 0, 0
	}
	if b != 0 && b == t.batch {
		t.inBatch++
	} else {
		t.batch, t.inBatch = b, 1
	}
}

type ctxKeyToolBatch struct{}

// WithToolBatch marks ctx as carrying one batch of tool calls: the calls a
// model sent together in one turn. The loop wraps each turn's dispatch in it,
// so the consecutive-call guard can tell a call that was sent after seeing an
// identical call's result from one sent alongside it. Nil-safe.
func (d *Dispatcher) WithToolBatch(ctx context.Context) context.Context {
	if d == nil {
		return ctx
	}
	d.repeats.mu.Lock()
	d.repeats.batches++
	id := d.repeats.batches
	d.repeats.mu.Unlock()
	return context.WithValue(ctx, ctxKeyToolBatch{}, id)
}

// toolBatch is ctx's batch id, 0 for a call outside any batch.
func toolBatch(ctx context.Context) uint64 {
	id, _ := ctx.Value(ctxKeyToolBatch{}).(uint64)
	return id
}

// resultHash is what "the same result" means: the same text, and the same
// success or failure.
func resultHash(res Result) [sha256.Size]byte {
	flag := byte(0)
	if res.IsError {
		flag = 1
	}
	return sha256.Sum256(append([]byte{flag}, res.Text...))
}

// consecutiveRefusal is the refusal for a call made more than
// consecutiveCallsAllowed times in a row with the same result each time. It
// says what happened rather than predicting the next result. It is a failure,
// so a model that will not break the loop is then caught by the failed-call
// guard, which ends the run.
func consecutiveRefusal(name string, made int) Result {
	return Result{
		Text: fmt.Sprintf("%s: you have made this exact call %d times in a row with the same arguments, and it returned the same result each time, so it was NOT run again. "+
			"Use the result you already have and take the next step, "+
			"change the arguments, or answer the user.", name, made),
		IsError: true,
		Error:   &ErrorInfo{Category: "business", Retryable: false},
	}
}

// repeatKey is the tool name and its canonical arguments, so key order and
// whitespace do not make two identical calls look different.
func repeatKey(name string, input json.RawMessage) string {
	var v any
	if json.Unmarshal(input, &v) == nil {
		if b, err := json.Marshal(v); err == nil {
			return name + "\x00" + string(b)
		}
	}
	var buf bytes.Buffer
	if json.Compact(&buf, input) == nil {
		return name + "\x00" + buf.String()
	}
	return name + "\x00" + string(input)
}

// refuseRepeat returns the refusal for a call that has already failed
// repeatFailuresAllowed times with these exact arguments, or false to run it.
func (d *Dispatcher) refuseRepeat(name string, input json.RawMessage) (Result, bool) {
	d.repeats.mu.Lock()
	defer d.repeats.mu.Unlock()
	k := repeatKey(name, input)
	n := d.repeats.failures[k]
	if n < repeatFailuresAllowed {
		return Result{}, false
	}
	d.repeats.refused++
	if d.repeats.refused >= repeatStopAfter && d.repeats.stopped == "" {
		d.repeats.stopped = fmt.Sprintf("the model sent the same failed %s call %d times", name, n+1)
	}
	return Result{
		Text: fmt.Sprintf("%s: this exact call has already failed %d times with these arguments, and it was NOT run again. "+
			"Sending it again cannot succeed. Change the arguments, take a different approach, or tell the user it cannot be done "+
			"and why.", name, n),
		IsError: true,
		Error:   &ErrorInfo{Category: "business", Retryable: false},
	}, true
}

// noteResult counts a failed call; a success clears the count for that exact
// call, since its failure is then not a fixed fact.
func (d *Dispatcher) noteResult(name string, input json.RawMessage, res Result) {
	d.repeats.mu.Lock()
	defer d.repeats.mu.Unlock()
	k := repeatKey(name, input)
	if res.IsError {
		if d.repeats.failures == nil {
			d.repeats.failures = map[string]int{}
		}
		d.repeats.failures[k]++
		return
	}
	delete(d.repeats.failures, k)
}

// RepeatedFailure reports whether the run has been flagged for re-sending a
// failed call it was told cannot succeed, with the reason. The loops check it
// after each batch of tool calls and end the run.
func (d *Dispatcher) RepeatedFailure() (string, bool) {
	if d == nil {
		return "", false
	}
	d.repeats.mu.Lock()
	defer d.repeats.mu.Unlock()
	return d.repeats.stopped, d.repeats.stopped != ""
}
