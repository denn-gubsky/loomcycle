package builtin

import (
	"context"
	stdfs "io/fs"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gbfs "github.com/ewhauser/gbash/fs"
)

// writeBudget is the byte allowance one Bashbox call has for in-memory files
// (RFC ES follow-up). gbash keeps a read-only volume's writes, and the shadow
// over the dynamic-volume root, in memory with no size limit, and that memory
// is loomcycle's heap, shared with every run on the replica: `yes > f` passed
// 3 GiB in a second. Every byte written, and every name a file or link is
// created under, is charged; nothing is refunded on delete, so the budget
// bounds what a call can put in memory at once, by the total it may write.
// Shared by the concurrent commands of a pipeline, hence atomic.
type writeBudget struct {
	max  int64
	used atomic.Int64
}

// take charges n bytes and reports how many fit: all of n, or what was left.
func (b *writeBudget) take(n int64) int64 {
	for {
		used := b.used.Load()
		left := b.max - used
		if left <= 0 {
			return 0
		}
		grant := n
		if grant > left {
			grant = left
		}
		if b.used.CompareAndSwap(used, used+grant) {
			return grant
		}
	}
}

// errNoSpace is what a write past the budget fails with. ENOSPC makes gbash's
// commands print "no space left on device", which a model reads as a full
// disk and stops writing.
func errNoSpace(op, name string) error {
	return &os.PathError{Op: op, Path: name, Err: syscall.ENOSPC}
}

// cappedFactory builds a session filesystem whose writes are charged to budget.
type cappedFactory struct {
	inner  gbfs.Factory
	budget *writeBudget
}

func (f cappedFactory) New(ctx context.Context) (gbfs.FileSystem, error) {
	fsys, err := f.inner.New(ctx)
	if err != nil {
		return nil, err
	}
	return &cappedFS{FileSystem: fsys, budget: f.budget, held: map[string]struct{}{}}, nil
}

// cappedFS charges whatever makes the in-memory layer hold more:
//
//   - data written through an open file;
//   - the names of new files, directories and links (a symlink's target is a
//     string as long as a shell variable allows);
//   - a copy-up: on a read-only volume, chmod, chown, touch, mv, ln or opening
//     a file to append copies a file of the volume into memory. gbash defers
//     reading its content until the copy is next opened, and keeps it from
//     then on, so the copy is charged at its full size when it is made (a
//     directory moved with mv, at the size of every file under it).
//
// gbash builds the in-memory layer inside its overlay with no way to wrap it,
// so this wraps the merged view and charges by path. held records the paths
// already accounted for — made here, or charged at a copy — so a second chmod
// costs nothing. It errs high, never low: a path moved by mv is charged again
// if it is copied again under its new name.
//
// Mkfifo and Lchtimes are optional filesystem methods gbash looks for, so
// they are forwarded explicitly.
type cappedFS struct {
	gbfs.FileSystem
	budget *writeBudget

	mu   sync.Mutex
	held map[string]struct{}
}

func (c *cappedFS) charge(op, name string, n int64) error {
	if n <= 0 {
		return nil
	}
	if c.budget.take(n) < n {
		return errNoSpace(op, name)
	}
	return nil
}

func (c *cappedFS) abs(name string) string { return gbfs.Resolve(c.FileSystem.Getwd(), name) }

func (c *cappedFS) hold(abs string) {
	c.mu.Lock()
	c.held[abs] = struct{}{}
	c.mu.Unlock()
}

// chargeCopy charges, once per path, the content an operation on name is
// about to copy into memory: a regular file's size, or for a directory the
// sizes of the files under it. follow says whether the operation acts on a
// symlink's target. A path that does not exist is not charged; the operation
// itself reports that.
func (c *cappedFS) chargeCopy(ctx context.Context, op, name string, follow bool) error {
	abs := c.abs(name)
	if follow {
		if real, err := c.FileSystem.Realpath(ctx, abs); err == nil {
			abs = real
		}
	}
	c.mu.Lock()
	_, done := c.held[abs]
	c.mu.Unlock()
	if done {
		return nil
	}
	info, err := c.FileSystem.Lstat(ctx, abs)
	if err != nil {
		return nil
	}
	switch {
	case info.Mode().IsRegular():
		if err := c.charge(op, name, info.Size()); err != nil {
			return err
		}
	case info.IsDir():
		entries, err := c.FileSystem.ReadDir(ctx, abs)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if err := c.chargeCopy(ctx, op, path.Join(abs, e.Name()), false); err != nil {
				return err
			}
		}
	}
	c.hold(abs)
	return nil
}

func (c *cappedFS) OpenFile(ctx context.Context, name string, flag int, perm stdfs.FileMode) (gbfs.File, error) {
	write := flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_TRUNC|os.O_CREATE) != 0
	if !write {
		return c.FileSystem.OpenFile(ctx, name, flag, perm)
	}
	// Truncating drops the content, so nothing of the old file is copied.
	if flag&os.O_TRUNC == 0 {
		if err := c.chargeCopy(ctx, "open", name, true); err != nil {
			return nil, err
		}
	}
	if flag&os.O_CREATE != 0 {
		if err := c.charge("open", name, int64(len(name))); err != nil {
			return nil, err
		}
	}
	f, err := c.FileSystem.OpenFile(ctx, name, flag, perm)
	if err != nil {
		return nil, err
	}
	c.hold(c.abs(name))
	return &cappedFile{File: f, name: name, budget: c.budget}, nil
}

func (c *cappedFS) MkdirAll(ctx context.Context, name string, perm stdfs.FileMode) error {
	if err := c.charge("mkdir", name, int64(len(name))); err != nil {
		return err
	}
	return c.FileSystem.MkdirAll(ctx, name, perm)
}

func (c *cappedFS) Symlink(ctx context.Context, target, linkName string) error {
	if err := c.charge("symlink", linkName, int64(len(target)+len(linkName))); err != nil {
		return err
	}
	if err := c.FileSystem.Symlink(ctx, target, linkName); err != nil {
		return err
	}
	c.hold(c.abs(linkName))
	return nil
}

func (c *cappedFS) Link(ctx context.Context, oldName, newName string) error {
	if err := c.chargeCopy(ctx, "link", oldName, false); err != nil {
		return err
	}
	if err := c.charge("link", newName, int64(len(newName))); err != nil {
		return err
	}
	if err := c.FileSystem.Link(ctx, oldName, newName); err != nil {
		return err
	}
	c.hold(c.abs(newName))
	return nil
}

func (c *cappedFS) Rename(ctx context.Context, oldName, newName string) error {
	if err := c.chargeCopy(ctx, "rename", oldName, false); err != nil {
		return err
	}
	if err := c.charge("rename", newName, int64(len(newName))); err != nil {
		return err
	}
	if err := c.FileSystem.Rename(ctx, oldName, newName); err != nil {
		return err
	}
	c.hold(c.abs(newName))
	return nil
}

func (c *cappedFS) Chmod(ctx context.Context, name string, mode stdfs.FileMode) error {
	if err := c.chargeCopy(ctx, "chmod", name, true); err != nil {
		return err
	}
	return c.FileSystem.Chmod(ctx, name, mode)
}

func (c *cappedFS) Chown(ctx context.Context, name string, uid, gid uint32, follow bool) error {
	if err := c.chargeCopy(ctx, "chown", name, follow); err != nil {
		return err
	}
	return c.FileSystem.Chown(ctx, name, uid, gid, follow)
}

func (c *cappedFS) Chtimes(ctx context.Context, name string, atime, mtime time.Time) error {
	if err := c.chargeCopy(ctx, "chtimes", name, true); err != nil {
		return err
	}
	return c.FileSystem.Chtimes(ctx, name, atime, mtime)
}

func (c *cappedFS) Mkfifo(ctx context.Context, name string, perm stdfs.FileMode) error {
	fifo, ok := c.FileSystem.(gbfs.FIFOFileSystem)
	if !ok {
		// What gbash answers for a filesystem without named pipes.
		return &os.PathError{Op: "mkfifo", Path: name, Err: stdfs.ErrPermission}
	}
	if err := c.charge("mkfifo", name, int64(len(name))); err != nil {
		return err
	}
	return fifo.Mkfifo(ctx, name, perm)
}

func (c *cappedFS) Lchtimes(ctx context.Context, name string, atime, mtime time.Time) error {
	if err := c.chargeCopy(ctx, "chtimes", name, false); err != nil {
		return err
	}
	if l, ok := c.FileSystem.(interface {
		Lchtimes(context.Context, string, time.Time, time.Time) error
	}); ok {
		return l.Lchtimes(ctx, name, atime, mtime)
	}
	return c.FileSystem.Chtimes(ctx, name, atime, mtime)
}

// cappedFile charges each write. gbash's in-memory files implement only Read,
// Write, ReadFrom, WriteTo, Close and Stat, and gbash looks for none of the
// last-but-two on a file it opened, so embedding the File interface hides
// nothing it uses. Hiding ReadFrom is the point: io.Copy into the file then
// goes through Write and is charged.
type cappedFile struct {
	gbfs.File
	name   string
	budget *writeBudget
}

func (f *cappedFile) Write(p []byte) (int, error) {
	fit := int(f.budget.take(int64(len(p))))
	n, err := f.File.Write(p[:fit])
	if err == nil && fit < len(p) {
		err = errNoSpace("write", f.name)
	}
	return n, err
}
