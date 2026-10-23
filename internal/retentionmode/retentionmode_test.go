package retentionmode

import "testing"

// A misspelled "none" must be refused, never read as the default: the default
// stores everything, so a typo would quietly keep what was meant to be kept
// nowhere.
func TestParse_AcceptsOnlyNoneAndTheDefault(t *testing.T) {
	for in, want := range map[string]string{"": "", "default": "", "none": None} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
	for _, in := range []string{"nonee", "None", "NONE", " none", "off", "false"} {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %q, nil; want an error", in, got)
		}
	}
}
