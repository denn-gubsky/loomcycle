package gitrepo

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// GrepOptions are Grep's.
type GrepOptions struct {
	// Pattern is an RE2 expression, matched against each line; required,
	// at most maxGrepPattern bytes.
	Pattern string
	// Rev names the commit whose tree is searched; "" is HEAD. The
	// worktree is not searched: uncommitted changes are not seen.
	Rev string
	// Paths limits the search to these files or directories, as git's
	// pathspecs do: "src" is src and what is under it, not "src2".
	Paths      []string
	IgnoreCase bool
	// Invert lists the lines that do not match.
	Invert bool
	// MaxMatches is required, > 0.
	MaxMatches int
}

// GrepMatch is one matching line.
type GrepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"` // cut to maxLineText bytes
}

// maxGrepPattern bounds a pattern. RE2 runs in time linear in the input,
// but compiling a long expression is not free and nothing legitimate is
// near this.
const maxGrepPattern = 1024

// Grep searches the files of a commit's tree, in path order, and returns at
// most o.MaxMatches lines; truncated reports there were more. Like `git
// grep`, it skips binary files (a NUL in the first 8000 bytes); a file over
// maxDiffFileBytes is skipped too. The library's grep is not used: it
// collects every match in memory before returning any.
func (r *Repo) Grep(o GrepOptions) ([]GrepMatch, bool, error) {
	if o.MaxMatches <= 0 {
		return nil, false, fmt.Errorf("%w: max matches must be > 0", ErrInvalidArgument)
	}
	if o.Pattern == "" || len(o.Pattern) > maxGrepPattern {
		return nil, false, fmt.Errorf("%w: a pattern of 1 to %d bytes is required", ErrInvalidArgument, maxGrepPattern)
	}
	expr := o.Pattern
	if o.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, false, fmt.Errorf("%w: the pattern does not compile: %v", ErrInvalidArgument, err)
	}
	specs, err := pathspecs(o.Paths, true)
	if err != nil {
		return nil, false, err
	}
	var c *object.Commit
	if o.Rev == "" {
		if c, err = r.headCommit(); err != nil {
			return nil, false, err
		}
		if c == nil {
			return []GrepMatch{}, false, nil // an unborn branch has no tree
		}
	} else if c, err = r.resolveCommit(o.Rev); err != nil {
		return nil, false, err
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, false, fmt.Errorf("read the tree of %s: %w", c.Hash, err)
	}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	out := []GrepMatch{}
	for {
		name, e, err := w.Next()
		if errors.Is(err, io.EOF) {
			return out, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("walk the tree of %s: %w", c.Hash, err)
		}
		if (e.Mode != filemode.Regular && e.Mode != filemode.Executable) || !matchPaths(specs, name) {
			continue
		}
		data, ok, err := r.content(&sideEntry{hash: e.Hash, mode: e.Mode})
		if err != nil {
			return nil, false, err
		}
		if !ok || isBinary(data) {
			continue
		}
		for i, line := range splitLines(string(data)) {
			if re.MatchString(line) == o.Invert {
				continue
			}
			if len(out) == o.MaxMatches {
				return out, true, nil
			}
			out = append(out, GrepMatch{Path: name, Line: i + 1, Text: cut(line, maxLineText)})
		}
	}
}

// NoteInfo is the note on one commit.
type NoteInfo struct {
	Commit string `json:"commit"`
	Note   string `json:"note"` // cut to maxNoteText bytes
}

// maxNoteText is where a note is cut.
const maxNoteText = 4096

// notesRef is where `git notes` keeps notes by default.
const notesRef plumbing.ReferenceName = "refs/notes/commits"

// Notes lists the notes refs/notes/commits holds for the commits reachable
// from rev ("" is HEAD), newest commit first, at most max of them. No
// notes ref, or an unborn branch, is an empty list.
func (r *Repo) Notes(rev string, max int) ([]NoteInfo, error) {
	if max <= 0 {
		return nil, fmt.Errorf("%w: max must be > 0", ErrInvalidArgument)
	}
	var start *object.Commit
	var err error
	if rev == "" {
		if start, err = r.headCommit(); err != nil {
			return nil, err
		}
		if start == nil {
			return []NoteInfo{}, nil
		}
	} else if start, err = r.resolveCommit(rev); err != nil {
		return nil, err
	}
	notes, err := r.notesByCommit()
	if err != nil {
		return nil, err
	}
	out := []NoteInfo{}
	if len(notes) == 0 {
		return out, nil
	}
	it, err := r.repo.Log(&git.LogOptions{From: start.Hash, Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, fmt.Errorf("log: %w", err)
	}
	defer it.Close()
	found := 0
	err = it.ForEach(func(c *object.Commit) error {
		blob, ok := notes[c.Hash]
		if !ok {
			return nil
		}
		text, err := r.noteText(blob)
		if err != nil {
			return err
		}
		out = append(out, NoteInfo{Commit: c.Hash.String(), Note: text})
		found++
		// Every note seen, or enough: the rest of the history has nothing
		// to add.
		if len(out) >= max || found == len(notes) {
			return storer.ErrStop
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("notes: %w", err)
	}
	return out, nil
}

// notesByCommit maps each annotated commit to its note's blob. A note's
// path in the notes tree is the commit's hash, split by "/" into fan-out
// directories once the tree grows ("ab/cdef..."); anything else in the tree
// is not a note and is ignored.
func (r *Repo) notesByCommit() (map[plumbing.Hash]plumbing.Hash, error) {
	ref, err := r.repo.Reference(notesRef, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", notesRef, err)
	}
	c, err := r.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", notesRef, err)
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", notesRef, err)
	}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	out := map[plumbing.Hash]plumbing.Hash{}
	for {
		name, e, err := w.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", notesRef, err)
		}
		if !e.Mode.IsFile() {
			continue
		}
		hex := strings.ReplaceAll(name, "/", "")
		if len(hex) != 40 || !plumbing.IsHash(hex) {
			continue
		}
		out[plumbing.NewHash(hex)] = e.Hash
	}
}

// noteText is a note's text, cut to maxNoteText bytes.
func (r *Repo) noteText(h plumbing.Hash) (string, error) {
	blob, err := r.repo.BlobObject(h)
	if err != nil {
		return "", fmt.Errorf("read note %s: %w", h, err)
	}
	rd, err := blob.Reader()
	if err != nil {
		return "", fmt.Errorf("read note %s: %w", h, err)
	}
	defer func() { _ = rd.Close() }()
	data, err := io.ReadAll(io.LimitReader(rd, maxNoteText))
	if err != nil {
		return "", fmt.Errorf("read note %s: %w", h, err)
	}
	return strings.ToValidUTF8(string(data), ""), nil
}
