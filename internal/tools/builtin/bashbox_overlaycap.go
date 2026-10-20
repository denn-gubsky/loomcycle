package builtin

import (
	"context"
	stdfs "io/fs"
	"os"
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
func (b *writeBudget) take(n int) int {
	for {
		used := b.used.Load()
		left := b.max - used
		if left <= 0 {
			return 0
		}
		grant := int64(n)
		if grant > left {
			grant = left
		}
		if b.used.CompareAndSwap(used, used+grant) {
			return int(grant)
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
	return &cappedFS{FileSystem: fsys, budget: f.budget}, nil
}

// cappedFS charges the calls that make a filesystem hold more: data written
// through an open file, and the names of new files, directories and links (a
// symlink's target is a string as long as a shell variable allows). Reads and
// metadata changes pass through. Mkfifo and Lchtimes are optional
// filesystem methods gbash looks for, so they are forwarded explicitly.
type cappedFS struct {
	gbfs.FileSystem
	budget *writeBudget
}

func (c *cappedFS) charge(op, name string, n int) error {
	if c.budget.take(n) < n {
		return errNoSpace(op, name)
	}
	return nil
}

func (c *cappedFS) OpenFile(ctx context.Context, name string, flag int, perm stdfs.FileMode) (gbfs.File, error) {
	if flag&os.O_CREATE != 0 {
		if err := c.charge("open", name, len(name)); err != nil {
			return nil, err
		}
	}
	f, err := c.FileSystem.OpenFile(ctx, name, flag, perm)
	if err != nil || flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_TRUNC) == 0 {
		return f, err
	}
	return &cappedFile{File: f, name: name, budget: c.budget}, nil
}

func (c *cappedFS) MkdirAll(ctx context.Context, name string, perm stdfs.FileMode) error {
	if err := c.charge("mkdir", name, len(name)); err != nil {
		return err
	}
	return c.FileSystem.MkdirAll(ctx, name, perm)
}

func (c *cappedFS) Symlink(ctx context.Context, target, linkName string) error {
	if err := c.charge("symlink", linkName, len(target)+len(linkName)); err != nil {
		return err
	}
	return c.FileSystem.Symlink(ctx, target, linkName)
}

func (c *cappedFS) Link(ctx context.Context, oldName, newName string) error {
	if err := c.charge("link", newName, len(newName)); err != nil {
		return err
	}
	return c.FileSystem.Link(ctx, oldName, newName)
}

func (c *cappedFS) Rename(ctx context.Context, oldName, newName string) error {
	if err := c.charge("rename", newName, len(newName)); err != nil {
		return err
	}
	return c.FileSystem.Rename(ctx, oldName, newName)
}

func (c *cappedFS) Mkfifo(ctx context.Context, name string, perm stdfs.FileMode) error {
	fifo, ok := c.FileSystem.(gbfs.FIFOFileSystem)
	if !ok {
		// What gbash answers for a filesystem without named pipes.
		return &os.PathError{Op: "mkfifo", Path: name, Err: stdfs.ErrPermission}
	}
	if err := c.charge("mkfifo", name, len(name)); err != nil {
		return err
	}
	return fifo.Mkfifo(ctx, name, perm)
}

func (c *cappedFS) Lchtimes(ctx context.Context, name string, atime, mtime time.Time) error {
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
	fit := f.budget.take(len(p))
	n, err := f.File.Write(p[:fit])
	if err == nil && fit < len(p) {
		err = errNoSpace("write", f.name)
	}
	return n, err
}
