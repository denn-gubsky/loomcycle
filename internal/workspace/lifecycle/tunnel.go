package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// Tunnel opens a byte stream to port in the ready workspace r (RFC GW P9
// plan §3.2). The caller has checked the pin; the worker checks that a
// running server of the workspace declared the port. maxIn and maxOut bound
// the bytes each way.
func (m *Manager) Tunnel(ctx context.Context, r store.WorkspaceRow, port int, maxIn, maxOut int64) (*dispatch.Tunnel, error) {
	if r.State != store.WorkspaceReady {
		return nil, fmt.Errorf("%w: %q is %s", ErrNotReady, r.Name, r.State)
	}
	args, err := json.Marshal(workspace.TunnelArgs{Port: port})
	if err != nil {
		return nil, err
	}
	return m.disp.Tunnel(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, UserID: r.UserID, WorkspaceID: r.ID, Op: workspace.OpTunnel, Args: args, MaxInput: maxIn, MaxOutput: maxOut})
}
