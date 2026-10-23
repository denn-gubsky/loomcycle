package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// encodeObject stores a tree or a commit built by hand and returns its hash.
func encodeObject(t *testing.T, r *Repo, o interface {
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

// commitOfTree stores a commit of top on parent and returns its hash.
func commitOfTree(t *testing.T, r *Repo, c *clock, top plumbing.Hash, parent string) string {
	t.Helper()
	sig := object.Signature{Name: "x", Email: "x@x", When: c.sig().When}
	return encodeObject(t, r, &object.Commit{Author: sig, Committer: sig, Message: "crafted", TreeHash: top, ParentHashes: []plumbing.Hash{plumbing.NewHash(parent)}}).String()
}

// bomb stores, on parent, a commit whose "bomb" directory names one subtree
// width times at each of levels levels: width^levels files from levels+1
// tree objects. The commit also keeps a.txt as "1\n".
func bomb(t *testing.T, r *Repo, c *clock, parent string, width, levels int) string {
	t.Helper()
	cur, mode := plant(t, r, "x\n"), filemode.Regular
	for l := 0; l < levels; l++ {
		var es []object.TreeEntry
		for i := 0; i < width; i++ {
			es = append(es, object.TreeEntry{Name: fmt.Sprintf("e%02d", i), Mode: mode, Hash: cur})
		}
		cur, mode = encodeObject(t, r, &object.Tree{Entries: es}), filemode.Dir
	}
	top := encodeObject(t, r, &object.Tree{Entries: []object.TreeEntry{
		{Name: "a.txt", Mode: filemode.Regular, Hash: plant(t, r, "1\n")},
		{Name: "bomb", Mode: filemode.Dir, Hash: cur},
	}})
	return commitOfTree(t, r, c, top, parent)
}

// assertUnmoved fails unless HEAD is where it was, the worktree has no
// "bomb" and status is clean.
func assertUnmoved(t *testing.T, r *Repo, fs billy.Filesystem, before HeadInfo, op string) {
	t.Helper()
	if h, _ := r.Head(); h != before {
		t.Errorf("%s moved HEAD from %+v to %+v", op, before, h)
	}
	if exists(fs, "bomb") {
		t.Errorf("%s wrote the tree out", op)
	}
	if st := statusMap(t, r); len(st) != 0 {
		t.Errorf("%s left status %v", op, st)
	}
}

// A tree that names one subtree many times is refused, as too large, by
// every operation that would bring it in, before HEAD, the index or a file
// moves: seven objects here stand for 10,000 files.
func TestTreeBomb_IsRefusedBeforeAnythingMoves(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	head := commitAll(t, r, c, "first")
	bad := bomb(t, r, c, head, 10, 4)
	if err := r.BranchCreate("bombed", bad); err != nil {
		t.Fatal(err)
	}
	r.SetLimits(Limits{MaxFiles: 1000})
	before, _ := r.Head()

	ops := []struct {
		name string
		run  func() error
	}{
		{"merge", func() error { _, err := r.MergeFF(bad); return err }},
		{"checkout of a branch", func() error { return r.Checkout(CheckoutOptions{Branch: "bombed"}) }},
		{"forced checkout of a branch", func() error { return r.Checkout(CheckoutOptions{Branch: "bombed", Force: true}) }},
		{"checkout of a revision", func() error { return r.Checkout(CheckoutOptions{Rev: bad}) }},
		{"keep checkout", func() error { return r.Checkout(CheckoutOptions{Rev: bad, Keep: true}) }},
		{"new branch at it", func() error { return r.Checkout(CheckoutOptions{Branch: "nb", Rev: bad, Create: true}) }},
		{"hard reset", func() error { return r.Reset("hard", bad, nil) }},
		{"mixed reset", func() error { return r.Reset("mixed", bad, nil) }},
		{"soft reset", func() error { return r.Reset("soft", bad, nil) }},
		{"path reset", func() error { return r.Reset("mixed", bad, []string{"bomb"}) }},
		{"show", func() error { _, _, _, err := r.Show(bad, 1<<20); return err }},
		{"diff", func() error { _, _, err := r.Diff(DiffOptions{From: head, To: bad, MaxBytes: 1 << 20}); return err }},
	}
	for _, op := range ops {
		err := op.run()
		if Class(err) != "source_too_large" {
			t.Errorf("%s of a tree bomb: %v (class %s), want source_too_large", op.name, err, Class(err))
		}
		assertUnmoved(t, r, fs, before, op.name)
	}
	if _, err := r.repo.Reference(plumbing.NewBranchReferenceName("nb"), false); err == nil {
		t.Error("a refused checkout left its new branch behind")
	}

	// With room, the same merge goes.
	r.SetLimits(Limits{MaxFiles: 20000})
	if _, err := r.MergeFF(bad); err != nil {
		t.Fatalf("a merge within the limits: %v", err)
	}
	if !exists(fs, "bomb/e09/e09/e09/e09") {
		t.Error("the merge within the limits did not write the tree")
	}
}

// The byte limit counts a blob once per path that names it.
func TestTreeBomb_ByteLimitCountsEveryPath(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	head := commitAll(t, r, c, "first")
	bad := bomb(t, r, c, head, 10, 2) // 100 paths of "x\n": 200 bytes, and the trees
	r.SetLimits(Limits{MaxBytes: 150})
	before, _ := r.Head()
	if _, err := r.MergeFF(bad); Class(err) != "source_too_large" {
		t.Errorf("a merge over the byte limit: %v, want source_too_large", err)
	}
	assertUnmoved(t, r, fs, before, "the merge over the byte limit")
	r.SetLimits(Limits{MaxBytes: 1 << 20})
	if _, err := r.MergeFF(bad); err != nil {
		t.Errorf("a merge within the byte limit: %v", err)
	}
}

// A listing over the file limit is refused rather than built.
func TestStatusAndIndex_AreBoundedByTheFileLimit(t *testing.T) {
	r, fs, c := memRepo(t)
	for i := 0; i < 5; i++ {
		write(t, fs, fmt.Sprintf("f%d.txt", i), "x\n")
	}
	commitAll(t, r, c, "five")
	for i := 0; i < 5; i++ {
		write(t, fs, fmt.Sprintf("u%d.txt", i), "x\n")
	}
	r.SetLimits(Limits{MaxFiles: 3})
	if _, err := r.Status(); Class(err) != "source_too_large" {
		t.Errorf("status of 5 changes with a limit of 3: %v", err)
	}
	if _, _, err := r.Diff(DiffOptions{Staged: true, MaxBytes: 1 << 20}); Class(err) != "source_too_large" {
		t.Errorf("a diff of 5 indexed files with a limit of 3: %v", err)
	}
	r.SetLimits(Limits{})
	if st, err := r.Status(); err != nil || len(st) != 5 {
		t.Errorf("status with the default limits = %v, %v", st, err)
	}
}

// A hostile tree holding .git/config is refused as an invalid path before
// HEAD moves: go-git's reset moves the branch first and then fails.
func TestCraftedTreeWithDotGit_IsRefusedBeforeHEADMoves(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	head := commitAll(t, r, c, "first")
	inner := encodeObject(t, r, &object.Tree{Entries: []object.TreeEntry{{Name: "config", Mode: filemode.Regular, Hash: plant(t, r, "[core]\n\tfsmonitor = /tmp/pwn\n")}}})
	top := encodeObject(t, r, &object.Tree{Entries: []object.TreeEntry{
		{Name: "a.txt", Mode: filemode.Regular, Hash: plant(t, r, "1\n")},
		{Name: "sub", Mode: filemode.Dir, Hash: encodeObject(t, r, &object.Tree{Entries: []object.TreeEntry{{Name: ".git", Mode: filemode.Dir, Hash: inner}}})},
	}})
	bad := commitOfTree(t, r, c, top, head)
	before, _ := r.Head()
	for name, run := range map[string]func() error{
		"hard reset": func() error { return r.Reset("hard", bad, nil) },
		"soft reset": func() error { return r.Reset("soft", bad, nil) },
		"merge":      func() error { _, err := r.MergeFF(bad); return err },
		"checkout":   func() error { return r.Checkout(CheckoutOptions{Rev: bad}) },
	} {
		if err := run(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s to a tree holding sub/.git/config: %v, want invalid_argument", name, err)
		}
		if h, _ := r.Head(); h != before {
			t.Errorf("the refused %s moved HEAD to %+v", name, h)
		}
	}
	if exists(fs, "sub") {
		t.Error("the hostile tree was written")
	}
}

// failingFS refuses to write one path once armed, as a full disk or a
// permission would: a failure after the tree check has passed.
type failingFS struct {
	billy.Filesystem
	bad *string
}

func (f failingFS) fail(p string, flag int) error {
	if *f.bad != "" && p == *f.bad && flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE) != 0 {
		return &os.PathError{Op: "open", Path: p, Err: errors.New("injected write failure")}
	}
	return nil
}

func (f failingFS) Create(p string) (billy.File, error) {
	return f.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

func (f failingFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	if err := f.fail(p, flag); err != nil {
		return nil, err
	}
	return f.Filesystem.OpenFile(p, flag, perm)
}

// A reset, merge or checkout that fails after it began leaves HEAD (the
// branch and HEAD's own form) and the index as they were.
func TestFailedSwitch_RestoresHEADAndTheBranch(t *testing.T) {
	var bad string
	fs := failingFS{Filesystem: memfs.New(), bad: &bad}
	r, err := Init(fs, "")
	if err != nil {
		t.Fatal(err)
	}
	c := newClock()
	write(t, fs, "a.txt", "1\n")
	base := commitAll(t, r, c, "base")
	if err := r.Checkout(CheckoutOptions{Branch: "next", Create: true}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "boom.txt", "b\n")
	next := commitAll(t, r, c, "next")
	if err := r.Checkout(CheckoutOptions{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	bad = "boom.txt"

	check := func(op string, want HeadInfo) {
		t.Helper()
		if h, _ := r.Head(); h != want {
			t.Errorf("the failed %s left HEAD %+v, want %+v", op, h, want)
		}
		if b, _ := r.repo.Reference(plumbing.NewBranchReferenceName("main"), false); b == nil || b.Hash().String() != base {
			t.Errorf("the failed %s moved main", op)
		}
		if st := statusMap(t, r); len(st) != 0 {
			t.Errorf("the failed %s left status %v", op, st)
		}
	}
	onMain := HeadInfo{Branch: "main", Hash: base}
	if err := r.Reset("hard", next, nil); err == nil {
		t.Fatal("the hard reset did not fail")
	}
	check("hard reset", onMain)
	if _, err := r.MergeFF(next); err == nil {
		t.Fatal("the merge did not fail")
	}
	check("merge", onMain)
	if err := r.Checkout(CheckoutOptions{Branch: "next"}); err == nil {
		t.Fatal("the checkout did not fail")
	}
	check("checkout", onMain)
	if err := r.Checkout(CheckoutOptions{Branch: "next", Force: true}); err == nil {
		t.Fatal("the forced checkout did not fail")
	}
	check("forced checkout", onMain)

	// Detached, HEAD goes back to the same hash, still detached.
	bad = ""
	if err := r.Checkout(CheckoutOptions{Rev: base}); err != nil {
		t.Fatal(err)
	}
	bad = "boom.txt"
	if err := r.Checkout(CheckoutOptions{Rev: next, Force: true}); err == nil {
		t.Fatal("the detached forced checkout did not fail")
	}
	check("detached checkout", HeadInfo{Hash: base, Detached: true})
}

// A lease is checked against the destination, but the library checks it
// against a ref named like the source; a renamed refspec is refused rather
// than leased against the wrong ref.
func TestPush_ForceWithLeaseRefusesARenamingRefspec(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	commitAll(t, r, c, "first")
	if _, err := r.repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"https://example.com/a.git"}}); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"refs/heads/main:refs/heads/other", "+refs/heads/a*:refs/heads/b*"} {
		_, err := r.Push(context.Background(), Network{}, PushOptions{RefSpecs: []string{spec}, ForceWithLease: true})
		if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "lease") {
			t.Errorf("force-with-lease with %s: %v, want invalid_argument naming the lease", spec, err)
		}
	}
}

// A tree of nested directories that reference one subtree many times holds
// no file, yet walking it is the expansion the limit exists for: the walk
// that builds a commit's file map counts every entry it visits.
func TestCommitSide_CountsDirectoriesAgainstTheLimit(t *testing.T) {
	r, _, c := memRepo(t)
	encode := func(o interface {
		Encode(plumbing.EncodedObject) error
	}) plumbing.Hash {
		return encodeObject(t, r, o)
	}
	level := encode(&object.Tree{})
	for depth := 0; depth < 4; depth++ {
		var entries []object.TreeEntry
		for i := 0; i < 10; i++ {
			entries = append(entries, object.TreeEntry{Name: fmt.Sprintf("d%d", i), Mode: filemode.Dir, Hash: level})
		}
		level = encode(&object.Tree{Entries: entries})
	}
	sig := object.Signature{Name: "x", Email: "x@x", When: c.sig().When}
	h := encode(&object.Commit{Author: sig, Committer: sig, Message: "dirs", TreeHash: level})
	commit, err := r.repo.CommitObject(h)
	if err != nil {
		t.Fatal(err)
	}
	r.SetLimits(Limits{MaxFiles: 1000})
	if _, err := r.commitSide(commit); Class(err) != "source_too_large" {
		t.Errorf("a tree of 11110 directory entries with a limit of 1000: %v, want source_too_large", err)
	}
}
