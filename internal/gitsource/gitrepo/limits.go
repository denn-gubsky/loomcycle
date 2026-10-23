package gitrepo

import (
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// Limits bound how far a commit's tree may expand when it is brought into
// the index and the worktree. A tree can name one subtree many times, so a
// handful of objects can stand for millions of files; the limits are checked
// against the expanded tree before HEAD, the index or a file moves.
type Limits struct {
	// MaxFiles bounds the entries of an expanded tree (files, links,
	// submodules and directories), and the paths any one listing (a diff
	// side, status) holds. Zero or less is DefaultMaxFiles.
	MaxFiles int
	// MaxBytes bounds the sum of the sizes of the objects an expanded tree
	// reads: every blob once per path that names it, and the trees. Zero or
	// less is DefaultMaxBytes.
	MaxBytes int64
}

// The limits a Repo has when none are set.
const (
	DefaultMaxFiles       = 100000
	DefaultMaxBytes int64 = 1 << 30
)

// SetLimits sets the repository's expansion limits; a zero field is its
// default.
func (r *Repo) SetLimits(l Limits) { r.limits = l }

func (r *Repo) maxFiles() int {
	if r.limits.MaxFiles > 0 {
		return r.limits.MaxFiles
	}
	return DefaultMaxFiles
}

func (r *Repo) maxBytes() int64 {
	if r.limits.MaxBytes > 0 {
		return r.limits.MaxBytes
	}
	return DefaultMaxBytes
}

// tooManyFiles is the refusal of a listing over the file limit.
func (r *Repo) tooManyFiles(what string) error {
	return fmt.Errorf("%w: %s has over %d paths", gitsource.ErrTooLarge, what, r.maxFiles())
}

// checkTree walks c's tree as a checkout would expand it, before anything
// is moved to it: every path is checked with treePath (a hostile tree that
// holds a .git path is refused here, before HEAD moves), and the walk stops
// as soon as the entries pass the file limit or the objects' sizes, read
// from their headers and counted once per path, pass the byte limit.
func (r *Repo) checkTree(c *object.Commit) error {
	maxFiles, maxBytes := r.maxFiles(), r.maxBytes()
	type dir struct {
		hash   plumbing.Hash
		prefix string
	}
	stack := []dir{{c.TreeHash, ""}}
	entries := 0
	var size int64
	add := func(h plumbing.Hash) error {
		n, err := r.repo.Storer.EncodedObjectSize(h)
		if err != nil {
			return fmt.Errorf("read the size of %s: %w", h, err)
		}
		if size += n; size > maxBytes {
			return fmt.Errorf("%w: the tree of %s holds over %d bytes", gitsource.ErrTooLarge, c.Hash, maxBytes)
		}
		return nil
	}
	for len(stack) > 0 {
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		// A tree is decoded whole, so its own size is counted before it is.
		if err := add(d.hash); err != nil {
			return err
		}
		tree, err := r.repo.TreeObject(d.hash)
		if err != nil {
			return fmt.Errorf("read the tree of %s: %w", c.Hash, err)
		}
		for _, e := range tree.Entries {
			if entries++; entries > maxFiles {
				return fmt.Errorf("%w: the tree of %s has over %d entries", gitsource.ErrTooLarge, c.Hash, maxFiles)
			}
			p := e.Name
			if d.prefix != "" {
				p = d.prefix + "/" + e.Name
			}
			if len(p) > maxArgLen {
				return fmt.Errorf("%w: the repository holds a path over %d bytes", ErrInvalidArgument, maxArgLen)
			}
			if err := treePath(p); err != nil {
				return err
			}
			switch e.Mode {
			case filemode.Dir:
				stack = append(stack, dir{e.Hash, p})
			case filemode.Submodule:
				// A commit of another repository: nothing here to read.
			default:
				if err := add(e.Hash); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// headState is HEAD as an operation found it: HEAD's own form, and the
// branch it names with that branch's value (nil when unborn or detached).
type headState struct {
	head   *plumbing.Reference
	branch *plumbing.Reference
	index  *index.Index
}

// saveHead records HEAD, its branch and the index before an operation that
// moves them. go-git's reset moves HEAD's branch before it touches the
// index or a file, so a failure after that would leave the branch at the
// target with the index and the files part-way there.
func (r *Repo) saveHead() (headState, error) {
	head, err := r.repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return headState{}, fmt.Errorf("read HEAD: %w", err)
	}
	s := headState{head: head}
	if head.Type() == plumbing.SymbolicReference {
		b, err := r.repo.Storer.Reference(head.Target())
		switch {
		case err == nil:
			s.branch = b
		case !errors.Is(err, plumbing.ErrReferenceNotFound):
			return headState{}, fmt.Errorf("read HEAD: %w", err)
		}
	}
	idx, err := r.repo.Storer.Index()
	if err != nil {
		return headState{}, fmt.Errorf("read the index: %w", err)
	}
	// The storer may hand out the index it holds, which a reset then edits
	// in place: keep a copy of the entries.
	cp := *idx
	cp.Entries = make([]*index.Entry, len(idx.Entries))
	for i, e := range idx.Entries {
		ec := *e
		cp.Entries[i] = &ec
	}
	s.index = &cp
	return s, nil
}

// restoreHead puts back what saveHead recorded: the branch to its old value
// (or removed, when it was unborn), HEAD to its old form, and the index.
// Files already written stay; with the index restored, status shows them.
func (r *Repo) restoreHead(s headState) error {
	var errs []error
	if s.head.Type() == plumbing.SymbolicReference {
		if s.branch != nil {
			errs = append(errs, r.repo.Storer.SetReference(s.branch))
		} else {
			err := r.repo.Storer.RemoveReference(s.head.Target())
			if !errors.Is(err, plumbing.ErrReferenceNotFound) {
				errs = append(errs, err)
			}
		}
	}
	errs = append(errs, r.repo.Storer.SetReference(s.head), r.repo.Storer.SetIndex(s.index))
	return errors.Join(errs...)
}

// keepHead runs fn, and when it fails puts HEAD, its branch and the index
// back as they were. The error is fn's, with any failure to restore added.
func (r *Repo) keepHead(fn func() error) error {
	saved, err := r.saveHead()
	if err != nil {
		return err
	}
	err = fn()
	if err == nil {
		return nil
	}
	if rerr := r.restoreHead(saved); rerr != nil {
		return fmt.Errorf("%w (restoring HEAD failed: %v)", err, rerr)
	}
	return err
}
