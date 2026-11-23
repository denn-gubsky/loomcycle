package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// workspaceViewMaxBytes bounds the file view reads into the core's memory.
// The dispatcher then scales anything over the 1568 px edge and refuses a
// decode that would be too large from the header (tools/images.go); this
// bound only keeps the read itself small. A chart or a screenshot is well
// under it; a photo that is not can be shrunk with exec first.
const workspaceViewMaxBytes = 20 << 20

// view returns a picture from the workspace to the model (RFC GW §5.8). The
// type is not checked here: the dispatcher reads it from the header, scales
// and bounds the picture, and the loop refuses it for a model that cannot
// see images. name is the run's own workspace or one shared with it; the
// caller has already checked the grant and, for a shared name, the share.
func (w *Workspace) view(ctx context.Context, o lifecycle.Owner, in workspaceInput) tools.Result {
	if in.Path == "" {
		return workspaceRefusal("invalid_argument", "view needs path")
	}
	var b bytes.Buffer
	if _, err := w.Manager.GetFile(ctx, o, in.Name, workspace.GetArgs{Path: in.Path}, &b, workspaceViewMaxBytes); err != nil {
		var oe *dispatch.OpError
		if errors.As(err, &oe) && oe.Class == workspace.ClassOutputTooLarge {
			return workspaceRefusal("too_large", fmt.Sprintf("%s is over the %d MiB a picture to view may be; make a smaller one with exec first", in.Path, workspaceViewMaxBytes>>20))
		}
		if _, _, shared := lifecycle.SplitShared(in.Name); shared {
			return workspaceSharedError(err, in.Name)
		}
		return workspaceError(err, in.Name)
	}
	res := workspaceOK(map[string]any{"name": in.Name, "path": in.Path, "bytes": b.Len()})
	res.Images = []tools.Image{{Data: b.Bytes()}}
	return res
}
