package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

type fakePending struct {
	queued map[store.MemoryScope]int
	err    error
}

func (f fakePending) MemoryPendingTargetsAllTenants(_ context.Context, scope store.MemoryScope, _ int) ([]store.MemoryPendingTarget, error) {
	if f.err != nil {
		return nil, f.err
	}
	return make([]store.MemoryPendingTarget, f.queued[scope]), nil
}

// A memory scope with no consolidator is reported when something is waiting
// in its queue, and not before: a profile nobody has used yet has nothing to
// lose, and said so four times at every start.
func TestUndrainedMemoryWarnings_OnlyAScopeWithQueuedRowsIsReported(t *testing.T) {
	scopes := []config.UndrainedMemoryScope{
		{Scope: "tenant", Advisory: "tenant advisory"},
		{Scope: "user", Advisory: "user advisory"},
	}
	if got := undrainedMemoryWarnings(context.Background(), fakePending{}, scopes); len(got) != 0 {
		t.Errorf("empty queues: %v, want nothing", got)
	}
	got := undrainedMemoryWarnings(context.Background(), fakePending{queued: map[store.MemoryScope]int{store.MemoryScopeUser: 1}}, scopes)
	if len(got) != 1 || !strings.Contains(got[0], "user advisory") {
		t.Errorf("user queue holds a row: %v, want the user advisory alone", got)
	}
	// A store that cannot say is not "empty": the advisory is shown.
	got = undrainedMemoryWarnings(context.Background(), fakePending{err: errors.New("boom")}, scopes)
	if len(got) != 2 {
		t.Errorf("store fault: %v, want both advisories", got)
	}
}

// The untouched profiles load with no advisory at all, and still carry the
// scopes the boot check asks the store about.
func TestProfiles_LoadWithNoAdvisory(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	for _, profile := range []string{"personal", "server"} {
		cfg, err := config.LoadLayers(bootStackFor(t, profile)...)
		if err != nil {
			t.Fatalf("%s: %v", profile, err)
		}
		if len(cfg.Warnings) != 0 {
			t.Errorf("%s loads with advisories: %v", profile, cfg.Warnings)
		}
		if len(cfg.UndrainedMemory) == 0 {
			t.Errorf("%s: no undrained memory scope recorded; the boot check would have nothing to ask about", profile)
		}
	}
}
