package teamgraph

import (
	"strings"
	"testing"
)

func TestValidateName_AcceptsOneSegmentRefusesSeparators(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"sdlc", true},
		{"pr-review_2", true},
		{"A", true},
		{strings.Repeat("a", MaxNameLen), true},

		{"", false},
		{"a/b", false},
		{"a:b", false},
		{"a b", false},
		{"a.b", false},
		{"..", false},
		{" sdlc", false},
		{"sdlc\n", false},
		{"команда", false},
		{strings.Repeat("a", MaxNameLen+1), false},
	}
	for _, c := range cases {
		err := ValidateName(c.name)
		if c.ok && err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", c.name)
		}
	}
}
