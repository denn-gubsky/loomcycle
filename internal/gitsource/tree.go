package gitsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Why a path of the commit was not written.
const (
	ReasonSymlink    = "symlink"     // a link whose target is absolute or may leave the tree
	ReasonSubmodule  = "submodule"   // another repository's commit; an empty directory stands in
	ReasonTooLarge   = "too_large"   // a file over the per-file limit
	ReasonUnsafePath = "unsafe_path" // a path that is not a plain relative path, or is reserved
	ReasonCollision  = "collision"   // a path that names the same file as one already written
)

// Omitted is one path of the commit that was left out of the tree.
type Omitted struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Size   int64  `json:"size,omitempty"`
}

// TreeLimits bound what WriteTree writes.
type TreeLimits struct {
	// MaxBytes bounds the total size of the files written. Required.
	MaxBytes int64
	// MaxFiles bounds how many files, links and submodule directories are
	// written. Required. With the depth limit on a path it also bounds how
	// many directories exist when the tree is done.
	MaxFiles int
	// MaxFileBytes is the largest single file written; a larger one is
	// omitted, not an error. Required.
	MaxFileBytes int64
	// Reserved are top-level names the commit's own paths may not use: the
	// caller writes its own files there afterwards.
	Reserved []string
}

// TreeResult describes what WriteTree wrote.
type TreeResult struct {
	Files int   // regular files and links written
	Bytes int64 // bytes of the regular files
	// Omitted lists what was left out, up to a fixed number of entries;
	// OmittedCount is how many were left out in all.
	Omitted      []Omitted
	OmittedCount int
}

// maxOmittedListed bounds the Omitted list, so a commit of a million
// symlinks cannot make the report itself the problem.
const maxOmittedListed = 1000

// WriteTree writes the fetched commit's tree into dest, which must be an
// existing, empty directory.
//
// The commit decides the paths and the contents; this function decides what
// reaching the disk means. Nothing the commit contains can make it write
// outside dest, create a file that is executable, or leave behind a link
// that resolves outside dest:
//
//   - Every write goes through an os.Root opened on dest, which refuses any
//     path that would leave it, whatever is on disk.
//   - A regular file is written with mode 0644. The executable bit is dropped.
//   - A symbolic link is created only when its target cannot leave the tree:
//     relative, and with ".." only at its start and no more of them than the
//     link is deep. Such a target stays inside whatever other links it passes
//     through, because each of those obeys the same rule. Any other link is
//     omitted. After the tree is written every link is resolved once more
//     for real, and one that does leave is removed.
//   - A submodule becomes an empty directory. A file over the per-file limit,
//     a path that is not a plain relative path or uses a reserved name, and a
//     path that collides with one already written (on a file system that
//     ignores case) are omitted.
//
// Passing a limit on total bytes or on the number of files is an error
// (ErrTooLarge), not an omission: the caller asked for the commit and would
// get part of it.
func (f *Fetched) WriteTree(ctx context.Context, dest string, lim TreeLimits) (TreeResult, error) {
	var res TreeResult
	if lim.MaxBytes <= 0 || lim.MaxFiles <= 0 || lim.MaxFileBytes <= 0 {
		return res, fmt.Errorf("%w: tree limits are required", ErrInvalidRequest)
	}
	// The object store must not be inside the tree: a reader of the tree
	// would find git's files in it.
	if within(f.storeDir, dest) {
		return res, fmt.Errorf("%w: the object store is inside the destination directory", ErrInvalidRequest)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return res, fmt.Errorf("%w: destination: %v", ErrFetchFailed, err)
	}
	if len(entries) != 0 {
		return res, fmt.Errorf("%w: the destination directory is not empty", ErrInvalidRequest)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return res, fmt.Errorf("%w: destination: %v", ErrFetchFailed, err)
	}
	defer root.Close()

	tree, err := f.commit.Tree()
	if err != nil {
		return res, fmt.Errorf("%w: read the commit's tree: %v", ErrFetchFailed, err)
	}
	reserved := map[string]bool{}
	for _, r := range lim.Reserved {
		reserved[strings.ToLower(r)] = true
	}
	omit := func(p, reason string, size int64) {
		res.OmittedCount++
		if len(res.Omitted) < maxOmittedListed {
			res.Omitted = append(res.Omitted, Omitted{Path: p, Reason: reason, Size: size})
		}
	}
	// Links created so far. A path UNDER one of them is refused: a tree has
	// no business holding both "l" as a link and "l/file", and writing the
	// second would follow the first.
	links := map[string]bool{}
	underLink := func(p string) bool {
		for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if links[dir] {
				return true
			}
		}
		return false
	}

	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	for {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		name, e, err := w.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, fmt.Errorf("%w: walk the tree: %v", ErrFetchFailed, err)
		}
		if e.Mode == filemode.Dir {
			continue // made when something is written under it
		}
		if !safeRelPath(name) || reserved[strings.ToLower(strings.SplitN(name, "/", 2)[0])] || underLink(name) {
			omit(name, ReasonUnsafePath, 0)
			continue
		}
		switch e.Mode {
		case filemode.Submodule:
			// It makes a directory, so it counts: a commit of nothing but
			// submodule entries must not be a way past the file limit.
			if res.Files >= lim.MaxFiles {
				return res, fmt.Errorf("%w: more than %d files", ErrTooLarge, lim.MaxFiles)
			}
			if err := root.MkdirAll(filepath.FromSlash(name), 0o755); err != nil {
				return res, fmt.Errorf("%w: %s: %v", ErrFetchFailed, name, err)
			}
			res.Files++
			omit(name, ReasonSubmodule, 0)
			continue
		case filemode.Regular, filemode.Executable, filemode.Deprecated, filemode.Symlink:
		default:
			omit(name, ReasonUnsafePath, 0)
			continue
		}

		blob, err := f.repo.BlobObject(e.Hash)
		if err != nil {
			return res, fmt.Errorf("%w: %s: %v", ErrFetchFailed, name, err)
		}
		if e.Mode == filemode.Symlink {
			target, ok := readLinkTarget(blob)
			if !ok || !linkStaysInside(name, target) {
				omit(name, ReasonSymlink, 0)
				continue
			}
			if res.Files >= lim.MaxFiles {
				return res, fmt.Errorf("%w: more than %d files", ErrTooLarge, lim.MaxFiles)
			}
			if err := mkParents(root, name); err != nil {
				return res, fmt.Errorf("%w: %s: %v", ErrFetchFailed, name, err)
			}
			if err := root.Symlink(filepath.FromSlash(target), filepath.FromSlash(name)); err != nil {
				if errors.Is(err, fs.ErrExist) {
					omit(name, ReasonCollision, 0)
					continue
				}
				return res, fmt.Errorf("%w: %s: %v", ErrFetchFailed, name, err)
			}
			links[name] = true
			res.Files++
			continue
		}

		if blob.Size > lim.MaxFileBytes {
			omit(name, ReasonTooLarge, blob.Size)
			continue
		}
		if res.Files >= lim.MaxFiles {
			return res, fmt.Errorf("%w: more than %d files", ErrTooLarge, lim.MaxFiles)
		}
		if res.Bytes+blob.Size > lim.MaxBytes {
			return res, fmt.Errorf("%w: the tree is over %d bytes", ErrTooLarge, lim.MaxBytes)
		}
		if err := mkParents(root, name); err != nil {
			return res, fmt.Errorf("%w: %s: %v", ErrFetchFailed, name, err)
		}
		n, err := writeBlob(root, name, blob)
		if errors.Is(err, fs.ErrExist) {
			omit(name, ReasonCollision, 0)
			continue
		}
		if err != nil {
			return res, fmt.Errorf("%w: %s: %v", ErrFetchFailed, name, err)
		}
		res.Files++
		res.Bytes += n
	}

	// The rule above keeps a link inside by construction. Resolve each one
	// for real as well: this is the property the readers depend on, and it
	// costs one lstat chain per link.
	realRoot, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return res, fmt.Errorf("%w: destination: %v", ErrFetchFailed, err)
	}
	for name := range links {
		full := filepath.Join(realRoot, filepath.FromSlash(name))
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			continue // dangling (its target was omitted): it leads nowhere
		}
		if !within(resolved, realRoot) {
			_ = root.Remove(filepath.FromSlash(name))
			res.Files--
			omit(name, ReasonSymlink, 0)
		}
	}
	return res, nil
}

// Limits on one path. A file brings its directories with it, so a path of a
// thousand segments would be a thousand directories for one counted file.
const (
	maxPathLength = 4096
	maxPathDepth  = 64
)

// within reports whether p is dir or inside it. Both are cleaned; neither is
// resolved.
func within(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// maxLinkTarget is the longest link target read: a real one is a short path.
const maxLinkTarget = 4096

func readLinkTarget(blob *object.Blob) (string, bool) {
	if blob.Size <= 0 || blob.Size > maxLinkTarget {
		return "", false
	}
	r, err := blob.Reader()
	if err != nil {
		return "", false
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, maxLinkTarget+1))
	if err != nil || len(b) > maxLinkTarget {
		return "", false
	}
	return string(b), true
}

// safeRelPath reports whether p is a plain relative path this package will
// write: slash-separated, no empty, "." or ".." segment, no ".git" segment in
// any letter case, and no character that means something else on some file
// system (a backslash, a colon, a control character).
func safeRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || len(p) > maxPathLength || strings.Count(p, "/") >= maxPathDepth {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' || r == ':' {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return false
		}
		// Names some file systems treat as the one above: a trailing dot or
		// space is dropped, so ".git." and ".git " open ".git".
		if trimmed := strings.TrimRight(seg, ". "); trimmed != seg && (trimmed == "" || strings.EqualFold(trimmed, ".git")) {
			return false
		}
	}
	return true
}

// linkStaysInside reports whether a link at linkPath with the given target
// cannot resolve outside the tree, whatever other links it passes through.
//
// The target must be relative, and its ".." segments must all come first and
// number no more than the link's depth. Going up from the link's own
// directory that many times lands on a real directory of the tree, and from
// there the target only goes down. Going down may pass through another link,
// which obeys this same rule and therefore lands inside too; no ".." can
// follow it, so there is no stepping up out of wherever it led.
//
// A check of the cleaned path alone would not do: "a/b/.." is "a" as text,
// and the parent of whatever a/b links to on disk.
func linkStaysInside(linkPath, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") {
		return false
	}
	for _, r := range target {
		if r < 0x20 || r == 0x7f || r == '\\' || r == ':' {
			return false
		}
	}
	depth := strings.Count(linkPath, "/")
	ups, down := 0, false
	for _, seg := range strings.Split(target, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if down {
				return false // a ".." after a name
			}
			ups++
		default:
			if strings.EqualFold(seg, ".git") {
				return false
			}
			down = true
		}
	}
	return ups <= depth
}

func mkParents(root *os.Root, name string) error {
	dir := path.Dir(name)
	if dir == "." {
		return nil
	}
	return root.MkdirAll(filepath.FromSlash(dir), 0o755)
}

// writeBlob writes one blob as a new file and returns its size. O_EXCL makes
// a second path that names the same file fail instead of replacing the first.
func writeBlob(root *os.Root, name string, blob *object.Blob) (int64, error) {
	r, err := blob.Reader()
	if err != nil {
		return 0, err
	}
	defer r.Close()
	out, err := root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	// The blob's size was checked against the limits; copy no more than
	// that, so an object that lies about its size cannot write past them.
	n, err := io.Copy(out, io.LimitReader(r, blob.Size))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}
