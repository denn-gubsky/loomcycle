//go:build unix

package builtin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"golang.org/x/sys/unix"
)

// volumeBilly is the billy.Filesystem the Git tool hands go-git, so every file
// go-git reads or writes in a worktree or a .git directory goes through the
// same verified walk as the file tools (RFC ES #1). go-billy's osfs, even
// bound, passes path strings to the kernel, which walks them again: a
// directory component swapped for a symlink between its check and the open
// carries the operation out of the volume.
//
// Every operation re-walks its parent directory from the volume root one
// handle at a time (volumeFS.step), then acts on the final name through that
// handle, never following a symlink there. Nothing in this filesystem follows
// a symlink, so the targets git stores in them are inert here.
//
// Paths are relative to the filesystem root. A leading "/" means that root,
// never the host root: go-git passes absolute paths for alternates, and
// treating them as host paths would leave the volume.
type volumeBilly struct {
	v *volumeFS
	// prefix is the root of this filesystem: a clean, slash-separated path
	// relative to the volume root, "." for the volume root itself.
	prefix string
}

var (
	errBillyEscape   = errors.New("path escapes the repository root")
	errBillySymlink  = errors.New("refusing to follow a symbolic link")
	errBillyPrefix   = errors.New("temporary file prefix must be a plain name")
	errBillyRoot     = errors.New("operation not permitted on the filesystem root")
	errBillyCrossDir = errors.New("moving a directory to another directory is not supported")
)

// gitDirMode is the mode for directories created implicitly as parents, as
// go-billy's osfs uses.
const gitDirMode = 0o755

var (
	_ billy.Filesystem = (*volumeBilly)(nil)
	_ billy.Change     = (*volumeBilly)(nil)
	_ billy.Capable    = (*volumeBilly)(nil)
)

// newVolumeBilly returns a billy.Filesystem whose root is the directory rel
// names inside v (rel: a clean, symlink-free path relative to the volume
// root; "" or "." = the volume root). v must stay open while the returned
// filesystem is in use; the caller closes v. The directory must exist.
func newVolumeBilly(v *volumeFS, rel string) (billy.Filesystem, error) {
	if filepath.IsAbs(rel) {
		return nil, fmt.Errorf("repository path %q must be relative to the volume root", rel)
	}
	prefix, err := cleanBillyPath(filepath.ToSlash(rel))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: rel, Err: err}
	}
	b := &volumeBilly{v: v, prefix: prefix}
	d, err := b.dirAt("open", rel, prefix, false)
	if err != nil {
		return nil, err
	}
	d.Close()
	return b, nil
}

// cleanBillyPath makes name relative to the filesystem root: leading slashes
// are dropped and the rest cleaned lexically. Lexical is the right reading
// because no symlink is ever followed. A path that climbs above the root is
// refused, not clamped, so a caller's mistake is not silently redirected.
func cleanBillyPath(name string) (string, error) {
	p := path.Clean(strings.TrimLeft(filepath.ToSlash(name), "/"))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", errBillyEscape
	}
	return p, nil
}

// resolve cleans name and returns its path relative to the volume root.
func (b *volumeBilly) resolve(op, name string) (rel, full string, err error) {
	rel, err = cleanBillyPath(name)
	if err != nil {
		return "", "", &os.PathError{Op: op, Path: name, Err: err}
	}
	return rel, path.Join(b.prefix, rel), nil
}

// billyErr reports err against name, the path relative to this filesystem's
// root, keeping the underlying error so os.IsNotExist and errors.Is hold, as
// go-git relies on them. The volume's host path is never in the text.
func billyErr(op, name string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	if errors.Is(err, unix.ELOOP) {
		err = errBillySymlink
	}
	return &os.PathError{Op: op, Path: name, Err: err}
}

// dirAt opens the directory full names (relative to the volume root) through
// the verified walk volumeFS.openDir does, creating missing components with
// perm when create is set. name is the caller's path, for errors.
func (b *volumeBilly) dirAt(op, name, full string, create bool) (*os.Root, error) {
	return b.dirAtMode(op, name, full, create, gitDirMode)
}

func (b *volumeBilly) dirAtMode(op, name, full string, create bool, perm os.FileMode) (*os.Root, error) {
	cur, err := b.v.step(b.v.root, ".")
	if err != nil {
		return nil, billyErr(op, name, err)
	}
	for _, c := range strings.Split(full, "/") {
		if c == "." {
			continue
		}
		next, err := b.v.step(cur, c)
		if create && errors.Is(err, fs.ErrNotExist) {
			// Mkdirat never follows a symlink at c, and the step after it
			// checks c is the directory just made, so a dangling or swapped
			// symlink is refused rather than created through.
			if merr := cur.Mkdir(c, perm.Perm()); merr != nil && !errors.Is(merr, fs.ErrExist) {
				cur.Close()
				return nil, billyErr(op, name, merr)
			}
			next, err = b.v.step(cur, c)
		}
		cur.Close()
		if err != nil {
			return nil, billyErr(op, name, err)
		}
		cur = next
	}
	return cur, nil
}

// parentOf opens the directory holding name and returns it with the final
// component. For the filesystem root itself (rel "."), the final component
// is the root's own name in its parent, or "." for the volume root.
func (b *volumeBilly) parentOf(op, name string, create bool) (dir *os.Root, rel, base string, err error) {
	rel, full, err := b.resolve(op, name)
	if err != nil {
		return nil, "", "", err
	}
	dir, err = b.dirAt(op, rel, path.Dir(full), create)
	if err != nil {
		return nil, "", "", err
	}
	return dir, rel, path.Base(full), nil
}

// openNoFollow opens name inside dir without following a symlink at name.
// os.Root follows a symlink that stays inside the handle, and does so before
// a later check could refuse it — with O_TRUNC or O_CREATE that already
// rewrote the target, which may be in the fence. openat with O_NOFOLLOW on
// the handle's own descriptor refuses it in the same syscall.
func openNoFollow(dir *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	d, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var fd int
	for {
		fd, err = unix.Openat(int(d.Fd()), name, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// openVerified opens the entry name inside dir without following a symlink
// and checks, as volumeFS.openIn does, that it is still the entry the name
// holds and not the fence.
func (b *volumeBilly) openVerified(dir *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := openNoFollow(dir, name, flag, perm)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil {
		err = b.v.sameEntry(dir, name, fi)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// refuseFence refuses an operation that would move, remove or re-own the
// fence itself. sameEntry guards what is opened; these act on the name. A
// fence renamed away would no longer be where the next call's openVolumeFS
// looks for it, leaving other runs' volumes reachable.
func (b *volumeBilly) refuseFence(dir *os.Root, name string) error {
	if b.v.fence == nil {
		return nil
	}
	if fi, err := dir.Lstat(name); err == nil && os.SameFile(fi, b.v.fence) {
		return errInFence
	}
	return nil
}

func (b *volumeBilly) Create(filename string) (billy.File, error) {
	return b.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

func (b *volumeBilly) Open(filename string) (billy.File, error) {
	return b.OpenFile(filename, os.O_RDONLY, 0)
}

// OpenFile creates missing parent directories when flag has O_CREATE, as
// go-billy's osfs does; go-git writes nested refs and worktree files relying
// on it.
func (b *volumeBilly) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	dir, rel, base, err := b.parentOf("open", filename, flag&os.O_CREATE != 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := b.openVerified(dir, base, flag, perm)
	if err != nil {
		return nil, billyErr("open", rel, err)
	}
	return &billyFile{f: f, name: rel}, nil
}

// Stat refuses a symlink rather than following it: following one could land
// outside the volume or in the fence, and go-git reads worktree entries with
// Lstat, using Stat only for files under .git.
func (b *volumeBilly) Stat(filename string) (os.FileInfo, error) {
	fi, err := b.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		rel, _ := cleanBillyPath(filename)
		return nil, &os.PathError{Op: "stat", Path: rel, Err: errBillySymlink}
	}
	return fi, nil
}

func (b *volumeBilly) Lstat(filename string) (os.FileInfo, error) {
	dir, rel, base, err := b.parentOf("lstat", filename, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fi, err := dir.Lstat(base)
	if err != nil {
		return nil, billyErr("lstat", rel, err)
	}
	return fi, nil
}

// Rename creates the destination's parent directories, as go-billy's osfs
// does: go-git writes a loose object to objects/pack/tmp_obj_* and renames it
// into objects/ab/, which may not exist yet.
//
// Within one directory it is a rename on that directory's handle. Across
// directories a file is copied into a temporary file beside the destination,
// renamed into place, and the source removed: os.Root has no renameat taking
// two handles, and renaming by path from a common ancestor would walk
// strings again, the race this type exists to close. A directory cannot be
// moved that way.
func (b *volumeBilly) Rename(oldpath, newpath string) error {
	oldRel, oldFull, err := b.resolve("rename", oldpath)
	if err != nil {
		return err
	}
	newRel, newFull, err := b.resolve("rename", newpath)
	if err != nil {
		return err
	}
	if oldRel == "." || newRel == "." {
		return &os.PathError{Op: "rename", Path: oldRel, Err: errBillyRoot}
	}
	oldBase, newBase := path.Base(oldFull), path.Base(newFull)
	if path.Dir(oldFull) == path.Dir(newFull) {
		dir, err := b.dirAt("rename", oldRel, path.Dir(oldFull), false)
		if err != nil {
			return err
		}
		defer dir.Close()
		if err := b.refuseFence(dir, oldBase); err != nil {
			return billyErr("rename", oldRel, err)
		}
		if err := b.refuseFence(dir, newBase); err != nil {
			return billyErr("rename", newRel, err)
		}
		if err := dir.Rename(oldBase, newBase); err != nil {
			return billyErr("rename", oldRel, err)
		}
		return nil
	}

	src, err := b.dirAt("rename", oldRel, path.Dir(oldFull), false)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := b.refuseFence(src, oldBase); err != nil {
		return billyErr("rename", oldRel, err)
	}
	fi, err := src.Lstat(oldBase)
	if err != nil {
		return billyErr("rename", oldRel, err)
	}
	if fi.IsDir() {
		return &os.PathError{Op: "rename", Path: oldRel, Err: errBillyCrossDir}
	}
	dst, err := b.dirAt("rename", newRel, path.Dir(newFull), true)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := b.refuseFence(dst, newBase); err != nil {
		return billyErr("rename", newRel, err)
	}
	var tmpName string
	if fi.Mode()&os.ModeSymlink != 0 {
		tmpName, err = b.copySymlink(src, oldBase, dst)
	} else {
		tmpName, err = b.copyFile(src, oldBase, dst)
	}
	if err != nil {
		return billyErr("rename", oldRel, err)
	}
	if err := dst.Rename(tmpName, newBase); err != nil {
		_ = dst.Remove(tmpName)
		return billyErr("rename", newRel, err)
	}
	if err := src.Remove(oldBase); err != nil {
		return billyErr("rename", oldRel, err)
	}
	return nil
}

// copyFile copies the regular file name in src into a new temporary file in
// dst, synced and with the source's permission bits, and returns its name.
func (b *volumeBilly) copyFile(src *os.Root, name string, dst *os.Root) (string, error) {
	in, err := b.openVerified(src, name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", name)
	}
	out, tmpName, err := createTemp(dst, ".loomcycle-git-")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(fi.Mode().Perm())
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = dst.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}

// copySymlink recreates the symlink name in src as a new temporary link in
// dst and returns its name.
func (b *volumeBilly) copySymlink(src *os.Root, name string, dst *os.Root) (string, error) {
	target, err := src.Readlink(name)
	if err != nil {
		return "", err
	}
	for range 10 {
		tmp := tempName(".loomcycle-git-")
		err := dst.Symlink(target, tmp)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return tmp, err
	}
	return "", errors.New("could not create a temporary symlink")
}

// tempName is a random name starting with prefix, made as createTemp makes
// its names.
func tempName(prefix string) string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return prefix + hex.EncodeToString(buf[:])
}

func (b *volumeBilly) Remove(filename string) error {
	dir, rel, base, err := b.parentOf("remove", filename, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	if rel == "." {
		return &os.PathError{Op: "remove", Path: rel, Err: errBillyRoot}
	}
	if err := b.refuseFence(dir, base); err != nil {
		return billyErr("remove", rel, err)
	}
	if err := dir.Remove(base); err != nil {
		return billyErr("remove", rel, err)
	}
	return nil
}

func (b *volumeBilly) Join(elem ...string) string { return filepath.Join(elem...) }

// TempFile treats dir "" as this filesystem's root, as go-billy's chroot does;
// go-git's packed-refs rewrite passes it. Missing directories are created.
// The file's Name is relative to this filesystem's root so go-git can pass it
// back to Rename and Remove.
func (b *volumeBilly) TempFile(dir, prefix string) (billy.File, error) {
	// createTemp joins prefix into a name the handle resolves; a separator
	// there would be a walk os.Root does on its own terms.
	if prefix == ".." || strings.ContainsAny(prefix, "/\x00") {
		return nil, &os.PathError{Op: "tempfile", Path: prefix, Err: errBillyPrefix}
	}
	rel, full, err := b.resolve("tempfile", dir)
	if err != nil {
		return nil, err
	}
	d, err := b.dirAt("tempfile", rel, full, true)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, name, err := createTemp(d, prefix)
	if err == nil {
		var fi os.FileInfo
		if fi, err = f.Stat(); err == nil {
			err = b.v.sameEntry(d, name, fi)
		}
		if err != nil {
			f.Close()
			_ = d.Remove(name)
		}
	}
	if err != nil {
		return nil, billyErr("tempfile", rel, err)
	}
	return &billyFile{f: f, name: path.Join(rel, name)}, nil
}

// ReadDir lists entries without following symlinks. Each entry is stat'ed
// through the directory handle: os.File.ReadDir's Info stats by path string.
func (b *volumeBilly) ReadDir(dirname string) ([]os.FileInfo, error) {
	rel, full, err := b.resolve("readdir", dirname)
	if err != nil {
		return nil, err
	}
	d, err := b.dirAt("readdir", rel, full, false)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, err := d.Open(".")
	if err != nil {
		return nil, billyErr("readdir", rel, err)
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, billyErr("readdir", rel, err)
	}
	slices.Sort(names)
	infos := make([]os.FileInfo, 0, len(names))
	for _, n := range names {
		fi, err := d.Lstat(n)
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed since the listing
		}
		if err != nil {
			return nil, billyErr("readdir", path.Join(rel, n), err)
		}
		infos = append(infos, fi)
	}
	return infos, nil
}

func (b *volumeBilly) MkdirAll(filename string, perm os.FileMode) error {
	rel, full, err := b.resolve("mkdir", filename)
	if err != nil {
		return err
	}
	d, err := b.dirAtMode("mkdir", rel, full, true, perm)
	if err != nil {
		return err
	}
	d.Close()
	return nil
}

// Symlink stores target as given; git records arbitrary link targets, and
// nothing in this filesystem follows one. Parent directories are created, as
// billy requires.
func (b *volumeBilly) Symlink(target, link string) error {
	dir, rel, base, err := b.parentOf("symlink", link, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Symlink(target, base); err != nil {
		return billyErr("symlink", rel, err)
	}
	return nil
}

func (b *volumeBilly) Readlink(link string) (string, error) {
	dir, rel, base, err := b.parentOf("readlink", link, false)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	target, err := dir.Readlink(base)
	if err != nil {
		return "", billyErr("readlink", rel, err)
	}
	return target, nil
}

// Chroot does not require the directory to exist yet: go-git chroots into
// .git before Init creates it, and into a submodule's directory before
// cloning it. Every operation re-walks the prefix with the full checks, so
// checking existence here would add no safety; a path that exists but is a
// symlink, the fence, or not a directory is refused now.
func (b *volumeBilly) Chroot(p string) (billy.Filesystem, error) {
	rel, full, err := b.resolve("chroot", p)
	if err != nil {
		return nil, err
	}
	d, err := b.dirAt("chroot", rel, full, false)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	} else {
		d.Close()
	}
	return &volumeBilly{v: b.v, prefix: full}, nil
}

// Root is the filesystem's root as a slash path relative to the volume, with
// a leading "/" ("/" for the volume root). It is not a host path: go-git only
// relates two Roots to each other (filepath.Rel for the worktree and .git),
// and must not be given it to open with osfs.
func (b *volumeBilly) Root() string {
	if b.prefix == "." {
		return "/"
	}
	return "/" + b.prefix
}

// Chmod, Chown and Chtimes act on an open descriptor so a symlink swapped in
// at the name is refused, not followed: os.Root checks for a symlink and then
// acts by name, a gap in which the entry can change. Like Stat, they refuse a
// symlink where billy would follow it. Opening needs read permission on the
// entry; go-git only chmods files it has just written.
func (b *volumeBilly) Chmod(name string, mode os.FileMode) error {
	return b.withEntry("chmod", name, func(f *os.File) error { return f.Chmod(mode) })
}

func (b *volumeBilly) Chown(name string, uid, gid int) error {
	return b.withEntry("chown", name, func(f *os.File) error { return f.Chown(uid, gid) })
}

func (b *volumeBilly) Chtimes(name string, atime, mtime time.Time) error {
	return b.withEntry("chtimes", name, func(f *os.File) error {
		return unix.Futimes(int(f.Fd()), []unix.Timeval{
			unix.NsecToTimeval(atime.UnixNano()),
			unix.NsecToTimeval(mtime.UnixNano()),
		})
	})
}

// Lchown acts on the name itself; fchownat with AT_SYMLINK_NOFOLLOW never
// follows a link, so there is nothing to swap.
func (b *volumeBilly) Lchown(name string, uid, gid int) error {
	dir, rel, base, err := b.parentOf("lchown", name, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := b.refuseFence(dir, base); err != nil {
		return billyErr("lchown", rel, err)
	}
	if err := dir.Lchown(base, uid, gid); err != nil {
		return billyErr("lchown", rel, err)
	}
	return nil
}

// withEntry opens name for metadata changes. O_NONBLOCK keeps a FIFO at the
// name from blocking the open.
func (b *volumeBilly) withEntry(op, name string, fn func(*os.File) error) error {
	dir, rel, base, err := b.parentOf(op, name, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := b.openVerified(dir, base, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return billyErr(op, rel, err)
	}
	defer f.Close()
	if err := fn(f); err != nil {
		return billyErr(op, rel, err)
	}
	return nil
}

func (b *volumeBilly) Capabilities() billy.Capability { return billy.DefaultCapabilities }

// billyFile is an *os.File reporting the name it was opened with, relative to
// the filesystem root. It wraps rather than embeds so the descriptor and
// os.File's path-based ReadDir are not reachable through it.
type billyFile struct {
	f    *os.File
	name string
}

func (f *billyFile) Name() string                                 { return f.name }
func (f *billyFile) Read(p []byte) (int, error)                   { return f.f.Read(p) }
func (f *billyFile) ReadAt(p []byte, off int64) (int, error)      { return f.f.ReadAt(p, off) }
func (f *billyFile) Write(p []byte) (int, error)                  { return f.f.Write(p) }
func (f *billyFile) Seek(offset int64, whence int) (int64, error) { return f.f.Seek(offset, whence) }
func (f *billyFile) Truncate(size int64) error                    { return f.f.Truncate(size) }
func (f *billyFile) Close() error                                 { return f.f.Close() }

// Lock takes an exclusive flock, as go-billy's osfs does; go-git locks
// packed-refs and the alternates file while rewriting them.
func (f *billyFile) Lock() error   { return unix.Flock(int(f.f.Fd()), unix.LOCK_EX) }
func (f *billyFile) Unlock() error { return unix.Flock(int(f.f.Fd()), unix.LOCK_UN) }
