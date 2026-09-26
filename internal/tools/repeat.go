package tools

import (
	"bytes"
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

	// last and streak are the most recent call and how many times in a row it
	// has been made, for refuseConsecutive.
	last   string
	streak int
}

// consecutiveCallsAllowed is how many times in a row a run may make the exact
// same call. The next one is refused unrun.
//
// A model looping on one call: measured live (ornith-1.5 behind chat/local),
// the same successful tenant save was re-sent 36, 17 and 12 times in a row,
// and one help article re-read 7 times. The failed-call guard never saw it,
// because every call succeeded. This is deliberately narrow: only the SAME
// tool with the SAME arguments, back to back. Any other call in between, or
// any change to the arguments, starts the count over, so re-reading a file
// after editing it, or polling between other steps, is untouched.
const consecutiveCallsAllowed = 2

// recordCall makes this call the run's latest and returns how many times in a
// row it has now been made. Every attempted call is recorded, refused ones
// included, so a model that keeps sending the same call keeps being refused.
func (d *Dispatcher) recordCall(name string, input json.RawMessage) int {
	d.repeats.mu.Lock()
	defer d.repeats.mu.Unlock()
	k := repeatKey(name, input)
	if k == d.repeats.last {
		d.repeats.streak++
	} else {
		d.repeats.last, d.repeats.streak = k, 1
	}
	return d.repeats.streak
}

// consecutiveRefusal is the refusal for a call made more than
// consecutiveCallsAllowed times in a row. It is a failure, so a model that will
// not break the loop is then caught by the failed-call guard, which ends the
// run.
func consecutiveRefusal(name string, streak int) Result {
	return Result{
		Text: fmt.Sprintf("%s: you have made this exact call %d times in a row with the same arguments, so it was NOT run again. "+
			"Repeating it will not change the result. Use the result you already have and take the next step, "+
			"change the arguments, or answer the user.", name, streak-1),
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
