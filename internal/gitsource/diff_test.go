package gitsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// branchFixture adds to the fixture a branch "feature" that leaves main at
// its head and makes four commits, while main gets one more of its own. The
// merge base of the two is the fixture's head.
func branchFixture(t *testing.T, f *fixture) (feature, mainTip string) {
	t.Helper()
	w := f.work
	write := func(rel, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(w, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, w, "checkout", "-q", "-b", "feature")
	write("README.md", "readme\nwith a second line\n")
	run(t, w, "commit", "-q", "-am", "f1")
	write("src/b.go", "package a\n\nfunc B() int { return 2 }\n")
	run(t, w, "add", ".")
	run(t, w, "commit", "-q", "-m", "f2")
	run(t, w, "rm", "-q", "bin/run.sh")
	write("data.bin", "bin\x00ary\x01")
	write("huge.txt", strings.Repeat("y\n", 1000)) // 2000 bytes: over the diff's file limit below
	run(t, w, "add", ".")
	run(t, w, "commit", "-q", "-m", "f3")
	if err := os.Remove(filepath.Join(w, "docs/latest")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../src/a.go", filepath.Join(w, "docs/latest")); err != nil {
		t.Fatal(err)
	}
	run(t, w, "add", ".")
	run(t, w, "commit", "-q", "-m", "f4")
	feature = run(t, w, "rev-parse", "HEAD")

	run(t, w, "checkout", "-q", "main")
	write("MAIN.md", "only on main\n")
	run(t, w, "add", ".")
	run(t, w, "commit", "-q", "-m", "m1")
	mainTip = run(t, w, "rev-parse", "HEAD")
	run(t, w, "push", "-q", "--force", f.bare, "main:refs/heads/main", "feature:refs/heads/feature")
	return feature, mainTip
}

var diffLimits = DiffLimits{MaxBytes: 1 << 20, MaxFileBytes: 1000, MaxFiles: 1000, Reserved: []string{"_meta"}}

func TestFetchWithMergeBase_WritesADiffThatMatchesGit(t *testing.T) {
	f := newFixture(t, false, true)
	feature, _ := branchFixture(t, f)
	wantBase := run(t, f.work, "merge-base", "main", feature)
	if wantBase != f.head {
		t.Fatalf("fixture: git's merge base is %s, the test expects the old head %s", wantBase, f.head)
	}

	req := f.request(t, feature)
	req.DiffBase = DiffBaseMergeBaseDefault
	got, err := Fetch(context.Background(), req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer got.Close()
	if got.Base != wantBase {
		t.Fatalf("Base = %s, want git's merge base %s", got.Base, wantBase)
	}

	dest := t.TempDir()
	if _, err := got.WriteTree(context.Background(), dest, limits); err != nil {
		t.Fatal(err)
	}
	res, err := got.WriteDiff(context.Background(), dest, "_meta/diff.patch", "_meta/numstat.json", diffLimits)
	if err != nil {
		t.Fatalf("WriteDiff: %v", err)
	}

	var sum struct {
		Base, Head string
		Files      []DiffFile
	}
	raw, _ := os.ReadFile(filepath.Join(dest, "_meta/numstat.json"))
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatalf("numstat.json: %v\n%s", err, raw)
	}
	if sum.Base != wantBase || sum.Head != feature || res.Files != len(sum.Files) {
		t.Errorf("summary base=%s head=%s files=%d, result %+v", sum.Base, sum.Head, len(sum.Files), res)
	}

	// The changed paths are exactly git's, renames off. main's own commit
	// (MAIN.md) is not in it: the base is the merge base, not main's tip.
	gitPaths := strings.Fields(run(t, f.work, "diff", "--no-renames", "--name-only", wantBase, feature))
	var ours []string
	byPath := map[string]DiffFile{}
	for _, d := range sum.Files {
		ours = append(ours, d.Path)
		byPath[d.Path] = d
	}
	sort.Strings(gitPaths)
	sort.Strings(ours)
	if strings.Join(ours, " ") != strings.Join(gitPaths, " ") {
		t.Errorf("changed paths:\n ours %v\n git  %v", ours, gitPaths)
	}
	for p, want := range map[string]DiffFile{
		"README.md":   {Status: "modified", Added: 1},
		"src/b.go":    {Status: "added", Added: 3},
		"bin/run.sh":  {Status: "deleted", Deleted: 2},
		"data.bin":    {Status: "added", Binary: true},
		"huge.txt":    {Status: "added", Skipped: ReasonTooLarge},
		"docs/latest": {Status: "modified", Skipped: ReasonSymlink},
	} {
		d := byPath[p]
		if d.Status != want.Status || d.Added != want.Added || d.Deleted != want.Deleted || d.Binary != want.Binary || d.Skipped != want.Skipped {
			t.Errorf("%s = %+v, want %+v", p, d, want)
		}
	}

	// The patch, applied to the base, gives the head's content for every
	// change it carries a body for.
	check := filepath.Join(t.TempDir(), "check")
	run(t, filepath.Dir(check), "clone", "-q", f.bare, check)
	run(t, check, "checkout", "-q", wantBase)
	patch := filepath.Join(dest, "_meta/diff.patch")
	run(t, check, "apply", "--check", patch)
	run(t, check, "apply", patch)
	for _, p := range []string{"README.md", "src/b.go"} {
		want := run(t, f.work, "show", feature+":"+p)
		b, err := os.ReadFile(filepath.Join(check, p))
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Errorf("%s after applying the patch = %q (err %v), want the head's", p, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(check, "bin/run.sh")); !os.IsNotExist(err) {
		t.Error("the patch did not delete bin/run.sh")
	}
	// A skipped change has no body: huge.txt is listed but not in the patch.
	if b, _ := os.ReadFile(patch); bytes.Contains(b, []byte("huge.txt")) {
		t.Error("a change over the file limit has a body in the patch")
	}
}

func TestFetchWithShaBase(t *testing.T) {
	for _, bySHA := range []bool{true, false} {
		f := newFixture(t, false, bySHA)
		feature, _ := branchFixture(t, f)
		req := f.request(t, feature)
		req.Ref = "refs/heads/feature"
		req.DiffBase = f.old // the very first commit, on main's history
		got, err := Fetch(context.Background(), req)
		if err != nil {
			t.Fatalf("by sha %v: Fetch: %v", bySHA, err)
		}
		if got.Base != f.old {
			t.Errorf("by sha %v: Base = %s, want %s", bySHA, got.Base, f.old)
		}
		dest := t.TempDir()
		if _, err := got.WriteDiff(context.Background(), dest, "d.patch", "n.json", diffLimits); err != nil {
			t.Errorf("by sha %v: WriteDiff: %v", bySHA, err)
		}
		got.Close()
	}
}

func TestFetchWithMergeBase_NotWithinTheHistoryLookedAt(t *testing.T) {
	f := newFixture(t, false, true)
	feature, _ := branchFixture(t, f) // four commits ahead, one behind
	req := f.request(t, feature)
	req.DiffBase = DiffBaseMergeBaseDefault
	req.MaxHistory = 2
	_, err := Fetch(context.Background(), req)
	if !errors.Is(err, ErrMergeBaseNotFound) || Class(err) != "merge_base_not_found" {
		t.Errorf("err = %v (class %s), want merge_base_not_found", err, Class(err))
	}
	if _, err := os.Stat(req.StoreDir); !os.IsNotExist(err) {
		t.Error("the failed fetch left its object store")
	}
}

func TestWriteDiff_LimitsAndMisuse(t *testing.T) {
	f := newFixture(t, false, true)
	feature, _ := branchFixture(t, f)
	req := f.request(t, feature)
	req.DiffBase = DiffBaseMergeBaseDefault
	got, err := Fetch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if _, err := got.WriteDiff(context.Background(), t.TempDir(), "d.patch", "n.json", DiffLimits{MaxBytes: 50, MaxFileBytes: 1000, MaxFiles: 1000}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a patch over its limit: %v, want source_too_large", err)
	}
	// More changed paths than the limit: refused before anything is diffed.
	if _, err := got.WriteDiff(context.Background(), t.TempDir(), "d.patch", "n.json", DiffLimits{MaxBytes: 1 << 20, MaxFileBytes: 1000, MaxFiles: 3}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("more changed paths than allowed: %v, want source_too_large", err)
	}
	// The summary counts too: room for the patch alone is not enough.
	full, err := got.WriteDiff(context.Background(), t.TempDir(), "d.patch", "n.json", diffLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got.WriteDiff(context.Background(), t.TempDir(), "d.patch", "n.json", DiffLimits{MaxBytes: full.Bytes + 10, MaxFileBytes: 1000, MaxFiles: 1000}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("room for the patch but not its summary: %v, want source_too_large", err)
	}
	for _, rel := range []string{"../out.patch", "/abs.patch", ".git/x", ""} {
		if _, err := got.WriteDiff(context.Background(), t.TempDir(), rel, "n.json", diffLimits); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("output path %q: %v, want invalid_request", rel, err)
		}
	}
	// An output that already exists is not replaced.
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "d.patch"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := got.WriteDiff(context.Background(), dest, "d.patch", "n.json", diffLimits); err == nil {
		t.Error("an existing output file was replaced")
	}
	// No base fetched: refused.
	plain, err := Fetch(context.Background(), f.request(t, feature))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.WriteDiff(context.Background(), t.TempDir(), "d.patch", "n.json", diffLimits); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("no base: %v, want invalid_request", err)
	}
}

func TestRequestValidate_DiffBase(t *testing.T) {
	good := Request{URL: "https://git.example/acme/app", Commit: strings.Repeat("a", 40), StoreDir: "/tmp/x", MaxFetchBytes: 1, MaxObjectBytes: 1,
		Transport: roundTripFunc(nil)}
	for _, ok := range []string{"", DiffBaseMergeBaseDefault, strings.Repeat("b", 40)} {
		r := good
		r.DiffBase = ok
		if _, err := r.validate(); err != nil {
			t.Errorf("diff base %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"main", "merge-base:main", "merge-base", strings.Repeat("b", 64), "HEAD~1"} {
		r := good
		r.DiffBase = bad
		if _, err := r.validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("diff base %q: %v, want invalid_request", bad, err)
		}
	}
}

// One file's diff stays inside the clone's deadline. The library allows each
// file an hour, and its time limit does not cover the search it makes first:
// two files built for that search (a repeated line between different first
// and last lines) took 6.7s at 40,000 lines with a 100ms deadline. A change
// with that many lines is listed without a body, at once, whatever its shape.
func TestWriteDiff_AChangeTooLongToDiffIsListedWithoutABody(t *testing.T) {
	const k = 40000
	var alt, altOther strings.Builder
	for i := 0; i < k; i++ {
		fmt.Fprintf(&alt, "line %d\n", i)
		if i%2 == 0 {
			fmt.Fprintf(&altOther, "line %d\n", i)
		} else {
			fmt.Fprintf(&altOther, "other %d\n", i)
		}
	}
	for name, sides := range map[string][2]string{
		// The search for a common run of lines, which never looks at the
		// clock.
		"repeated": {"x\n" + strings.Repeat("a\n", k) + "y\n", "z\n" + strings.Repeat("a\n", 3*k/4) + "w\n"},
		// The search the clock does stop.
		"alternating": {alt.String(), altOther.String()},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, false, true)
			base := f.commit(t, "hard.txt", []byte(sides[0]))
			head := f.commit(t, "hard.txt", []byte(sides[1]))
			req := f.request(t, head)
			req.DiffBase = base
			got, err := Fetch(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			defer got.Close()

			lim := DiffLimits{MaxBytes: 64 << 20, MaxFileBytes: 4 << 20, MaxFiles: 1000}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			dest := t.TempDir()
			start := time.Now()
			_, err = got.WriteDiff(ctx, dest, "d.patch", "n.json", lim)
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("a 100ms deadline was kept to within %s", d)
			}
			if err != nil {
				t.Fatalf("WriteDiff: %v", err)
			}
			var sum struct{ Files []DiffFile }
			raw, _ := os.ReadFile(filepath.Join(dest, "n.json"))
			if err := json.Unmarshal(raw, &sum); err != nil {
				t.Fatal(err)
			}
			if len(sum.Files) != 1 || sum.Files[0].Path != "hard.txt" || sum.Files[0].Skipped != ReasonTooLarge {
				t.Errorf("files = %+v, want hard.txt skipped as %s", sum.Files, ReasonTooLarge)
			}
		})
	}
}

func TestChangedLines_CountsOnlyWhatDiffersBetweenTheSharedStartAndEnd(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a\nb\nc\n", "a\nb\nc\n", 0},
		{"a\nb\nc\n", "a\nX\nc\n", 1},
		{"a\nb\nc\n", "a\nc\n", 1},
		{"a\nb", "a\nb\n", 1}, // a missing last newline is a different line
		{"x\n" + strings.Repeat("a\n", 10) + "y\n", "z\n" + strings.Repeat("a\n", 5) + "w\n", 12},
		// A side that is all shared start: the rest of the other is the change.
		{"a\n", "a\nb\nc\n", 2},
		// Shared lines are not counted twice when the start and end overlap.
		{"a\na\n", "a\na\na\n", 1},
	} {
		if got := changedLines(c.a, c.b); got != c.want {
			t.Errorf("changedLines(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// deadlinePassed reports a deadline already gone while its Err still says
// nil: the moment between the loop's check and a file's own, which a real
// clock hits only now and then (it did under -race).
type deadlinePassed struct{ context.Context }

func (deadlinePassed) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

// A deadline that passes while a diff is being made is a timeout, wherever
// it lands: not a failed fetch.
func TestWriteDiff_ADeadlineAnywhereIsATimeout(t *testing.T) {
	f := newFixture(t, false, true)
	base := f.commit(t, "a.txt", []byte("one\n"))
	head := f.commit(t, "a.txt", []byte("two\n"))
	req := f.request(t, head)
	req.DiffBase = base
	got, err := Fetch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	lim := DiffLimits{MaxBytes: 1 << 20, MaxFileBytes: 1 << 20, MaxFiles: 100}

	// Gone before a file's diff starts.
	if _, err := got.WriteDiff(deadlinePassed{context.Background()}, t.TempDir(), "d.patch", "n.json", lim); !errors.Is(err, ErrTimeout) {
		t.Errorf("deadline gone before a file's diff: err = %v, want timeout", err)
	}
	// Gone while the trees are compared.
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := got.WriteDiff(done, t.TempDir(), "d.patch", "n.json", lim); !errors.Is(err, ErrTimeout) {
		t.Errorf("context done while the trees are compared: err = %v, want timeout", err)
	}
}
