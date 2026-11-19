package fsops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// entries lists dir's names, so a test can see a temporary file left behind.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func openDir(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestReplaceFile_FailureLeavesTargetAndNoTempFile(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name  string
		ctx   context.Context
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"cancelled before the rename", cancelled, nil, "context canceled"},
		{"rename onto a non-empty directory", context.Background(), func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "target"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, "target", "keep"), "x")
		}, "rename: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.setup != nil {
				c.setup(t, dir)
			} else {
				writeFile(t, filepath.Join(dir, "target"), "original\n")
			}
			before := entries(t, dir)
			err := ReplaceFile(c.ctx, openDir(t, dir), "target", ".loomcycle-write-", "new content\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if got := entries(t, dir); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Errorf("directory went from %v to %v: a temporary file was left", before, got)
			}
			if c.setup == nil {
				if b, _ := os.ReadFile(filepath.Join(dir, "target")); string(b) != "original\n" {
					t.Errorf("target changed to %q", b)
				}
			}
		})
	}
}

func TestWrite_RefusedWriteCreatesNothing(t *testing.T) {
	root, _ := sandbox(t)
	before := entries(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Write(ctx, root, "", "new.txt", "x", 0); err == nil {
		t.Error("a cancelled write succeeded")
	}
	if _, err := Write(context.Background(), root, "", "big.txt", "12345", 4); err == nil || err.Error() != "content exceeds 4 bytes (got 5)" {
		t.Errorf("over-cap write: err = %v", err)
	}
	if _, err := Write(context.Background(), root, "", "missing/dir/f.txt", "x", 0); err == nil {
		t.Error("a write into a missing directory succeeded")
	}
	if got := entries(t, root); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("root went from %v to %v", before, got)
	}
}

func TestReplace_RequiresAUniqueMatchUnlessAll(t *testing.T) {
	for _, c := range []struct {
		name, body, old, new string
		all                  bool
		want                 string
		count                int
		err                  string
	}{
		{"unique", "a b c", "b", "B", false, "a B c", 1, ""},
		{"ambiguous", "x x", "x", "y", false, "", 0, "old_string occurs 2 times; supply more context or set replace_all=true"},
		{"all", "x x", "x", "y", true, "y y", 2, ""},
		{"not found", "abc", "z", "y", false, "", 0, ErrEditNotFound.Error()},
		{"no change", "abc", "a", "a", false, "", 0, ErrEditNoChange.Error()},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, n, err := Replace(c.body, c.old, c.new, c.all)
			if c.err != "" {
				if err == nil || err.Error() != c.err {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil || got != c.want || n != c.count {
				t.Fatalf("got %q, %d, %v; want %q, %d", got, n, err, c.want, c.count)
			}
		})
	}
}

func TestEdit_OverCapFileIsRefusedUnchanged(t *testing.T) {
	root, _ := sandbox(t)
	writeFile(t, filepath.Join(root, "f.txt"), "0123456789")
	_, err := Edit(context.Background(), root, "", EditRequest{Path: "f.txt", OldString: "0", NewString: "x", MaxBytes: 5})
	if err == nil || err.Error() != "file size 10 exceeds limit 5" {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "f.txt")); string(b) != "0123456789" {
		t.Errorf("file changed to %q", b)
	}
}

func TestRead_BoundsTheBytesReturned(t *testing.T) {
	root, _ := sandbox(t)
	writeFile(t, filepath.Join(root, "big.txt"), strings.Repeat("a", 100))
	got, err := Read(context.Background(), root, "", ReadRequest{Path: "big.txt", MaxBytes: 10})
	if err != nil || got != strings.Repeat("a", 10) {
		t.Fatalf("got %q, %v", got, err)
	}
	writeFile(t, filepath.Join(root, "bin"), "a\x00b\n")
	if _, err := Read(context.Background(), root, "", ReadRequest{Path: "bin", Lines: &LineWindow{Offset: 1}}); !errors.Is(err, ErrBinaryLineRange) {
		t.Errorf("binary line range: err = %v", err)
	}
}

func TestGlobAndGrep_StopAtTheirLimits(t *testing.T) {
	root, _ := sandbox(t)
	for _, n := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(root, n+".log"), "hit\n")
	}
	got, n, err := Glob(root, "", GlobRequest{Pattern: "*.log", MaxResults: 2})
	if err != nil || n != 2 || !strings.Contains(got, "[truncated at max_results=2; 3 total matches]") {
		t.Errorf("glob: %q (%d), %v", got, n, err)
	}
	got, n, err = Grep(root, "", GrepRequest{Pattern: "hit", HeadLimit: 2})
	if err != nil || n != 2 || !strings.Contains(got, "[truncated at head_limit=2") {
		t.Errorf("grep: %q (%d), %v", got, n, err)
	}
}

func TestOperations_RefuseAnEmptyPathOrPattern(t *testing.T) {
	root, _ := sandbox(t)
	ctx := context.Background()
	if _, err := Read(ctx, root, "", ReadRequest{}); !errors.Is(err, ErrPathRequired) {
		t.Errorf("read: %v", err)
	}
	if _, err := Write(ctx, root, "", "", "x", 0); !errors.Is(err, ErrPathRequired) {
		t.Errorf("write: %v", err)
	}
	if _, err := Edit(ctx, root, "", EditRequest{OldString: "a", NewString: "b"}); !errors.Is(err, ErrPathRequired) {
		t.Errorf("edit: %v", err)
	}
	if _, _, err := Glob(root, "", GlobRequest{}); !errors.Is(err, ErrPatternRequired) {
		t.Errorf("glob: %v", err)
	}
	if _, _, err := Grep(root, "", GrepRequest{}); !errors.Is(err, ErrPatternRequired) {
		t.Errorf("grep: %v", err)
	}
}
