package loop

import (
	"context"
	"testing"
)

// The park decision must be read AT THE BOUNDARY, not captured at start.
//
// The whole request this serves is "convert a running agent to interactive so I
// can correct it" — an operator who wants that had no way to ask for it when the
// run began, so a flag captured at start can never answer it.
func TestInteractiveAtBoundary_PrefersTheLiveAnswerOverTheStartFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		started bool
		now     func(context.Context) bool
		want    bool
	}{
		{
			name:    "no callback — the start flag stands, which is almost every run",
			started: true,
			want:    true,
		},
		{
			name:    "promoted while running: started plain, parks now",
			started: false,
			now:     func(context.Context) bool { return true },
			want:    true,
		},
		{
			// The other direction has to work too, or "interactive" becomes a
			// one-way door: a run started interactive could never be released.
			name:    "demoted while running: started interactive, finishes now",
			started: true,
			now:     func(context.Context) bool { return false },
			want:    false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &RunOptions{Interactive: tc.started, InteractiveNow: tc.now}
			if got := o.interactiveAtBoundary(context.Background()); got != tc.want {
				t.Errorf("interactiveAtBoundary = %v, want %v", got, tc.want)
			}
		})
	}
}
