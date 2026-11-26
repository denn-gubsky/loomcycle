package worker

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitrepo"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// Every class a git op can answer reaches the model by name: one the core
// does not know is reported as failed, and git's not_found would read as a
// workspace whose directory is gone.
func TestGitClasses_EveryGitClassIsOneTheCoreReportsByName(t *testing.T) {
	for _, c := range gitrepo.Classes() {
		got := gitClassName(c)
		if !workspace.KnownWorkerClasses[got] {
			t.Errorf("git class %q answers %q, which the core reports as failed", c, got)
		}
		if got == workspace.ClassNotFound {
			t.Errorf("git class %q answers the workspace's own not_found", c)
		}
	}
}
