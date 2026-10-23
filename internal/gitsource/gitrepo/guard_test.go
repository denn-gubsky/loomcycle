package gitrepo

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"

	"github.com/go-git/go-billy/v5"
	billyutil "github.com/go-git/go-billy/v5/util"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestIsDotGitVariant(t *testing.T) {
	for _, c := range []string{".git", ".GIT", ".Git", ".git.", ".git ", ".git. .", "git~1", "GIT~1",
		".git::$INDEX_ALLOCATION", ".g\u200cit", "\ufeff.git"} {
		if !isDotGitVariant(c) {
			t.Errorf("%q is .git on some filesystem, not refused", c)
		}
	}
	for _, c := range []string{".gitignore", ".gitmodules", ".gitkeep", "git", "a.git", "git~2", "src"} {
		if isDotGitVariant(c) {
			t.Errorf("%q refused, but it is not .git", c)
		}
	}
}

// The guard on the worktree refuses any path through .git, however spelt,
// and leaves ordinary dotfiles alone. A submodule's pointer file at
// "sub/.git" (the last component) stays reachable.
func TestWorktreeGuard_RefusesPathsThroughDotGit(t *testing.T) {
	g := guardWorktree(memfs.New())
	for _, p := range []string{".git/config", ".GIT/config", "git~1/config", "a/.git/hooks/x", "a/.Git./x", ".git"} {
		if _, err := g.Create(p); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Create(%q): %v, want refused", p, err)
		}
	}
	if err := g.MkdirAll(".git/hooks", 0o755); err == nil {
		t.Error("MkdirAll(.git/hooks) was allowed")
	}
	if err := g.Rename("a.txt", ".git/config"); err == nil {
		t.Error("Rename into .git was allowed")
	}
	if err := g.Symlink("/etc/passwd", "a/.git/hooks/post-checkout"); err == nil {
		t.Error("Symlink into a nested .git was allowed")
	}
	if _, err := g.Chroot(".git"); err == nil {
		t.Error("Chroot(.git) was allowed")
	}
	for _, p := range []string{".gitignore", "a/.gitkeep", "sub/.git"} {
		f, err := g.Create(p)
		if err != nil {
			t.Errorf("Create(%q): %v", p, err)
			continue
		}
		_ = f.Close()
	}
}

// dotGit snapshots every file under .git: path → content.
func dotGit(t *testing.T, fsys billy.Filesystem) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := billyutil.Walk(fsys, ".git", func(p string, fi fs.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		out[p] = read(t, fsys, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// plant puts a blob in the store and returns its hash.
func plant(t *testing.T, r *Repo, content string) plumbing.Hash {
	t.Helper()
	obj := r.repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	h, err := r.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// An index entry naming a path inside .git — the repository's own, or a
// nested one's hooks — is never written or removed: restore, hard reset and
// clean refuse, and .git is untouched. go-git's own checkout refuses these
// paths; the replacements here must too.
func TestPlantedIndexEntryNeverReachesDotGit(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	commitAll(t, r, c, "first")
	evil := plant(t, r, "[core]\n\tfsmonitor = /tmp/pwn\n")

	idx, err := r.repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git/config", ".git/HEAD", "sub/.git/hooks/post-checkout", ".GIT/config"} {
		idx.Entries = append(idx.Entries, &index.Entry{Name: name, Hash: evil, Mode: filemode.Regular})
	}
	if err := r.repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
	// The snapshot is taken after the index is written, so only what the
	// operations below do to .git is compared.
	before := dotGit(t, fs)

	if err := r.Restore(nil, false, true); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("restore over planted entries: %v, want invalid_argument", err)
	}
	if after := dotGit(t, fs); !reflect.DeepEqual(after, before) {
		t.Fatalf("restore changed .git: %v", diffKeys(before, after))
	}
	if exists(fs, "sub/.git") {
		t.Fatal("restore created a nested .git")
	}
	// A hard reset drops the planted entries from the index and would then
	// remove the files they name: .git/config itself.
	if err := r.Reset("hard", "HEAD", nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("hard reset over planted entries: %v, want invalid_argument", err)
	}
	if after := dotGit(t, fs); after[".git/HEAD"] != before[".git/HEAD"] || after[".git/config"] != before[".git/config"] {
		t.Fatalf("hard reset changed .git: %v", diffKeys(before, after))
	}
}

// A commit whose tree holds .git/config, as a hostile repository's can, is
// refused by a hard reset to it, and the repository's config survives.
func TestCraftedTreeWithDotGitIsNeverCheckedOut(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	head := commitAll(t, r, c, "first")
	_, hadConfig := dotGit(t, fs)[".git/config"]
	evil := plant(t, r, "[core]\n\tfsmonitor = /tmp/pwn\n")
	keep := plant(t, r, "1\n")

	encode := func(o interface {
		Encode(plumbing.EncodedObject) error
	}) plumbing.Hash {
		t.Helper()
		obj := r.repo.Storer.NewEncodedObject()
		if err := o.Encode(obj); err != nil {
			t.Fatal(err)
		}
		h, err := r.repo.Storer.SetEncodedObject(obj)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	inner := encode(&object.Tree{Entries: []object.TreeEntry{{Name: "config", Mode: filemode.Regular, Hash: evil}}})
	top := encode(&object.Tree{Entries: []object.TreeEntry{
		{Name: ".git", Mode: filemode.Dir, Hash: inner},
		{Name: "a.txt", Mode: filemode.Regular, Hash: keep},
	}})
	sig := object.Signature{Name: "x", Email: "x@x", When: c.sig().When}
	bad := encode(&object.Commit{Author: sig, Committer: sig, Message: "hostile", TreeHash: top, ParentHashes: []plumbing.Hash{plumbing.NewHash(head)}})

	if err := r.Reset("hard", bad.String(), nil); err == nil {
		t.Error("a hard reset to a tree holding .git/config succeeded")
	}
	if got, ok := dotGit(t, fs)[".git/config"]; ok != hadConfig || (ok && got == "[core]\n\tfsmonitor = /tmp/pwn\n") {
		t.Fatalf("the hostile tree wrote .git/config: %q", got)
	}
}

// diffKeys names the .git files added, removed or changed between a and b.
func diffKeys(a, b map[string]string) []string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}
