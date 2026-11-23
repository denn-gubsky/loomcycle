package fsops

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// VolumeFS is one operation's handle on a root (RFC ES #1). Every open, create
// and rename goes through an os.Root opened at the root's real path, never
// through a path string the kernel walks again. Resolving a path with
// EvalSymlinks and then opening that string left a gap in which another writer
// could swap a directory component for a symlink and carry the open out of the
// root.
//
// ResolveInsideRoot still runs first: it keeps the error text callers and
// tests rely on, and gives a symlink-free path to walk. The walk then opens
// that path one directory handle at a time and checks that each step is still
// the real directory the path named, so a swap made after the check is refused
// rather than followed. That also keeps the fence (a directory inside the root
// holding other runs' volumes) atomic: os.Root alone would follow a symlink
// swapped in that points into the fence, since the fence is inside the root.
type VolumeFS struct {
	root *os.Root
	// dir is the real path the root was opened at; resolved paths are made
	// relative to it.
	dir string
	// fence is the fence directory's identity, or nil when the binding has
	// none or it does not exist.
	fence os.FileInfo
}

var (
	// ErrPathChanged is a path component that is no longer the entry the
	// checked path named: a symlink swapped in, or a different file.
	ErrPathChanged = errors.New("the path changed while it was being opened; try again")
	// ErrNotRegular is a FIFO, a socket or a device: never opened, since a
	// blocking open of a FIFO waits for a writer for ever (and holds
	// whatever lock its caller holds), and reading a device has no end.
	ErrNotRegular = errors.New("not a regular file (a FIFO, a socket or a device), so it is not opened")
	// ErrInFence is a path that reaches the fence.
	ErrInFence = errors.New("the path is inside the directory that holds dynamic and ephemeral volumes; name that volume in the \"volume\" argument instead")
)

// OpenVolumeFS opens root (possibly through a symlink) for one operation, with
// fence ("" for none). The caller must Close it.
func OpenVolumeFS(root, fence string) (*VolumeFS, error) {
	dir, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	return openResolved(dir, fence)
}

// openResolved is OpenVolumeFS for a root whose symlinks are already
// evaluated, so an operation resolves its root once.
func openResolved(dir, fence string) (*VolumeFS, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, rootError(err)
	}
	v := &VolumeFS{root: r, dir: dir}
	if fence != "" {
		// The fence is operator configuration, so its path is trusted. One
		// that does not exist yet holds nothing to reach.
		if fi, err := os.Stat(fence); err == nil {
			v.fence = fi
		}
	}
	return v, nil
}

// Close releases the root handle.
func (v *VolumeFS) Close() error { return v.root.Close() }

// Dir is the real path the root was opened at.
func (v *VolumeFS) Dir() string { return v.dir }

// Root is the handle on the root. An operation through it is confined to the
// root by os.Root, but follows a symlink that stays inside it — into the fence
// too; use OpenDir, Open and OpenIn for the per-component checks.
func (v *VolumeFS) Root() *os.Root { return v.root }

// Fence is the fence directory's identity, or nil when there is none.
func (v *VolumeFS) Fence() os.FileInfo { return v.fence }

// Rel turns a path ResolveInsideRoot (or ResolveParentInsideRoot) returned
// into one relative to the root. Both paths are host paths, so a refusal
// names neither.
func (v *VolumeFS) Rel(resolved string) (string, error) {
	if !inside(v.dir, resolved) {
		return "", ErrOutsideVolume
	}
	return filepath.Rel(v.dir, resolved)
}

// PathErr gives an error from a root-relative operation the path relative to
// the root, never the root's host path: the model reads this text. It stays
// an *fs.PathError, so errors.Is and errors.As hold.
func (v *VolumeFS) PathErr(op, rel string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return &fs.PathError{Op: op, Path: filepath.ToSlash(rel), Err: err}
}

// withPath renames the path an *fs.PathError names to rel, keeping its op and
// errno; any other error (nil included) is returned as it is. A file opened
// through an os.Root is named by its host path, so an error reading,
// writing or closing it ("read /srv/vol/dir: is a directory") would carry
// that path to the model.
func withPath(err error, rel string) error {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return err
	}
	return &fs.PathError{Op: pe.Op, Path: filepath.ToSlash(rel), Err: pe.Err}
}

// OpenDir opens the directory rel names (a symlink-free path relative to the
// root), one component at a time. The caller must Close the result.
func (v *VolumeFS) OpenDir(rel string) (*os.Root, error) {
	cur, err := v.Step(v.root, ".")
	if err != nil {
		return nil, v.PathErr("open", ".", err)
	}
	for _, name := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if name == "." {
			continue
		}
		next, err := v.Step(cur, name)
		cur.Close()
		if err != nil {
			return nil, v.PathErr("open", rel, err)
		}
		cur = next
	}
	return cur, nil
}

// Step opens the directory name inside parent, refusing a symlink, a swapped
// entry and the fence. The caller must Close the result.
func (v *VolumeFS) Step(parent *os.Root, name string) (*os.Root, error) {
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	fi, err := child.Stat(".")
	if err == nil {
		err = v.SameEntry(parent, name, fi)
	}
	if err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

// SameEntry checks that what was opened as name inside parent is the entry
// itself. os.Root follows a symlink that stays inside the directory it is
// opened from, so a name swapped for a symlink before the open would
// otherwise be followed: the entry is refused when it is a symlink now or is
// a different file from the one opened. The fence is refused too.
func (v *VolumeFS) SameEntry(parent *os.Root, name string, opened os.FileInfo) error {
	entry, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if entry.Mode()&os.ModeSymlink != 0 || !os.SameFile(entry, opened) {
		return ErrPathChanged
	}
	if v.fence != nil && os.SameFile(opened, v.fence) {
		return ErrInFence
	}
	return nil
}

// Open opens the file rel names (a symlink-free path relative to the root)
// for reading.
func (v *VolumeFS) Open(rel string) (*os.File, error) {
	dir, err := v.OpenDir(filepath.Dir(rel))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := v.OpenIn(dir, filepath.Base(rel))
	if err != nil {
		return nil, v.PathErr("open", rel, err)
	}
	return f, nil
}

// OpenIn opens name inside dir for reading: a regular file or a directory.
// A FIFO, a socket or a device is refused with ErrNotRegular: checked before
// the open, and the open itself is non-blocking with the check repeated on
// the opened handle, so an entry swapped for a FIFO in between is refused
// too rather than waited on.
func (v *VolumeFS) OpenIn(dir *os.Root, name string) (*os.File, error) {
	if fi, err := dir.Lstat(name); err == nil && special(fi.Mode()) {
		return nil, ErrNotRegular
	}
	f, err := dir.OpenFile(name, os.O_RDONLY|nonBlock, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && special(fi.Mode()) {
		err = ErrNotRegular
	}
	if err == nil {
		err = v.SameEntry(dir, name, fi)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// CreateTemp creates a new file in dir for an atomic replace, as
// os.CreateTemp does — mode 0600, a random name starting with prefix, never
// an existing file — but relative to the directory handle. The caller renames
// it into place with dir.Rename and removes it with dir.Remove on failure.
func CreateTemp(dir *os.Root, prefix string) (*os.File, string, error) {
	for range 10 {
		var b [8]byte
		_, _ = rand.Read(b[:])
		name := prefix + hex.EncodeToString(b[:])
		f, err := dir.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, name, err
	}
	return nil, "", fmt.Errorf("could not create a temporary file with prefix %q", prefix)
}

// Walk calls fn for every entry under the directory rel names that is not a
// directory, depth first in lexical order as filepath.WalkDir visits them.
// It descends one directory handle at a time with the checks of OpenDir, so
// a directory swapped for a symlink mid-walk, and the fence, are skipped,
// never followed. path is the entry's path relative to rel; dir is the handle
// of the directory holding it. When rel names a file, fn is called once with
// path ".". A start that cannot be opened yields nothing, as a WalkDir whose
// callback ignores errors does.
func (v *VolumeFS) Walk(rel string, fn func(dir *os.Root, path string, d fs.DirEntry)) {
	parent, err := v.OpenDir(filepath.Dir(rel))
	if err != nil {
		return
	}
	defer parent.Close()
	base := filepath.Base(rel)
	fi, err := parent.Lstat(base)
	if err != nil {
		return
	}
	if !fi.IsDir() {
		fn(parent, ".", fs.FileInfoToDirEntry(fi))
		return
	}
	start, err := v.Step(parent, base)
	if err != nil {
		return
	}
	defer start.Close()
	v.walkDir(start, "", fn)
}

func (v *VolumeFS) walkDir(dir *os.Root, prefix string, fn func(dir *os.Root, path string, d fs.DirEntry)) {
	f, err := dir.Open(".")
	if err != nil {
		return
	}
	// A partial listing is still walked, as filepath.WalkDir does.
	ents, _ := f.ReadDir(-1)
	f.Close()
	slices.SortFunc(ents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range ents {
		p := filepath.Join(prefix, e.Name())
		if !e.IsDir() {
			fn(dir, p, e)
			continue
		}
		sub, err := v.Step(dir, e.Name())
		if err != nil {
			continue
		}
		v.walkDir(sub, p, fn)
		sub.Close()
	}
}

// special reports a mode that is neither a regular file, a directory nor a
// symlink.
func special(m os.FileMode) bool {
	return m&(os.ModeNamedPipe|os.ModeSocket|os.ModeDevice|os.ModeCharDevice|os.ModeIrregular) != 0
}
