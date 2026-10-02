package teamgraph

import "testing"

// One rule for every reader of the walk's input as a value: valid JSON is
// that value compacted, anything else is {"text": input} — and neither form
// HTML-escapes what a model will read.
func TestInputValue_JSONCompactedTextWrappedNothingEscaped(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{` { "a" : 1 } `, `{"a":1}`},
		{`[1, 2]`, `[1,2]`},
		{`"s"`, `"s"`},
		{`{"q":"<b> & c"}`, `{"q":"<b> & c"}`},
		{`plain <words> & more`, `{"text":"plain <words> & more"}`},
		{``, `{"text":""}`},
		{`  `, `{"text":"  "}`},
		{`{"a":`, `{"text":"{\"a\":"}`},
	} {
		if got := string(InputValue(tc.in)); got != tc.want {
			t.Errorf("InputValue(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
