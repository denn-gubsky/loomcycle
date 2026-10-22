package gitrepo

import (
	"context"
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

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
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
	Path string `json:"path"` // cut to maxGrepPath bytes
	// PathTruncated reports Path was cut: it names no file as it stands.
	PathTruncated bool   `json:"path_truncated,omitempty"`
	Line          int    `json:"line"`
	Text          string `json:"text"` // cut to maxLineText bytes
}

// maxGrepPath is where a match's path is cut. A tree may hold paths up to
// maxArgLen bytes, and one is repeated on every matching line.
const maxGrepPath = 1024

// maxGrepPattern bounds a pattern. RE2 runs in time linear in the input,
// but compiling a long expression is not free and nothing legitimate is
// near this.
const maxGrepPattern = 1024

// Grep searches the files of a commit's tree, in path order, and returns at
// most o.MaxMatches lines; truncated reports there were more. Like `git
// grep`, it skips binary files (a NUL in the first 8000 bytes); a file over
// maxDiffFileBytes is skipped too. The library's grep is not used: it
// collects every match in memory before returning any.
//
// The walk is bounded as a checkout's is: every entry visited, directories
// too, counts against the file limit, and the trees and files read against
// the byte limit; past either the search is refused (too large). o.Paths
// keeps the walk out of the directories it cannot reach, so a narrow search
// of a large tree costs what its part of the tree does.
func (r *Repo) Grep(ctx context.Context, o GrepOptions) ([]GrepMatch, bool, error) {
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
	b := r.newTreeBudget("the tree of " + c.Hash.String())
	out := []GrepMatch{}
	truncated := false
	err = r.walkTree(ctx, c.TreeHash, b, func(dir string) bool { return mayHold(specs, dir) }, func(name string, e object.TreeEntry) (bool, error) {
		if (e.Mode != filemode.Regular && e.Mode != filemode.Executable) || !matchPaths(specs, name) {
			return false, nil
		}
		n, err := r.repo.Storer.EncodedObjectSize(e.Hash)
		if err != nil {
			return true, fmt.Errorf("read the size of %s: %w", e.Hash, err)
		}
		if n > maxDiffFileBytes {
			return false, nil // skipped, not read
		}
		if err := b.read(n); err != nil {
			return true, err
		}
		data, ok, err := r.content(&sideEntry{hash: e.Hash, mode: e.Mode})
		if err != nil {
			return true, err
		}
		if !ok || isBinary(data) {
			return false, nil
		}
		path, pathCut := cut(name, maxGrepPath), len(name) > maxGrepPath
		for i, line := range splitLines(string(data)) {
			if re.MatchString(line) == o.Invert {
				continue
			}
			if len(out) == o.MaxMatches {
				truncated = true
				return true, nil
			}
			out = append(out, GrepMatch{Path: path, PathTruncated: pathCut, Line: i + 1, Text: cut(line, maxLineText)})
		}
		return false, nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, truncated, nil
}

// mayHold reports whether directory dir holds or is a path specs match, so
// a walk must go into it: it is one of specs or under one, or above one.
func mayHold(specs []string, dir string) bool {
	if matchPaths(specs, dir) {
		return true
	}
	for _, s := range specs {
		if strings.HasPrefix(s, dir+"/") {
			return true
		}
	}
	return false
}

// NoteInfo is the note on one commit.
type NoteInfo struct {
	Commit string `json:"commit"`
	Note   string `json:"note"` // cut to maxNoteText bytes
}

// maxNoteText is where a note is cut.
const maxNoteText = 4096

// maxNotes bounds the notes one notes tree may hold for Notes: past it the
// listing is refused (too large) rather than built in memory.
const maxNotes = 10000

// notesRef is where `git notes` keeps notes by default.
const notesRef plumbing.ReferenceName = "refs/notes/commits"

// Notes lists the notes refs/notes/commits holds for the commits reachable
// from rev ("" is HEAD), newest commit first, at most max of them. No
// notes ref, or an unborn branch, is an empty list. The notes tree is walked
// within the repository's limits, and ctx is checked at every entry and
// every commit of the history: past its deadline the listing is a timeout.
func (r *Repo) Notes(ctx context.Context, rev string, max int) ([]NoteInfo, error) {
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
	notes, err := r.notesByCommit(ctx)
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
		if err := ctxDone(ctx); err != nil {
			return err
		}
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
// is not a note and is ignored. The walk is bounded as Grep's is, and a
// tree of more than maxNotes notes is refused (too large).
func (r *Repo) notesByCommit(ctx context.Context) (map[plumbing.Hash]plumbing.Hash, error) {
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
	out := map[plumbing.Hash]plumbing.Hash{}
	err = r.walkTree(ctx, c.TreeHash, r.newTreeBudget(notesRef.String()), nil, func(name string, e object.TreeEntry) (bool, error) {
		if !e.Mode.IsFile() {
			return false, nil
		}
		hex := strings.ReplaceAll(name, "/", "")
		if len(hex) != 40 || !plumbing.IsHash(hex) {
			return false, nil
		}
		if len(out) == maxNotes {
			return true, fmt.Errorf("%w: %s holds over %d notes", gitsource.ErrTooLarge, notesRef, maxNotes)
		}
		out[plumbing.NewHash(hex)] = e.Hash
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
