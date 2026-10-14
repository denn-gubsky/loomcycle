package gitsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

// DiffLimits bound what WriteDiff writes.
type DiffLimits struct {
	// MaxBytes bounds the size of the patch. Passing it is an error
	// (ErrTooLarge). Required.
	MaxBytes int64
	// MaxFileBytes is the largest file whose content is diffed; a change to
	// a larger one is listed without a body. Required. It also bounds the
	// time the diff takes, which grows with the size of the files compared.
	MaxFileBytes int64
	// Reserved are top-level names whose paths are left out, as WriteTree
	// leaves them out.
	Reserved []string
}

// DiffFile is one changed path, as numstat.json lists it.
type DiffFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // added, deleted, modified
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
	// Skipped says why the change has no body in the patch: the same
	// reasons WriteTree leaves a path out for.
	Skipped string `json:"skipped,omitempty"`
}

// DiffResult describes what WriteDiff wrote.
type DiffResult struct {
	Files int   // changed paths, skipped ones included
	Bytes int64 // bytes of the patch
}

// WriteDiff writes, under dest, a unified diff from the base commit's tree to
// the fetched commit's tree (patchRel) and a per-file summary (numstatRel).
//
// It describes the same tree WriteTree wrote: a path that WriteTree leaves
// out (a link, a submodule, a file over the size limit, an unsafe or reserved
// path) has no body in the patch and is listed in the summary with the
// reason. Renames are not detected: a rename is a deletion and an addition.
// The text is a valid patch, and is not byte for byte what `git diff`
// prints: hunk boundaries can differ.
func (f *Fetched) WriteDiff(ctx context.Context, dest, patchRel, numstatRel string, lim DiffLimits) (DiffResult, error) {
	var res DiffResult
	if f.baseCommit == nil {
		return res, fmt.Errorf("%w: no diff base was fetched", ErrInvalidRequest)
	}
	if lim.MaxBytes <= 0 || lim.MaxFileBytes <= 0 {
		return res, fmt.Errorf("%w: diff limits are required", ErrInvalidRequest)
	}
	for _, rel := range []string{patchRel, numstatRel} {
		if !safeRelPath(rel) {
			return res, fmt.Errorf("%w: output path %q", ErrInvalidRequest, rel)
		}
	}
	baseTree, err := f.baseCommit.Tree()
	if err != nil {
		return res, fmt.Errorf("%w: read the base tree: %v", ErrFetchFailed, err)
	}
	headTree, err := f.commit.Tree()
	if err != nil {
		return res, fmt.Errorf("%w: read the head tree: %v", ErrFetchFailed, err)
	}
	changes, err := object.DiffTreeWithOptions(ctx, baseTree, headTree, &object.DiffTreeOptions{DetectRenames: false})
	if err != nil {
		return res, fmt.Errorf("%w: diff the trees: %v", ErrFetchFailed, err)
	}
	sort.SliceStable(changes, func(i, j int) bool { return changePath(changes[i]) < changePath(changes[j]) })

	reserved := map[string]bool{}
	for _, r := range lim.Reserved {
		reserved[strings.ToLower(r)] = true
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return res, fmt.Errorf("%w: destination: %v", ErrFetchFailed, err)
	}
	defer root.Close()
	// The patch is written as it is made, one file's change at a time, so
	// memory holds one change and not the whole patch.
	patch, err := createNew(root, patchRel)
	if err != nil {
		return res, fmt.Errorf("%w: write %s: %v", ErrFetchFailed, patchRel, err)
	}
	defer patch.Close()
	var written int64
	files := make([]DiffFile, 0, len(changes))
	for _, c := range changes {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		action, err := c.Action()
		if err != nil {
			return res, fmt.Errorf("%w: %v", ErrFetchFailed, err)
		}
		df := DiffFile{Path: changePath(c), Status: statusOf(action)}
		if reason := f.skipReason(c, df.Path, reserved, lim.MaxFileBytes); reason != "" {
			df.Skipped = reason
			files = append(files, df)
			continue
		}
		p, err := c.PatchContext(ctx)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return res, fmt.Errorf("%w: %v", ErrTimeout, err)
			}
			return res, fmt.Errorf("%w: diff %s: %v", ErrFetchFailed, df.Path, err)
		}
		for _, fp := range p.FilePatches() {
			if fp.IsBinary() {
				df.Binary = true
			}
		}
		for _, st := range p.Stats() {
			df.Added += st.Addition
			df.Deleted += st.Deletion
		}
		var one bytes.Buffer
		if err := p.Encode(&one); err != nil {
			return res, fmt.Errorf("%w: encode %s: %v", ErrFetchFailed, df.Path, err)
		}
		if written+int64(one.Len()) > lim.MaxBytes {
			return res, fmt.Errorf("%w: the patch is over %d bytes", ErrTooLarge, lim.MaxBytes)
		}
		if _, err := patch.Write(one.Bytes()); err != nil {
			return res, fmt.Errorf("%w: write %s: %v", ErrFetchFailed, patchRel, err)
		}
		written += int64(one.Len())
		files = append(files, df)
	}
	if err := patch.Close(); err != nil {
		return res, fmt.Errorf("%w: write %s: %v", ErrFetchFailed, patchRel, err)
	}

	summary, err := json.MarshalIndent(map[string]any{
		"base":  f.Base,
		"head":  f.Head,
		"files": files,
	}, "", "  ")
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	out, err := createNew(root, numstatRel)
	if err != nil {
		return res, fmt.Errorf("%w: write %s: %v", ErrFetchFailed, numstatRel, err)
	}
	if _, err := out.Write(append(summary, '\n')); err != nil {
		_ = out.Close()
		return res, fmt.Errorf("%w: write %s: %v", ErrFetchFailed, numstatRel, err)
	}
	if err := out.Close(); err != nil {
		return res, fmt.Errorf("%w: write %s: %v", ErrFetchFailed, numstatRel, err)
	}
	res.Files = len(files)
	res.Bytes = written
	return res, nil
}

// skipReason gives the reason a change has no body, or "" when it has one.
// It applies WriteTree's rules to both sides of the change.
func (f *Fetched) skipReason(c *object.Change, p string, reserved map[string]bool, maxFileBytes int64) string {
	if !safeRelPath(p) || reserved[strings.ToLower(strings.SplitN(p, "/", 2)[0])] {
		return ReasonUnsafePath
	}
	for _, e := range []object.ChangeEntry{c.From, c.To} {
		if e.Name == "" {
			continue // the side that does not exist
		}
		switch e.TreeEntry.Mode {
		case filemode.Submodule:
			return ReasonSubmodule
		case filemode.Symlink:
			return ReasonSymlink
		}
		blob, err := f.repo.BlobObject(e.TreeEntry.Hash)
		if err != nil || blob.Size > maxFileBytes {
			return ReasonTooLarge
		}
	}
	return ""
}

func changePath(c *object.Change) string {
	if c.To.Name != "" {
		return c.To.Name
	}
	return c.From.Name
}

func statusOf(a merkletrie.Action) string {
	switch a {
	case merkletrie.Insert:
		return "added"
	case merkletrie.Delete:
		return "deleted"
	}
	return "modified"
}

// createNew creates a new file under root, making its directory. It refuses
// to replace one that exists.
func createNew(root *os.Root, rel string) (*os.File, error) {
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			return nil, err
		}
	}
	return root.OpenFile(filepath.FromSlash(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}
