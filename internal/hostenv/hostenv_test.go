package hostenv

import (
	"reflect"
	"testing"
)

// TestScrubbed_PassesPathAndTheAllowedNamesOnly — a secret set in this
// process does not reach the child; PATH and the allowed names do, in order,
// and an allowed name that is unset is left out.
func TestScrubbed_PassesPathAndTheAllowedNamesOnly(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LOOMCYCLE_AUTH_TOKEN", "not-for-children")
	t.Setenv("SOME_API_KEY", "not-for-children")
	t.Setenv("HOSTENV_B", "b")
	t.Setenv("HOSTENV_A", "a")
	got := Scrubbed([]string{"HOSTENV_B", "HOSTENV_UNSET", "HOSTENV_A"})
	want := []string{"PATH=/usr/bin:/bin", "HOSTENV_B=b", "HOSTENV_A=a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Scrubbed = %v, want %v", got, want)
	}
}
