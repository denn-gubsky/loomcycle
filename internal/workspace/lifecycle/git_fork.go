package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// git_fork.go — forks and restores of a git workspace (RFC GW P11 plan PR 9).
// A fork copies the source's files and git directory as any fork does, then
// becomes a git workspace of its own on the worker: its own branch at the
// copy's HEAD, its mirror marker and its pins.

// SetGitPush gives the manager workspace.push, resolved. Called once at
// boot; unset, the defaults hold.
func (m *Manager) SetGitPush(p config.WorkspacePushConfig) { m.gitPush = p.Resolved() }

func (m *Manager) pushConfig() config.WorkspacePushConfig {
	if m.gitPush.RefPrefixes == nil {
		return config.WorkspacePushConfig{}.Resolved()
	}
	return m.gitPush
}

// checkForkBranch holds a fork's named branch to the open's rules before
// anything is copied: a valid name under the push prefixes, never the
// forge's default branch nor the source's own. A fork asks the forge
// nothing, so it cannot tell whether a named branch exists there: one is
// taken only where the operator lets a workspace adopt an existing branch.
// "" is a branch named for the fork, checked once its id is known.
func (m *Manager) checkForkBranch(src store.WorkspaceGit, branch string) error {
	if branch == "" {
		return nil
	}
	push := m.pushConfig()
	g := GitOpen{Push: push}
	switch {
	case !g.branchAllowed(branch):
		return fmt.Errorf("%w: %q is not a valid branch name under workspace.push.ref_prefixes", ErrGitBranch, truncateRef(branch))
	case isDefaultBranch(src.DefaultBranch, branch):
		return fmt.Errorf("%w: %s is the forge's default branch", ErrGitBranch, branch)
	case branch == src.Branch:
		return fmt.Errorf("%w: %s is the source's own branch", ErrGitBranch, branch)
	case !push.AdoptExisting:
		return fmt.Errorf("%w: a fork names its own branch only where workspace.push.adopt_existing is on (it cannot ask the forge whether %s exists); leave branch out for one named for the fork", ErrGitBranch, branch)
	}
	return nil
}

// isDefaultBranch reports whether short branch b is the forge's default
// branch def (a full ref), or main or master while that is unknown.
func isDefaultBranch(def, b string) bool {
	own := "refs/heads/" + b
	return own == def || def == "" && (b == "main" || b == "master")
}

// adoptGitFork makes the copied workspace of row a git workspace of its
// own: a branch named for it when none was asked, then the worker's adopt,
// then the row's record.
func (m *Manager) adoptGitFork(ctx context.Context, row *store.WorkspaceRow) error {
	if row.Git.Branch == "" {
		g := GitOpen{Push: m.pushConfig()}
		b := g.defaultBranchName(row.Name, row.ID)
		if !g.branchAllowed(b) || isDefaultBranch(row.Git.DefaultBranch, b) {
			return fmt.Errorf("%w: no valid branch name for %q under workspace.push.ref_prefixes; name one", ErrGitBranch, row.Name)
		}
		row.Git.Branch = b
	}
	if err := m.gitAdopt(ctx, *row, true); err != nil {
		return err
	}
	ok, err := m.store.WorkspaceSetGit(ctx, row.TenantID, row.ID, *row.Git)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: the workspace was released while it opened", ErrNotReady)
	}
	return nil
}

// gitAdopt runs the worker's git_adopt on row: with newBranch for a fork,
// without after a restore.
func (m *Manager) gitAdopt(ctx context.Context, row store.WorkspaceRow, newBranch bool) error {
	args, err := json.Marshal(workspace.GitAdoptArgs{Mirror: row.Git.Mirror, URL: row.Git.URL, Branch: row.Git.Branch, Base: row.Git.Base, NewBranch: newBranch})
	if err != nil {
		return err
	}
	_, err = m.disp.Do(ctx, row.WorkerID, dispatch.Request{TenantID: row.TenantID, UserID: row.UserID, WorkspaceID: row.ID, Op: workspace.OpGitAdopt, Args: args})
	return err
}
