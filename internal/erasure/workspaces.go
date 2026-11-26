package erasure

import (
	"context"
	"fmt"
)

// Durable workspaces (RFC GW P12, ruling Q2): kept past their run under the
// user, they go only on discard or the user's erasure. The rows are this
// layer's to count; removing them is the workspace layer's
// (EraseDurableWorkspaces), since the worker removes each directory.

const durablePlane = "workspaces_durable"

// eraseDurableWorkspaces counts the subject's durable workspaces on a dry
// run, and otherwise has the workspace layer remove them. Execute's step.
func (s *Service) eraseDurableWorkspaces(ctx context.Context, tenant, subject string, dryRun bool, res *Result, fail func(string, error)) {
	n, err := s.Store.WorkspaceCountDurableByUser(ctx, tenant, subject)
	if err != nil {
		fail(durablePlane, err)
		return
	}
	if n == 0 {
		return
	}
	if dryRun {
		res.Deleted[durablePlane] = n
		return
	}
	if s.EraseDurableWorkspaces == nil {
		res.Retained[durablePlane] = fmt.Sprintf("%d durable workspace(s) of this subject are kept: this transport has no workspace layer to remove them; discard them, or erase again where workspaces are served", n)
		return
	}
	removed, err := s.EraseDurableWorkspaces(ctx, tenant, subject)
	if err != nil {
		fail(durablePlane, err)
	}
	res.Deleted[durablePlane] = removed
}
