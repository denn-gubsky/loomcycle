package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitrepo"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
)

// git_open.go — opening a git workspace (RFC GW §4.2, P11 plan §3.6).
//
// The worker fills the tenant's mirror of the repository through the core,
// under the open's ticket, then makes the workspace a working tree on its own
// branch. What decides the open is read by the core from the forge's own
// answers as they passed: that the opener's credential was let in (the proof
// of access, §11), the forge's default branch, and whether the branch asked
// for exists. The worker's word decides none of it.

var (
	// ErrGitAccess: the forge did not let the opener's credential read the
	// repository, so no workspace links the opener to its mirror.
	ErrGitAccess = errors.New("the forge did not let this credential read the repository")
	// ErrGitBranch: a branch the workspace may not own: not a valid name,
	// outside workspace.push.ref_prefixes, or the forge's default branch.
	ErrGitBranch = errors.New("the workspace may not own this branch")
	// ErrGitBranchExists: the forge has the branch already, and the
	// operator does not let a workspace adopt one (workspace.push.
	// adopt_existing): a forced update of the own branch is accepted, so
	// the workspace could rewrite someone else's work.
	ErrGitBranchExists = errors.New("the forge has this branch already")
	// ErrInvalidGitOpen: an open's git arguments the caller should have
	// refused.
	ErrInvalidGitOpen = errors.New("invalid git workspace open")
	// ErrGitUnsupported: an op this release does not do on a git workspace.
	ErrGitUnsupported = errors.New("not supported on a git workspace")
)

// defaultGitOpenTimeout bounds an open that names no timeout.
const defaultGitOpenTimeout = 10 * time.Minute

// GitOpen is a git workspace's open (OpenRequest.Git).
type GitOpen struct {
	// URL is the repository, validated by the caller (gitsource.ValidateURL,
	// the host list).
	URL string
	// Branch is the workspace's own branch, a short name; "" names one
	// under the first heads prefix: <prefix>/<name>-<6 hex of the id>.
	Branch string
	// BaseRef is the full ref or commit the workspace starts at; "" is the
	// forge's default branch, or the branch itself when an existing one is
	// adopted.
	BaseRef string
	// Credential is the reference recorded on the row ("$cred:<name>");
	// Username and Token are what it resolved to, for this open only.
	Credential      string
	Username, Token string
	// MaxObjectBytes and MaxReadBytes bound the mirror fetch.
	MaxObjectBytes, MaxReadBytes int64
	// Timeout bounds the open; 0 is defaultGitOpenTimeout.
	Timeout time.Duration
	// Push is workspace.push, resolved.
	Push config.WorkspacePushConfig
}

func (g GitOpen) check() error {
	switch {
	case g.URL == "":
		return fmt.Errorf("%w: no repository", ErrInvalidGitOpen)
	case g.MaxObjectBytes <= 0 || g.MaxReadBytes <= 0:
		return fmt.Errorf("%w: the fetch needs its bounds", ErrInvalidGitOpen)
	case g.Branch != "" && !g.branchAllowed(g.Branch):
		return fmt.Errorf("%w: %q is not a valid branch name under workspace.push.ref_prefixes", ErrGitBranch, truncateRef(g.Branch))
	}
	return nil
}

// branchAllowed holds a branch to the names a push may move, before any
// request: a valid short name under a heads prefix.
func (g GitOpen) branchAllowed(b string) bool {
	return gitrepo.ValidBranchName(b) && g.Push.BranchAllowed(b)
}

// row is the git record the workspace's row starts with.
func (g GitOpen) row(tenantID string) *store.WorkspaceGit {
	return &store.WorkspaceGit{URL: g.URL, Branch: g.Branch, BaseRef: g.BaseRef, Credential: g.Credential, Mirror: gitrepo.MirrorKey(tenantID, g.URL)}
}

// defaultBranchName is the branch an open that names none gets: under the
// first heads prefix, from the workspace's name and id. A name's dots are
// dashes, so no name makes a ref git refuses.
func (g GitOpen) defaultBranchName(name, id string) string {
	dir := "loom/"
	for _, pre := range g.Push.RefPrefixes {
		if strings.HasPrefix(pre, "refs/heads/") && pre != "refs/heads/" {
			dir = strings.TrimPrefix(pre, "refs/heads/")
			break
		}
	}
	tail := strings.TrimPrefix(id, "ws_")
	if len(tail) > 6 {
		tail = tail[:6]
	}
	return dir + strings.ReplaceAll(name, ".", "-") + "-" + tail
}

// openGit makes the workspace of row (opening, git) on its worker: its
// directory, the mirror fetch that proves access, the branch checks, the
// working tree, then the row's repository record.
func (m *Manager) openGit(ctx context.Context, row *store.WorkspaceRow, g GitOpen) error {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = defaultGitOpenTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	branch := g.Branch
	if branch == "" {
		branch = g.defaultBranchName(row.Name, row.ID)
		if !g.branchAllowed(branch) {
			return fmt.Errorf("%w: no valid branch name for %q under workspace.push.ref_prefixes; name one", ErrGitBranch, row.Name)
		}
	}
	if err := m.disp.OpenWith(ctx, row.TenantID, row.UserID, row.WorkerID, row.ID, workspace.OpenArgs{DiskQuotaBytes: m.limits.DiskQuotaBytes}); err != nil {
		return err
	}
	var exists atomic.Bool
	tk, err := m.MintGitTicket(GitTicketSpec{Row: *row, ForOpen: true, TTL: timeout, Grant: egress.GitGrant{
		WorkspaceID: row.ID, RepoURL: g.URL, Username: g.Username, Token: g.Token,
		Shapes:     []egress.GitShape{egress.GitAdvertiseUpload, egress.GitUploadPack},
		ReadBudget: egress.NewByteBudget(g.MaxReadBytes),
		WatchRef:   "refs/heads/" + branch, OnWatchedRef: func() { exists.Store(true) },
	}})
	if err != nil {
		return err
	}
	defer tk.End()
	fetchArgs, err := json.Marshal(workspace.GitFetchArgs{Mirror: row.Git.Mirror, URL: g.URL, Ticket: tk.Secret(),
		MaxObjectBytes: g.MaxObjectBytes, MaxReadBytes: g.MaxReadBytes, Prove: true})
	if err != nil {
		return err
	}
	_, fetchErr := m.disp.Do(ctx, row.WorkerID, dispatch.Request{TenantID: row.TenantID, UserID: row.UserID, WorkspaceID: row.ID, Op: workspace.OpGitFetch, Args: fetchArgs})
	// The proof is the forge's answers as the core read them; a worker that
	// answers success without them opens nothing.
	if !tk.Proved() {
		if fetchErr != nil {
			return fmt.Errorf("%w (%v)", ErrGitAccess, fetchErr)
		}
		return ErrGitAccess
	}
	if fetchErr != nil {
		return fetchErr
	}
	def := tk.DefaultBranch()
	own := "refs/heads/" + branch
	if own == def || def == "" && (branch == "main" || branch == "master") {
		return fmt.Errorf("%w: %s is the forge's default branch", ErrGitBranch, own)
	}
	base := g.BaseRef
	if exists.Load() {
		if !g.Push.AdoptExisting {
			return fmt.Errorf("%w: %s (workspace.push.adopt_existing is off)", ErrGitBranchExists, own)
		}
		if base == "" {
			base = own
		}
	}
	if base == "" {
		if def == "" {
			return fmt.Errorf("%w: the forge named no default branch; name a base", ErrInvalidGitOpen)
		}
		base = def
	}
	openArgs, err := json.Marshal(workspace.GitOpenArgs{Mirror: row.Git.Mirror, URL: g.URL, Branch: branch, BaseRef: base})
	if err != nil {
		return err
	}
	res, err := m.disp.Do(ctx, row.WorkerID, dispatch.Request{TenantID: row.TenantID, UserID: row.UserID, WorkspaceID: row.ID, Op: workspace.OpGitOpen, Args: openArgs})
	if err != nil {
		return err
	}
	var ans workspace.GitOpenAnswer
	if json.Unmarshal(res.Output, &ans) != nil || len(ans.Base) != 40 {
		return &dispatch.OpError{Class: workspace.ClassProtocol, Detail: "the worker's answer to git_open is not a commit"}
	}
	git := *row.Git
	git.Branch, git.BaseRef, git.Base, git.DefaultBranch = branch, base, ans.Base, def
	ok, err := m.store.WorkspaceSetGit(ctx, row.TenantID, row.ID, git)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: the workspace was released while it opened", ErrNotReady)
	}
	row.Git = &git
	return nil
}

func truncateRef(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
