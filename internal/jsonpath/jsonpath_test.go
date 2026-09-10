package jsonpath

import "testing"

// The grammar tests moved here with the parser they cover (from
// internal/api/webhook). The REJECT list is the security-relevant half: every
// entry is a JSONPath construct this subset deliberately cannot express, and
// each one must stay unreachable no matter which caller supplies the path.

func TestParsePath_RejectsDisallowedShapes(t *testing.T) {
	cases := []string{
		"$.a[*]",      // wildcard index
		"$..a",        // recursive descent
		"$.a[?(@.x)]", // filter
		"a.b",         // missing $ root
		"$.",          // empty key
		"$.a[xyz]",    // non-integer index
		"$.a[-1]",     // negative index
	}
	for _, c := range cases {
		if _, err := Parse(c); err == nil {
			t.Errorf("path %q: want reject, got accept", c)
		}
	}
}

func TestParsePath_AcceptsAllowedShapes(t *testing.T) {
	cases := []string{
		"$",
		"$.a",
		"$.a.b.c",
		"$.a[0]",
		"$.a[10].b",
	}
	for _, c := range cases {
		if _, err := Parse(c); err != nil {
			t.Errorf("path %q: want accept, got %v", c, err)
		}
	}
}
