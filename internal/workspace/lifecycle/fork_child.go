package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// errChildFork: a fork for a child the spawn cannot make as asked.
var errChildFork = errors.New("cannot fork a workspace for this child")

// ChildFork is one fork a spawn makes for its child (RFC GW §5.4 "For a
// fan-out", `Agent op=spawn workspaces: [{fork, as?}]`).
type ChildFork struct {
	// Child is the child run: it owns the fork.
	Child Owner
	// Name is the parent's workspace to copy; As the fork's name in the
	// child ("" = Name).
	Name, As string
	// Egress is the child's egress grant: the fork may use the source's
	// routes that it names (D-P6-17).
	Egress []string
	// Engine is the child's engine floor; "" accepts any (D-P6-18).
	Engine string
}

// ForkForChild copies the parent's workspace f.Name into a new workspace
// owned by f.Child, on the source's worker, from its present files. The
// source is found by the parent's pin, so a workspace of any other run,
// one shared with the parent included, is ErrNotFound. The child must be
// another run of the parent's tenant and user (D-P6-16): a spawn's child is
// one by construction, and the check keeps a fork from ever handing one
// owner's files to another.
func (m *Manager) ForkForChild(ctx context.Context, parent Owner, f ChildFork) (store.WorkspaceRow, error) {
	c := f.Child
	switch {
	case parent.IsPrincipal() || c.IsPrincipal():
		return store.WorkspaceRow{}, fmt.Errorf("%w: a fork at spawn is made between runs", errChildFork)
	case parent.RunID == "" || c.RunID == "" || c.RunID == parent.RunID:
		return store.WorkspaceRow{}, fmt.Errorf("%w: the child must be another run", errChildFork)
	case c.TenantID != parent.TenantID || c.UserID != parent.UserID:
		return store.WorkspaceRow{}, fmt.Errorf("%w: the child is not a run of this tenant and user", errChildFork)
	}
	as := f.As
	if as == "" {
		as = f.Name
	}
	if !ValidName(as) {
		return store.WorkspaceRow{}, fmt.Errorf("%w: as: 1 to %d letters, digits, '.', '-' or '_', starting with a letter or digit", ErrInvalidName, MaxNameLen)
	}
	src, err := m.Get(ctx, parent, f.Name)
	if err != nil {
		return store.WorkspaceRow{}, err
	}
	if f.Engine != "" && workspace.EngineRank(src.Engine) < workspace.EngineRank(f.Engine) {
		return store.WorkspaceRow{}, fmt.Errorf("%w: %q runs on the %s engine; the child requires %s", errChildFork, src.Name, src.Engine, f.Engine)
	}
	egress := []string{}
	for _, r := range src.Egress {
		if slices.Contains(f.Egress, r) {
			egress = append(egress, r)
		}
	}
	row, _, err := m.fork(ctx, c, src, as, "", egress, forkAtSpawn)
	return row, err
}
