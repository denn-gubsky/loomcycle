package main

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// The dead-link collection deletes chunk bodies. It starts only for the two
// modes that name it, and deletes only for "on": every other mode, an empty or
// unknown one included, either does not start or only counts.
func TestDeadLinkGCRun_DeletesOnlyForOn(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		start, dryRun bool
	}{
		{config.DeadLinkGCOn, true, false},
		{config.DeadLinkGCDryRun, true, true},
		{config.DeadLinkGCOff, false, true},
		{"", false, true},
		{"ON", false, true},
		{"1", false, true},
	} {
		start, dryRun := deadLinkGCRun(tc.mode)
		if start != tc.start || dryRun != tc.dryRun {
			t.Errorf("mode %q: start=%v dryRun=%v, want start=%v dryRun=%v", tc.mode, start, dryRun, tc.start, tc.dryRun)
		}
		if start && !dryRun && tc.mode != config.DeadLinkGCOn {
			t.Errorf("mode %q deletes", tc.mode)
		}
	}
}
