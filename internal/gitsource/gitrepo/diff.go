package gitrepo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	godiff "github.com/go-git/go-git/v5/utils/diff"
)

// DiffOptions choose the two sides of Diff.
//
//   - From and To empty: the index against the worktree (unstaged changes).
//   - Staged, From and To empty: HEAD against the index.
//   - From set, To empty: From against the worktree (against the index with
//     Staged).
//   - From and To set: one commit against the other.
type DiffOptions struct {
	From, To string
	Staged   bool
	// Paths limits the diff to these paths or directories.
	Paths []string
	// MaxBytes is required, > 0: the patch is cut there.
	MaxBytes int
}

// maxDiffFileBytes is the largest file whose content is diffed. A larger one
// is listed with a note and no body, so one huge file costs no more memory
// than this.
const maxDiffFileBytes = 8 << 20

// maxDiffLines is the most lines either side of a change may have, once the
// lines both sides share at the start and end are set aside, to get a body.
// The diff library's time limit does not bound its time (its search for a
// common run of lines never looks at the clock and grows with the cube of
// the lines; gitsource/diff.go has the measurements), so the size is bounded
// before it is called.
const maxDiffLines = 5000

// maxFileDiffTime bounds one file's diff; past it the library returns a
// coarser, still correct, patch.
const maxFileDiffTime = 10 * time.Second

// binarySniffBytes is how much of a file is looked at for a NUL byte, as git
// does to call a file binary.
const binarySniffBytes = 8000

// sideEntry is one path on one side of a diff.
type sideEntry struct {
	hash plumbing.Hash
	mode filemode.FileMode
	// data is the content when it is already in hand (a worktree file or a
	// submodule's line); nil means the blob is read when needed.
	data []byte
	// tooLarge marks a worktree file over maxDiffFileBytes, hashed but not
	// kept.
	tooLarge bool
}

// mergedStage is an index entry's stage when it is not in conflict. The
// library's index.Merged constant is 1, the same as AncestorMode, while the
// entries it decodes carry 0; comparing with it would skip every entry.
const mergedStage index.Stage = 0

// side is one side of a diff: every file path it has.
type side map[string]*sideEntry

// Diff returns a unified diff of the two sides o names, cut at o.MaxBytes.
// Renames are not detected: a rename is a deletion and an addition. The
// text is a valid patch but not byte for byte what `git diff` prints (hunk
// boundaries can differ, and index lines carry full hashes).
func (r *Repo) Diff(o DiffOptions) (string, bool, error) {
	if o.MaxBytes <= 0 {
		return "", false, fmt.Errorf("%w: max bytes must be > 0", ErrInvalidArgument)
	}
	if o.To != "" && (o.From == "" || o.Staged) {
		return "", false, fmt.Errorf("%w: to needs from, and does not go with staged", ErrInvalidArgument)
	}
	specs, err := pathspecs(o.Paths, true)
	if err != nil {
		return "", false, err
	}
	var from, to side
	switch {
	case o.From != "" && o.To != "":
		if from, err = r.revSide(o.From); err != nil {
			return "", false, err
		}
		if to, err = r.revSide(o.To); err != nil {
			return "", false, err
		}
	case o.From != "":
		if from, err = r.revSide(o.From); err != nil {
			return "", false, err
		}
		if to, err = r.indexSide(); err != nil {
			return "", false, err
		}
		if !o.Staged {
			if to, err = r.worktreeSide(to); err != nil {
				return "", false, err
			}
		}
	case o.Staged:
		head, err := r.headCommit()
		if err != nil {
			return "", false, err
		}
		from = side{}
		if head != nil {
			if from, err = r.commitSide(head); err != nil {
				return "", false, err
			}
		}
		if to, err = r.indexSide(); err != nil {
			return "", false, err
		}
	default:
		if from, err = r.indexSide(); err != nil {
			return "", false, err
		}
		if to, err = r.worktreeSide(from); err != nil {
			return "", false, err
		}
	}
	return r.diffSides(from, to, specs, o.MaxBytes)
}

func (r *Repo) revSide(rev string) (side, error) {
	c, err := r.resolveCommit(rev)
	if err != nil {
		return nil, err
	}
	return r.commitSide(c)
}

// commitSide is every file of a commit's tree, refused (too large) past
// the file limit rather than built: a tree can name one subtree many times.
func (r *Repo) commitSide(c *object.Commit) (side, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("read the tree of %s: %w", c.Hash, err)
	}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	s := side{}
	for {
		name, e, err := w.Next()
		if errors.Is(err, io.EOF) {
			return s, nil
		}
		if err != nil {
			return nil, fmt.Errorf("walk the tree of %s: %w", c.Hash, err)
		}
		if e.Mode == filemode.Dir {
			continue
		}
		if len(s) >= r.maxFiles() {
			return nil, r.tooManyFiles("the tree of " + c.Hash.String())
		}
		s[name] = &sideEntry{hash: e.Hash, mode: e.Mode}
	}
}

// indexSide is every merged entry of the index, refused (too large) past the
// file limit. A conflicted path's stages are left out: there is no one
// content to show for it.
func (r *Repo) indexSide() (side, error) {
	idx, err := r.repo.Storer.Index()
	if err != nil {
		return nil, fmt.Errorf("read the index: %w", err)
	}
	s := side{}
	for _, e := range idx.Entries {
		if e.Stage != mergedStage {
			continue
		}
		if len(s) >= r.maxFiles() {
			return nil, r.tooManyFiles("the index")
		}
		s[e.Name] = &sideEntry{hash: e.Hash, mode: e.Mode}
	}
	return s, nil
}

// worktreeSide is the tracked files as they are in the worktree: the paths
// of the index side, read from disk where status says they changed. An
// untracked file is not on it, as `git diff` does not show one.
func (r *Repo) worktreeSide(idx side) (side, error) {
	st, err := r.status()
	if err != nil {
		return nil, err
	}
	s := side{}
	for p, e := range idx {
		fs, ok := st[p]
		if !ok || fs.Worktree == git.Unmodified || e.mode == filemode.Submodule {
			s[p] = e
			continue
		}
		if fs.Worktree == git.Deleted {
			continue
		}
		we, err := r.readWorktreeFile(p)
		if err != nil {
			return nil, err
		}
		if we != nil {
			s[p] = we
		}
	}
	return s, nil
}

// readWorktreeFile reads one file as a diff side; nil when the path is now a
// directory or gone.
func (r *Repo) readWorktreeFile(p string) (*sideEntry, error) {
	fi, err := r.wt.Lstat(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", p, err)
	}
	switch {
	case fi.IsDir():
		return nil, nil
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := r.wt.Readlink(p)
		if err != nil {
			return nil, fmt.Errorf("read link %s: %w", p, err)
		}
		data := []byte(target)
		return &sideEntry{hash: plumbing.ComputeHash(plumbing.BlobObject, data), mode: filemode.Symlink, data: data}, nil
	}
	mode := filemode.Regular
	if fi.Mode().Perm()&0o111 != 0 {
		mode = filemode.Executable
	}
	f, err := r.wt.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	if fi.Size() > maxDiffFileBytes {
		h := plumbing.NewHasher(plumbing.BlobObject, fi.Size())
		if _, err := io.Copy(h, f); err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		return &sideEntry{hash: h.Sum(), mode: mode, tooLarge: true}, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, maxDiffFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	if len(data) > maxDiffFileBytes { // grew since the stat
		return &sideEntry{hash: plumbing.ComputeHash(plumbing.BlobObject, data), mode: mode, tooLarge: true}, nil
	}
	return &sideEntry{hash: plumbing.ComputeHash(plumbing.BlobObject, data), mode: mode, data: data}, nil
}

// content is an entry's bytes; ok false when it is too large to diff.
func (r *Repo) content(e *sideEntry) ([]byte, bool, error) {
	if e.tooLarge {
		return nil, false, nil
	}
	if e.data != nil {
		return e.data, true, nil
	}
	if e.mode == filemode.Submodule {
		return []byte("Subproject commit " + e.hash.String() + "\n"), true, nil
	}
	blob, err := r.repo.BlobObject(e.hash)
	if err != nil {
		return nil, false, fmt.Errorf("read blob %s: %w", e.hash, err)
	}
	if blob.Size > maxDiffFileBytes {
		return nil, false, nil
	}
	rd, err := blob.Reader()
	if err != nil {
		return nil, false, fmt.Errorf("read blob %s: %w", e.hash, err)
	}
	defer func() { _ = rd.Close() }()
	data, err := io.ReadAll(io.LimitReader(rd, maxDiffFileBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read blob %s: %w", e.hash, err)
	}
	return data, len(data) <= maxDiffFileBytes, nil
}

// kind groups modes the way git decides between one patch and a deletion
// plus an addition: a file that became a link is not "modified".
func kind(m filemode.FileMode) int {
	switch m {
	case filemode.Symlink:
		return 1
	case filemode.Submodule:
		return 2
	}
	return 0
}

// diffSides writes the patch from one side to the other, path by path, and
// stops once it passes maxBytes.
func (r *Repo) diffSides(from, to side, specs []string, maxBytes int) (string, bool, error) {
	names := make([]string, 0, len(from)+len(to))
	for p := range from {
		if matchPaths(specs, p) {
			names = append(names, p)
		}
	}
	for p := range to {
		if _, dup := from[p]; !dup && matchPaths(specs, p) {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	var out strings.Builder
	for _, p := range names {
		f, t := from[p], to[p]
		if f != nil && t != nil && f.hash == t.hash && f.mode == t.mode {
			continue
		}
		pairs := [][2]*sideEntry{{f, t}}
		if f != nil && t != nil && kind(f.mode) != kind(t.mode) {
			pairs = [][2]*sideEntry{{f, nil}, {nil, t}}
		}
		for _, pair := range pairs {
			text, err := r.encodeFile(p, pair[0], pair[1])
			if err != nil {
				return "", false, err
			}
			if out.Len()+len(text) > maxBytes {
				out.WriteString(strings.ToValidUTF8(text[:maxBytes-out.Len()], ""))
				return out.String(), true, nil
			}
			out.WriteString(text)
		}
	}
	return out.String(), false, nil
}

// encodeFile is one path's patch in git's unified format.
func (r *Repo) encodeFile(p string, from, to *sideEntry) (string, error) {
	fp := &filePatch{path: p, from: from, to: to}
	if from == nil || to == nil || from.hash != to.hash {
		var fromData, toData []byte
		fromOK, toOK := true, true
		var err error
		if from != nil {
			if fromData, fromOK, err = r.content(from); err != nil {
				return "", err
			}
		}
		if to != nil {
			if toData, toOK, err = r.content(to); err != nil {
				return "", err
			}
		}
		switch {
		case !fromOK || !toOK:
			return tooLargeNote(p, fmt.Sprintf("a side is over %d bytes", maxDiffFileBytes)), nil
		case isBinary(fromData) || isBinary(toData):
			fp.binary = true
		default:
			a, b := string(fromData), string(toData)
			if changedLines(a, b) > maxDiffLines {
				return tooLargeNote(p, fmt.Sprintf("the change has over %d lines", maxDiffLines)), nil
			}
			for _, d := range godiff.DoWithTimeout(a, b, maxFileDiffTime) {
				// The operation's values are diffmatchpatch's (-1 delete,
				// 1 insert, 0 equal), compared as numbers so this package
				// depends on go-git alone.
				op := fdiff.Equal
				switch d.Type {
				case -1:
					op = fdiff.Delete
				case 1:
					op = fdiff.Add
				}
				fp.chunks = append(fp.chunks, chunk{d.Text, op})
			}
		}
	}
	var buf bytes.Buffer
	if err := fdiff.NewUnifiedEncoder(&buf, fdiff.DefaultContextLines).Encode(onePatch{fp}); err != nil {
		return "", fmt.Errorf("encode %s: %w", p, err)
	}
	return buf.String(), nil
}

// tooLargeNote stands in for the body of a change too large to diff.
func tooLargeNote(p, why string) string {
	return fmt.Sprintf("diff --git a/%s b/%s\n# change not shown: %s\n", p, p, why)
}

func isBinary(b []byte) bool {
	if len(b) > binarySniffBytes {
		b = b[:binarySniffBytes]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// changedLines is the line count of the longer side of a change, once the
// whole lines both sides start and end with are set aside: the part the
// diff library actually searches. (The same measure as gitsource's.)
func changedLines(a, b string) int {
	for a != "" && b != "" {
		la, lb := lineEnd(a), lineEnd(b)
		if a[:la] != b[:lb] {
			break
		}
		a, b = a[la:], b[lb:]
	}
	for a != "" && b != "" {
		la := strings.LastIndexByte(a[:len(a)-1], '\n') + 1
		lb := strings.LastIndexByte(b[:len(b)-1], '\n') + 1
		if a[la:] != b[lb:] {
			break
		}
		a, b = a[:la], b[:lb]
	}
	return max(countLines(a), countLines(b))
}

func lineEnd(s string) int {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return i + 1
	}
	return len(s)
}

func countLines(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// The library's encoder takes a patch through these interfaces.
type (
	onePatch  struct{ fp *filePatch }
	filePatch struct {
		path     string
		from, to *sideEntry
		chunks   []fdiff.Chunk
		binary   bool
	}
	fileSide struct {
		path string
		e    *sideEntry
	}
	chunk struct {
		text string
		op   fdiff.Operation
	}
)

func (p onePatch) FilePatches() []fdiff.FilePatch { return []fdiff.FilePatch{p.fp} }
func (p onePatch) Message() string                { return "" }

func (fp *filePatch) IsBinary() bool        { return fp.binary }
func (fp *filePatch) Chunks() []fdiff.Chunk { return fp.chunks }
func (fp *filePatch) Files() (from, to fdiff.File) {
	if fp.from != nil {
		from = fileSide{fp.path, fp.from}
	}
	if fp.to != nil {
		to = fileSide{fp.path, fp.to}
	}
	return from, to
}

func (s fileSide) Hash() plumbing.Hash     { return s.e.hash }
func (s fileSide) Mode() filemode.FileMode { return s.e.mode }
func (s fileSide) Path() string            { return s.path }

func (c chunk) Content() string       { return c.text }
func (c chunk) Type() fdiff.Operation { return c.op }
