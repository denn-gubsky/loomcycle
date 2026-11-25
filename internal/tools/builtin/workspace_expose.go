package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// exposeOp publishes a port of the run's own workspace to the run's user, or
// withdraws it (RFC GW §5.3). The page is served at base_path by the API to
// that user's bearer only, and only while a server of the workspace listens
// on the port itself; the run's end, the workspace's release and the idle
// timeout close it.
func (w *Workspace) exposeOp(ctx context.Context, o lifecycle.Owner, in workspaceInput) tools.Result {
	if in.Port < 1 || in.Port > 65535 {
		return workspaceRefusal("invalid_argument", in.Op+" needs port, from 1 to 65535: the port a server of the workspace listens on")
	}
	if in.Op == "unexpose" {
		ok, err := w.Manager.Unexpose(ctx, o, in.Name, in.Port)
		if err != nil {
			return workspaceError(err, in.Name)
		}
		return workspaceOK(map[string]any{"name": in.Name, "port": in.Port, "closed": ok})
	}
	e, err := w.Manager.Expose(ctx, o, in.Name, in.Port, in.Title, in.Kind)
	switch {
	case errors.Is(err, lifecycle.ErrExposeOff):
		return workspaceRefusal("expose_off", err.Error())
	case errors.Is(err, lifecycle.ErrExposureLimit):
		return workspaceRefusal("limit", err.Error())
	case errors.Is(err, lifecycle.ErrPortExposed):
		return workspaceRefusal("exists", fmt.Sprintf("workspace %q exposes port %d already", in.Name, in.Port))
	case errors.Is(err, lifecycle.ErrExposeArgument):
		return workspaceRefusal("invalid_argument", err.Error())
	case err != nil:
		return workspaceError(err, in.Name)
	}
	tools.EventEmitter(ctx)(surfaceOpened(in.Name, e.ID, e.Title, e.Kind, w.Manager.ExposeIdleTimeout()))
	return workspaceOK(map[string]any{"name": in.Name, "exposure_id": e.ID, "port": e.Port, "title": e.Title,
		"kind": e.Kind, "base_path": lifecycle.ExposurePrefix(e.ID)})
}
