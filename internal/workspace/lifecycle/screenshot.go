package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// ErrNoBrowser: the workspace's worker offers no screenshot. It runs the
// host engine, or its session image carries no browser (no browser label).
var ErrNoBrowser = errors.New("this workspace's worker offers no screenshot: it needs a container-engine worker whose session image carries the browser")

// screenshotGrace is how long past the worker's own kill the core waits for
// its answer, so a worker that never answers cannot hold the call.
const screenshotGrace = 15 * time.Second

// Screenshot takes a picture of a page a running server of the run's own
// workspace serves (RFC GW §5.8, P10 plan §10) and returns its bytes, at
// most workspace.MaxScreenshotBytes. args must already be checked and their
// viewport defaulted (workspace.ScreenshotURL, ScreenshotViewport); the
// worker checks them again. A shared name is refused: a screenshot is the
// owner's. The worker must report the browser label on the container
// engine; an attached worker that does not is refused before the op is
// sent, and one not attached to this replica fails as any op does.
func (m *Manager) Screenshot(ctx context.Context, o Owner, name string, args workspace.ScreenshotArgs) ([]byte, error) {
	if _, _, shared := SplitShared(name); shared {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	r, err := m.ready(ctx, o, name, false)
	if err != nil {
		return nil, err
	}
	for _, info := range m.disp.Workers() {
		if info.ID == r.WorkerID && (info.Engine != workspace.EngineContainer || !slices.Contains(info.Labels, workspace.LabelBrowser)) {
			return nil, ErrNoBrowser
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, workspace.ScreenshotTimeout+screenshotGrace)
	defer cancel()
	res, err := m.disp.Do(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, UserID: r.UserID, WorkspaceID: r.ID, Op: workspace.OpScreenshot, Args: raw, MaxOutput: workspace.MaxScreenshotBytes})
	if err != nil {
		return nil, err
	}
	return res.Output, nil
}
