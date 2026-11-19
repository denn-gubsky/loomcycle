package fsops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secret = "OUTSIDE_SECRET_7f3a"

// sandbox makes a root and, beside it, a directory outside the root holding a
// file with secret in it. Both are real paths (macOS's /var is a symlink).
func sandbox(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(outside, "secret.txt"), secret+"\n")
	writeFile(t, filepath.Join(root, "inside.txt"), "inside\n")
	return root, outside
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

// escapes are the paths every operation must refuse, each named for the way
// it leaves the root. setup plants what the path needs.
var escapes = []struct {
	name  string
	path  func(root, outside string) string
	setup func(t *testing.T, root, outside string)
}{
	{"dot-dot", func(_, _ string) string { return "../outside/secret.txt" }, nil},
	{"absolute path outside", func(_, outside string) string { return filepath.Join(outside, "secret.txt") }, nil},
	{"symlinked file", func(_, _ string) string { return "link.txt" }, func(t *testing.T, root, outside string) {
		symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt"))
	}},
	{"symlinked directory", func(_, _ string) string { return "linkdir/secret.txt" }, func(t *testing.T, root, outside string) {
		symlink(t, outside, filepath.Join(root, "linkdir"))
	}},
	{"symlink chain", func(_, _ string) string { return "a.txt" }, func(t *testing.T, root, outside string) {
		symlink(t, filepath.Join(root, "b.txt"), filepath.Join(root, "a.txt"))
		symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "b.txt"))
	}},
	{"through a symlinked parent", func(_, _ string) string { return "sub/deeper/secret.txt" }, func(t *testing.T, root, outside string) {
		if err := os.MkdirAll(filepath.Join(outside, "deeper"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(outside, "deeper", "secret.txt"), secret+"\n")
		symlink(t, outside, filepath.Join(root, "sub"))
	}},
}

func TestResolveInsideRoot_RefusesEveryEscape(t *testing.T) {
	for _, c := range escapes {
		t.Run(c.name, func(t *testing.T) {
			root, outside := sandbox(t)
			if c.setup != nil {
				c.setup(t, root, outside)
			}
			if got, err := ResolveInsideRoot(root, c.path(root, outside)); err == nil {
				t.Fatalf("resolved to %q, want a refusal", got)
			}
		})
	}
}

func TestRead_RefusesEveryEscapeWithoutReturningItsContent(t *testing.T) {
	for _, c := range escapes {
		t.Run(c.name, func(t *testing.T) {
			root, outside := sandbox(t)
			if c.setup != nil {
				c.setup(t, root, outside)
			}
			for _, lines := range []*LineWindow{nil, {Offset: 1}} {
				got, err := Read(context.Background(), root, "", ReadRequest{Path: c.path(root, outside), Lines: lines})
				if err == nil || strings.Contains(got, secret) || strings.Contains(err.Error(), secret) {
					t.Fatalf("lines=%v: got %q, err %v; want a refusal", lines != nil, got, err)
				}
			}
		})
	}
}

func TestEdit_RefusesEveryEscapeAndLeavesTheTargetUnchanged(t *testing.T) {
	for _, c := range escapes {
		t.Run(c.name, func(t *testing.T) {
			root, outside := sandbox(t)
			if c.setup != nil {
				c.setup(t, root, outside)
			}
			_, err := Edit(context.Background(), root, "", EditRequest{Path: c.path(root, outside), OldString: secret, NewString: "pwned"})
			if err == nil {
				t.Fatal("edit outside the root was not refused")
			}
			assertSecretsIntact(t, outside)
		})
	}
}

func TestWrite_RefusesEveryEscapeAndWritesNothingOutside(t *testing.T) {
	for _, c := range escapes {
		t.Run(c.name, func(t *testing.T) {
			root, outside := sandbox(t)
			if c.setup != nil {
				c.setup(t, root, outside)
			}
			p := c.path(root, outside)
			_, err := Write(context.Background(), root, "", p, "pwned", 0)
			switch {
			case err != nil:
			case c.name == "symlinked file" || c.name == "symlink chain":
				// A symlink AT the target is replaced by the rename, never
				// followed: the write lands in the root, the outside file
				// stays as it was.
				fi, lerr := os.Lstat(filepath.Join(root, p))
				if lerr != nil || fi.Mode()&os.ModeSymlink != 0 {
					t.Fatalf("the link at %s was not replaced by a regular file (err %v)", p, lerr)
				}
			default:
				t.Fatal("write outside the root was not refused")
			}
			assertSecretsIntact(t, outside)
			assertNoNewFiles(t, outside)
		})
	}
}

func TestGlobAndGrep_RefuseAnEscapingPath(t *testing.T) {
	for _, c := range escapes {
		t.Run(c.name, func(t *testing.T) {
			root, outside := sandbox(t)
			if c.setup != nil {
				c.setup(t, root, outside)
			}
			// Search the directory the escape goes through, or the escaping
			// file itself when it sits in the root.
			p := c.path(root, outside)
			if d := filepath.Dir(p); d != "." {
				p = d
			}
			if got, _, err := Glob(root, "", GlobRequest{Pattern: "**", Path: p}); err == nil {
				t.Errorf("glob of %s: got %q, want a refusal", p, got)
			}
			if got, _, err := Grep(root, "", GrepRequest{Pattern: secret, Path: p, OutputMode: "content"}); err == nil {
				t.Errorf("grep of %s: got %q, want a refusal", p, got)
			}
		})
	}
}

// A walk from the root never descends a symlinked directory and never opens a
// symlinked file that leads out, so nothing outside is listed or searched.
func TestGlobAndGrep_WalkFromTheRootStaysInside(t *testing.T) {
	root, outside := sandbox(t)
	for _, c := range escapes {
		if c.setup != nil && c.name != "through a symlinked parent" {
			c.setup(t, root, outside)
		}
	}
	got, _, err := Glob(root, "", GlobRequest{Pattern: "**/secret.txt"})
	if err != nil || got != GlobEmpty {
		t.Errorf("glob: got %q, err %v; want no match", got, err)
	}
	got, _, err = Grep(root, "", GrepRequest{Pattern: "SECRET|inside", OutputMode: "content"})
	if err != nil || strings.Contains(got, secret) || !strings.Contains(got, "inside.txt:1:inside") {
		t.Errorf("grep: got %q, err %v; want only the inside match", got, err)
	}
}

func TestOperations_RefuseThePathInTheFence(t *testing.T) {
	root, _ := sandbox(t)
	fence := filepath.Join(root, "dynamic")
	if err := os.MkdirAll(filepath.Join(fence, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fence, "other", "f.txt"), secret+"\n")
	symlink(t, filepath.Join(fence, "other"), filepath.Join(root, "into-fence"))
	ctx := context.Background()
	for _, p := range []string{"dynamic/other/f.txt", "into-fence/f.txt"} {
		if _, err := Read(ctx, root, fence, ReadRequest{Path: p}); err == nil {
			t.Errorf("read %s: not refused", p)
		}
		if _, err := Write(ctx, root, fence, p, "x", 0); err == nil {
			t.Errorf("write %s: not refused", p)
		}
		if _, err := Edit(ctx, root, fence, EditRequest{Path: p, OldString: secret, NewString: "x"}); err == nil {
			t.Errorf("edit %s: not refused", p)
		}
	}
	if got, _, _ := Grep(root, fence, GrepRequest{Pattern: secret}); strings.Contains(got, "f.txt") {
		t.Errorf("grep searched the fence: %q", got)
	}
	if got, _, _ := Glob(root, fence, GlobRequest{Pattern: "**/f.txt"}); got != GlobEmpty {
		t.Errorf("glob listed the fence: %q", got)
	}
}

// The root itself may be a symlink (a volume mounted through one): it is the
// one path resolved, and every operation works under it.
func TestOperations_WorkUnderASymlinkedRoot(t *testing.T) {
	real, _ := sandbox(t)
	root := filepath.Join(filepath.Dir(real), "root-link")
	symlink(t, real, root)
	ctx := context.Background()
	if _, err := Write(ctx, root, "", "new.txt", "hello world\n", 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Edit(ctx, root, "", EditRequest{Path: "new.txt", OldString: "world", NewString: "there"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got, err := Read(ctx, root, "", ReadRequest{Path: "new.txt"}); err != nil || got != "hello there\n" {
		t.Fatalf("read: %q, %v", got, err)
	}
	if got, n, err := Glob(root, "", GlobRequest{Pattern: "*.txt"}); err != nil || n != 2 {
		t.Fatalf("glob: %q (%d), %v", got, n, err)
	}
	if got, n, err := Grep(root, "", GrepRequest{Pattern: "there"}); err != nil || n != 1 || got != "new.txt\n" {
		t.Fatalf("grep: %q (%d), %v", got, n, err)
	}
}

func assertSecretsIntact(t *testing.T, outside string) {
	t.Helper()
	_ = filepath.WalkDir(outside, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil || string(b) != secret+"\n" {
			t.Errorf("%s changed: %q (%v)", p, b, rerr)
		}
		return nil
	})
}

func assertNoNewFiles(t *testing.T, outside string) {
	t.Helper()
	_ = filepath.WalkDir(outside, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) != "secret.txt" {
			t.Errorf("a file appeared outside the root: %s", p)
		}
		return err
	})
}
