package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// errOverBudget is a tree copy that would hold more bytes than it may. What
// it made is removed.
var errOverBudget = errors.New("the copy is over its byte budget")

// copyResult is what a tree copy made, counted as the quota counts
// (entrySize): regular files and links.
type copyResult struct {
	bytes   int64
	files   int
	skipped []string
}

// copyTree makes dst, which must not exist, a copy of the folder src (a
// path with no link in it) by the worker's mechanism, leaving out the
// workspace's work folder at its top. It never follows a link: a link is
// copied as a link, whatever it points at. Past budget bytes it stops with
// errOverBudget and leaves nothing at dst.
func (w *worker) copyTree(ctx context.Context, src, dst string, budget int64) (copyResult, error) {
	var (
		res copyResult
		err error
	)
	if w.mech == workspace.MechanismClone {
		res, err = cloneTree(ctx, src, dst, budget)
	} else {
		res, err = walkCopy(ctx, src, dst, budget, w.mech == workspace.MechanismReflink)
	}
	if err != nil {
		_ = removeDir(dst)
	}
	return res, err
}

// cloneTree clones src whole (APFS), then removes the work folder from the
// clone and counts it. A clone shares its blocks with src, so it costs no
// disk until either side is rewritten; it is still counted at full size.
func cloneTree(ctx context.Context, src, dst string, budget int64) (copyResult, error) {
	if err := cloneDir(src, dst); err != nil {
		return copyResult{}, fmt.Errorf("cloning the folder: %w", err)
	}
	r, err := os.OpenRoot(dst)
	if err != nil {
		return copyResult{}, err
	}
	defer r.Close()
	if err := r.RemoveAll(workDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return copyResult{}, err
	}
	// The top folder of a clone keeps its source's mode; the worker must
	// own it whole.
	if err := os.Chmod(dst, 0o700); err != nil {
		return copyResult{}, err
	}
	v, err := fsops.OpenVolumeFS(dst, "")
	if err != nil {
		return copyResult{}, err
	}
	defer v.Close()
	var res copyResult
	walkTree(v, v.Root(), "", func(_ *os.Root, _ string, fi os.FileInfo) bool {
		if ctx.Err() != nil {
			return false
		}
		res.bytes += entrySize(fi)
		if fi.Mode().IsRegular() {
			res.files++
		}
		return res.bytes <= budget
	}, nil)
	if err := ctx.Err(); err != nil {
		return copyResult{}, err
	}
	if res.bytes > budget {
		return copyResult{}, errOverBudget
	}
	return res, nil
}

// walkCopy copies src into dst entry by entry: each folder made, each link
// recreated as a link, each regular file cloned (reflink, where the file
// system takes it) or copied. Anything else (a FIFO, a socket, a device) is
// skipped and named. A file's permission bits and modification time are
// kept; a folder is kept writable by its owner, the worker, so a
// checkpoint can always be removed.
func walkCopy(ctx context.Context, src, dst string, budget int64, reflink bool) (copyResult, error) {
	v, err := fsops.OpenVolumeFS(src, "")
	if err != nil {
		return copyResult{}, err
	}
	defer v.Close()
	if err := os.Mkdir(dst, 0o700); err != nil {
		return copyResult{}, err
	}
	out, err := os.OpenRoot(dst)
	if err != nil {
		return copyResult{}, err
	}
	defer out.Close()
	type dirTime struct {
		rel string
		fi  os.FileInfo
	}
	var (
		res  copyResult
		dirs []dirTime
		werr error
	)
	walkTree(v, v.Root(), "", func(parent *os.Root, rel string, fi os.FileInfo) bool {
		if ctx.Err() != nil {
			werr = ctx.Err()
			return false
		}
		if rel == workDir {
			return true
		}
		switch m := fi.Mode(); {
		case m.IsDir():
			if werr = out.Mkdir(rel, (m.Perm()|0o700)&0o777); werr != nil {
				return false
			}
			dirs = append(dirs, dirTime{rel, fi})
		case m&os.ModeSymlink != 0:
			if res.bytes+fi.Size() > budget {
				werr = errOverBudget
				return false
			}
			target, err := parent.Readlink(filepath.Base(rel))
			if err != nil {
				werr = err
				return false
			}
			if werr = out.Symlink(target, rel); werr != nil {
				return false
			}
			res.bytes += fi.Size()
		case m.IsRegular():
			n, err := copyRegular(v, parent, filepath.Base(rel), out, rel, fi, budget-res.bytes, reflink)
			switch {
			case errors.Is(err, errUnreadable):
				addSkipped(&res, rel)
				return true
			case err != nil:
				werr = err
				return false
			}
			res.bytes += n
			res.files++
		default:
			addSkipped(&res, rel)
		}
		return true
	}, func(rel string) bool { return rel != workDir })
	if werr != nil {
		return copyResult{}, werr
	}
	// Folders last, deepest first: writing a file into one changes its time.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = out.Chtimes(dirs[i].rel, dirs[i].fi.ModTime(), dirs[i].fi.ModTime())
	}
	return res, nil
}

func addSkipped(res *copyResult, rel string) {
	if len(res.skipped) < workspace.MaxSkippedNamed {
		res.skipped = append(res.skipped, filepath.ToSlash(rel))
	}
}

// errUnreadable is a regular file the worker may not read (its mode lets
// no one read it): it is left out of the copy and named, never made
// readable, since the workspace's files are its agent's.
var errUnreadable = errors.New("unreadable")

// copyRegular copies one regular file, opened without following a link and
// never a FIFO (fsops.OpenIn), into out at rel, and returns its size. A file
// bigger than room stops the copy with errOverBudget: a command still
// writing can grow a file after it was listed, so the bound holds on what
// is copied, not on what was listed.
func copyRegular(v *fsops.VolumeFS, parent *os.Root, base string, out *os.Root, rel string, fi os.FileInfo, room int64, reflink bool) (int64, error) {
	if fi.Size() > room {
		return 0, errOverBudget
	}
	in, err := v.OpenIn(parent, base)
	if errors.Is(err, fs.ErrPermission) {
		return 0, errUnreadable
	}
	if err != nil {
		return 0, err
	}
	defer in.Close()
	f, err := out.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := fillFile(f, in, room, reflink)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if err := out.Chmod(rel, fi.Mode().Perm()); err != nil {
		return 0, err
	}
	_ = out.Chtimes(rel, fi.ModTime(), fi.ModTime())
	return n, nil
}

// fillFile writes in's content into f, by a clone where reflink is set and
// the file system takes it, else by copying at most room bytes.
func fillFile(f, in *os.File, room int64, reflink bool) (int64, error) {
	if reflink && cloneFile(f, in) == nil {
		st, err := f.Stat()
		if err != nil {
			return 0, err
		}
		if st.Size() > room {
			return 0, errOverBudget
		}
		return st.Size(), nil
	}
	n, err := io.Copy(f, io.LimitReader(in, room+1))
	if err != nil {
		return 0, err
	}
	if n > room {
		return 0, errOverBudget
	}
	return n, nil
}

// removeDir removes a folder the worker made (a checkpoint, a restore's
// old tree), first making every folder in it writable by its owner, so a
// folder a command made read-only cannot keep its files on the disk. Links
// are never followed.
func removeDir(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
