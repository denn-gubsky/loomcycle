package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// A screenshot is the owner's: a workspace shared with the run is not found
// for it, even through a real share. The tool refuses a shared name first;
// this is the Manager's own check, so it holds for any other caller.
func TestScreenshot_ASharedWorkspaceIsNotFound(t *testing.T) {
	f, owner, peer := shareFixture(t)
	f.share(t, owner, "work", peer, store.WorkspaceShareRead)
	args := workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600}
	if _, err := f.m.Screenshot(context.Background(), peer, "work@"+owner.AgentID, args); !errors.Is(err, ErrNotFound) {
		t.Errorf("screenshot of a shared workspace = %v, want ErrNotFound", err)
	}
	// The owner's own call reaches the worker check (a host worker: no browser).
	if _, err := f.m.Screenshot(context.Background(), owner, "work", args); !errors.Is(err, ErrNoBrowser) {
		t.Errorf("the owner's screenshot = %v, want ErrNoBrowser", err)
	}
}
