package pause

import "testing"

// TestRuntimeState_StringWireStable pins the wire-stable string forms.
// Operators alert on these strings + dashboards key off them; a typo
// or capitalisation change is a back-compat break.
func TestRuntimeState_StringWireStable(t *testing.T) {
	cases := []struct {
		state RuntimeState
		want  string
	}{
		{StateRunning, "running"},
		{StatePausing, "pausing"},
		{StatePaused, "paused"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// TestRuntimeState_AcceptsNewRuns_OnlyRunning pins the gate semantic:
// the runtime accepts new runs ONLY in StateRunning. StatePausing
// already refuses (avoids unbounded queue growth during the wind-down
// window); StatePaused refuses by definition.
func TestRuntimeState_AcceptsNewRuns_OnlyRunning(t *testing.T) {
	cases := []struct {
		state RuntimeState
		want  bool
	}{
		{StateRunning, true},
		{StatePausing, false},
		{StatePaused, false},
	}
	for _, tc := range cases {
		if got := tc.state.AcceptsNewRuns(); got != tc.want {
			t.Errorf("%s.AcceptsNewRuns() = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// TestRuntimeState_UnknownStringForm provides defensive output for an
// out-of-range value. Lets a future enum-expansion bug be visible in
// logs without crashing.
func TestRuntimeState_UnknownStringForm(t *testing.T) {
	got := RuntimeState(99).String()
	if got != "unknown(99)" {
		t.Errorf("out-of-range state.String() = %q, want %q", got, "unknown(99)")
	}
}
