package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// awaitSlack is how much longer than the wait itself the call may take: the
// op's dispatch to the worker and back.
const awaitSlack = 30 * time.Second

// await waits for the next update the workspace's application posted (RFC
// GW §5.3, P9 plan §3.4): what a person did in its page. The update is
// untrusted output, returned once; the address it was posted to never is.
func (w *Workspace) await(ctx context.Context, row store.WorkspaceRow, in workspaceInput) tools.Result {
	timeout, bad := w.timeout(in)
	if bad != nil {
		return *bad
	}
	if timeout > workspace.MaxAwaitSeconds*time.Second {
		return workspaceRefusal("invalid_argument", fmt.Sprintf("await waits at most %d seconds", workspace.MaxAwaitSeconds))
	}
	args, _ := json.Marshal(workspace.AwaitArgs{TimeoutMS: timeout.Milliseconds()})
	ctx, cancel := context.WithTimeout(ctx, timeout+awaitSlack)
	defer cancel()
	res, err := w.Manager.Do(ctx, row, dispatch.Request{Op: workspace.OpAwait, Args: args})
	if err != nil {
		return workspaceError(err, in.Name)
	}
	var a workspace.AwaitAnswer
	if json.Unmarshal(res.Output, &a) != nil {
		return workspaceRefusal(workspace.ClassProtocol, "the worker's answer was not an update")
	}
	out := map[string]any{"name": row.Name, "pending": a.Pending}
	switch {
	case a.Update != nil:
		out["update"] = *a.Update
	case a.UpdateBase64 != nil:
		out["update_base64"] = *a.UpdateBase64
	default:
		out["timed_out"] = true
	}
	if a.Dropped > 0 {
		out["dropped"] = a.Dropped
	}
	return unescapedResult(out)
}
