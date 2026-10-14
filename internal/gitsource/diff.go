package gitsource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	godiff "github.com/go-git/go-git/v5/utils/diff"
	"github.com/go-git/go-git/v5/utils/merkletrie"
	"github.com/sergi/go-diff/diffmatchpatch"
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
	// MaxFiles bounds how many changed paths are listed, skipped ones
	// included. Required. The base tree is not bounded by WriteTree's file
	// limit, so a base of a million files deleted would otherwise be a
	// million entries.
	MaxFiles int
	// Reserved are top-level names whose paths are left out, as WriteTree
	// leaves them out.
	Reserved []string
}

// maxFileDiffTime is how long one file's diff may take when the context sets
// no deadline. The diff library would otherwise allow it an hour.
const maxFileDiffTime = 30 * time.Second

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
	if lim.MaxBytes <= 0 || lim.MaxFileBytes <= 0 || lim.MaxFiles <= 0 {
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
	if len(changes) > lim.MaxFiles {
		return res, fmt.Errorf("%w: %d changed paths, more than %d", ErrTooLarge, len(changes), lim.MaxFiles)
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
		fp, err := f.filePatch(ctx, c)
		if err != nil {
			return res, fmt.Errorf("%w: diff %s: %v", ErrFetchFailed, df.Path, err)
		}
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		df.Binary = fp.IsBinary()
		for _, ch := range fp.chunks {
			n := strings.Count(ch.Content(), "\n")
			if !strings.HasSuffix(ch.Content(), "\n") && ch.Content() != "" {
				n++ // a last line with no newline
			}
			switch ch.Type() {
			case fdiff.Add:
				df.Added += n
			case fdiff.Delete:
				df.Deleted += n
			}
		}
		var one bytes.Buffer
		if err := fdiff.NewUnifiedEncoder(&one, fdiff.DefaultContextLines).Encode(onePatch{fp}); err != nil {
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
	// The summary is written too, so it counts against the same limit.
	if written+int64(len(summary))+1 > lim.MaxBytes {
		return res, fmt.Errorf("%w: the patch and its summary are over %d bytes", ErrTooLarge, lim.MaxBytes)
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

// filePatch builds one file's patch. It is what the library's own Patch does,
// with one change: the diff gets the time left before the context's deadline,
// not the hour the library allows each file. A diff that runs out of time
// comes back coarser (more lines removed and added than needed), still a
// correct patch, and the deadline is then reported by the caller.
func (f *Fetched) filePatch(ctx context.Context, c *object.Change) (*filePatch, error) {
	from, to, err := c.Files()
	if err != nil {
		return nil, err
	}
	fromText, fromBinary, err := fileText(from)
	if err != nil {
		return nil, err
	}
	toText, toBinary, err := fileText(to)
	if err != nil {
		return nil, err
	}
	fp := &filePatch{from: c.From, to: c.To}
	if fromBinary || toBinary {
		fp.binary = true
		return fp, nil
	}
	budget := maxFileDiffTime
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
	}
	if budget <= 0 {
		return nil, ctx.Err()
	}
	for _, d := range godiff.DoWithTimeout(fromText, toText, budget) {
		op := fdiff.Equal
		switch d.Type {
		case diffmatchpatch.DiffDelete:
			op = fdiff.Delete
		case diffmatchpatch.DiffInsert:
			op = fdiff.Add
		}
		fp.chunks = append(fp.chunks, chunk{d.Text, op})
	}
	return fp, nil
}

func fileText(f *object.File) (string, bool, error) {
	if f == nil {
		return "", false, nil
	}
	binary, err := f.IsBinary()
	if err != nil || binary {
		return "", binary, err
	}
	text, err := f.Contents()
	return text, false, err
}

// The library's diff encoder takes a patch through these interfaces.
type (
	onePatch  struct{ fp *filePatch }
	filePatch struct {
		from, to object.ChangeEntry
		chunks   []fdiff.Chunk
		binary   bool
	}
	fileSide struct{ e object.ChangeEntry }
	chunk    struct {
		text string
		op   fdiff.Operation
	}
)

func (p onePatch) FilePatches() []fdiff.FilePatch { return []fdiff.FilePatch{p.fp} }
func (p onePatch) Message() string                { return "" }

func (fp *filePatch) IsBinary() bool        { return fp.binary }
func (fp *filePatch) Chunks() []fdiff.Chunk { return fp.chunks }
func (fp *filePatch) Files() (from, to fdiff.File) {
	if fp.from.Name != "" {
		from = fileSide{fp.from}
	}
	if fp.to.Name != "" {
		to = fileSide{fp.to}
	}
	return from, to
}

func (s fileSide) Hash() plumbing.Hash {
	if !s.e.TreeEntry.Mode.IsFile() {
		return plumbing.ZeroHash
	}
	return s.e.TreeEntry.Hash
}
func (s fileSide) Mode() filemode.FileMode { return s.e.TreeEntry.Mode }
func (s fileSide) Path() string            { return s.e.Name }

func (c chunk) Content() string       { return c.text }
func (c chunk) Type() fdiff.Operation { return c.op }
