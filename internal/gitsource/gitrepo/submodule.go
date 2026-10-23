package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-billy/v5"
	billyutil "github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// A submodule here is a standalone repository with its own .git directory
// inside its path, not git's layout of a .git file pointing into the
// superproject's .git/modules: a .git file can point anywhere, and Open
// refuses one. The library's submodule update is not used: it dials the URL
// from the configuration, which is the agent's to write, and keeps the
// submodule's store in .git/modules. Nested submodules are left alone: an
// update is not recursive.

// SubmoduleInfo is one submodule of HEAD.
type SubmoduleInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// URL is .gitmodules's, with a relative one ("../x.git") resolved
	// against the "origin" remote's URL; "" when that cannot be done.
	URL string `json:"url"`
	// Commit is the gitlink HEAD's tree records at Path, "" if none.
	Commit string `json:"commit"`
	// Initialized is whether Path holds a repository checked out at
	// Commit.
	Initialized bool `json:"initialized"`
}

// maxGitmodulesBytes bounds the .gitmodules file read.
const maxGitmodulesBytes = 1 << 20

// gitmodules is HEAD's .gitmodules and tree. Both come from HEAD, as the
// gitlinks do, so what is listed and what is recorded agree; an unborn
// branch, or a tree with no .gitmodules, has no submodules.
func (r *Repo) gitmodules() (*config.Modules, *object.Tree, error) {
	head, err := r.headCommit()
	if err != nil || head == nil {
		return nil, nil, err
	}
	tree, err := head.Tree()
	if err != nil {
		return nil, nil, fmt.Errorf("read HEAD's tree: %w", err)
	}
	e, err := tree.FindEntry(".gitmodules")
	if err != nil || !e.Mode.IsFile() || e.Mode == filemode.Symlink {
		return nil, tree, nil
	}
	blob, err := r.repo.BlobObject(e.Hash)
	if err != nil {
		return nil, nil, fmt.Errorf("read .gitmodules: %w", err)
	}
	if blob.Size > maxGitmodulesBytes {
		return nil, nil, fmt.Errorf("%w: .gitmodules is over %d bytes", ErrInvalidArgument, maxGitmodulesBytes)
	}
	rd, err := blob.Reader()
	if err != nil {
		return nil, nil, fmt.Errorf("read .gitmodules: %w", err)
	}
	defer func() { _ = rd.Close() }()
	data, err := io.ReadAll(io.LimitReader(rd, maxGitmodulesBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("read .gitmodules: %w", err)
	}
	m := config.NewModules()
	if err := m.Unmarshal(data); err != nil {
		return nil, nil, fmt.Errorf("%w: .gitmodules does not parse: %v", ErrInvalidArgument, err)
	}
	return m, tree, nil
}

// Submodules lists HEAD's submodules, sorted by name.
func (r *Repo) Submodules() ([]SubmoduleInfo, error) {
	m, tree, err := r.gitmodules()
	if err != nil {
		return nil, err
	}
	out := []SubmoduleInfo{}
	if m == nil {
		return out, nil
	}
	origin, _ := r.RemoteURL(git.DefaultRemoteName)
	for _, sm := range m.Submodules {
		out = append(out, r.submoduleInfo(sm, tree, origin))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *Repo) submoduleInfo(sm *config.Submodule, tree *object.Tree, origin string) SubmoduleInfo {
	info := SubmoduleInfo{Name: sm.Name, Path: sm.Path, URL: resolveSubmoduleURL(sm.URL, origin)}
	h := gitlink(tree, sm.Path)
	if h.IsZero() {
		return info
	}
	info.Commit = h.String()
	if r.noLinksOnPath(sm.Path) != nil {
		return info
	}
	if fi, err := r.base.Lstat(sm.Path + "/" + git.GitDirName); err != nil || !fi.IsDir() {
		return info
	}
	sub, err := r.openSubmodule(sm.Path)
	if err != nil {
		return info
	}
	head, err := sub.Head()
	info.Initialized = err == nil && head.Hash == info.Commit
	return info
}

// gitlink is the commit tree records at p, zero when p is not a gitlink or
// not a path git accepts.
func gitlink(tree *object.Tree, p string) plumbing.Hash {
	if tree == nil || treePath(p) != nil {
		return plumbing.ZeroHash
	}
	e, err := tree.FindEntry(p)
	if err != nil || e.Mode != filemode.Submodule {
		return plumbing.ZeroHash
	}
	return e.Hash
}

// resolveSubmoduleURL resolves a relative submodule URL ("./x", "../x")
// against base, as git does: each "../" drops one component of base's path.
// Anything else is returned as given. "" is a URL that cannot be resolved.
func resolveSubmoduleURL(raw, base string) string {
	if !strings.HasPrefix(raw, "./") && !strings.HasPrefix(raw, "../") {
		return raw
	}
	u, err := url.Parse(base)
	if base == "" || err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	p := strings.TrimSuffix(u.Path, "/")
	rest := raw
	for {
		if r, ok := strings.CutPrefix(rest, "./"); ok {
			rest = r
			continue
		}
		if r, ok := strings.CutPrefix(rest, "../"); ok {
			if p == "" {
				return "" // above the host
			}
			rest, p = r, path.Dir(p)
			if p == "/" {
				p = ""
			}
			continue
		}
		break
	}
	if rest == "" {
		return ""
	}
	for _, c := range strings.Split(rest, "/") {
		if c == "" || c == "." || c == ".." {
			return ""
		}
	}
	out := *u
	out.Path, out.RawPath = p+"/"+rest, ""
	return out.String()
}

// noLinksOnPath refuses a path whose existing components are not all
// directories: a link at "sub" pointing at ".git" would otherwise put the
// submodule's checkout into the superproject's repository.
func (r *Repo) noLinksOnPath(p string) error {
	parts := strings.Split(p, "/")
	for i := range parts {
		q := strings.Join(parts[:i+1], "/")
		fi, err := r.wt.Lstat(q)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: stat %s: %v", ErrInvalidArgument, q, err)
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s, on the submodule's path, is a link or a file", ErrInvalidArgument, q)
		}
	}
	return nil
}

// openSubmodule opens the repository at p. Its .git is reached through the
// caller's filesystem, its worktree through the guard, as Open does. It has
// this repository's limits: they are the operator's, and a submodule's
// checkout expands a tree as the superproject's does.
func (r *Repo) openSubmodule(p string) (*Repo, error) {
	base, err := r.base.Chroot(p)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	sub, err := Open(base)
	if err != nil {
		return nil, err
	}
	sub.SetLimits(r.limits)
	return sub, nil
}

// SubmoduleUpdate checks the submodule name out at the commit HEAD records
// for it, cloning it from n.URL first if its path is empty and fetching
// from n.URL when the commit is not there yet. Only n.URL is dialled; the
// caller resolves and validates it (Submodules gives .gitmodules's URL).
// The checkout is detached, and refused (ErrWorktreeDirty) when the
// submodule has local changes. Not recursive.
func (r *Repo) SubmoduleUpdate(ctx context.Context, name string, n Network) (_ SubmoduleInfo, err error) {
	m, tree, err := r.gitmodules()
	if err != nil {
		return SubmoduleInfo{}, err
	}
	var sm *config.Submodule
	if m != nil {
		sm = m.Submodules[name]
	}
	if sm == nil {
		return SubmoduleInfo{}, fmt.Errorf("%w: no submodule %q in HEAD's .gitmodules", ErrInvalidArgument, truncate(name, 200))
	}
	p := sm.Path
	if err := treePath(p); err != nil {
		return SubmoduleInfo{}, err
	}
	want := gitlink(tree, p)
	if want.IsZero() {
		return SubmoduleInfo{}, fmt.Errorf("%w: HEAD records no submodule commit at %s", ErrInvalidArgument, p)
	}
	if err := n.receiving(); err != nil {
		return SubmoduleInfo{}, err
	}
	if err := r.noLinksOnPath(p); err != nil {
		return SubmoduleInfo{}, err
	}
	fresh, err := r.submoduleState(p)
	if err != nil {
		return SubmoduleInfo{}, err
	}
	var sub *Repo
	if !fresh {
		if sub, err = r.openSubmodule(p); err != nil {
			return SubmoduleInfo{}, err
		}
		// Refused before anything is fetched; the checkout checks again,
		// for untracked files the commit would write over.
		st, err := sub.status()
		if err != nil {
			return SubmoduleInfo{}, err
		}
		for f, fs := range st {
			if fs.Staging != git.Unmodified || (fs.Worktree != git.Unmodified && fs.Worktree != git.Untracked) {
				return SubmoduleInfo{}, fmt.Errorf("%w: the submodule has uncommitted changes in %s", ErrWorktreeDirty, f)
			}
		}
	}
	if fresh || !sub.hasCommit(want) {
		bctx, sess, auth, berr := n.bind(ctx)
		if berr != nil {
			return SubmoduleInfo{}, berr
		}
		if fresh {
			if merr := r.wt.MkdirAll(p, 0o755); merr != nil {
				return SubmoduleInfo{}, fmt.Errorf("create %s: %w", p, merr)
			}
			base, cerr := r.base.Chroot(p)
			if cerr != nil {
				return SubmoduleInfo{}, fmt.Errorf("open %s: %w", p, cerr)
			}
			// The directory was empty: a clone that is not checked out in
			// the end is removed, or it would read as a repository with
			// every file deleted and refuse the next update as dirty.
			defer func() {
				if err != nil {
					emptyDir(base)
				}
			}()
			if sub, err = cloneBound(bctx, sess, auth, base, n, git.DefaultRemoteName, CloneOptions{NoCheckout: true, Limits: r.limits}); err != nil {
				return SubmoduleInfo{}, err
			}
		}
		if err = sub.fetchGitlink(bctx, sess, auth, n, want); err != nil {
			return SubmoduleInfo{}, err
		}
	}
	// A fresh clone has no index or files yet, which reads as every file
	// deleted: nothing there is anyone's work, so its checkout is forced.
	err = sub.Checkout(CheckoutOptions{Rev: want.String(), Force: fresh})
	// The ref a fetch by sha needed is not the submodule's to keep: the
	// detached HEAD holds the commit now.
	_ = sub.repo.Storer.RemoveReference(gitlinkRef)
	if err != nil {
		return SubmoduleInfo{}, err
	}
	origin, _ := r.RemoteURL(git.DefaultRemoteName)
	return r.submoduleInfo(sm, tree, origin), nil
}

// emptyDir removes everything in fs, best effort.
func emptyDir(fs billy.Filesystem) {
	entries, err := fs.ReadDir(".")
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = billyutil.RemoveAll(fs, e.Name())
	}
}

// submoduleState reports whether p is to be cloned into (nothing there, or
// an empty directory) rather than holding the submodule's repository.
func (r *Repo) submoduleState(p string) (fresh bool, err error) {
	fi, err := r.wt.Lstat(p)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: stat %s: %v", ErrInvalidArgument, p, err)
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%w: %s is not a directory", ErrInvalidArgument, p)
	}
	gi, err := r.base.Lstat(p + "/" + git.GitDirName)
	switch {
	case err == nil && gi.IsDir():
		return false, nil
	case err == nil:
		return false, fmt.Errorf("%w: %s/.git is not a directory", ErrInvalidArgument, p)
	case !os.IsNotExist(err):
		return false, fmt.Errorf("%w: stat %s/.git: %v", ErrInvalidArgument, p, err)
	}
	entries, err := r.wt.ReadDir(p)
	if err != nil {
		return false, fmt.Errorf("%w: read %s: %v", ErrInvalidArgument, p, err)
	}
	if len(entries) > 0 {
		return false, fmt.Errorf("%w: %s holds files but no repository", ErrAlreadyExists, p)
	}
	return true, nil
}

func (r *Repo) hasCommit(h plumbing.Hash) bool {
	_, err := r.repo.CommitObject(h)
	return err == nil
}

// gitlinkRef is where a fetch by sha puts the commit; removed after the
// checkout.
const gitlinkRef plumbing.ReferenceName = "refs/loomcycle/gitlink"

// fetchGitlink fetches want from n.URL: by its sha, and when the server
// will not serve that (or the fetch fails for another reason that does not
// rule the broader fetch out), every branch.
func (r *Repo) fetchGitlink(ctx context.Context, sess *gitsource.Session, auth transport.AuthMethod, n Network, want plumbing.Hash) error {
	if r.hasCommit(want) {
		return nil
	}
	if _, err := r.repo.Remote(git.DefaultRemoteName); err != nil {
		if err := r.RemoteAdd(git.DefaultRemoteName, n.URL); err != nil {
			return err
		}
	}
	fetch := func(spec string) error {
		_, err := r.fetch(ctx, sess, n, &git.FetchOptions{
			RemoteName: git.DefaultRemoteName,
			RemoteURL:  n.URL,
			RefSpecs:   []config.RefSpec{config.RefSpec(spec)},
			Auth:       auth,
			Tags:       git.NoTags,
		})
		return err
	}
	err := fetch(want.String() + ":" + gitlinkRef.String())
	if err != nil && !errors.Is(err, gitsource.ErrFetchFailed) {
		return err
	}
	if err != nil || !r.hasCommit(want) {
		if err := fetch("+refs/heads/*:refs/remotes/" + git.DefaultRemoteName + "/*"); err != nil {
			return err
		}
	}
	if !r.hasCommit(want) {
		return fmt.Errorf("%w: the remote does not have the submodule's commit %s", gitsource.ErrCommitNotFound, want)
	}
	return nil
}
