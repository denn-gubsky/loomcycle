package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// screenshot returns a picture of a page a running server of the run's own
// workspace serves (RFC GW §5.8, P10 plan §10), taken by a headless browser
// in the workspace's container session. Only a port and a path are taken,
// never a URL; the viewport defaults to 1280x800. The dispatcher then
// scales and bounds the picture as it does view's, and the loop refuses it
// for a model that cannot see images. The caller has checked the connect
// grant; a shared name never reaches here (not a shared op).
func (w *Workspace) screenshot(ctx context.Context, o lifecycle.Owner, in workspaceInput) tools.Result {
	path := in.Path
	if path == "" {
		path = "/"
	}
	width, height, err := workspace.ScreenshotViewport(in.Width, in.Height)
	if err != nil {
		return workspaceRefusal("invalid_argument", err.Error())
	}
	args := workspace.ScreenshotArgs{Port: in.Port, Path: path, Width: width, Height: height}
	if _, err := workspace.ScreenshotURL(args); err != nil {
		return workspaceRefusal("invalid_argument", err.Error())
	}
	pic, err := w.Manager.Screenshot(ctx, o, in.Name, args)
	if err != nil {
		var oe *dispatch.OpError
		switch {
		case errors.Is(err, lifecycle.ErrNoBrowser):
			return workspaceRefusal(workspace.ClassBrowserUnavailable, err.Error())
		case errors.As(err, &oe) && oe.Class == workspace.ClassOutputTooLarge:
			return workspaceRefusal("too_large", fmt.Sprintf("the picture is over the %d MiB a screenshot may be; try a smaller viewport", workspace.MaxScreenshotBytes>>20))
		}
		return workspaceError(err, in.Name)
	}
	res := workspaceOK(map[string]any{"name": in.Name, "port": in.Port, "path": path, "width": width, "height": height, "bytes": len(pic)})
	res.Images = []tools.Image{{Data: pic}}
	return res
}
