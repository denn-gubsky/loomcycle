package fsops

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertNoHostPath fails when err's text names the root's host path, or the
// directory the root and the outside directory share, unless the caller
// wrote that path itself (an absolute path elsewhere is the caller's own).
// The model reads this text; a host path in it says where the volume lives on
// the server.
func assertNoHostPath(t *testing.T, err error, root, written string) {
	t.Helper()
	if err == nil {
		t.Fatal("not refused")
	}
	msg := err.Error()
	if strings.Contains(msg, root) {
		t.Errorf("refusal %q names the volume's host path %q", msg, root)
	}
	base := filepath.Dir(root)
	if !strings.Contains(written, base) && strings.Contains(msg, base) {
		t.Errorf("refusal %q names the host path %q the caller never wrote", msg, base)
	}
}

func TestRefusals_EscapesNameOnlyThePathTheCallerWrote(t *testing.T) {
	ctx := context.Background()
	for _, c := range escapes {
		t.Run(c.name, func(t *testing.T) {
			root, outside := sandbox(t)
			if c.setup != nil {
				c.setup(t, root, outside)
			}
			p := c.path(root, outside)
			dir := p
			if d := filepath.Dir(p); d != "." {
				dir = d
			}
			_, err := ResolveInsideRoot(root, p)
			assertNoHostPath(t, err, root, p)
			if !errors.Is(err, ErrOutsideVolume) {
				t.Errorf("resolve: %v does not match ErrOutsideVolume", err)
			}
			if !strings.Contains(err.Error(), p) {
				t.Errorf("resolve: refusal %q does not name the path %q", err, p)
			}
			// A symlink AT the target is replaced by a write, not followed,
			// so the parent check and the write may accept it.
			replacesLink := c.name == "symlinked file" || c.name == "symlink chain"
			if _, err = ResolveParentInsideRoot(root, p); !replacesLink {
				assertNoHostPath(t, err, root, p)
			}
			_, err = Read(ctx, root, "", ReadRequest{Path: p})
			assertNoHostPath(t, err, root, p)
			_, err = Edit(ctx, root, "", EditRequest{Path: p, OldString: secret, NewString: "x"})
			assertNoHostPath(t, err, root, p)
			_, _, err = Glob(root, "", GlobRequest{Pattern: "**", Path: dir})
			assertNoHostPath(t, err, root, dir)
			_, _, err = Grep(root, "", GrepRequest{Pattern: secret, Path: dir})
			assertNoHostPath(t, err, root, dir)
			// Last: a write over a link replaces it with a file in the root.
			if _, err = Write(ctx, root, "", p, "x", 0); !replacesLink {
				assertNoHostPath(t, err, root, p)
			}
		})
	}
}

func TestRefusals_SymlinkOutIsNamedAsALink(t *testing.T) {
	root, outside := sandbox(t)
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt"))
	_, err := ResolveInsideRoot(root, "link.txt")
	assertNoHostPath(t, err, root, "link.txt")
	if want := `path "link.txt" goes through a symbolic link that leads outside the volume`; err.Error() != want {
		t.Errorf("got %q, want %q", err, want)
	}
	_, err = ResolveInsideRoot(root, "../outside/secret.txt")
	if want := `path "../outside/secret.txt" escapes the volume`; err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
}

func TestRefusals_FenceNamesNoPath(t *testing.T) {
	root, _ := sandbox(t)
	fence := filepath.Join(root, "dynamic")
	if err := os.MkdirAll(filepath.Join(fence, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fence, "other", "f.txt"), "x\n")
	ctx := context.Background()
	p := "dynamic/other/f.txt"
	_, err := Read(ctx, root, fence, ReadRequest{Path: p})
	assertNoHostPath(t, err, root, p)
	if !errors.Is(err, ErrInFence) {
		t.Errorf("read: %v does not match ErrInFence", err)
	}
	_, err = Write(ctx, root, fence, p, "y", 0)
	assertNoHostPath(t, err, root, p)
	_, err = Edit(ctx, root, fence, EditRequest{Path: p, OldString: "x", NewString: "y"})
	assertNoHostPath(t, err, root, p)
	resolved, _ := ResolveInsideRoot(root, p)
	assertNoHostPath(t, OutsideFence(fence, resolved), root, p)
}

func TestRefusals_MissingFileIsNotExistWithoutAHostPath(t *testing.T) {
	root, _ := sandbox(t)
	ctx := context.Background()
	for _, p := range []string{"missing.txt", "no/such/dir/f.txt"} {
		_, err := ResolveInsideRoot(root, p)
		assertNoHostPath(t, err, root, p)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("resolve %s: %v does not match fs.ErrNotExist", p, err)
		}
		_, err = Read(ctx, root, "", ReadRequest{Path: p})
		assertNoHostPath(t, err, root, p)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("read %s: %v does not match fs.ErrNotExist", p, err)
		}
		_, err = Edit(ctx, root, "", EditRequest{Path: p, OldString: "a", NewString: "b"})
		assertNoHostPath(t, err, root, p)
		_, _, err = Glob(root, "", GlobRequest{Pattern: "*", Path: p})
		assertNoHostPath(t, err, root, p)
		_, _, err = Grep(root, "", GrepRequest{Pattern: "a", Path: p})
		assertNoHostPath(t, err, root, p)
	}
	_, err := Read(ctx, root, "", ReadRequest{Path: "missing.txt"})
	if want := `path "missing.txt" does not exist`; err.Error() != want {
		t.Errorf("got %q, want %q", err, want)
	}
}

func TestRefusals_MissingParentIsNotExistWithoutAHostPath(t *testing.T) {
	root, _ := sandbox(t)
	p := "no/such/dir/f.txt"
	_, err := ResolveParentInsideRoot(root, p)
	assertNoHostPath(t, err, root, p)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%v does not match fs.ErrNotExist", err)
	}
	_, err = Write(context.Background(), root, "", p, "x", 0)
	assertNoHostPath(t, err, root, p)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("write: %v does not match fs.ErrNotExist", err)
	}
}

func TestRefusals_BrokenRootNamesNoPath(t *testing.T) {
	root, _ := sandbox(t)
	gone := filepath.Join(root, "gone")
	ctx := context.Background()
	_, err := ResolveInsideRoot(gone, "f.txt")
	assertNoHostPath(t, err, root, "f.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%v does not match fs.ErrNotExist", err)
	}
	_, err = ResolveParentInsideRoot(gone, "f.txt")
	assertNoHostPath(t, err, root, "f.txt")
	_, err = OpenVolumeFS(gone, "")
	assertNoHostPath(t, err, root, "")
	_, err = Read(ctx, gone, "", ReadRequest{Path: "f.txt"})
	assertNoHostPath(t, err, root, "f.txt")
	_, err = Write(ctx, gone, "", "f.txt", "x", 0)
	assertNoHostPath(t, err, root, "f.txt")
	_, _, err = Glob(gone, "", GlobRequest{Pattern: "*"})
	assertNoHostPath(t, err, root, "")
	_, _, err = Grep(gone, "", GrepRequest{Pattern: "a"})
	assertNoHostPath(t, err, root, "")
}

// A file opened through the root is named by its host path, so an error
// reading it (a directory read as a file) would carry that path.
func TestRefusals_ReadErrorNamesTheRelativePath(t *testing.T) {
	root, _ := sandbox(t)
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, lines := range []*LineWindow{nil, {Offset: 1}} {
		_, err := Read(ctx, root, "", ReadRequest{Path: "adir", Lines: lines})
		assertNoHostPath(t, err, root, "adir")
	}
	_, err := Edit(ctx, root, "", EditRequest{Path: "adir", OldString: "a", NewString: "b"})
	assertNoHostPath(t, err, root, "adir")
}

func TestVolumeFS_RelAndPathErrNameNoHostPath(t *testing.T) {
	root, outside := sandbox(t)
	v, err := OpenVolumeFS(root, "")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	_, err = v.Rel(filepath.Join(outside, "secret.txt"))
	assertNoHostPath(t, err, root, "")
	if !errors.Is(err, ErrOutsideVolume) {
		t.Errorf("%v does not match ErrOutsideVolume", err)
	}
	_, err = v.Open("missing.txt")
	assertNoHostPath(t, err, root, "missing.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%v does not match fs.ErrNotExist", err)
	}
}
