package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
)

// git_netops.go — a git workspace's network ops (RFC GW P11 plan §3.7):
// git_fetch, git_pull (fast-forward only) and git_push. Each op mints its own
// ticket, for the shapes it needs and nothing more, and ends it when the op
// returns: a ticket never outlives its op, and a push ticket carries the push
// policy of the workspace's own branch, which the core holds every
// receive-pack to.

// GitNet is what a network op runs with: the credential the caller resolved
// in the run, and the operator's bounds.
type GitNet struct {
	Username, Token string
	// MaxObjectBytes and MaxReadBytes bound what a fetch takes in.
	MaxObjectBytes, MaxReadBytes int64
	// MaxPushBytes bounds a push's body; 0 is workspace.push.max_bytes.
	MaxPushBytes int64
	// Timeout bounds the op; 0 is defaultGitOpenTimeout.
	Timeout time.Duration
	// Push is workspace.push, resolved.
	Push config.WorkspacePushConfig
}

// GitNetResult is what a network op did.
type GitNetResult = workspace.GitNetAnswer

// The shapes each op may send.
var (
	gitFetchShapes = []egress.GitShape{egress.GitAdvertiseUpload, egress.GitUploadPack}
	// A push reads the forge's refs first (an upload advertisement), then
	// pushes.
	gitPushShapes = []egress.GitShape{egress.GitAdvertiseUpload, egress.GitAdvertiseReceive, egress.GitReceivePack}
)

// GitFetch fetches the workspace's mirror and makes its remote-tracking refs
// the forge's branches, pinned.
func (m *Manager) GitFetch(ctx context.Context, r store.WorkspaceRow, n GitNet) (GitNetResult, error) {
	return m.gitNetOp(ctx, r, n, workspace.OpGitFetchWorkspace, gitFetchShapes, false)
}

// GitPull is GitFetch, then a fast-forward of the current branch to its
// remote-tracking ref. A pull that is no fast-forward is refused
// (non_fast_forward); the fetched commits are kept.
func (m *Manager) GitPull(ctx context.Context, r store.WorkspaceRow, n GitNet) (GitNetResult, error) {
	return m.gitNetOp(ctx, r, n, workspace.OpGitPull, gitFetchShapes, false)
}

// GitPush pushes the workspace's own branch to the same name. Without force
// a push that is no fast-forward is refused on the worker before it is
// sent; the core holds every push to the own branch whatever the worker
// sends.
func (m *Manager) GitPush(ctx context.Context, r store.WorkspaceRow, n GitNet, force bool) (GitNetResult, error) {
	return m.gitNetOp(ctx, r, n, workspace.OpGitPush, gitPushShapes, force)
}

func (m *Manager) gitNetOp(ctx context.Context, r store.WorkspaceRow, n GitNet, op string, shapes []egress.GitShape, force bool) (GitNetResult, error) {
	if r.Kind != store.WorkspaceKindGit || r.Git == nil {
		return GitNetResult{}, fmt.Errorf("%w: %q is not a git workspace", ErrInvalidGitOpen, r.Name)
	}
	if r.State != store.WorkspaceReady {
		return GitNetResult{}, fmt.Errorf("%w: %q is %s", ErrNotReady, r.Name, r.State)
	}
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = defaultGitOpenTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	n.Timeout = timeout
	tk, err := m.mintGitOp(r, n, shapes)
	if err != nil {
		return GitNetResult{}, err
	}
	defer tk.End()
	args, err := json.Marshal(workspace.GitNetArgs{Mirror: r.Git.Mirror, URL: r.Git.URL, Branch: r.Git.Branch, Ticket: tk.Secret(),
		MaxObjectBytes: n.MaxObjectBytes, MaxReadBytes: n.MaxReadBytes, MaxPushBytes: n.pushBytes(), Force: force})
	if err != nil {
		return GitNetResult{}, err
	}
	res, err := m.Do(ctx, r, dispatch.Request{Op: op, Args: args})
	m.learnDefaultBranch(r, tk)
	if err != nil {
		return GitNetResult{}, err
	}
	var out GitNetResult
	if json.Unmarshal(res.Output, &out) != nil {
		return GitNetResult{}, &dispatch.OpError{Class: workspace.ClassProtocol, Detail: "the worker's answer to " + op + " is not JSON"}
	}
	return out, nil
}

func (n GitNet) pushBytes() int64 {
	if n.MaxPushBytes > 0 {
		return n.MaxPushBytes
	}
	return n.Push.MaxBytes
}

// mintGitOp mints the ticket of one network op of row: the shapes it needs,
// the resolved credential, the read budget and, for a push, the policy of
// the row's own branch, as the store holds it.
func (m *Manager) mintGitOp(r store.WorkspaceRow, n GitNet, shapes []egress.GitShape) (*GitTicket, error) {
	tk, err := m.MintGitTicket(GitTicketSpec{Row: r, TTL: n.Timeout, Grant: egress.GitGrant{
		WorkspaceID: r.ID, RepoURL: r.Git.URL, Username: n.Username, Token: n.Token, Shapes: shapes,
		Push:         egress.PushPolicy{Branch: r.Git.Branch, DefaultBranch: r.Git.DefaultBranch, AllowTags: n.Push.AllowsTags()},
		MaxPushBytes: n.pushBytes(), ReadBudget: egress.NewByteBudget(n.MaxReadBytes),
	}})
	if err != nil {
		return nil, err
	}
	if m.onGitTicket != nil {
		m.onGitTicket(tk)
	}
	return tk, nil
}

// learnDefaultBranch records the forge's default branch on the row when an
// advertisement under the op's ticket named one the row did not have: the
// push policy of the next op reads it. Best effort: a store fault leaves the
// row's older value, which the next op learns again.
func (m *Manager) learnDefaultBranch(r store.WorkspaceRow, tk *GitTicket) {
	def := tk.DefaultBranch()
	if def == "" || def == r.Git.DefaultBranch {
		return
	}
	g := *r.Git
	g.DefaultBranch = def
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := m.store.WorkspaceSetGit(ctx, r.TenantID, r.ID, g); err != nil {
		m.log.Warn("workspace: recording the forge's default branch failed", "workspace", r.ID, "err", err)
	}
}
