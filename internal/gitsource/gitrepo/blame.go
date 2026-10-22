package gitrepo

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	godiff "github.com/go-git/go-git/v5/utils/diff"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// BlameResult is Blame's: the file's first lines, each with the commit that
// last changed it.
type BlameResult struct {
	Lines     []BlameLine `json:"lines"`
	Truncated bool        `json:"truncated"`
}

// BlameLine is one line of a blamed file.
type BlameLine struct {
	Line        int       `json:"line"` // 1-based
	Hash        string    `json:"hash"`
	Author      string    `json:"author"`
	AuthorEmail string    `json:"author_email"`
	Date        time.Time `json:"date"`
	Text        string    `json:"text"`
}

// maxBlameCommits bounds how many commits one blame visits. A commit whose
// version of the file is its parent's costs a tree lookup; one that changed
// it, a comparison counted against maxBlameBytes.
const maxBlameCommits = 20_000

// maxBlameBytes bounds the bytes one blame compares: both versions of the
// file, at every commit that changed it. Each version may be up to
// maxDiffFileBytes, so the commit bound alone allows hundreds of gigabytes;
// the repository's own byte limit applies when it is lower.
const maxBlameBytes int64 = 512 << 20

// maxLineText is where a line's text is cut in a result.
const maxLineText = 500

// Blame attributes each of the first maxLines lines of path at rev ("" is
// HEAD) to the commit that last changed it.
//
// The library's own blame is not used: it diffs every version of the file
// with no bound on size or time, and where a parent lacks the path it
// computes the whole commit's patch, every file of it, to look for a rename.
// This one follows the path only (a rename is where its lines start, as in
// Diff, which detects no renames either), diffs a version against its
// parent only when the change has at most maxDiffLines lines once the lines
// both sides share at the start and end are set aside (beyond that the
// blame is refused, not guessed) and that change holds a line still being
// blamed, visits at most maxBlameCommits commits and compares at most
// maxBlameBytes (or the repository's byte limit, when lower); past either
// bound the blame is refused (too large), and past ctx's deadline it is a
// timeout. A parent missing from a shallow history is a boundary: the lines
// that reach it are blamed on its child.
func (r *Repo) Blame(ctx context.Context, p, rev string, maxLines int) (BlameResult, error) {
	if maxLines <= 0 {
		return BlameResult{}, fmt.Errorf("%w: max lines must be > 0", ErrInvalidArgument)
	}
	p, err := cleanPath(p)
	if err != nil {
		return BlameResult{}, err
	}
	if rev == "" {
		rev = "HEAD"
	}
	start, err := r.resolveCommit(rev)
	if err != nil {
		return BlameResult{}, err
	}
	blob, ok, err := blameEntry(start, p)
	if err != nil {
		return BlameResult{}, err
	}
	if !ok {
		return BlameResult{}, fmt.Errorf("%w: no file %s at %s", ErrInvalidArgument, p, truncate(rev, 200))
	}
	text, binary, err := r.blameContent(blob)
	if err != nil {
		return BlameResult{}, err
	}
	if binary {
		return BlameResult{}, fmt.Errorf("%w: %s is a binary file", ErrInvalidArgument, p)
	}
	lines := splitLines(text)
	res := BlameResult{Lines: []BlameLine{}}
	n := len(lines)
	if n > maxLines {
		n, res.Truncated = maxLines, true
	}
	if n == 0 {
		return res, nil
	}
	owners, err := r.blameLines(ctx, start, blob, p, n)
	if err != nil {
		return BlameResult{}, err
	}
	for i := 0; i < n; i++ {
		c := owners[i]
		res.Lines = append(res.Lines, BlameLine{
			Line:        i + 1,
			Hash:        c.Hash.String(),
			Author:      c.Author.Name,
			AuthorEmail: c.Author.Email,
			Date:        c.Author.When,
			Text:        cut(lines[i], maxLineText),
		})
	}
	return res, nil
}

// blameEntry is the blob path names in c's tree; ok false when there is no
// file there (nothing, a directory or a submodule).
func blameEntry(c *object.Commit, p string) (plumbing.Hash, bool, error) {
	tree, err := c.Tree()
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("read the tree of %s: %w", c.Hash, err)
	}
	e, err := tree.FindEntry(p)
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return plumbing.ZeroHash, false, nil
	}
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("read the tree of %s: %w", c.Hash, err)
	}
	if !e.Mode.IsFile() {
		return plumbing.ZeroHash, false, nil
	}
	return e.Hash, true, nil
}

// blameContent reads one version of the blamed file. A version over
// maxDiffFileBytes refuses the blame.
func (r *Repo) blameContent(h plumbing.Hash) (string, bool, error) {
	blob, err := r.repo.BlobObject(h)
	if err != nil {
		return "", false, fmt.Errorf("read blob %s: %w", h, err)
	}
	if blob.Size > maxDiffFileBytes {
		return "", false, fmt.Errorf("%w: a version of the file is over %d bytes, too large to blame", ErrInvalidArgument, maxDiffFileBytes)
	}
	rd, err := blob.Reader()
	if err != nil {
		return "", false, fmt.Errorf("read blob %s: %w", h, err)
	}
	defer func() { _ = rd.Close() }()
	data, err := io.ReadAll(io.LimitReader(rd, maxDiffFileBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("read blob %s: %w", h, err)
	}
	if len(data) > maxDiffFileBytes {
		return "", false, fmt.Errorf("%w: a version of the file is over %d bytes, too large to blame", ErrInvalidArgument, maxDiffFileBytes)
	}
	return string(data), isBinary(data), nil
}

// splitLines splits text into lines without their "\n".
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// cut cuts s to at most n bytes, on a character boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// blameLine is a line still looking for its commit: its index in the final
// file and its index in the version being looked at.
type blameLine struct{ final, cur int }

// suspect is a commit some lines may have come from.
type suspect struct {
	commit *object.Commit
	blob   plumbing.Hash
	lines  []blameLine
}

// suspects is a queue of suspects, newest committer time first, so a commit
// is usually looked at after every child that passes it lines. Correctness
// does not rest on the order: lines that reach a commit already looked at
// make it a suspect again.
type suspects []*suspect

func (q suspects) Len() int { return len(q) }
func (q suspects) Less(i, j int) bool {
	return q[i].commit.Committer.When.After(q[j].commit.Committer.When)
}
func (q suspects) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *suspects) Push(x any)   { *q = append(*q, x.(*suspect)) }
func (q *suspects) Pop() any {
	old := *q
	s := old[len(old)-1]
	*q = old[:len(old)-1]
	return s
}

// blameLines finds the commit of each of the first n lines of blob, the
// version of p at start.
func (r *Repo) blameLines(ctx context.Context, start *object.Commit, blob plumbing.Hash, p string, n int) ([]*object.Commit, error) {
	budget := min(maxBlameBytes, r.maxBytes())
	var compared int64
	owners := make([]*object.Commit, n)
	all := make([]blameLine, n)
	for i := range all {
		all[i] = blameLine{final: i, cur: i}
	}
	q := &suspects{}
	pending := map[plumbing.Hash]*suspect{}
	push := func(c *object.Commit, blob plumbing.Hash, lines []blameLine) {
		if s, ok := pending[c.Hash]; ok {
			s.lines = append(s.lines, lines...)
			return
		}
		s := &suspect{commit: c, blob: blob, lines: lines}
		pending[c.Hash] = s
		heap.Push(q, s)
	}
	push(start, blob, all)
	for visited := 0; q.Len() > 0; visited++ {
		if visited >= maxBlameCommits {
			return nil, fmt.Errorf("%w: the file's history passes %d commits, too long to blame", gitsource.ErrTooLarge, maxBlameCommits)
		}
		if err := ctxDone(ctx); err != nil {
			return nil, err
		}
		s := heap.Pop(q).(*suspect)
		delete(pending, s.commit.Hash)
		left := s.lines
		var text string // this version's content, read once when needed
		haveText := false
		for _, ph := range s.commit.ParentHashes {
			if len(left) == 0 {
				break
			}
			parent, err := r.repo.CommitObject(ph)
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				continue // a shallow boundary
			}
			if err != nil {
				return nil, fmt.Errorf("read commit %s: %w", ph, err)
			}
			pblob, ok, err := blameEntry(parent, p)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if pblob == s.blob {
				push(parent, pblob, left)
				left = nil
				break
			}
			if !haveText {
				if text, _, err = r.blameContent(s.blob); err != nil {
					return nil, err
				}
				haveText = true
			}
			ptext, pbinary, err := r.blameContent(pblob)
			if err != nil {
				return nil, err
			}
			// Counted before the binary check: a binary version is read too.
			if compared += int64(len(ptext) + len(text)); compared > budget {
				return nil, fmt.Errorf("%w: the file's history compares over %d bytes, too large to blame", gitsource.ErrTooLarge, budget)
			}
			if pbinary {
				continue // no line of a binary version is a line of this one
			}
			passed, kept, err := passLines(ptext, text, left)
			if err != nil {
				return nil, err
			}
			if len(passed) > 0 {
				push(parent, pblob, passed)
			}
			left = kept
		}
		for _, l := range left {
			owners[l.final] = s.commit
		}
	}
	return owners, nil
}

// passLines splits lines (indexes into child) into those the child has
// unchanged from parent, renumbered as parent's lines, and the rest. The
// lines both versions start and end with pass as they are; only when a
// line in between is asked for is the middle diffed, and only then is a
// middle over maxDiffLines refused: a large change elsewhere in the file
// does not stop the blame of lines it did not touch.
func passLines(parent, child string, lines []blameLine) (passed, kept []blameLine, err error) {
	head, pmid, cmid := commonLines(parent, child)
	headLines := countLines(parent[:head])
	cEnd := headLines + countLines(cmid) // child's first line after the middle
	shift := countLines(pmid) - countLines(cmid)
	inMiddle := false
	for _, l := range lines {
		if l.cur >= headLines && l.cur < cEnd {
			inMiddle = true
			break
		}
	}
	var toParent []int // child middle line i -> parent line, -1 when added
	if inMiddle {
		if max(countLines(pmid), countLines(cmid)) > maxDiffLines {
			return nil, nil, fmt.Errorf("%w: a change in the file's history has over %d lines, too large to blame", ErrInvalidArgument, maxDiffLines)
		}
		toParent = make([]int, countLines(cmid))
		pl, cl := headLines, 0
		for _, d := range godiff.DoWithTimeout(pmid, cmid, maxFileDiffTime) {
			k := countLines(d.Text)
			// diffmatchpatch's operations, compared as numbers as diff.go
			// does: -1 delete, 1 insert, 0 equal.
			switch d.Type {
			case -1:
				pl += k
			case 1:
				for i := 0; i < k && cl < len(toParent); i++ {
					toParent[cl] = -1
					cl++
				}
			default:
				for i := 0; i < k && cl < len(toParent); i++ {
					toParent[cl] = pl
					cl++
					pl++
				}
			}
		}
	}
	for _, l := range lines {
		switch {
		case l.cur < headLines:
			passed = append(passed, l)
		case l.cur >= cEnd:
			passed = append(passed, blameLine{final: l.final, cur: l.cur + shift})
		case l.cur-headLines < len(toParent) && toParent[l.cur-headLines] >= 0:
			passed = append(passed, blameLine{final: l.final, cur: toParent[l.cur-headLines]})
		default:
			kept = append(kept, l)
		}
	}
	return passed, kept, nil
}

// commonLines sets aside the whole lines a and b start and end with: head
// is the byte length of the shared start (the same in both), and amid and
// bmid are what is left between it and the shared end.
func commonLines(a, b string) (head int, amid, bmid string) {
	for a != "" && b != "" {
		la, lb := lineEnd(a), lineEnd(b)
		if a[:la] != b[:lb] {
			break
		}
		a, b, head = a[la:], b[lb:], head+la
	}
	for a != "" && b != "" {
		la := strings.LastIndexByte(a[:len(a)-1], '\n') + 1
		lb := strings.LastIndexByte(b[:len(b)-1], '\n') + 1
		if a[la:] != b[lb:] {
			break
		}
		a, b = a[:la], b[:lb]
	}
	return head, a, b
}
