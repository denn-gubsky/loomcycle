package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// treeBomb stores a commit whose tree is 64 directories of 64 directories,
// four levels deep, over one tree of 64 files: a few kilobytes of objects
// that expand to 64^5 (over a billion) paths. dirName names the directories.
func treeBomb(t *testing.T, r *Repo, c *clock, dirName func(int) string) plumbing.Hash {
	t.Helper()
	blob := storeBlob(t, r, "x\n")
	var files []object.TreeEntry
	for i := 0; i < 64; i++ {
		files = append(files, object.TreeEntry{Name: fmt.Sprintf("f%02d", i), Mode: filemode.Regular, Hash: blob})
	}
	h := storeObject(t, r, &object.Tree{Entries: files})
	for lvl := 0; lvl < 4; lvl++ {
		var dirs []object.TreeEntry
		for i := 0; i < 64; i++ {
			dirs = append(dirs, object.TreeEntry{Name: dirName(i), Mode: filemode.Dir, Hash: h})
		}
		h = storeObject(t, r, &object.Tree{Entries: dirs})
	}
	s := c.sig()
	sig := object.Signature{Name: s.Name, Email: s.Email, When: s.When}
	return storeObject(t, r, &object.Commit{TreeHash: h, Author: sig, Committer: sig, Message: "bomb\n"})
}

// within runs fn and fails the test when it has not returned in d: an
// unbounded walk would otherwise hang the suite rather than fail it.
func within(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("still running after %v: the walk is not bounded", d)
	}
}

// expired is a context whose deadline has passed.
func expired(t *testing.T) context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}

func wantTooLarge(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, gitsource.ErrTooLarge) || Class(err) != "source_too_large" {
		t.Errorf("%s: %v (class %q), want source_too_large", what, err, Class(err))
	}
}

func wantTimeout(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrTimeout) || Class(err) != "timeout" {
		t.Errorf("%s: %v (class %q), want timeout", what, err, Class(err))
	}
}

func dDir(i int) string   { return fmt.Sprintf("d%02d", i) }
func hexDir(i int) string { return fmt.Sprintf("%02x", i) }

func TestGrep_RefusesATreeBombPastTheFileLimit(t *testing.T) {
	r, _, c := memRepo(t)
	bomb := treeBomb(t, r, c, dDir)
	// Most entries are files, each read: a limit below the default keeps
	// the test quick under -race; the default refuses in under a second.
	r.SetLimits(Limits{MaxFiles: 10000})
	var err error
	within(t, 10*time.Second, func() {
		_, _, err = r.Grep(t.Context(), GrepOptions{Pattern: "nomatch", Rev: bomb.String(), MaxMatches: 10})
	})
	wantTooLarge(t, "Grep of a tree bomb", err)
}

// A path filter keeps the walk out of the directories it cannot reach, so
// a filtered search of a huge tree costs what the filtered part does.
func TestGrep_PrunesDirectoriesThePathsCannotReach(t *testing.T) {
	r, _, c := memRepo(t)
	bomb := treeBomb(t, r, c, dDir)
	within(t, 10*time.Second, func() {
		got, trunc, err := r.Grep(t.Context(), GrepOptions{Pattern: "x", Rev: bomb.String(), Paths: []string{"zzz"}, MaxMatches: 10})
		if err != nil || len(got) != 0 || trunc {
			t.Errorf("Grep under a path the tree lacks = %v, %v, %v; want nothing", got, trunc, err)
		}
		got, _, err = r.Grep(t.Context(), GrepOptions{Pattern: "x", Rev: bomb.String(), Paths: []string{"d00/d01/d02/d03/f04"}, MaxMatches: 10})
		if err != nil || len(got) != 1 || got[0].Path != "d00/d01/d02/d03/f04" {
			t.Errorf("Grep of one deep file = %+v, %v", got, err)
		}
		// "d0" is not a prefix of "d00" as a path is.
		if got, _, err = r.Grep(t.Context(), GrepOptions{Pattern: "x", Rev: bomb.String(), Paths: []string{"d0"}, MaxMatches: 10}); err != nil || len(got) != 0 {
			t.Errorf("Grep under d0 = %+v, %v; want nothing", got, err)
		}
	})
}

func TestGrep_RefusesPastTheByteLimit(t *testing.T) {
	r, fs, c := memRepo(t)
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		write(t, fs, f, strings.Repeat("line\n", 200)) // 1000 bytes
	}
	commitAll(t, r, c, "three files")
	if _, _, err := r.Grep(t.Context(), GrepOptions{Pattern: "line", MaxMatches: 1000}); err != nil {
		t.Fatalf("Grep within the default limits: %v", err)
	}
	r.SetLimits(Limits{MaxBytes: 2500})
	_, _, err := r.Grep(t.Context(), GrepOptions{Pattern: "nomatch", MaxMatches: 10})
	wantTooLarge(t, "Grep past MaxBytes", err)
}

func TestGrep_StopsAtTheDeadline(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	commitAll(t, r, c, "first")
	_, _, err := r.Grep(expired(t), GrepOptions{Pattern: "a", MaxMatches: 10})
	wantTimeout(t, "Grep past its deadline", err)
}

func TestGrep_CutsALongPathAndMarksIt(t *testing.T) {
	r, fs, c := memRepo(t)
	long := strings.Repeat(strings.Repeat("p", 200)+"/", 6) + "f.txt"
	write(t, fs, long, "hit\n")
	write(t, fs, "short.txt", "hit\n")
	commitAll(t, r, c, "long path")
	got, _, err := r.Grep(t.Context(), GrepOptions{Pattern: "hit", MaxMatches: 10})
	if err != nil || len(got) != 2 {
		t.Fatalf("Grep = %+v, %v", got, err)
	}
	if m := got[0]; len(m.Path) != maxGrepPath || !m.PathTruncated || !strings.HasPrefix(long, m.Path) {
		t.Errorf("long path match: %d bytes, truncated %v", len(m.Path), m.PathTruncated)
	}
	if m := got[1]; m.Path != "short.txt" || m.PathTruncated {
		t.Errorf("short path match = %+v", m)
	}
}

func TestNotes_RefusesATreeBombPastTheFileLimit(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	commitAll(t, r, c, "first")
	// Hex directory names: the fan-out a notes tree really uses.
	bomb := treeBomb(t, r, c, hexDir)
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(notesRef, bomb)); err != nil {
		t.Fatal(err)
	}
	var err error
	within(t, 10*time.Second, func() { _, err = r.Notes(t.Context(), "", 10) })
	wantTooLarge(t, "Notes of a tree-bomb notes ref", err)
}

// notesCommit stores a notes commit whose tree holds one note per name.
func notesCommit(t *testing.T, r *Repo, c *clock, names []string) plumbing.Hash {
	t.Helper()
	blob := storeBlob(t, r, "note\n")
	var es []object.TreeEntry
	for _, n := range names {
		es = append(es, object.TreeEntry{Name: n, Mode: filemode.Regular, Hash: blob})
	}
	tree := storeObject(t, r, &object.Tree{Entries: es})
	s := c.sig()
	sig := object.Signature{Name: s.Name, Email: s.Email, When: s.When}
	return storeObject(t, r, &object.Commit{TreeHash: tree, Author: sig, Committer: sig, Message: "notes\n"})
}

func TestNotes_RefusesMoreNotesThanTheCap(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	commitAll(t, r, c, "first")
	names := make([]string, maxNotes+1)
	for i := range names {
		names[i] = fmt.Sprintf("%040x", i+1)
	}
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(notesRef, notesCommit(t, r, c, names))); err != nil {
		t.Fatal(err)
	}
	_, err := r.Notes(t.Context(), "", 10)
	wantTooLarge(t, "Notes of a tree over the note cap", err)
	// At the cap, it lists.
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(notesRef, notesCommit(t, r, c, names[:maxNotes]))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Notes(t.Context(), "", 10); err != nil {
		t.Errorf("Notes of a tree at the cap: %v", err)
	}
}

func TestNotes_StopsAtTheDeadline(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	h := commitAll(t, r, c, "first")
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(notesRef, notesCommit(t, r, c, []string{h}))); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Notes(t.Context(), "", 10); err != nil || len(got) != 1 {
		t.Fatalf("Notes = %+v, %v", got, err)
	}
	_, err := r.Notes(expired(t), "", 10)
	wantTimeout(t, "Notes past its deadline", err)
}

// A change over the diff line limit that touches none of the lines still
// being blamed (a reformat further down the file) does not stop the blame.
func TestBlame_KeepsBlamingPastALargeChangeElsewhere(t *testing.T) {
	r, fs, c := memRepo(t)
	var top, a, b strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&top, "top%d\n", i)
	}
	for i := 0; i <= maxDiffLines; i++ {
		fmt.Fprintf(&a, "a%d\n", i)
		fmt.Fprintf(&b, "b%d\n", i)
	}
	write(t, fs, "f.txt", top.String()+a.String())
	first := commitAs(t, r, c, "Ann", "first")
	write(t, fs, "f.txt", top.String()+b.String())
	commitAs(t, r, c, "Bob", "reformat the rest")
	res, err := r.Blame(t.Context(), "f.txt", "", 3)
	if err != nil {
		t.Fatalf("Blame of lines a large change does not touch: %v", err)
	}
	for _, l := range res.Lines {
		if l.Hash != first || l.Author != "Ann" {
			t.Errorf("line %d blamed on %s (%s), want %s", l.Line, l.Hash, l.Author, first)
		}
	}
	// The same change refuses the blame of a line inside it.
	if _, err := r.Blame(t.Context(), "f.txt", "", 11); !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "too large to blame") {
		t.Errorf("Blame of a line inside a %d-line change: %v", maxDiffLines+1, err)
	}
}

func TestBlame_RefusesPastTheByteBudget(t *testing.T) {
	r, fs, c := memRepo(t)
	body := strings.Repeat("same\n", 200) // 1000 bytes under line 1
	for i := 0; i < 4; i++ {
		write(t, fs, "f.txt", fmt.Sprintf("v%d\n", i)+body)
		commitAll(t, r, c, fmt.Sprintf("v%d", i))
	}
	// Line 2 is the first version's: each of the three later commits
	// compares two versions of about 1000 bytes to pass it on.
	if res, err := r.Blame(t.Context(), "f.txt", "", 2); err != nil || len(res.Lines) != 2 {
		t.Fatalf("Blame within the default limits = %+v, %v", res, err)
	}
	r.SetLimits(Limits{MaxBytes: 3000})
	_, err := r.Blame(t.Context(), "f.txt", "", 2)
	wantTooLarge(t, "Blame past its byte budget", err)
}

func TestBlame_StopsAtTheDeadline(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "f.txt", "a\n")
	commitAll(t, r, c, "first")
	_, err := r.Blame(expired(t), "f.txt", "", 1)
	wantTimeout(t, "Blame past its deadline", err)
}

// The operator's limits bind a submodule already on disk as they do a
// fresh clone of one.
func TestSubmoduleUpdate_AppliesTheOperatorLimitsToAnExistingSubmodule(t *testing.T) {
	wt, dir := osWorktree(t)
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "init", "-q", "-b", "main", "lib")
	writeOS(t, dir, "lib/f.txt", "sub\n")
	run(t, dir+"/lib", "add", ".")
	run(t, dir+"/lib", "commit", "-q", "-m", "sub1")
	for _, f := range []string{"a", "b", "c", "d", "e"} {
		writeOS(t, dir, "lib/"+f+".txt", f+"\n")
	}
	run(t, dir+"/lib", "add", ".")
	run(t, dir+"/lib", "commit", "-q", "-m", "sub2: six files")
	big := run(t, dir+"/lib", "rev-parse", "HEAD")
	run(t, dir+"/lib", "checkout", "-q", "HEAD~1")
	writeOS(t, dir, ".gitmodules", "[submodule \"lib\"]\n\tpath = lib\n\turl = https://x.example/lib.git\n")
	run(t, dir, "add", ".gitmodules")
	run(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+big+",lib")
	run(t, dir, "commit", "-q", "-m", "with lib")
	r, err := Open(wt)
	if err != nil {
		t.Fatal(err)
	}
	r.SetLimits(Limits{MaxFiles: 3})
	// The commit is in the submodule already: nothing is dialled.
	n := Network{URL: "https://x.example/lib.git", MaxObjectBytes: 1 << 20, SpoolDir: t.TempDir()}
	_, err = r.SubmoduleUpdate(t.Context(), "lib", n)
	wantTooLarge(t, "SubmoduleUpdate to a commit over MaxFiles", err)
	if head := run(t, dir+"/lib", "rev-parse", "HEAD"); head == big {
		t.Error("the submodule was checked out past the operator's MaxFiles")
	}
}

// populatedGitlink makes a repository whose first commit has a.txt, and
// whose HEAD adds gone.txt and a submodule at lib, with lib then holding the
// submodule's repository as submodule_update leaves it.
func populatedGitlink(t *testing.T) (r *Repo, fs billy.Filesystem, c *clock, first, withLib string) {
	t.Helper()
	r, fs, c = memRepo(t)
	write(t, fs, "a.txt", "a\n")
	first = commitAll(t, r, c, "first")
	write(t, fs, "gone.txt", "g\n")
	write(t, fs, ".gitmodules", "[submodule \"lib\"]\n\tpath = lib\n\turl = https://x.example/lib.git\n")
	if err := r.Add([]string{"gone.txt", ".gitmodules"}); err != nil {
		t.Fatal(err)
	}
	idx, err := r.repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	idx.Entries = append(idx.Entries, &index.Entry{Name: "lib", Mode: filemode.Submodule, Hash: mustHash("1111111111111111111111111111111111111111")})
	if err := r.repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
	s := c.sig()
	if withLib, err = r.Commit(CommitOptions{Message: "add lib", Author: s, Committer: s}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "lib/.git/HEAD", "1111111111111111111111111111111111111111\n")
	write(t, fs, "lib/f.txt", "sub\n")
	return r, fs, c, first, withLib
}

func assertSubmoduleKept(t *testing.T, fs billy.Filesystem) {
	t.Helper()
	if !exists(fs, "lib/.git/HEAD") || !exists(fs, "lib/f.txt") {
		t.Error("the submodule's repository or files were removed")
	}
}

// A hard reset, or a forced checkout, to a commit without the submodule
// leaves its populated directory in place, as git does, and resets the
// rest.
func TestReset_HardLeavesAPopulatedSubmoduleInPlace(t *testing.T) {
	t.Run("reset", func(t *testing.T) {
		r, fs, _, first, _ := populatedGitlink(t)
		if err := r.Reset("hard", first, nil); err != nil {
			t.Fatalf("Reset --hard past a populated submodule: %v", err)
		}
		assertSubmoduleKept(t, fs)
		if exists(fs, "gone.txt") || exists(fs, ".gitmodules") {
			t.Error("the files the target lacks were not removed")
		}
	})
	t.Run("forced checkout", func(t *testing.T) {
		r, fs, _, first, _ := populatedGitlink(t)
		if err := r.Checkout(CheckoutOptions{Rev: first, Force: true}); err != nil {
			t.Fatalf("forced checkout past a populated submodule: %v", err)
		}
		assertSubmoduleKept(t, fs)
		if exists(fs, "gone.txt") {
			t.Error("gone.txt was not removed")
		}
	})
}

// A hard reset to a commit with a file where the submodule's repository
// is refuses before any file is touched: writing the file would remove the
// submodule's files and then fail at its .git.
func TestReset_HardRefusesAFileOverAPopulatedSubmodule(t *testing.T) {
	r, fs, c, _, withLib := populatedGitlink(t)
	a := storeBlob(t, r, "a\n")
	f := storeBlob(t, r, "now a file\n")
	tree := storeObject(t, r, &object.Tree{Entries: []object.TreeEntry{
		{Name: "a.txt", Mode: filemode.Regular, Hash: a},
		{Name: "lib", Mode: filemode.Regular, Hash: f},
	}})
	s := c.sig()
	sig := object.Signature{Name: s.Name, Email: s.Email, When: s.When}
	target := storeObject(t, r, &object.Commit{TreeHash: tree, Author: sig, Committer: sig, Message: "lib is a file\n", ParentHashes: []plumbing.Hash{mustHash(withLib)}})
	err := r.Reset("hard", target.String(), nil)
	if !errors.Is(err, ErrWorktreeDirty) {
		t.Errorf("Reset --hard writing a file over a populated submodule: %v, want worktree_dirty", err)
	}
	assertSubmoduleKept(t, fs)
	if !exists(fs, "gone.txt") || !exists(fs, ".gitmodules") {
		t.Error("files were removed before the reset was refused")
	}
	if head, err := r.headCommit(); err != nil || head.Hash.String() != withLib {
		t.Errorf("HEAD after the refused reset = %v, %v; want %s", head, err, withLib)
	}
}
