// Package gitrepo is the repository work behind the Git tool: status, log,
// diff, commit, branch, and clone/fetch/pull/push over HTTPS, done in-process
// with go-git.
//
// It exists so an agent can work on a repository without git being run for
// it and without ever holding what opens the remote. A `git` run through a
// shell would read the user's and the system's git config, run hooks and
// filters the repository names, and need the forge token in its environment.
// Here nothing is executed, no config but the repository's own is read, and
// the token travels only in the Authorization header of a request the
// gitsource dispatcher has bound to one host: it is never put into a URL,
// into .git/config, onto disk, or into an error's text.
//
// The package knows nothing of volumes, agents or credentials. The caller
// hands it a billy.Filesystem rooted at the working directory (already
// confined to what the agent may touch; the repository's .git is reached
// through it, never through the OS), resolved credentials, and the guard of
// each network operation.
package gitrepo

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-billy/v5"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// The classes of failure. Like gitsource's they are a closed list, so what a
// caller shows a model is one of these words and never a server's own text;
// the wrapped detail is for the operator's log.
var (
	ErrNotARepository  = errors.New("not_a_repository")
	ErrAlreadyExists   = errors.New("already_exists")
	ErrInvalidArgument = errors.New("invalid_argument")
	ErrInvalidRevision = errors.New("invalid_revision")
	ErrNotFastForward  = errors.New("non_fast_forward")
	ErrNothingToCommit = errors.New("nothing_to_commit")
	ErrWorktreeDirty   = errors.New("worktree_dirty")
	ErrPushRejected    = errors.New("push_rejected")
	// ErrUpToDate is never returned: Fetch, Pull and Push report it in their
	// result. It is kept so Class has a word for it.
	ErrUpToDate = errors.New("already_up_to_date")
)

// classGitFailed is the class of anything not otherwise classified.
const classGitFailed = "git_failed"

var ownClasses = []error{
	ErrNotARepository, ErrAlreadyExists, ErrInvalidArgument, ErrInvalidRevision,
	ErrNotFastForward, ErrNothingToCommit, ErrWorktreeDirty, ErrPushRejected, ErrUpToDate,
}

// gitsource's classes, which a network failure carries. ErrFetchFailed is
// last: a classified error never also wraps it, and an unclassified network
// error is reported as a network failure rather than as git_failed.
var networkClasses = []error{
	gitsource.ErrUploadTooLarge, gitsource.ErrInvalidRequest, gitsource.ErrAuthFailed,
	gitsource.ErrNotFound, gitsource.ErrCommitNotFound, gitsource.ErrTooLarge,
	gitsource.ErrTimeout, gitsource.ErrMergeBaseNotFound, gitsource.ErrFetchFailed,
}

// Class returns the class of an error from this package: one of the Err*
// values' text, a gitsource class for a network failure, or "git_failed".
func Class(err error) string {
	if err == nil {
		return ""
	}
	for _, c := range ownClasses {
		if errors.Is(err, c) {
			return c.Error()
		}
	}
	for _, c := range networkClasses {
		if errors.Is(err, c) {
			return c.Error()
		}
	}
	return classGitFailed
}

// Repo is an open repository: a working directory and the .git inside it.
type Repo struct {
	// base is the caller's filesystem, on which .git is opened.
	base billy.Filesystem
	// wt is base behind worktreeGuard: what go-git and the writers use for
	// the working tree, where no path may reach into .git.
	wt   billy.Filesystem
	repo *git.Repository
}

func newRepo(base billy.Filesystem, repo *git.Repository) *Repo {
	return &Repo{base: base, wt: guardWorktree(base), repo: repo}
}

// newStorage is the repository's object store, read through the caller's
// filesystem. Objects over a megabyte are read as streams, not into memory.
func newStorage(wt billy.Filesystem) (*filesystem.Storage, error) {
	dot, err := wt.Chroot(git.GitDirName)
	if err != nil {
		return nil, fmt.Errorf("open .git: %w", err)
	}
	return filesystem.NewStorageWithOptions(dot, cache.NewObjectLRUDefault(),
		filesystem.Options{LargeObjectThreshold: 1 << 20}), nil
}

// hasDotGit reports whether wt has a .git entry of any kind.
func hasDotGit(wt billy.Filesystem) (bool, error) {
	_, err := wt.Lstat(git.GitDirName)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Init creates a repository in wt with initialBranch ("" is "main") as its
// unborn HEAD.
func Init(wt billy.Filesystem, initialBranch string) (*Repo, error) {
	if initialBranch == "" {
		initialBranch = "main"
	}
	if err := validBranchName(initialBranch); err != nil {
		return nil, err
	}
	exists, err := hasDotGit(wt)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if exists {
		return nil, fmt.Errorf("%w: the directory already has a .git", ErrAlreadyExists)
	}
	st, err := newStorage(wt)
	if err != nil {
		return nil, err
	}
	repo, err := git.InitWithOptions(st, guardWorktree(wt), git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(initialBranch)})
	if err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	return newRepo(wt, repo), nil
}

// Open opens the repository whose working directory is wt. Only a .git
// directory is a repository here: a .git file is a pointer ("gitdir: ...")
// that can name any directory at all, so it is refused.
func Open(wt billy.Filesystem) (*Repo, error) {
	fi, err := wt.Lstat(git.GitDirName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: no .git directory", ErrNotARepository)
		}
		return nil, fmt.Errorf("%w: %v", ErrNotARepository, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: .git is not a directory", ErrNotARepository)
	}
	repo, err := openRepo(wt)
	if err != nil {
		return nil, err
	}
	return newRepo(wt, repo), nil
}

func openRepo(wt billy.Filesystem) (*git.Repository, error) {
	st, err := newStorage(wt)
	if err != nil {
		return nil, err
	}
	repo, err := git.Open(st, guardWorktree(wt))
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return nil, fmt.Errorf("%w: %v", ErrNotARepository, err)
		}
		return nil, fmt.Errorf("open: %w", err)
	}
	return repo, nil
}

// maxArgLen bounds a path, revision or name argument. Nothing legitimate is
// near it, and it keeps an argument from becoming a large error text.
const maxArgLen = 4096

// hasControl reports a NUL, another control character or DEL.
func hasControl(s string) bool {
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// cleanPath checks a repository-relative path. It must be relative, use "/",
// and have no empty, "." or ".." component and no component that a
// filesystem could take for ".git" (any case, trailing dots or spaces): a
// path into .git would let an agent write the repository's config or hooks.
// One trailing "/" is accepted, as for a directory.
func cleanPath(p string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%w: path %q %s", ErrInvalidArgument, truncate(p, 200), why)
	}
	if len(p) > maxArgLen {
		return bad("is too long")
	}
	if hasControl(p) || strings.Contains(p, "\\") {
		return bad("has a control character or a backslash")
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return bad("is empty")
	}
	if strings.HasPrefix(p, "/") {
		return bad("must be relative to the repository")
	}
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "":
			return bad("has an empty component")
		case ".", "..":
			return bad("has a . or .. component")
		}
		if isDotGitVariant(c) {
			return bad("reaches into .git")
		}
	}
	return p, nil
}

// pathspecs checks paths for a command that takes a list. "." (or, when
// emptyIsAll, an empty list or "") means the whole tree, returned as nil.
func pathspecs(paths []string, emptyIsAll bool) ([]string, error) {
	if len(paths) == 0 {
		if emptyIsAll {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: at least one path is required", ErrInvalidArgument)
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "." || (emptyIsAll && p == "") {
			return nil, nil
		}
		c, err := cleanPath(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// matchPaths reports whether name is one of specs or under one of them. nil
// specs match everything.
func matchPaths(specs []string, name string) bool {
	if specs == nil {
		return true
	}
	for _, s := range specs {
		if name == s || strings.HasPrefix(name, s+"/") {
			return true
		}
	}
	return false
}

// validBranchName checks a short branch or tag name.
func validBranchName(name string) error {
	if name == "" || len(name) > 255 || strings.HasPrefix(name, "-") || name == "HEAD" || strings.HasPrefix(name, "refs/") {
		return fmt.Errorf("%w: invalid branch name %q", ErrInvalidArgument, truncate(name, 200))
	}
	if err := plumbing.NewBranchReferenceName(name).Validate(); err != nil {
		return fmt.Errorf("%w: invalid branch name %q", ErrInvalidArgument, truncate(name, 200))
	}
	return nil
}

// validRemoteName checks a remote's name: one ref-name component.
func validRemoteName(name string) error {
	if name == "" || strings.Contains(name, "/") || validBranchName(name) != nil {
		return fmt.Errorf("%w: invalid remote name %q", ErrInvalidArgument, truncate(name, 200))
	}
	return nil
}

// validRev checks a revision expression before the library parses it.
func validRev(rev string) error {
	if rev == "" || len(rev) > 1024 || strings.HasPrefix(rev, "-") || hasControl(rev) {
		return fmt.Errorf("%w: invalid revision %q", ErrInvalidArgument, truncate(rev, 200))
	}
	return nil
}

// truncate cuts s to n bytes for an error's text.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}
