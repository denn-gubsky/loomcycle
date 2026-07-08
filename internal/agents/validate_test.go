package agents

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		// Flat names (the pre-RFC-BA form) still pass.
		{"chat", true},
		{"doc-manager", true},
		{"file_editor", true},
		{"agent123", true},
		// `/`-grouped names (the new form).
		{"doc/manager", true},
		{"chat/medium", true},
		{"chat/local", true},
		{"doc/file-editor", true},
		{"a/b/c/deep", true},
		// Rejects.
		{"", false},            // empty
		{"/leading", false},    // leading slash → empty first segment
		{"trailing/", false},   // trailing slash → empty last segment
		{"a//b", false},        // double slash → empty middle segment
		{"..", false},          // dot-dot whole
		{"a/../b", false},      // dot-dot segment (path-traversal floor)
		{".", false},           // dot whole
		{"a/./b", false},       // dot segment
		{"doc/*", false},       // glob char
		{"doc/man?ger", false}, // glob char
		{"has space", false},   // invalid rune
		{"has.dot", false},     // invalid rune (dot only legal as a whole `.` reject; interior dot not allowed)
		{"back\\slash", false}, // backslash not a separator/allowed rune
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.name)
			if tc.valid && err != nil {
				t.Errorf("ValidateName(%q) = %v, want nil", tc.name, err)
			}
			if !tc.valid && err == nil {
				t.Errorf("ValidateName(%q) = nil, want error", tc.name)
			}
		})
	}
}

func TestValidateName_TooLong(t *testing.T) {
	long := strings.Repeat("a", MaxNameLen+1)
	if err := ValidateName(long); err == nil {
		t.Errorf("ValidateName(len %d) = nil, want too-long error", len(long))
	}
	ok := strings.Repeat("a", MaxNameLen)
	if err := ValidateName(ok); err != nil {
		t.Errorf("ValidateName(len %d) = %v, want nil (at cap)", len(ok), err)
	}
}
