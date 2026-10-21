package builtin

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

// volumeFS is one file-tool call's handle on a volume (RFC ES #1). Every
// open, create and rename goes through an os.Root opened at the volume's real
// root, never through a path string the kernel walks again. Resolving a path
// with EvalSymlinks and then opening that string left a gap in which another
// writer could swap a directory component for a symlink and carry the open
// out of the volume.
//
// resolveInsideRoot still runs first: it keeps the error text callers and
// tests rely on, and gives a symlink-free path to walk. The walk then opens
// that path one directory handle at a time and checks that each step is
// still the real directory the path named, so a swap made after the check is
// refused rather than followed. That also keeps the fence (a directory inside
// the volume holding other runs' volumes) atomic: os.Root alone would follow
// a symlink swapped in that points into the fence, since the fence is inside
// the root.
type volumeFS struct {
	root *os.Root
	// dir is the real path the root was opened at; resolved paths are made
	// relative to it.
	dir string
	// fence is the fence directory's identity, or nil when the binding has
	// none or it does not exist.
	fence os.FileInfo
}

var (
	errPathChanged = errors.New("the path changed while it was being opened; try again")
	errInFence     = errors.New("the path is inside the directory that holds dynamic and ephemeral volumes; name that volume in the \"volume\" argument instead")
)

// openVolumeFS opens root (the binding's root, possibly through a symlink)
// for one call. The caller must Close it.
func openVolumeFS(root, fence string) (*volumeFS, error) {
	dir, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("sandbox root: %w", err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("sandbox root: %w", err)
	}
	v := &volumeFS{root: r, dir: dir}
	if fence != "" {
		// The fence is operator configuration, so its path is trusted. One
		// that does not exist yet holds nothing to reach.
		if fi, err := os.Stat(fence); err == nil {
			v.fence = fi
		}
	}
	return v, nil
}

func (v *volumeFS) Close() error { return v.root.Close() }

// rel turns a path resolveInsideRoot (or resolveParentInsideRoot) returned
// into one relative to the root.
func (v *volumeFS) rel(resolved string) (string, error) {
	rel, err := filepath.Rel(v.dir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes sandbox %q", resolved, v.dir)
	}
	return rel, nil
}

// pathErr gives an error from a root-relative operation the absolute path,
// so the model sees the same "open /abs/path: …" text os.Open produced.
func (v *volumeFS) pathErr(op, rel string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return &fs.PathError{Op: op, Path: filepath.Join(v.dir, rel), Err: err}
}

// openDir opens the directory rel names (a symlink-free path relative to the
// root), one component at a time. The caller must Close the result.
func (v *volumeFS) openDir(rel string) (*os.Root, error) {
	cur, err := v.step(v.root, ".")
	if err != nil {
		return nil, v.pathErr("open", ".", err)
	}
	for _, name := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if name == "." {
			continue
		}
		next, err := v.step(cur, name)
		cur.Close()
		if err != nil {
			return nil, v.pathErr("open", rel, err)
		}
		cur = next
	}
	return cur, nil
}

// step opens the directory name inside parent.
func (v *volumeFS) step(parent *os.Root, name string) (*os.Root, error) {
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	fi, err := child.Stat(".")
	if err == nil {
		err = v.sameEntry(parent, name, fi)
	}
	if err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

// sameEntry checks that what was opened as name inside parent is the entry
// itself. os.Root follows a symlink that stays inside the directory it is
// opened from, so a name swapped for a symlink before the open would
// otherwise be followed: the entry is refused when it is a symlink now or is
// a different file from the one opened. The fence is refused too.
func (v *volumeFS) sameEntry(parent *os.Root, name string, opened os.FileInfo) error {
	entry, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if entry.Mode()&os.ModeSymlink != 0 || !os.SameFile(entry, opened) {
		return errPathChanged
	}
	if v.fence != nil && os.SameFile(opened, v.fence) {
		return errInFence
	}
	return nil
}

// open opens the file rel names (a symlink-free path relative to the root)
// for reading.
func (v *volumeFS) open(rel string) (*os.File, error) {
	dir, err := v.openDir(filepath.Dir(rel))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := v.openIn(dir, filepath.Base(rel))
	if err != nil {
		return nil, v.pathErr("open", rel, err)
	}
	return f, nil
}

// openIn opens name inside dir for reading.
func (v *volumeFS) openIn(dir *os.Root, name string) (*os.File, error) {
	f, err := dir.Open(name)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil {
		err = v.sameEntry(dir, name, fi)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// createTemp creates a new file in dir for an atomic replace, as
// os.CreateTemp does — mode 0600, a random name starting with prefix, never
// an existing file — but relative to the directory handle. The caller renames
// it into place with dir.Rename and removes it with dir.Remove on failure.
func createTemp(dir *os.Root, prefix string) (*os.File, string, error) {
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

// walk calls fn for every entry under the directory rel names that is not a
// directory, depth first in lexical order as filepath.WalkDir visits them.
// It descends one directory handle at a time with the checks of openDir, so
// a directory swapped for a symlink mid-walk, and the fence, are skipped,
// never followed. path is the entry's path relative to rel; dir is the handle
// of the directory holding it. When rel names a file, fn is called once with
// path ".". A start that cannot be opened yields nothing, as a WalkDir whose
// callback ignores errors does.
func (v *volumeFS) walk(rel string, fn func(dir *os.Root, path string, d fs.DirEntry)) {
	parent, err := v.openDir(filepath.Dir(rel))
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
	start, err := v.step(parent, base)
	if err != nil {
		return
	}
	defer start.Close()
	v.walkDir(start, "", fn)
}

func (v *volumeFS) walkDir(dir *os.Root, prefix string, fn func(dir *os.Root, path string, d fs.DirEntry)) {
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
		sub, err := v.step(dir, e.Name())
		if err != nil {
			continue
		}
		v.walkDir(sub, p, fn)
		sub.Close()
	}
}
