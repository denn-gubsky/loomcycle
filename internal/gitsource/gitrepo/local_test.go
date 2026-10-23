package gitrepo

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func mustHash(s string) plumbing.Hash { return plumbing.NewHash(s) }

func tagOpts(s Signature, msg string) *git.CreateTagOptions {
	return &git.CreateTagOptions{Tagger: &object.Signature{Name: s.Name, Email: s.Email, When: s.When}, Message: msg}
}

func TestInit_CreatesAnUnbornRepositoryAndRefusesASecond(t *testing.T) {
	r, fs, _ := memRepo(t)
	h, err := r.Head()
	if err != nil || h != (HeadInfo{}) {
		t.Fatalf("Head of a new repository = %+v, %v; want unborn", h, err)
	}
	if fi, err := fs.Lstat(".git"); err != nil || !fi.IsDir() {
		t.Fatalf(".git: %v", err)
	}
	if !strings.Contains(read(t, fs, ".git/HEAD"), "refs/heads/main") {
		t.Errorf("HEAD = %q, want refs/heads/main", read(t, fs, ".git/HEAD"))
	}
	if _, err := Init(fs, ""); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("a second Init: %v, want already_exists", err)
	}
	other := memfs.New()
	if _, err := Init(other, "-evil"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Init with a branch named -evil: %v, want invalid_argument", err)
	}
	if _, err := Init(other, "trunk"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, other, ".git/HEAD"), "refs/heads/trunk") {
		t.Errorf("HEAD = %q, want refs/heads/trunk", read(t, other, ".git/HEAD"))
	}
}

// A .git file is a pointer that can name any directory; only a .git
// directory is a repository.
func TestOpen_RefusesNoGitAndAGitFile(t *testing.T) {
	if _, err := Open(memfs.New()); !errors.Is(err, ErrNotARepository) {
		t.Errorf("Open of an empty directory: %v, want not_a_repository", err)
	}
	fs := memfs.New()
	write(t, fs, ".git", "gitdir: /etc\n")
	if _, err := Open(fs); !errors.Is(err, ErrNotARepository) {
		t.Errorf("Open of a .git file: %v, want not_a_repository", err)
	}
	_, mfs, _ := memRepo(t)
	if _, err := Open(mfs); err != nil {
		t.Errorf("Open of a repository: %v", err)
	}
}

func TestStatusAddCommit_RecordsSignaturesAndRefusesNothing(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	write(t, fs, "dir/b.txt", "b\n")
	if got := statusMap(t, r); got["a.txt"] != "??" || got["dir/b.txt"] != "??" {
		t.Fatalf("status of new files = %v, want ??", got)
	}
	if err := r.Add([]string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := statusMap(t, r); got["a.txt"] != "A " || got["dir/b.txt"] != "??" {
		t.Fatalf("status after adding a.txt = %v", got)
	}
	author := Signature{Name: "Ann", Email: "ann@example.com", When: c.sig().When}
	committer := Signature{Name: "Bot", Email: "bot@example.com", When: c.sig().When}
	h, err := r.Commit(CommitOptions{Message: "add a", Author: author, Committer: committer})
	if err != nil {
		t.Fatal(err)
	}
	log, err := r.Log(LogOptions{Max: 10})
	if err != nil || len(log) != 1 {
		t.Fatalf("Log = %v, %v", log, err)
	}
	got := log[0]
	if got.Hash != h || got.Author != "Ann" || got.AuthorEmail != "ann@example.com" || got.Committer != "Bot" ||
		got.CommitterEmail != "bot@example.com" || !got.AuthorDate.Equal(author.When) || !got.CommitDate.Equal(committer.When) ||
		strings.TrimSpace(got.Message) != "add a" || len(got.Parents) != 0 {
		t.Errorf("commit = %+v", got)
	}

	// Nothing staged: refused, unless empty commits are allowed.
	s := c.sig()
	if _, err := r.Commit(CommitOptions{Message: "again", Author: s, Committer: s}); !errors.Is(err, ErrNothingToCommit) {
		t.Errorf("a commit with nothing staged: %v, want nothing_to_commit", err)
	}
	if _, err := r.Commit(CommitOptions{Message: "empty", Author: s, Committer: s, AllowEmpty: true}); err != nil {
		t.Errorf("an allowed empty commit: %v", err)
	}
	// Required fields.
	if _, err := r.Commit(CommitOptions{Message: "  ", Author: s, Committer: s}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a blank message: %v", err)
	}
	if _, err := r.Commit(CommitOptions{Message: "m", Author: s}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("no committer: %v", err)
	}

	// All stages modified tracked files, not untracked ones; Amend replaces.
	write(t, fs, "a.txt", "a2\n")
	h2, err := r.Commit(CommitOptions{Message: "modify a", Author: s, Committer: s, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := statusMap(t, r); got["a.txt"] != "" || got["dir/b.txt"] != "??" {
		t.Errorf("status after commit -a = %v", got)
	}
	h3, err := r.Commit(CommitOptions{Message: "modify a, reworded", Author: s, Committer: s, Amend: true})
	if err != nil || h3 == h2 {
		t.Fatalf("Amend = %s, %v", h3, err)
	}
	log, _ = r.Log(LogOptions{Max: 10})
	if len(log) != 3 || strings.TrimSpace(log[0].Message) != "modify a, reworded" {
		t.Errorf("log after amend = %+v", log)
	}
}

func TestStatus_UsesGitLetters(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "keep.txt", "k\n")
	write(t, fs, "mod.txt", "m\n")
	write(t, fs, "del.txt", "d\n")
	commitAll(t, r, c, "base")
	write(t, fs, "mod.txt", "m2\n")
	if err := fs.Remove("del.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "new.txt", "n\n")
	if err := r.Add([]string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	st, err := r.Status()
	if err != nil {
		t.Fatal(err)
	}
	want := []FileStatus{{"del.txt", " ", "D"}, {"mod.txt", " ", "M"}, {"new.txt", "A", " "}}
	if fmt.Sprint(st) != fmt.Sprint(want) {
		t.Errorf("Status = %v, want %v (sorted, clean files left out)", st, want)
	}
}

func TestLog_HonoursMaxPathAndRev(t *testing.T) {
	r, fs, c := memRepo(t)
	var hashes []string
	for i := 0; i < 5; i++ {
		name := "a.txt"
		if i%2 == 1 {
			name = "b.txt"
		}
		write(t, fs, name, fmt.Sprintf("%d\n", i))
		hashes = append(hashes, commitAll(t, r, c, fmt.Sprintf("c%d", i)))
	}
	if _, err := r.Log(LogOptions{}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Log without max: %v", err)
	}
	log, err := r.Log(LogOptions{Max: 2})
	if err != nil || len(log) != 2 || log[0].Hash != hashes[4] || log[1].Hash != hashes[3] {
		t.Errorf("Log max 2 = %v, %v", log, err)
	}
	log, err = r.Log(LogOptions{Max: 10, Path: "b.txt"})
	if err != nil || len(log) != 2 || log[0].Hash != hashes[3] || log[1].Hash != hashes[1] {
		t.Errorf("Log of b.txt = %+v, %v", log, err)
	}
	log, err = r.Log(LogOptions{Max: 10, Rev: "HEAD~3"})
	if err != nil || len(log) != 2 || log[0].Hash != hashes[1] {
		t.Errorf("Log from HEAD~3 = %+v, %v", log, err)
	}
	if _, err := r.Log(LogOptions{Max: 1, Rev: "nope"}); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("Log of an unknown rev: %v", err)
	}
	if _, err := r.Log(LogOptions{Max: 1, Path: "../x"}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Log of ../x: %v", err)
	}
	empty, _, _ := memRepo(t)
	if log, err := empty.Log(LogOptions{Max: 1}); err != nil || len(log) != 0 {
		t.Errorf("Log of an unborn branch = %v, %v", log, err)
	}
}

func TestShow_PatchAgainstFirstParentOrEmptyTree(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "one\n")
	root := commitAll(t, r, c, "root")
	write(t, fs, "a.txt", "two\n")
	second := commitAll(t, r, c, "second")

	info, patch, truncated, err := r.Show(root, 1<<20)
	if err != nil || truncated || info.Hash != root {
		t.Fatalf("Show root = %+v, %v, %v", info, truncated, err)
	}
	for _, want := range []string{"diff --git a/a.txt b/a.txt", "new file mode 100644", "--- /dev/null", "+++ b/a.txt", "+one"} {
		if !strings.Contains(patch, want) {
			t.Errorf("root patch lacks %q:\n%s", want, patch)
		}
	}
	info, patch, _, err = r.Show("", 1<<20)
	if err != nil || info.Hash != second || len(info.Parents) != 1 || info.Parents[0] != root {
		t.Fatalf("Show HEAD = %+v, %v", info, err)
	}
	if !strings.Contains(patch, "-one") || !strings.Contains(patch, "+two") {
		t.Errorf("second patch:\n%s", patch)
	}
	_, patch, truncated, err = r.Show(second, 10)
	if err != nil || !truncated || len(patch) > 10 {
		t.Errorf("Show cut at 10 = %q, %v, %v", patch, truncated, err)
	}
	if _, _, _, err := r.Show(second, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Show without a limit: %v", err)
	}
}

func TestDiff_AllFourModesTruncationBinaryAndPaths(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "alpha\n")
	write(t, fs, "b.txt", "beta\n")
	write(t, fs, "bin.dat", "x\x00y")
	first := commitAll(t, r, c, "first")
	write(t, fs, "a.txt", "alpha committed\n")
	write(t, fs, "bin.dat", "x\x00z")
	second := commitAll(t, r, c, "second")

	// Staged: a new file. Unstaged: a change to b.txt.
	write(t, fs, "new.txt", "fresh\n")
	if err := r.Add([]string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "b.txt", "beta changed\n")

	diff := func(o DiffOptions) string {
		t.Helper()
		if o.MaxBytes == 0 {
			o.MaxBytes = 1 << 20
		}
		p, truncated, err := r.Diff(o)
		if err != nil || truncated {
			t.Fatalf("Diff(%+v) = %v, %v", o, truncated, err)
		}
		return p
	}

	unstaged := diff(DiffOptions{})
	if !strings.Contains(unstaged, "diff --git a/b.txt b/b.txt") || !strings.Contains(unstaged, "+beta changed") || strings.Contains(unstaged, "new.txt") {
		t.Errorf("unstaged diff:\n%s", unstaged)
	}
	staged := diff(DiffOptions{Staged: true})
	if !strings.Contains(staged, "new file mode 100644") || !strings.Contains(staged, "+fresh") || strings.Contains(staged, "b.txt") {
		t.Errorf("staged diff:\n%s", staged)
	}
	vsWorktree := diff(DiffOptions{From: first})
	for _, want := range []string{"+alpha committed", "+beta changed", "+fresh", "Binary files a/bin.dat and b/bin.dat differ"} {
		if !strings.Contains(vsWorktree, want) {
			t.Errorf("diff from first to the worktree lacks %q:\n%s", want, vsWorktree)
		}
	}
	commits := diff(DiffOptions{From: first, To: second})
	if !strings.Contains(commits, "-alpha\n+alpha committed") || !strings.Contains(commits, "Binary files a/bin.dat and b/bin.dat differ") ||
		strings.Contains(commits, "beta") {
		t.Errorf("diff between commits:\n%s", commits)
	}
	if !strings.HasPrefix(commits, "diff --git a/a.txt b/a.txt\nindex ") || !strings.Contains(commits, "\n--- a/a.txt\n+++ b/a.txt\n@@ ") {
		t.Errorf("not git's format:\n%s", commits)
	}
	limited := diff(DiffOptions{From: first, To: second, Paths: []string{"a.txt"}})
	if strings.Contains(limited, "bin.dat") || !strings.Contains(limited, "a.txt") {
		t.Errorf("diff limited to a.txt:\n%s", limited)
	}

	p, truncated, err := r.Diff(DiffOptions{From: first, To: second, MaxBytes: 25})
	if err != nil || !truncated || len(p) > 25 || !strings.HasPrefix(commits, p) {
		t.Errorf("truncated diff = %q, %v, %v", p, truncated, err)
	}
	for _, bad := range []DiffOptions{
		{MaxBytes: 0},
		{To: second, MaxBytes: 10},
		{From: first, To: second, Staged: true, MaxBytes: 10},
		{Paths: []string{".git/config"}, MaxBytes: 10},
	} {
		if _, _, err := r.Diff(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Diff(%+v): %v, want invalid_argument", bad, err)
		}
	}
	if _, _, err := r.Diff(DiffOptions{From: "nope", MaxBytes: 10}); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("Diff from an unknown rev: %v", err)
	}
}

func TestCheckout_SwitchesCreatesDetachesAndKeepsWork(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "main\n")
	base := commitAll(t, r, c, "base")
	if err := r.Checkout(CheckoutOptions{Branch: "feature", Create: true}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "a.txt", "feature\n")
	write(t, fs, "f.txt", "only on feature\n")
	feature := commitAll(t, r, c, "feature work")

	if err := r.Checkout(CheckoutOptions{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.Head(); h.Branch != "main" || h.Hash != base {
		t.Errorf("Head after switching to main = %+v", h)
	}
	if read(t, fs, "a.txt") != "main\n" || exists(fs, "f.txt") {
		t.Errorf("worktree on main: a.txt=%q f.txt exists=%v", read(t, fs, "a.txt"), exists(fs, "f.txt"))
	}

	// A local change is not lost to a checkout.
	write(t, fs, "a.txt", "uncommitted\n")
	if err := r.Checkout(CheckoutOptions{Branch: "feature"}); !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("checkout over a local change: %v, want worktree_dirty", err)
	}
	if h, _ := r.Head(); h.Branch != "main" || read(t, fs, "a.txt") != "uncommitted\n" {
		t.Errorf("a refused checkout moved something: head %+v, a.txt %q", h, read(t, fs, "a.txt"))
	}
	// Nor is a staged one.
	if err := r.Add([]string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Checkout(CheckoutOptions{Branch: "feature"}); !errors.Is(err, ErrWorktreeDirty) {
		t.Errorf("checkout over a staged change: %v, want worktree_dirty", err)
	}
	// Nor an untracked file the target would write over.
	if err := r.Restore([]string{"a.txt"}, true, true); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "f.txt", "mine\n")
	if err := r.Checkout(CheckoutOptions{Branch: "feature"}); !errors.Is(err, ErrWorktreeDirty) {
		t.Errorf("checkout over an untracked file: %v, want worktree_dirty", err)
	}
	// Force discards what is in the way, and only that: an untracked file
	// the target does not have survives, as with git.
	write(t, fs, "notes.txt", "my notes\n")
	if err := r.Checkout(CheckoutOptions{Branch: "feature", Force: true}); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.Head(); h.Branch != "feature" || h.Hash != feature {
		t.Errorf("Head after a forced checkout = %+v", h)
	}
	if read(t, fs, "f.txt") != "only on feature\n" || read(t, fs, "a.txt") != "feature\n" || read(t, fs, "notes.txt") != "my notes\n" {
		t.Errorf("forced checkout: f.txt %q a.txt %q", read(t, fs, "f.txt"), read(t, fs, "a.txt"))
	}
	if st := statusMap(t, r); len(st) != 1 || st["notes.txt"] != "??" {
		t.Errorf("status after a forced checkout = %v", st)
	}
	// So does an ordinary one.
	if err := r.Checkout(CheckoutOptions{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if read(t, fs, "notes.txt") != "my notes\n" || exists(fs, "f.txt") {
		t.Error("a checkout lost an untracked file or kept a tracked one")
	}
	if err := r.Checkout(CheckoutOptions{Branch: "feature"}); err != nil {
		t.Fatal(err)
	}

	// Detached at a revision.
	if err := r.Checkout(CheckoutOptions{Rev: base}); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.Head(); !h.Detached || h.Hash != base || h.Branch != "" {
		t.Errorf("detached Head = %+v", h)
	}
	// Create at a start point.
	if err := r.Checkout(CheckoutOptions{Branch: "from-feature", Rev: feature, Create: true}); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.Head(); h.Branch != "from-feature" || h.Hash != feature {
		t.Errorf("Head after create = %+v", h)
	}
	for _, bad := range []CheckoutOptions{
		{},
		{Branch: "main", Rev: base},
		{Branch: "main", Force: true, Keep: true},
		{Branch: "-x", Create: true},
	} {
		if err := r.Checkout(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Checkout(%+v): %v, want invalid_argument", bad, err)
		}
	}
	if err := r.Checkout(CheckoutOptions{Branch: "main", Create: true}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("creating an existing branch: %v", err)
	}
	if err := r.Checkout(CheckoutOptions{Branch: "missing"}); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("checkout of a missing branch: %v", err)
	}
}

func TestBranchCreateDelete_Rules(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	base := commitAll(t, r, c, "base")
	if err := r.BranchCreate("topic", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.BranchCreate("topic", ""); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("a second topic: %v", err)
	}
	for _, bad := range []string{"-f", "a..b", "x y", "HEAD", "refs/heads/x", ""} {
		if err := r.BranchCreate(bad, ""); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("BranchCreate(%q): %v, want invalid_argument", bad, err)
		}
	}
	if err := r.BranchCreate("x", "nope"); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("BranchCreate at an unknown rev: %v", err)
	}

	// A branch with a commit main lacks is not deleted without force.
	if err := r.Checkout(CheckoutOptions{Branch: "topic"}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "t.txt", "t\n")
	commitAll(t, r, c, "topic work")
	if err := r.BranchDelete("topic", true); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("deleting the current branch: %v", err)
	}
	if err := r.Checkout(CheckoutOptions{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := r.BranchDelete("topic", false); !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "not fully merged") {
		t.Errorf("deleting an unmerged branch: %v", err)
	}
	if err := r.BranchDelete("topic", true); err != nil {
		t.Errorf("forced delete: %v", err)
	}
	// A merged one goes without force.
	if err := r.BranchCreate("merged", base); err != nil {
		t.Fatal(err)
	}
	if err := r.BranchDelete("merged", false); err != nil {
		t.Errorf("deleting a merged branch: %v", err)
	}
	bs, err := r.Branches()
	if err != nil || len(bs) != 1 || bs[0].Name != "main" || !bs[0].Current || bs[0].Hash != base {
		t.Errorf("Branches = %+v, %v", bs, err)
	}
	if err := r.BranchDelete("gone", true); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("deleting a missing branch: %v", err)
	}
}

func TestMergeFF_FastForwardsAndRefusesDiverged(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	base := commitAll(t, r, c, "base")
	if err := r.Checkout(CheckoutOptions{Branch: "ahead", Create: true}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "b.txt", "b\n")
	ahead := commitAll(t, r, c, "ahead")
	if err := r.Checkout(CheckoutOptions{Branch: "main"}); err != nil {
		t.Fatal(err)
	}

	// A local change in the way refuses, and changes nothing.
	write(t, fs, "a.txt", "local\n")
	if _, err := r.MergeFF("ahead"); !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("merge over a local change: %v", err)
	}
	if h, _ := r.Head(); h.Hash != base {
		t.Fatalf("a refused merge moved HEAD to %s", h.Hash)
	}
	if err := r.Restore([]string{"a.txt"}, false, true); err != nil {
		t.Fatal(err)
	}
	got, err := r.MergeFF("ahead")
	if err != nil || got != ahead {
		t.Fatalf("MergeFF = %s, %v; want %s", got, err, ahead)
	}
	if h, _ := r.Head(); h.Branch != "main" || h.Hash != ahead || read(t, fs, "b.txt") != "b\n" {
		t.Errorf("after the merge: head %+v", h)
	}
	if len(statusMap(t, r)) != 0 {
		t.Errorf("status after a merge = %v, want clean", statusMap(t, r))
	}
	// Merging what HEAD already has changes nothing.
	if got, err := r.MergeFF(base); err != nil || got != ahead {
		t.Errorf("MergeFF of an ancestor = %s, %v", got, err)
	}
	// Diverged.
	if err := r.Checkout(CheckoutOptions{Branch: "side", Rev: base, Create: true}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "s.txt", "s\n")
	commitAll(t, r, c, "side")
	if _, err := r.MergeFF("main"); !errors.Is(err, ErrNotFastForward) {
		t.Errorf("a diverged merge: %v, want non_fast_forward", err)
	}
}

func TestReset_SoftMixedHardAndPaths(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	first := commitAll(t, r, c, "first")
	write(t, fs, "a.txt", "2\n")
	second := commitAll(t, r, c, "second")

	if err := r.Reset("soft", first, nil); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.Head(); h.Hash != first || statusMap(t, r)["a.txt"] != "M " {
		t.Errorf("soft reset: head %+v status %v", h, statusMap(t, r))
	}
	if err := r.Reset("mixed", "", nil); err != nil {
		t.Fatal(err)
	}
	if statusMap(t, r)["a.txt"] != " M" || read(t, fs, "a.txt") != "2\n" {
		t.Errorf("mixed reset: status %v", statusMap(t, r))
	}
	// Hard discards tracked changes and leaves untracked files, as git does.
	write(t, fs, "untracked.txt", "keep me\n")
	if err := r.Reset("hard", second, nil); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.Head(); h.Hash != second || read(t, fs, "a.txt") != "2\n" || read(t, fs, "untracked.txt") != "keep me\n" {
		t.Errorf("hard reset: head %+v a.txt %q", h, read(t, fs, "a.txt"))
	}
	// With paths: the index entry only, HEAD stays.
	write(t, fs, "a.txt", "3\n")
	if err := r.Add([]string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Reset("mixed", "", []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.Head(); h.Hash != second || statusMap(t, r)["a.txt"] != " M" {
		t.Errorf("path reset: head %+v status %v", h, statusMap(t, r))
	}
	if err := r.Reset("mixed", first, []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.Head(); h.Hash != second || statusMap(t, r)["a.txt"] != "MM" {
		t.Errorf("path reset to first: head %+v status %v", h, statusMap(t, r))
	}
	// A file tracked now and not at the target goes; the untracked one stays.
	if err := r.Reset("hard", "", nil); err != nil { // drops the a.txt changes
		t.Fatal(err)
	}
	write(t, fs, "extra.txt", "x\n")
	if err := r.Add([]string{"extra.txt"}); err != nil {
		t.Fatal(err)
	}
	es := c.sig()
	if _, err := r.Commit(CommitOptions{Message: "extra", Author: es, Committer: es}); err != nil {
		t.Fatal(err)
	}
	if err := r.Reset("hard", first, nil); err != nil {
		t.Fatal(err)
	}
	if exists(fs, "extra.txt") || read(t, fs, "a.txt") != "1\n" || read(t, fs, "untracked.txt") != "keep me\n" {
		t.Errorf("hard reset to first: extra.txt exists %v, a.txt %q", exists(fs, "extra.txt"), read(t, fs, "a.txt"))
	}
	if st := statusMap(t, r); len(st) != 1 || st["untracked.txt"] != "??" {
		t.Errorf("status after hard reset = %v", st)
	}
	if err := r.Reset("hard", "", []string{"a.txt"}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("hard reset with paths: %v", err)
	}
	if err := r.Reset("bogus", "", nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("bogus mode: %v", err)
	}
}

func TestRestore_StagedAndWorktree(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "1\n")
	write(t, fs, "d/b.txt", "b\n")
	commitAll(t, r, c, "first")
	write(t, fs, "a.txt", "2\n")
	if err := r.Add([]string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "a.txt", "3\n")
	if err := fs.Remove("d/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := r.Restore([]string{"a.txt"}, false, false); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("restore of neither: %v", err)
	}
	if err := r.Restore([]string{"a.txt"}, false, true); err != nil {
		t.Fatal(err)
	}
	if read(t, fs, "a.txt") != "2\n" || statusMap(t, r)["a.txt"] != "M " {
		t.Errorf("worktree restore: a.txt %q status %v", read(t, fs, "a.txt"), statusMap(t, r))
	}
	if err := r.Restore([]string{"a.txt"}, true, false); err != nil {
		t.Fatal(err)
	}
	if read(t, fs, "a.txt") != "2\n" || statusMap(t, r)["a.txt"] != " M" {
		t.Errorf("staged restore: a.txt %q status %v", read(t, fs, "a.txt"), statusMap(t, r))
	}
	if err := r.Restore([]string{"."}, true, true); err != nil {
		t.Fatal(err)
	}
	if read(t, fs, "a.txt") != "1\n" || read(t, fs, "d/b.txt") != "b\n" || len(statusMap(t, r)) != 0 {
		t.Errorf("restore of everything: status %v", statusMap(t, r))
	}
	if err := r.Restore([]string{"missing.txt"}, true, false); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("restore of nothing tracked: %v", err)
	}
}

func TestRemoveMove_TrackAndRefuseLosingWork(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	write(t, fs, "d/b.txt", "b\n")
	write(t, fs, "c.txt", "c\n")
	commitAll(t, r, c, "first")

	if err := r.Remove([]string{"a.txt"}, true); err != nil {
		t.Fatal(err)
	}
	// The file stays, and the index no longer has it: the staged diff is
	// its deletion. (go-git's status folds a path deleted from the index
	// and present on disk into "??", so the diff is what is asserted.)
	staged, _, err := r.Diff(DiffOptions{Staged: true, MaxBytes: 1 << 20})
	if err != nil || !exists(fs, "a.txt") || !strings.Contains(staged, "deleted file mode 100644") {
		t.Errorf("cached remove: exists %v, staged diff %q, %v", exists(fs, "a.txt"), staged, err)
	}
	if err := r.Remove([]string{"d"}, false); err != nil {
		t.Fatal(err)
	}
	if exists(fs, "d/b.txt") || exists(fs, "d") || statusMap(t, r)["d/b.txt"] != "D " {
		t.Errorf("remove of d: status %v", statusMap(t, r))
	}
	write(t, fs, "c.txt", "changed\n")
	if err := r.Remove([]string{"c.txt"}, false); !errors.Is(err, ErrWorktreeDirty) {
		t.Errorf("removing a changed file: %v", err)
	}
	if read(t, fs, "c.txt") != "changed\n" {
		t.Error("a refused remove lost the change")
	}
	if err := r.Remove([]string{"nothing.txt"}, false); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("removing an untracked path: %v", err)
	}

	r2, fs2, c2 := memRepo(t)
	write(t, fs2, "old.txt", "o\n")
	commitAll(t, r2, c2, "first")
	if err := r2.Move("old.txt", "sub/new.txt"); err != nil {
		t.Fatal(err)
	}
	if exists(fs2, "old.txt") || read(t, fs2, "sub/new.txt") != "o\n" {
		t.Error("move did not move the file")
	}
	st := statusMap(t, r2)
	if st["old.txt"] != "D " || st["sub/new.txt"] != "A " {
		t.Errorf("status after move = %v", st)
	}
	write(t, fs2, "x.txt", "x\n")
	if err := r2.Move("sub/new.txt", "x.txt"); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("move onto an existing file: %v", err)
	}
	if err := r2.Move("x.txt", "y.txt"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("move of an untracked file: %v", err)
	}
	if err := r2.Move("x.txt", ".git/hooks/pre-commit"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("move into .git: %v", err)
	}
}

func TestClean_ListsThenRemovesUntrackedOnly(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "tracked.txt", "t\n")
	write(t, fs, "src/keep.go", "k\n")
	write(t, fs, ".gitignore", "*.log\n")
	commitAll(t, r, c, "first")
	write(t, fs, "junk.txt", "j\n")
	write(t, fs, "src/junk.go", "j\n")
	write(t, fs, "debug.log", "ignored\n")
	write(t, fs, "build/out.bin", "o\n")
	write(t, fs, "nested/.git/HEAD", "ref: refs/heads/main\n")
	write(t, fs, "nested/file.txt", "f\n")

	got, err := r.Clean(false, true)
	if err != nil || fmt.Sprint(got) != "[junk.txt src/junk.go]" {
		t.Errorf("dry run without dirs = %v, %v", got, err)
	}
	got, err = r.Clean(true, true)
	if err != nil || fmt.Sprint(got) != "[build/out.bin junk.txt src/junk.go build/]" {
		t.Errorf("dry run with dirs = %v, %v", got, err)
	}
	if !exists(fs, "junk.txt") || !exists(fs, "build/out.bin") {
		t.Fatal("a dry run removed something")
	}
	if _, err := r.Clean(true, false); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"junk.txt", "src/junk.go", "build"} {
		if exists(fs, gone) {
			t.Errorf("%s survived the clean", gone)
		}
	}
	for _, kept := range []string{"tracked.txt", "src/keep.go", "debug.log", "nested/.git/HEAD", "nested/file.txt"} {
		if !exists(fs, kept) {
			t.Errorf("%s was cleaned", kept)
		}
	}
}

// A path reaching outside the worktree or into .git would let an agent write
// the repository's config or hooks.
func TestPaths_RejectEscapesAndTheGitDirectory(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	commitAll(t, r, c, "first")
	bad := []string{"../x", "/abs", ".git/config", "a/.GIT/x", "a/.git", ".Git", ".git. ", "a//b", "a/./b", "a\x00b", "a\\b", ""}
	for _, p := range bad {
		if _, err := cleanPath(p); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("cleanPath(%q) = %v, want invalid_argument", p, err)
		}
		if err := r.Add([]string{p}); p != "" && !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Add(%q) = %v, want invalid_argument", p, err)
		}
	}
	for _, p := range []string{"a.txt", "dir/x", "dir/", ".github/workflows/x.yml", ".gitignore"} {
		if _, err := cleanPath(p); err != nil {
			t.Errorf("cleanPath(%q) = %v, want ok", p, err)
		}
	}
	if exists(fs, "x") {
		t.Error("a refused path was written")
	}
}

// Remotes report the URL the library would dial, which is not what the
// config's remote section says once an insteadOf rule rewrites it: a caller
// that validated only the section would be fooled.
func TestRemotes_ReportTheInsteadOfRewrittenURL(t *testing.T) {
	_, fs, _ := memRepo(t)
	existing, _ := util.ReadFile(fs, ".git/config") // a new repository may have none yet
	cfg := string(existing)
	cfg += "[remote \"origin\"]\n\turl = https://good.example/acme/app.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n" +
		"[url \"https://evil.example/\"]\n\tinsteadOf = https://good.example/\n"
	if err := util.WriteFile(fs, ".git/config", []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(fs)
	if err != nil {
		t.Fatal(err)
	}
	u, err := r.RemoteURL("origin")
	if err != nil || u != "https://evil.example/acme/app.git" {
		t.Errorf("RemoteURL = %q, %v; want the rewritten URL", u, err)
	}
	rs, err := r.Remotes()
	if err != nil || len(rs) != 1 || rs[0].URL != "https://evil.example/acme/app.git" {
		t.Errorf("Remotes = %+v, %v", rs, err)
	}
	if _, err := r.RemoteURL("upstream"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("RemoteURL of a missing remote: %v", err)
	}
}

func TestTagsRevParseMergeBase(t *testing.T) {
	r, fs, c := memRepo(t)
	write(t, fs, "a.txt", "a\n")
	first := commitAll(t, r, c, "first")
	write(t, fs, "a.txt", "b\n")
	second := commitAll(t, r, c, "second")
	// Tags are made with the library itself: the package has no tag op.
	if _, err := r.repo.CreateTag("light", mustHash(first), nil); err != nil {
		t.Fatal(err)
	}
	s := c.sig()
	if _, err := r.repo.CreateTag("v1", mustHash(second), tagOpts(s, "release one")); err != nil {
		t.Fatal(err)
	}
	tags, err := r.Tags()
	if err != nil || len(tags) != 2 {
		t.Fatalf("Tags = %+v, %v", tags, err)
	}
	if tags[0] != (TagInfo{Name: "light", Hash: first}) {
		t.Errorf("lightweight tag = %+v", tags[0])
	}
	if tags[1].Name != "v1" || tags[1].Hash != second || !tags[1].Annotated || strings.TrimSpace(tags[1].Message) != "release one" {
		t.Errorf("annotated tag = %+v", tags[1])
	}
	for rev, want := range map[string]string{"HEAD": second, "HEAD~1": first, "main^": first, "light": first, second[:8]: second} {
		if got, err := r.RevParse(rev); err != nil || got != want {
			t.Errorf("RevParse(%q) = %s, %v; want %s", rev, got, err, want)
		}
	}
	if _, err := r.RevParse("-x"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("RevParse(-x): %v", err)
	}
	if _, err := r.RevParse("nope"); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("RevParse(nope): %v", err)
	}
	if _, err := r.RevParse("HEAD~~~~~~~9"); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("RevParse past the root: %v", err)
	}
	if err := r.Checkout(CheckoutOptions{Branch: "side", Rev: first, Create: true}); err != nil {
		t.Fatal(err)
	}
	write(t, fs, "s.txt", "s\n")
	commitAll(t, r, c, "side")
	mb, err := r.MergeBase("main", "side")
	if err != nil || len(mb) != 1 || mb[0] != first {
		t.Errorf("MergeBase = %v, %v; want [%s]", mb, err, first)
	}
}

func TestClass_IsOneOfTheClosedList(t *testing.T) {
	for _, c := range ownClasses {
		if got := Class(fmt.Errorf("wrapped: %w", c)); got != c.Error() {
			t.Errorf("Class(%v) = %s", c, got)
		}
	}
	if got := Class(errors.New("server said: give me your token")); got != "git_failed" {
		t.Errorf("Class of an unknown error = %s", got)
	}
	if Class(nil) != "" {
		t.Error("Class(nil) is not empty")
	}
}
