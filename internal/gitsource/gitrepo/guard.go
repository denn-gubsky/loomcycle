package gitrepo

import (
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-billy/v5"
)

// A repository's own content names paths, and a hostile one can name a path
// inside a .git directory: ".git/config", "sub/.git/hooks/post-checkout",
// ".GIT/config", "git~1/config". Written out, such an entry replaces the
// repository's config or plants a nested repository's hooks. This library
// runs no hook, but a real git run later in the same directory (a host
// command, an operator) would, and a planted config can redirect remotes.
// Git refuses these paths in every tree and index entry; so does this
// package, in two places:
//
//   - every path taken from a tree or the index is checked by treePath
//     before anything is written or removed at it;
//   - the worktree filesystem handed to go-git and used by the writers is
//     wrapped by worktreeGuard, which refuses any path through a .git
//     component. The repository's own store is opened on the caller's
//     filesystem directly, so it is not affected.

// hfsIgnorable are the code points HFS+ drops when it compares names, so
// ".g\u200cit" names .git there.
var hfsIgnorable = strings.NewReplacer(
	"\u200c", "", "\u200d", "", "\u200e", "", "\u200f", "",
	"\u202a", "", "\u202b", "", "\u202c", "", "\u202d", "", "\u202e", "",
	"\u206a", "", "\u206b", "", "\u206c", "", "\u206d", "", "\u206e", "", "\u206f", "",
	"\ufeff", "",
)

// isDotGitVariant reports whether a path component is ".git" under any
// filesystem's reading of it: any case, HFS+ ignorable code points, NTFS
// trailing dots and spaces, an NTFS stream suffix (".git::$INDEX_ALLOCATION")
// and the NTFS short name "git~1".
func isDotGitVariant(c string) bool {
	c = strings.ToLower(hfsIgnorable.Replace(c))
	if i := strings.IndexByte(c, ':'); i >= 0 {
		c = c[:i]
	}
	c = strings.TrimRight(c, ". ")
	return c == ".git" || c == "git~1"
}

// treePath checks a path taken from a tree or the index before it is written
// or removed in the worktree. It refuses what git's verify_path refuses for
// an entry: a control character, a backslash, an empty, "." or ".."
// component, and a .git variant anywhere.
func treePath(p string) error {
	bad := func(why string) error {
		return fmt.Errorf("%w: the repository holds a path git refuses (%q %s)", ErrInvalidArgument, truncate(p, 200), why)
	}
	if p == "" || hasControl(p) || strings.Contains(p, "\\") {
		return bad("is empty or has a control character or a backslash")
	}
	for _, c := range strings.Split(p, "/") {
		switch {
		case c == "" || c == "." || c == "..":
			return bad("has an empty, . or .. component")
		case isDotGitVariant(c):
			return bad("reaches into .git")
		}
	}
	return nil
}

// worktreeGuard refuses any path through a .git component: as the first
// component, or as any but the last (a submodule's ".git" pointer file at
// "sub/.git" stays reachable, as go-git's own worktree filesystem allows).
type worktreeGuard struct {
	billy.Filesystem
}

func guardWorktree(fs billy.Filesystem) billy.Filesystem { return worktreeGuard{fs} }

func (g worktreeGuard) check(paths ...string) error {
	for _, p := range paths {
		parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
		for i, c := range parts {
			if isDotGitVariant(c) && (i == 0 || i < len(parts)-1) {
				return &os.PathError{Op: "open", Path: p, Err: fmt.Errorf("%w: the path reaches into .git", ErrInvalidArgument)}
			}
		}
	}
	return nil
}

func (g worktreeGuard) Create(p string) (billy.File, error) {
	if err := g.check(p); err != nil {
		return nil, err
	}
	return g.Filesystem.Create(p)
}

func (g worktreeGuard) Open(p string) (billy.File, error) {
	if err := g.check(p); err != nil {
		return nil, err
	}
	return g.Filesystem.Open(p)
}

func (g worktreeGuard) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	if err := g.check(p); err != nil {
		return nil, err
	}
	return g.Filesystem.OpenFile(p, flag, perm)
}

func (g worktreeGuard) Stat(p string) (os.FileInfo, error) {
	if err := g.check(p); err != nil {
		return nil, err
	}
	return g.Filesystem.Stat(p)
}

func (g worktreeGuard) Lstat(p string) (os.FileInfo, error) {
	if err := g.check(p); err != nil {
		return nil, err
	}
	return g.Filesystem.Lstat(p)
}

func (g worktreeGuard) Rename(from, to string) error {
	if err := g.check(from, to); err != nil {
		return err
	}
	return g.Filesystem.Rename(from, to)
}

func (g worktreeGuard) Remove(p string) error {
	if err := g.check(p); err != nil {
		return err
	}
	return g.Filesystem.Remove(p)
}

func (g worktreeGuard) ReadDir(p string) ([]os.FileInfo, error) {
	if err := g.check(p); err != nil {
		return nil, err
	}
	return g.Filesystem.ReadDir(p)
}

func (g worktreeGuard) MkdirAll(p string, perm os.FileMode) error {
	if err := g.check(p); err != nil {
		return err
	}
	return g.Filesystem.MkdirAll(p, perm)
}

func (g worktreeGuard) Symlink(target, link string) error {
	if err := g.check(link); err != nil {
		return err
	}
	return g.Filesystem.Symlink(target, link)
}

func (g worktreeGuard) Readlink(p string) (string, error) {
	if err := g.check(p); err != nil {
		return "", err
	}
	return g.Filesystem.Readlink(p)
}

func (g worktreeGuard) TempFile(dir, prefix string) (billy.File, error) {
	if err := g.check(dir); err != nil {
		return nil, err
	}
	return g.Filesystem.TempFile(dir, prefix)
}

// Chroot keeps the guard on the narrower filesystem, so a submodule's
// worktree is guarded the same way.
func (g worktreeGuard) Chroot(p string) (billy.Filesystem, error) {
	if err := g.check(p); err != nil {
		return nil, err
	}
	fs, err := g.Filesystem.Chroot(p)
	if err != nil {
		return nil, err
	}
	return guardWorktree(fs), nil
}
