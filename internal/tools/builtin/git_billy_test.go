//go:build linux || darwin

package builtin

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// billyAt opens the volume at root (with fence, "" for none) and returns a
// billy filesystem on the directory rel names inside it.
func billyAt(t *testing.T, root, fence, rel string) billy.Filesystem {
	t.Helper()
	v, err := openVolumeFS(root, fence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	b, err := newVolumeBilly(v, rel)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeBilly(t *testing.T, b billy.Filesystem, name, content string) {
	t.Helper()
	if err := util.WriteFile(b, name, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func readHost(t *testing.T, p string) string {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

func TestVolumeBilly_CreateWritesUnderRootAndCreatesParents(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	f, err := b.Create("a/b/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name() != "a/b/c.txt" {
		t.Errorf("Name() = %q, want a/b/c.txt", f.Name())
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readHost(t, filepath.Join(root, "a", "b", "c.txt")); got != "hello" {
		t.Errorf("content = %q", got)
	}
	// A leading "/" is the filesystem root, not the host root.
	writeBilly(t, b, "/top.txt", "top")
	if got := readHost(t, filepath.Join(root, "top.txt")); got != "top" {
		t.Errorf("content = %q", got)
	}
}

func TestVolumeBilly_OpenFileHonoursFlagsAndFileMethods(t *testing.T) {
	b := billyAt(t, realDir(t), "", "")
	writeBilly(t, b, "f.txt", "0123456789")

	if _, err := b.OpenFile("f.txt", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644); !os.IsExist(err) {
		t.Errorf("O_EXCL on an existing file: err = %v, want exist", err)
	}
	if _, err := b.Open("missing.txt"); !os.IsNotExist(err) {
		t.Errorf("Open missing: err = %v, want not-exist", err)
	}
	if _, err := b.Open("nodir/missing.txt"); !os.IsNotExist(err) {
		t.Errorf("Open under a missing dir: err = %v, want not-exist", err)
	}

	f, err := b.OpenFile("f.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := f.ReadAt(buf, 4); err != nil || string(buf) != "456" {
		t.Errorf("ReadAt = %q, %v", buf, err)
	}
	if _, err := f.Seek(8, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(buf[:2]); err != nil || string(buf[:2]) != "89" {
		t.Errorf("Read after Seek = %q, %v", buf[:2], err)
	}
	if err := f.Truncate(4); err != nil {
		t.Fatal(err)
	}
	if err := f.Lock(); err != nil {
		t.Errorf("Lock: %v", err)
	}
	if err := f.Unlock(); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	f.Close()

	a, err := b.OpenFile("f.txt", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("XY")); err != nil {
		t.Fatal(err)
	}
	a.Close()
	r, err := b.Open("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "0123XY" {
		t.Errorf("after truncate+append: %q, want 0123XY", got)
	}
	if billy.Capabilities(b) != billy.DefaultCapabilities {
		t.Errorf("Capabilities = %v, want all", billy.Capabilities(b))
	}
}

func TestVolumeBilly_StatAndLstatDescribeEntriesWithoutFollowing(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	writeBilly(t, b, "f.txt", "abc")
	if err := b.Symlink("f.txt", "link"); err != nil {
		t.Fatal(err)
	}

	fi, err := b.Stat("f.txt")
	if err != nil || fi.Size() != 3 || fi.Name() != "f.txt" {
		t.Errorf("Stat f.txt = %v, %v", fi, err)
	}
	for _, name := range []string{"", "/", "."} {
		fi, err := b.Stat(name)
		if err != nil || !fi.IsDir() {
			t.Errorf("Stat(%q) = %v, %v; want the root directory", name, fi, err)
		}
	}
	fi, err = b.Lstat("link")
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("Lstat link = %v, %v; want a symlink", fi, err)
	}
	// Stat would follow; it refuses instead.
	if _, err := b.Stat("link"); !errors.Is(err, errBillySymlink) {
		t.Errorf("Stat link: err = %v, want a symlink refusal", err)
	}
	if _, err := b.Stat("missing"); !os.IsNotExist(err) {
		t.Errorf("Stat missing: err = %v, want not-exist", err)
	}
}

func TestVolumeBilly_RenameInOneDirReplacesTarget(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	writeBilly(t, b, "d/old.txt", "new content")
	writeBilly(t, b, "d/target.txt", "replaced")
	if err := b.Rename("d/old.txt", "d/target.txt"); err != nil {
		t.Fatal(err)
	}
	if got := readHost(t, filepath.Join(root, "d", "target.txt")); got != "new content" {
		t.Errorf("target = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "d", "old.txt")); !os.IsNotExist(err) {
		t.Errorf("source still present: %v", err)
	}
	// A directory moves within its parent.
	if err := b.MkdirAll("d/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Rename("d/sub", "d/sub2"); err != nil {
		t.Errorf("rename a directory in one parent: %v", err)
	}
	if err := b.Rename(".", "x"); !errors.Is(err, errBillyRoot) {
		t.Errorf("rename the root: err = %v, want refusal", err)
	}
}

func TestVolumeBilly_RenameAcrossDirsMovesFileLinkAndCreatesParent(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	f, err := b.TempFile("objects/pack", "tmp_obj_")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("object bytes"))
	f.Close()
	if err := b.(billy.Change).Chmod(f.Name(), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := b.Rename(f.Name(), "objects/ab/cdef"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "objects", "ab", "cdef")
	if got := readHost(t, dst); got != "object bytes" {
		t.Errorf("moved content = %q", got)
	}
	if fi, _ := os.Stat(dst); fi == nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("moved mode = %v, want 0640", fi)
	}
	if _, err := os.Lstat(filepath.Join(root, f.Name())); !os.IsNotExist(err) {
		t.Errorf("source still present: %v", err)
	}
	ents, _ := os.ReadDir(filepath.Join(root, "objects", "ab"))
	if len(ents) != 1 {
		t.Errorf("destination dir holds %d entries, want only the moved file (no temp left)", len(ents))
	}

	if err := b.Symlink("../target", "l/link"); err != nil {
		t.Fatal(err)
	}
	if err := b.Rename("l/link", "m/link"); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Readlink("m/link"); err != nil || got != "../target" {
		t.Errorf("moved link = %q, %v", got, err)
	}

	if err := b.MkdirAll("x/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Rename("x/dir", "y/dir"); !errors.Is(err, errBillyCrossDir) {
		t.Errorf("rename a directory across parents: err = %v, want unsupported", err)
	}
}

func TestVolumeBilly_RemoveDeletesFilesAndOnlyEmptyDirs(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	writeBilly(t, b, "d/f.txt", "x")
	if err := b.Remove("d"); err == nil {
		t.Error("removed a non-empty directory")
	}
	if err := b.Remove("d/f.txt"); err != nil {
		t.Fatal(err)
	}
	if err := b.Remove("d"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "d")); !os.IsNotExist(err) {
		t.Errorf("d still present: %v", err)
	}
	if err := b.Remove("missing"); !os.IsNotExist(err) {
		t.Errorf("Remove missing: err = %v, want not-exist", err)
	}
	if err := b.Remove("/"); !errors.Is(err, errBillyRoot) {
		t.Errorf("Remove root: err = %v, want refusal", err)
	}
	// util.RemoveAll walks through ReadDir/Lstat/Remove.
	writeBilly(t, b, "tree/a/b.txt", "x")
	if err := b.Symlink("/etc", "tree/link"); err != nil {
		t.Fatal(err)
	}
	if err := util.RemoveAll(b, "tree"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "tree")); !os.IsNotExist(err) {
		t.Errorf("tree still present: %v", err)
	}
}

func TestVolumeBilly_MkdirAllCreatesNestedDirsIdempotently(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	for range 2 {
		if err := b.MkdirAll("a/b/c", 0o750); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Lstat(filepath.Join(root, "a", "b", "c"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("a/b/c = %v, %v", fi, err)
	}
	// go-git passes os.ModeDir|perm; the extra bit must not fail Mkdir.
	if err := b.MkdirAll("m", os.ModeDir|0o755); err != nil {
		t.Errorf("MkdirAll with ModeDir: %v", err)
	}
	writeBilly(t, b, "file", "x")
	if err := b.MkdirAll("file/sub", 0o755); err == nil {
		t.Error("MkdirAll through a file succeeded")
	}
}

func TestVolumeBilly_TempFileNameIsRelativeToFilesystemRoot(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	f, err := b.TempFile("objects/pack", "tmp_obj_")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if !strings.HasPrefix(f.Name(), "objects/pack/tmp_obj_") {
		t.Errorf("Name() = %q, want objects/pack/tmp_obj_*", f.Name())
	}
	if _, err := os.Lstat(filepath.Join(root, f.Name())); err != nil {
		t.Errorf("temp file not on disk: %v", err)
	}
	g, err := b.TempFile("", "packed-refs")
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if strings.Contains(g.Name(), "/") || !strings.HasPrefix(g.Name(), "packed-refs") {
		t.Errorf("Name() = %q, want a bare name in the root", g.Name())
	}
	sub, err := b.Chroot("repo/.git")
	if err != nil {
		t.Fatal(err)
	}
	h, err := sub.TempFile("refs", "t")
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	if !strings.HasPrefix(h.Name(), "refs/t") {
		t.Errorf("chrooted Name() = %q, want refs/t*", h.Name())
	}
	if _, err := os.Lstat(filepath.Join(root, "repo", ".git", h.Name())); err != nil {
		t.Errorf("chrooted temp file not on disk: %v", err)
	}
	for _, prefix := range []string{"a/b", "..", "x\x00"} {
		if _, err := b.TempFile("", prefix); !errors.Is(err, errBillyPrefix) {
			t.Errorf("TempFile prefix %q: err = %v, want refusal", prefix, err)
		}
	}
}

func TestVolumeBilly_ReadDirListsSortedLstatInfos(t *testing.T) {
	b := billyAt(t, realDir(t), "", "")
	writeBilly(t, b, "d/b.txt", "bb")
	writeBilly(t, b, "d/a.txt", "a")
	if err := b.MkdirAll("d/c", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Symlink("/etc", "d/0link"); err != nil {
		t.Fatal(err)
	}
	infos, err := b.ReadDir("d")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, fi := range infos {
		names = append(names, fi.Name())
	}
	if strings.Join(names, ",") != "0link,a.txt,b.txt,c" {
		t.Errorf("names = %v", names)
	}
	if infos[0].Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink entry mode = %v, want a symlink (not followed)", infos[0].Mode())
	}
	if !infos[3].IsDir() || infos[2].Size() != 2 {
		t.Errorf("infos = %v", infos)
	}
	if _, err := b.ReadDir("missing"); !os.IsNotExist(err) {
		t.Errorf("ReadDir missing: err = %v, want not-exist", err)
	}
}

func TestVolumeBilly_SymlinkStoresTargetAsGivenAndIsNeverFollowed(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	if err := b.Symlink("/etc/passwd", "l/link"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(root, "l", "link")); err != nil || got != "/etc/passwd" {
		t.Errorf("on-disk link = %q, %v", got, err)
	}
	if got, err := b.Readlink("l/link"); err != nil || got != "/etc/passwd" {
		t.Errorf("Readlink = %q, %v", got, err)
	}
	if _, err := b.Open("l/link"); err == nil {
		t.Error("Open followed a symlink")
	}
	if _, err := b.Readlink("missing"); !os.IsNotExist(err) {
		t.Errorf("Readlink missing: err = %v, want not-exist", err)
	}
}

func TestVolumeBilly_ChrootScopesPathsAndRoot(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	if b.Root() != "/" {
		t.Errorf("Root() = %q, want /", b.Root())
	}
	// A directory that does not exist yet is accepted, as go-git chroots into
	// .git before Init creates it.
	sub, err := b.Chroot("repo")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Root() != "/repo" {
		t.Errorf("Root() = %q, want /repo", sub.Root())
	}
	writeBilly(t, sub, "/f.txt", "in repo")
	if got := readHost(t, filepath.Join(root, "repo", "f.txt")); got != "in repo" {
		t.Errorf("content = %q", got)
	}
	inner, err := sub.Chroot(".git")
	if err != nil {
		t.Fatal(err)
	}
	if rel, _ := filepath.Rel(sub.Root(), inner.Root()); rel != ".git" {
		t.Errorf("Rel(worktree, .git) = %q, want .git (go-git relies on it)", rel)
	}
	if _, err := sub.Chroot("../x"); !errors.Is(err, errBillyEscape) {
		t.Errorf("Chroot ../x: err = %v, want escape refusal", err)
	}
	if _, err := sub.Open("../f.txt"); !errors.Is(err, errBillyEscape) {
		t.Errorf("a chroot reached its parent: err = %v", err)
	}
	if err := sub.Remove("."); !errors.Is(err, errBillyRoot) {
		t.Errorf("Remove of the chroot root: err = %v, want refusal", err)
	}
	writeBilly(t, b, "file", "x")
	if _, err := b.Chroot("file"); err == nil {
		t.Error("Chroot into a file succeeded")
	}
}

func TestVolumeBilly_ChangeMethodsActOnTheEntry(t *testing.T) {
	root := realDir(t)
	b := billyAt(t, root, "", "")
	c := b.(billy.Change)
	writeBilly(t, b, "run.sh", "#!/bin/sh")
	if err := c.Chmod("run.sh", 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "run.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode())
	}
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := c.Chtimes("run.sh", mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "run.sh")); !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime = %v, want %v", fi.ModTime(), mtime)
	}
	if err := c.Chown("run.sh", os.Getuid(), os.Getgid()); err != nil {
		t.Errorf("Chown to self: %v", err)
	}
	if err := b.Symlink("run.sh", "link"); err != nil {
		t.Fatal(err)
	}
	if err := c.Lchown("link", os.Getuid(), os.Getgid()); err != nil {
		t.Errorf("Lchown to self: %v", err)
	}
	if err := c.Chmod("link", 0o777); !errors.Is(err, errBillySymlink) {
		t.Errorf("Chmod through a symlink: err = %v, want refusal", err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "run.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("the link's target changed mode: %v", fi.Mode())
	}
}

func TestVolumeBilly_NewRefusesMissingEscapingOrAbsoluteRoot(t *testing.T) {
	root := realDir(t)
	v, err := openVolumeFS(root, "")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	for _, rel := range []string{"missing", "../x", "/abs"} {
		if _, err := newVolumeBilly(v, rel); err == nil {
			t.Errorf("newVolumeBilly(%q) succeeded", rel)
		}
	}
	for _, rel := range []string{"", "."} {
		if _, err := newVolumeBilly(v, rel); err != nil {
			t.Errorf("newVolumeBilly(%q): %v", rel, err)
		}
	}
}

// Paths climbing above the root are refused outright, never clamped to it
// and never resolved against the host.
func TestVolumeBilly_EscapingPathsRefused(t *testing.T) {
	base := realDir(t)
	root := filepath.Join(base, "vol")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	b := billyAt(t, root, "", "")
	writeBilly(t, b, "f.txt", "x")
	for _, p := range []string{"../x", "/../x", "a/../../x"} {
		checks := map[string]error{}
		_, checks["Open"] = b.Open(p)
		_, checks["Create"] = b.Create(p)
		_, checks["OpenFile"] = b.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
		_, checks["Stat"] = b.Stat(p)
		_, checks["Lstat"] = b.Lstat(p)
		checks["MkdirAll"] = b.MkdirAll(p, 0o755)
		checks["Remove"] = b.Remove(p)
		checks["RenameFrom"] = b.Rename(p, "y")
		checks["RenameTo"] = b.Rename("f.txt", p)
		_, checks["TempFile"] = b.TempFile(p, "t")
		_, checks["ReadDir"] = b.ReadDir(p)
		_, checks["Chroot"] = b.Chroot(p)
		checks["Symlink"] = b.Symlink("t", p)
		_, checks["Readlink"] = b.Readlink(p)
		checks["Chmod"] = b.(billy.Change).Chmod(p, 0o777)
		for op, err := range checks {
			if !errors.Is(err, errBillyEscape) {
				t.Errorf("%s(%q): err = %v, want escape refusal", op, p, err)
			}
		}
	}
	ents, _ := os.ReadDir(base)
	if len(ents) != 1 {
		t.Errorf("entries beside the volume: %v", ents)
	}
	if got := readHost(t, filepath.Join(root, "f.txt")); got != "x" {
		t.Errorf("f.txt = %q", got)
	}
}

// A directory component that is a symlink is refused, whether it points out
// of the volume or to a directory inside it.
func TestVolumeBilly_SymlinkComponentRefused(t *testing.T) {
	root, outside := realDir(t), realDir(t)
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte(swapSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "secret"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "in")); err != nil {
		t.Fatal(err)
	}
	b := billyAt(t, root, "", "")
	writeBilly(t, b, "f.txt", "x")
	for _, link := range []string{"out", "in"} {
		checks := map[string]error{}
		_, checks["Open"] = b.Open(link + "/secret")
		_, checks["Create"] = b.Create(link + "/new")
		_, checks["Stat"] = b.Stat(link + "/secret")
		checks["MkdirAll"] = b.MkdirAll(link+"/sub/deeper", 0o755)
		checks["Remove"] = b.Remove(link + "/secret")
		checks["RenameFrom"] = b.Rename(link+"/secret", "moved")
		checks["RenameTo"] = b.Rename("f.txt", link+"/f.txt")
		_, checks["TempFile"] = b.TempFile(link, "t")
		_, checks["ReadDir"] = b.ReadDir(link)
		_, checks["Chroot"] = b.Chroot(link)
		checks["Symlink"] = b.Symlink("t", link+"/l")
		checks["Chmod"] = b.(billy.Change).Chmod(link+"/secret", 0o777)
		for op, err := range checks {
			if err == nil {
				t.Errorf("%s through the %q symlink succeeded", op, link)
			}
		}
	}
	for _, dir := range []string{outside, filepath.Join(root, "real")} {
		ents, _ := os.ReadDir(dir)
		if len(ents) != 1 || ents[0].Name() != "secret" {
			t.Errorf("%s changed: %v", dir, ents)
		}
	}
	if got := readHost(t, filepath.Join(outside, "secret")); got != swapSentinel {
		t.Errorf("outside secret = %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(outside, "secret")); fi.Mode().Perm() != 0o600 {
		t.Errorf("outside secret mode = %v", fi.Mode())
	}
}

// A final symlink is refused, not followed — including by an open that
// truncates or creates, where following it would rewrite the target before
// any check after the open could refuse it.
func TestVolumeBilly_FinalSymlinkRefused(t *testing.T) {
	root, outside := realDir(t), realDir(t)
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte(swapSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "in")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("created-through-link", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	b := billyAt(t, root, "", "")
	for _, link := range []string{"out", "in", "dangling"} {
		checks := map[string]error{}
		_, checks["Open"] = b.Open(link)
		_, checks["OpenFile"] = b.OpenFile(link, os.O_RDWR|os.O_TRUNC, 0)
		_, checks["Create"] = b.Create(link)
		_, checks["Stat"] = b.Stat(link)
		for op, err := range checks {
			if err == nil {
				t.Errorf("%s(%q) followed the symlink", op, link)
			}
		}
	}
	if got := readHost(t, filepath.Join(outside, "secret")); got != swapSentinel {
		t.Errorf("outside file = %q, want untouched", got)
	}
	if got := readHost(t, filepath.Join(root, "real.txt")); got != "inside" {
		t.Errorf("in-volume link target = %q, want untouched", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "created-through-link")); !os.IsNotExist(err) {
		t.Errorf("Create made the dangling link's target: %v", err)
	}
}

// The fence (the directory inside a static volume holding other runs'
// volumes) can be neither entered nor moved, removed or replaced.
func TestVolumeBilly_FenceRefused(t *testing.T) {
	root := realDir(t)
	fence := filepath.Join(root, "dynamic")
	if err := os.Mkdir(fence, 0o700); err != nil {
		t.Fatal(err)
	}
	b := billyAt(t, root, fence, "")
	if err := b.MkdirAll("emptydir", 0o755); err != nil {
		t.Fatal(err)
	}
	c := b.(billy.Change)
	checks := map[string]error{}
	_, checks["Open"] = b.Open("dynamic")
	_, checks["Create"] = b.Create("dynamic/x")
	checks["MkdirAll"] = b.MkdirAll("dynamic/x", 0o755)
	_, checks["ReadDir"] = b.ReadDir("dynamic")
	_, checks["Chroot"] = b.Chroot("dynamic")
	_, checks["TempFile"] = b.TempFile("dynamic", "t")
	checks["Remove"] = b.Remove("dynamic")
	checks["RenameFrom"] = b.Rename("dynamic", "moved")
	checks["RenameOnto"] = b.Rename("emptydir", "dynamic")
	checks["Chmod"] = c.Chmod("dynamic", 0o777)
	checks["Lchown"] = c.Lchown("dynamic", os.Getuid(), os.Getgid())
	for op, err := range checks {
		if !errors.Is(err, errInFence) {
			t.Errorf("%s on the fence: err = %v, want fence refusal", op, err)
		}
	}
	fi, err := os.Lstat(fence)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("fence changed: %v, %v", fi, err)
	}
	if ents, _ := os.ReadDir(fence); len(ents) != 0 {
		t.Errorf("something was created in the fence: %v", ents)
	}
}

// go-git runs end to end on the adapter: init, add, commit, branch, reopen.
func TestVolumeBilly_GoGitInitCommitAndReopen(t *testing.T) {
	root := realDir(t)
	if err := os.Mkdir(filepath.Join(root, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := billyAt(t, root, "", "repo")
	dot, err := wt.Chroot(".git")
	if err != nil {
		t.Fatal(err)
	}
	r, err := git.Init(filesystem.NewStorage(dot, cache.NewObjectLRUDefault()), wt)
	if err != nil {
		t.Fatal(err)
	}
	writeBilly(t, wt, "hello.txt", "hello from the volume\n")
	writeBilly(t, wt, "dir/sub/nested.txt", "nested\n")
	w, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1_700_000_000, 0)}
	h, err := w.Commit("first commit", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatal(err)
	}
	st, err := w.Status()
	if err != nil || !st.IsClean() {
		t.Errorf("status after commit = %v, %v; want clean", st, err)
	}
	if err := w.Checkout(&git.CheckoutOptions{Branch: "refs/heads/feature/x", Create: true}); err != nil {
		t.Fatal(err)
	}

	dot2, err := billyAt(t, root, "", "repo").Chroot(".git")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := git.Open(filesystem.NewStorage(dot2, cache.NewObjectLRUDefault()), wt)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r2.Head()
	if err != nil || head.Hash() != h || head.Name().String() != "refs/heads/feature/x" {
		t.Fatalf("HEAD = %v, %v; want %s on feature/x", head, err, h)
	}
	c, err := r2.CommitObject(h)
	if err != nil || c.Message != "first commit" {
		t.Fatalf("commit = %v, %v", c, err)
	}
	file, err := c.File("dir/sub/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := file.Contents(); got != "nested\n" {
		t.Errorf("nested.txt = %q", got)
	}
	// The worktree root has a real .git directory, not a "gitdir:" file.
	if fi, err := os.Lstat(filepath.Join(root, "repo", ".git")); err != nil || !fi.IsDir() {
		t.Errorf(".git = %v, %v; want a directory", fi, err)
	}

	// The repository is valid to stock git as well, when it is installed.
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return
	}
	out, err := exec.Command(gitBin, "-C", filepath.Join(root, "repo"), "cat-file", "-p", "HEAD:hello.txt").CombinedOutput()
	if err != nil || string(out) != "hello from the volume\n" {
		t.Errorf("git cat-file = %q, %v", out, err)
	}
}

// The swap race runs the adapter's writing operations while a goroutine flips
// a directory component between a real directory and a symlink out of the
// volume. A path-string walk at any of them would land a file outside.
func TestVolumeBilly_SymlinkSwapRaceNeverEscapes(t *testing.T) {
	s := newSwapRace(t, "inside the volume", swapSentinel)
	b := billyAt(t, s.root, "", "")
	runBillySwapRace(t, s, b)
	s.outsideUntouched(t)
}

// The fence holds under the same race: the symlink swapped in is relative
// and stays inside the volume, so containment in the root alone would follow
// it into another run's volume.
func TestVolumeBilly_SymlinkSwapIntoFenceNeverWrites(t *testing.T) {
	root := realDir(t)
	fence := filepath.Join(root, "dynamic")
	other := filepath.Join(fence, "_ephemeral", "run-2")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	s := newSwapRaceAt(t, root, other, filepath.Join("dynamic", "_ephemeral", "run-2"), "inside the volume", swapSentinel)
	b := billyAt(t, s.root, fence, "")
	runBillySwapRace(t, s, b)
	s.outsideUntouched(t)
}

func runBillySwapRace(t *testing.T, s *swapRace, b billy.Filesystem) {
	t.Helper()
	writeBilly(t, b, "src.txt", "moved across")
	creates, renames, moves, mkdirs := 0, 0, 0, 0
	// Create runs every iteration; the other operations take turns, which
	// keeps each above a thousand attempts at a fraction of the run time.
	for i := range swapRaceIterations {
		if f, err := b.Create("d/secret.txt"); err == nil {
			_, werr := f.Write([]byte("written through the adapter"))
			if f.Close() == nil && werr == nil {
				creates++
			}
		}
		switch i % 3 {
		case 0:
			if f, err := b.TempFile("d", "tmp_"); err == nil {
				f.Write([]byte("renamed into place"))
				f.Close()
				if b.Rename(f.Name(), "d/secret.txt") == nil {
					renames++
				} else {
					_ = b.Remove(f.Name())
				}
			}
		case 1:
			if b.Rename("src.txt", "d/moved.txt") == nil {
				moves++
				// The swap can hide d for a while; keep trying until the
				// file is back for the next turn.
				for i := 0; b.Rename("d/moved.txt", "src.txt") != nil; i++ {
					if i == 100000 {
						t.Fatal("could not move the file back")
					}
				}
			}
		case 2:
			if b.MkdirAll("d/sub/deeper", 0o755) == nil {
				mkdirs++
				_ = b.Remove("d/sub/deeper")
				_ = b.Remove("d/sub")
			}
		}
	}
	s.stop()
	if creates == 0 || renames == 0 || moves == 0 || mkdirs == 0 {
		t.Errorf("in %d iterations: %d creates, %d renames, %d moves, %d mkdirs succeeded; the race never let every operation run",
			swapRaceIterations, creates, renames, moves, mkdirs)
	}
}
