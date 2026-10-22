package builtin

import (
	"context"
	stdfs "io/fs"
	"os"
	"path"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ewhauser/gbash"
	gbfs "github.com/ewhauser/gbash/fs"
)

// writeBudget is the byte allowance one Bashbox call has for in-memory files
// (RFC ES follow-up). gbash keeps a read-only volume's writes, and the shadow
// over the dynamic-volume root, in memory with no size limit, and that memory
// is loomcycle's heap, shared with every run on the replica: `yes > f` passed
// 3 GiB in a second. Every byte written, and every entry created, is
// charged; nothing is refunded on delete, so the budget is the TOTAL a call
// may write, which bounds what it can hold at once. Shared by the concurrent
// commands of a pipeline, hence atomic.
type writeBudget struct {
	max  int64
	used atomic.Int64
	// hit records that a charge was refused, so the result can say so; a call
	// that used exactly the budget without being refused did not hit it.
	hit atomic.Bool
}

// take charges n bytes and reports how many fit: all of n, or what was left.
func (b *writeBudget) take(n int64) int64 {
	for {
		used := b.used.Load()
		left := max(b.max-used, 0)
		grant := n
		if grant > left {
			grant = left
			b.hit.Store(true)
		}
		if grant == 0 || b.used.CompareAndSwap(used, used+grant) {
			return grant
		}
	}
}

// entryOverhead is what one entry costs gbash's in-memory filesystem beyond
// its absolute path: the node, the nodes-map key holding that path, the
// parent's child entry, and a child map for a directory — about 700 bytes
// measured. Charging only the name let 500k touch grow the heap by 354 MiB
// and repeated mkdir by 743 MiB under a 64 MiB cap.
const entryOverhead = 512

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

// cappedReadOnlyVolume is gbash.HostDirectoryFileSystem built from gbash's
// public parts, with the host (lower) layer wrapped so the bytes its copy-ups
// read into memory are charged. gbash.HostDirectoryFileSystem is exactly
// gbfs.Overlay(gbfs.Host(root at the workspace mount point)); building it here
// is what gives the lower layer a seam, since the overlay's in-memory upper
// layer is created inside gbfs.NewOverlay and cannot be wrapped.
func cappedReadOnlyVolume(root string, budget *writeBudget) gbash.FileSystemConfig {
	host := gbfs.Host(gbfs.HostOptions{Root: root, VirtualRoot: gbash.DefaultWorkspaceMountPoint})
	return gbash.FileSystemConfig{
		Factory: cappedFactory{
			inner:  gbfs.Overlay(copyUpChargedFactory{inner: host, budget: budget}),
			budget: budget,
		},
		WorkingDir: gbash.DefaultWorkspaceMountPoint,
	}
}

// cappedFS charges what a script writes into the in-memory layer: data
// written through an open file, and each entry it creates at entryOverhead
// plus the entry's absolute path (and a symlink's target, a string as long as
// a shell variable allows). Content copied up from a read-only volume is
// charged where it is read, by copyUpChargedFS.
//
// Not charged: renaming a directory of the read-only volume makes gbash clone
// every entry under it straight into the in-memory layer, below this wrapper
// (OverlayFS.Rename → moveMerged → cloneDirNode). That costs metadata only —
// file content stays lazy and is charged when read — and is bounded by the
// size of the operator's tree, not by anything the script can grow.
//
// Mkfifo and Lchtimes are optional filesystem methods gbash looks for, so
// they are forwarded explicitly.
type cappedFS struct {
	gbfs.FileSystem
	budget *writeBudget
}

func (c *cappedFS) charge(op, name string, n int) error {
	if c.budget.take(int64(n)) < int64(n) {
		return errNoSpace(op, name)
	}
	return nil
}

// entryCost is what creating name costs: entryOverhead plus its absolute
// path, which gbash stores as the node's map key.
func (c *cappedFS) entryCost(name string) int {
	return entryOverhead + len(gbfs.Resolve(c.FileSystem.Getwd(), name))
}

// exists reports whether name is already an entry, so reopening an existing
// file, or mkdir -p over existing directories, creates nothing to charge.
func (c *cappedFS) exists(ctx context.Context, name string) bool {
	_, err := c.FileSystem.Lstat(ctx, name)
	return err == nil
}

func (c *cappedFS) OpenFile(ctx context.Context, name string, flag int, perm stdfs.FileMode) (gbfs.File, error) {
	if flag&os.O_CREATE != 0 && !c.exists(ctx, name) {
		if err := c.charge("open", name, c.entryCost(name)); err != nil {
			return nil, err
		}
	}
	f, err := c.FileSystem.OpenFile(ctx, name, flag, perm)
	if err != nil || flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_TRUNC) == 0 {
		return f, err
	}
	// A named pipe is charged too. gbash v0.0.40 buffered it in memory without
	// limit, so a writer whose reader never read grew the heap past the cap;
	// v0.0.41 bounds the pipe at 64 KiB and blocks the writer, so the charge is
	// now a second guard against a gbash regression. The cost is a false
	// refusal for a pipeline streaming more than the cap through a FIFO.
	return &cappedFile{File: f, name: name, budget: c.budget}, nil
}

// MkdirAll charges every directory it will create: each component of the
// path, from the deepest that exists down.
func (c *cappedFS) MkdirAll(ctx context.Context, name string, perm stdfs.FileMode) error {
	abs := gbfs.Resolve(c.FileSystem.Getwd(), name)
	cost := 0
	for p := abs; p != "/" && p != "." && !c.exists(ctx, p); p = path.Dir(p) {
		cost += entryOverhead + len(p)
	}
	if err := c.charge("mkdir", name, cost); err != nil {
		return err
	}
	return c.FileSystem.MkdirAll(ctx, name, perm)
}

func (c *cappedFS) Symlink(ctx context.Context, target, linkName string) error {
	if err := c.charge("symlink", linkName, c.entryCost(linkName)+len(target)); err != nil {
		return err
	}
	return c.FileSystem.Symlink(ctx, target, linkName)
}

func (c *cappedFS) Link(ctx context.Context, oldName, newName string) error {
	if err := c.charge("link", newName, c.entryCost(newName)); err != nil {
		return err
	}
	return c.FileSystem.Link(ctx, oldName, newName)
}

func (c *cappedFS) Rename(ctx context.Context, oldName, newName string) error {
	if err := c.charge("rename", newName, c.entryCost(newName)); err != nil {
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
	if err := c.charge("mkfifo", name, c.entryCost(name)); err != nil {
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

// copyUpChargedFactory wraps an overlay's lower (host) layer.
type copyUpChargedFactory struct {
	inner  gbfs.Factory
	budget *writeBudget
}

func (f copyUpChargedFactory) New(ctx context.Context) (gbfs.FileSystem, error) {
	fsys, err := f.inner.New(ctx)
	if err != nil {
		return nil, err
	}
	return &copyUpChargedFS{FileSystem: fsys, budget: f.budget}, nil
}

// copyUpChargedFS charges every byte read through Open. On a read-only
// volume, chmod, chown, touch, mv, ln or an append copies the volume's file
// into the overlay's in-memory layer; gbash reads its content the first time
// the copy is opened, through the lower layer's Open, and keeps all of it.
// Charging the bytes as they are read counts what is actually stored, however
// many names reach the file, whatever was removed before, and however much
// the file grew after the copy was made. A read past the budget fails, so the
// copy fails with "no space left on device" instead of being kept.
//
// Ordinary reads of the volume go through OpenFile, stream from disk, keep
// nothing in memory, and are not charged. That split is gbash's (v0.0.41
// overlay.go copyUpLowerEntry uses Open; every other lower read uses
// OpenFile). If a gbash bump changed it, ordinary reads would start to be
// charged — refused early, never an uncounted copy — and
// TestBashbox_RoReadsOfTheVolumeAreNotCharged would fail.
type copyUpChargedFS struct {
	gbfs.FileSystem
	budget *writeBudget
}

func (l *copyUpChargedFS) Open(ctx context.Context, name string) (gbfs.File, error) {
	f, err := l.FileSystem.Open(ctx, name)
	if err != nil {
		return nil, err
	}
	return &copyUpChargedFile{File: f, name: name, budget: l.budget}, nil
}

// copyUpChargedFile charges each read, failing once the budget is spent.
type copyUpChargedFile struct {
	gbfs.File
	name   string
	budget *writeBudget
}

func (f *copyUpChargedFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		if fit := f.budget.take(int64(n)); fit < int64(n) {
			return int(fit), errNoSpace("read", f.name)
		}
	}
	return n, err
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
