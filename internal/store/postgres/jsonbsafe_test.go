package postgres

import (
	"encoding/json"
	"testing"
)

// A NUL escape becomes U+FFFD, everything else — including an escaped
// backslash followed by the text "u0000", and a document with no NUL at all —
// is byte-for-byte unchanged, and the result is still valid JSON.
func TestJSONBSafe_ReplacesOnlyNULEscapes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"final_text":"a\u0000b"}`, `{"final_text":"a\ufffdb"}`},
		{`{"k\u0000":["\u0000","\u00001"]}`, `{"k\ufffd":["\ufffd","\ufffd1"]}`},
		{`{"t":"a\\u0000b"}`, `{"t":"a\\u0000b"}`},
		{`{"t":"a\\\u0000b"}`, `{"t":"a\\\ufffdb"}`},
		{`{"t":"é \n"}`, `{"t":"é \n"}`},
	} {
		got := string(jsonbSafe([]byte(c.in)))
		if got != c.want {
			t.Errorf("jsonbSafe(%s) = %s, want %s", c.in, got, c.want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("jsonbSafe(%s) = %s is not valid JSON", c.in, got)
		}
	}
}
