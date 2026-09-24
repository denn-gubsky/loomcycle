package codejs

import (
	"encoding/json"

	"github.com/dop251/goja"
)

// The exports below let other operator JavaScript — code-js hook bodies —
// run in the same sandbox as a code agent, rather than a second, drifting
// copy of it.

// HardenSandbox removes the dynamic-code globals and installs the
// deterministic clock and RNG on a fresh runtime, before any operator code
// runs. See hardenSandbox.
func HardenSandbox(rt *goja.Runtime, seed uint32, anchorMs int64) {
	hardenSandbox(rt, seed, anchorMs)
}

// StableJSValue converts a Go value into a JS value whose object keys are in
// sorted order, so the same input produces the same JS object on every
// replay. See stableJSValue.
func StableJSValue(rt *goja.Runtime, v any) goja.Value {
	return stableJSValue(rt, v)
}

// SameCanonicalJSON reports whether two JSON values are equal ignoring key
// order and whitespace — the replay divergence check.
func SameCanonicalJSON(a, b json.RawMessage) bool {
	return sameCanonicalJSON(a, b)
}
