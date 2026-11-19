package fsops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The default bounds of the file operations, the same as the tools state in
// their descriptions.
const (
	DefaultReadMaxBytes  = 256 * 1024 // Read: bytes returned
	DefaultWriteMaxBytes = 1 << 20    // Write: content accepted
	DefaultEditMaxBytes  = 1 << 20    // Edit: file size loaded
)

var (
	// ErrPathRequired is an operation given no path. Without the check an
	// empty path would name the root itself.
	ErrPathRequired = errors.New("path is required")
	// ErrEditNoChange is an edit whose old and new strings are equal.
	ErrEditNoChange = errors.New("old_string and new_string must differ")
	// ErrEditNotFound is an edit whose old string is not in the file.
	ErrEditNotFound = errors.New("old_string not found in file")
)

// ReadRequest is one read. MaxBytes 0 means DefaultReadMaxBytes. Lines nil
// reads the file's bytes from the start; non-nil reads a line range (its
// MaxBytes is set from the request's).
type ReadRequest struct {
	Path     string
	MaxBytes int64
	Lines    *LineWindow
}

// Read returns the text of the file req.Path names under root, confined as the
// package states. Without Lines it returns at most MaxBytes bytes, silently
// cut; with Lines it returns what ReadLines does (ErrBinaryLineRange for a
// binary file).
func Read(ctx context.Context, root, fence string, req ReadRequest) (string, error) {
	if req.Path == "" {
		return "", ErrPathRequired
	}
	dir, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	resolved, err := resolveUnder(dir, req.Path)
	if err == nil {
		err = OutsideFence(fence, resolved)
	}
	if err != nil {
		return "", err
	}

	maxBytes := req.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultReadMaxBytes
	}

	vfs, err := openResolved(dir, fence)
	if err != nil {
		return "", err
	}
	defer vfs.Close()
	rel, err := vfs.Rel(resolved)
	if err != nil {
		return "", err
	}
	f, err := vfs.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if req.Lines != nil {
		w := *req.Lines
		w.MaxBytes = maxBytes
		return ReadLines(ctx, f, w)
	}

	// io.ReadAll over a LimitReader handles a short read (a single
	// os.File.Read may legally return fewer bytes than requested — e.g. on a
	// FIFO/device, a network FS, or a >1 GiB read) and treats EOF as success.
	// A file larger than maxBytes is bounded to exactly maxBytes.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Write creates or replaces whole the file path names under root with
// content, at most maxBytes bytes (0 means DefaultWriteMaxBytes). Only the
// parent directory has to exist. The write is atomic (ReplaceFile): on any
// failure the target is unchanged and no temporary file is left. Returns the
// result line the Write tool reports.
func Write(ctx context.Context, root, fence, path, content string, maxBytes int64) (string, error) {
	if path == "" {
		return "", ErrPathRequired
	}
	if maxBytes == 0 {
		maxBytes = DefaultWriteMaxBytes
	}
	if int64(len(content)) > maxBytes {
		return "", fmt.Errorf("content exceeds %d bytes (got %d)", maxBytes, len(content))
	}
	dirPath, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	target, err := resolveParentUnder(dirPath, path)
	if err == nil {
		err = OutsideFence(fence, target)
	}
	if err != nil {
		return "", err
	}

	vfs, err := openResolved(dirPath, fence)
	if err != nil {
		return "", err
	}
	defer vfs.Close()
	rel, err := vfs.Rel(target)
	if err != nil {
		return "", err
	}
	dir, err := vfs.OpenDir(filepath.Dir(rel))
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err := ReplaceFile(ctx, dir, filepath.Base(rel), ".loomcycle-write-", content); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), target), nil
}

// EditRequest is one edit. MaxBytes 0 means DefaultEditMaxBytes.
type EditRequest struct {
	Path       string
	OldString  string
	NewString  string
	ReplaceAll bool
	MaxBytes   int64
}

// Edit replaces text in the existing file req.Path names under root, with
// Replace's semantics, and writes the result back atomically (ReplaceFile). A
// file larger than MaxBytes is refused unread. Returns the result line the
// Edit tool reports.
func Edit(ctx context.Context, root, fence string, req EditRequest) (string, error) {
	if req.Path == "" {
		return "", ErrPathRequired
	}
	if req.OldString == req.NewString {
		return "", ErrEditNoChange
	}
	maxBytes := req.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultEditMaxBytes
	}
	dirPath, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	// The file must exist, so the whole path is resolved (Write resolves the
	// parent because its file may be new).
	resolved, err := resolveUnder(dirPath, req.Path)
	if err == nil {
		err = OutsideFence(fence, resolved)
	}
	if err != nil {
		return "", err
	}

	// Read and replace through one handle on the file's directory.
	vfs, err := openResolved(dirPath, fence)
	if err != nil {
		return "", err
	}
	defer vfs.Close()
	rel, err := vfs.Rel(resolved)
	if err != nil {
		return "", err
	}
	dir, err := vfs.OpenDir(filepath.Dir(rel))
	if err != nil {
		return "", err
	}
	defer dir.Close()
	base := filepath.Base(rel)

	f, err := vfs.OpenIn(dir, base)
	if err != nil {
		return "", vfs.PathErr("open", rel, err)
	}
	body, err := readCapped(f, maxBytes)
	if err != nil {
		return "", err
	}
	replaced, count, err := Replace(string(body), req.OldString, req.NewString, req.ReplaceAll)
	if err != nil {
		return "", err
	}
	if err := ReplaceFile(ctx, dir, base, ".loomcycle-edit-", replaced); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d replacement%s)", resolved, count, plural(count)), nil
}

// readCapped reads f whole and closes it, refusing a file whose size is over
// maxBytes before reading any of it.
func readCapped(f *os.File, maxBytes int64) ([]byte, error) {
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("file size %d exceeds limit %d", info.Size(), maxBytes)
	}
	return io.ReadAll(f)
}

// Replace replaces oldStr in body with newStr. Without all, oldStr must occur
// exactly once: an ambiguous match is an error asking for more context, not a
// guess. Returns the new body and the number of occurrences replaced.
func Replace(body, oldStr, newStr string, all bool) (string, int, error) {
	if oldStr == newStr {
		return "", 0, ErrEditNoChange
	}
	count := strings.Count(body, oldStr)
	if count == 0 {
		return "", 0, ErrEditNotFound
	}
	if count > 1 && !all {
		return "", 0, fmt.Errorf("old_string occurs %d times; supply more context or set replace_all=true", count)
	}
	if all {
		return strings.ReplaceAll(body, oldStr, newStr), count, nil
	}
	return strings.Replace(body, oldStr, newStr, 1), count, nil
}

// ReplaceFile writes content to name inside dir atomically: into a temporary
// file (tempPrefix + random, mode 0600) in the same directory, then one
// rename into place, so a reader never sees a partial file (POSIX rename is
// atomic on one filesystem). On any failure, ctx's cancellation before the
// rename included, the temporary file is removed and name is unchanged.
//
// A symlink at name is replaced by the rename, never followed.
func ReplaceFile(ctx context.Context, dir *os.Root, name, tempPrefix, content string) error {
	tmp, tmpName, err := CreateTemp(dir, tempPrefix)
	if err != nil {
		return fmt.Errorf("create tempfile: %w", err)
	}
	// After a successful rename the temp name no longer exists, so the
	// Remove is a no-op.
	defer dir.Remove(tmpName)

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tempfile: %w", err)
	}
	// Honour cancellation BEFORE the visible rename so a cancelled call
	// leaves no trace at the target.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dir.Rename(tmpName, name); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
