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
