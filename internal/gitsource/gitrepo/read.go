package gitrepo

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// HeadInfo is where HEAD points.
type HeadInfo struct {
	// Branch is the short name, "" when detached or unborn.
	Branch string `json:"branch"`
	// Hash is "" when the branch is unborn.
	Hash     string `json:"hash"`
	Detached bool   `json:"detached"`
}

// FileStatus is one path's state, in git's one-letter codes: " ", "M", "A",
// "D", "R", "C", "U", "?".
type FileStatus struct {
	Path     string `json:"path"`
	Staging  string `json:"staging"`
	Worktree string `json:"worktree"`
}

// LogOptions select commits for Log.
type LogOptions struct {
	Rev   string // "" = HEAD
	Path  string
	Max   int // required, > 0
	Since *time.Time
	Until *time.Time
	All   bool
}

// CommitInfo is one commit.
type CommitInfo struct {
	Hash           string    `json:"hash"`
	Author         string    `json:"author"`
	AuthorEmail    string    `json:"author_email"`
	AuthorDate     time.Time `json:"author_date"`
	Committer      string    `json:"committer"`
	CommitterEmail string    `json:"committer_email"`
	CommitDate     time.Time `json:"commit_date"`
	Message        string    `json:"message"`
	Parents        []string  `json:"parents"`
}

// BranchInfo is one local branch.
type BranchInfo struct {
	Name    string `json:"name"`
	Hash    string `json:"hash"`
	Current bool   `json:"current"`
	// Upstream is "remote/branch" from the repository's config.
	Upstream string `json:"upstream,omitempty"`
}

// TagInfo is one tag.
type TagInfo struct {
	Name string `json:"name"`
	// Hash is what the tag points at, peeled through annotated tags.
	Hash      string `json:"hash"`
	Annotated bool   `json:"annotated"`
	Message   string `json:"message,omitempty"`
}

// RemoteInfo is one configured remote.
type RemoteInfo struct {
	Name string `json:"name"`
	// URL is the fetch URL as the library would dial it: after the
	// url.<base>.insteadOf rules of .git/config. The repository's config is
	// the agent's to write, so a caller that dials it validates it first.
	URL string `json:"url"`
}

// Head reports where HEAD points.
func (r *Repo) Head() (HeadInfo, error) {
	ref, err := r.repo.Reference(plumbing.HEAD, false)
	if err != nil {
		return HeadInfo{}, fmt.Errorf("read HEAD: %w", err)
	}
	if ref.Type() == plumbing.HashReference {
		return HeadInfo{Hash: ref.Hash().String(), Detached: true}, nil
	}
	target, err := r.repo.Reference(ref.Target(), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return HeadInfo{}, nil // unborn
	}
	if err != nil {
		return HeadInfo{}, fmt.Errorf("read HEAD: %w", err)
	}
	info := HeadInfo{Hash: target.Hash().String()}
	if ref.Target().IsBranch() {
		info.Branch = ref.Target().Short()
	}
	return info, nil
}

// headCommit is HEAD's commit, nil when the branch is unborn.
func (r *Repo) headCommit() (*object.Commit, error) {
	ref, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read HEAD: %w", err)
	}
	c, err := r.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("read HEAD's commit: %w", err)
	}
	return c, nil
}

// currentBranch is the branch HEAD names ("" when detached); it may be
// unborn.
func (r *Repo) currentBranch() (plumbing.ReferenceName, error) {
	ref, err := r.repo.Reference(plumbing.HEAD, false)
	if err != nil {
		return "", fmt.Errorf("read HEAD: %w", err)
	}
	if ref.Type() == plumbing.SymbolicReference && ref.Target().IsBranch() {
		return ref.Target(), nil
	}
	return "", nil
}

// Status lists the paths that differ from HEAD or from the index, sorted.
func (r *Repo) Status() ([]FileStatus, error) {
	s, err := r.status()
	if err != nil {
		return nil, err
	}
	out := make([]FileStatus, 0, len(s))
	for p, fs := range s {
		if fs.Staging == git.Unmodified && fs.Worktree == git.Unmodified {
			continue
		}
		out = append(out, FileStatus{Path: p, Staging: string(rune(fs.Staging)), Worktree: string(rune(fs.Worktree))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (r *Repo) status() (git.Status, error) {
	w, err := r.repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	s, err := w.Status()
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	// The library builds the whole map; what is built on it is bounded.
	if len(s) > r.maxFiles() {
		return nil, r.tooManyFiles("the status")
	}
	return s, nil
}

// resolve resolves a revision to the hash it names, as the library does.
// A malformed expression is an invalid argument; one that names nothing is
// an invalid revision.
func (r *Repo) resolve(rev string) (plumbing.Hash, error) {
	if err := validRev(rev); err != nil {
		return plumbing.ZeroHash, err
	}
	h, err := r.repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		// The parser's error type is internal to the library, so a
		// malformed expression is told apart by the text it always has.
		if strings.HasPrefix(err.Error(), "Revision invalid") {
			return plumbing.ZeroHash, fmt.Errorf("%w: revision %q: %v", ErrInvalidArgument, truncate(rev, 200), err)
		}
		return plumbing.ZeroHash, fmt.Errorf("%w: %q: %v", ErrInvalidRevision, truncate(rev, 200), err)
	}
	return *h, nil
}

// maxTagChain bounds how many tags-of-tags are followed to a commit.
const maxTagChain = 16

// resolveCommit resolves a revision to a commit, through annotated tags.
func (r *Repo) resolveCommit(rev string) (*object.Commit, error) {
	h, err := r.resolve(rev)
	if err != nil {
		return nil, err
	}
	for i := 0; i < maxTagChain; i++ {
		obj, err := r.repo.Object(plumbing.AnyObject, h)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrInvalidRevision, truncate(rev, 200), err)
		}
		switch o := obj.(type) {
		case *object.Commit:
			return o, nil
		case *object.Tag:
			h = o.Target
			continue
		}
		break
	}
	return nil, fmt.Errorf("%w: %q does not name a commit", ErrInvalidRevision, truncate(rev, 200))
}

func commitInfo(c *object.Commit) CommitInfo {
	parents := make([]string, 0, len(c.ParentHashes))
	for _, p := range c.ParentHashes {
		parents = append(parents, p.String())
	}
	return CommitInfo{
		Hash:           c.Hash.String(),
		Author:         c.Author.Name,
		AuthorEmail:    c.Author.Email,
		AuthorDate:     c.Author.When,
		Committer:      c.Committer.Name,
		CommitterEmail: c.Committer.Email,
		CommitDate:     c.Committer.When,
		Message:        c.Message,
		Parents:        parents,
	}
}

// Log lists commits newest first, at most o.Max of them.
func (r *Repo) Log(o LogOptions) ([]CommitInfo, error) {
	if o.Max <= 0 {
		return nil, fmt.Errorf("%w: max must be > 0", ErrInvalidArgument)
	}
	opts := &git.LogOptions{All: o.All, Since: o.Since, Until: o.Until, Order: git.LogOrderCommitterTime}
	if o.Path != "" {
		p, err := cleanPath(o.Path)
		if err != nil {
			return nil, err
		}
		spec := []string{p}
		opts.PathFilter = func(name string) bool { return matchPaths(spec, name) }
	}
	if !o.All {
		if o.Rev == "" {
			head, err := r.headCommit()
			if err != nil {
				return nil, err
			}
			if head == nil {
				return []CommitInfo{}, nil // an unborn branch has no history
			}
			opts.From = head.Hash
		} else {
			c, err := r.resolveCommit(o.Rev)
			if err != nil {
				return nil, err
			}
			opts.From = c.Hash
		}
	}
	it, err := r.repo.Log(opts)
	if err != nil {
		return nil, fmt.Errorf("log: %w", err)
	}
	defer it.Close()
	out := []CommitInfo{}
	err = it.ForEach(func(c *object.Commit) error {
		out = append(out, commitInfo(c))
		if len(out) >= o.Max {
			return storer.ErrStop
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("log: %w", err)
	}
	return out, nil
}

// Show returns a commit and its patch against its first parent (a root
// commit's against the empty tree), cut at maxPatchBytes.
func (r *Repo) Show(rev string, maxPatchBytes int) (CommitInfo, string, bool, error) {
	if maxPatchBytes <= 0 {
		return CommitInfo{}, "", false, fmt.Errorf("%w: max patch bytes must be > 0", ErrInvalidArgument)
	}
	if rev == "" {
		rev = "HEAD"
	}
	c, err := r.resolveCommit(rev)
	if err != nil {
		return CommitInfo{}, "", false, err
	}
	to, err := r.commitSide(c)
	if err != nil {
		return CommitInfo{}, "", false, err
	}
	from := side{}
	if len(c.ParentHashes) > 0 {
		p, err := r.repo.CommitObject(c.ParentHashes[0])
		if err != nil {
			return CommitInfo{}, "", false, fmt.Errorf("read the parent: %w", err)
		}
		if from, err = r.commitSide(p); err != nil {
			return CommitInfo{}, "", false, err
		}
	}
	patch, truncated, err := r.diffSides(from, to, nil, maxPatchBytes)
	if err != nil {
		return CommitInfo{}, "", false, err
	}
	return commitInfo(c), patch, truncated, nil
}

// Branches lists the local branches, sorted by name.
func (r *Repo) Branches() ([]BranchInfo, error) {
	current, err := r.currentBranch()
	if err != nil {
		return nil, err
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	it, err := r.repo.Branches()
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	out := []BranchInfo{}
	err = it.ForEach(func(ref *plumbing.Reference) error {
		b := BranchInfo{Name: ref.Name().Short(), Hash: ref.Hash().String(), Current: ref.Name() == current}
		if bc, ok := cfg.Branches[b.Name]; ok && bc.Remote != "" && bc.Merge != "" {
			b.Upstream = bc.Remote + "/" + bc.Merge.Short()
		}
		out = append(out, b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Tags lists the tags, sorted by name.
func (r *Repo) Tags() ([]TagInfo, error) {
	it, err := r.repo.Tags()
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	out := []TagInfo{}
	err = it.ForEach(func(ref *plumbing.Reference) error {
		t := TagInfo{Name: ref.Name().Short(), Hash: ref.Hash().String()}
		h := ref.Hash()
		for i := 0; i < maxTagChain; i++ {
			tag, err := r.repo.TagObject(h)
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				break // a lightweight tag, or the end of a chain
			}
			if err != nil {
				return err
			}
			if !t.Annotated {
				t.Annotated, t.Message = true, tag.Message
			}
			h = tag.Target
		}
		t.Hash = h.String()
		out = append(out, t)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Remotes lists the configured remotes, sorted by name.
func (r *Repo) Remotes() ([]RemoteInfo, error) {
	cfg, err := r.repo.Config()
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	out := make([]RemoteInfo, 0, len(cfg.Remotes))
	for name, rc := range cfg.Remotes {
		ri := RemoteInfo{Name: name}
		if len(rc.URLs) > 0 {
			ri.URL = rc.URLs[0]
		}
		out = append(out, ri)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemoteURL is one remote's effective fetch URL (see RemoteInfo.URL).
func (r *Repo) RemoteURL(name string) (string, error) {
	if err := validRemoteName(name); err != nil {
		return "", err
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}
	rc, ok := cfg.Remotes[name]
	if !ok || len(rc.URLs) == 0 || rc.URLs[0] == "" {
		return "", fmt.Errorf("%w: no remote %q with a URL", ErrInvalidArgument, name)
	}
	return rc.URLs[0], nil
}

// MergeBase returns the best common ancestors of two revisions.
func (r *Repo) MergeBase(a, b string) ([]string, error) {
	ca, err := r.resolveCommit(a)
	if err != nil {
		return nil, err
	}
	cb, err := r.resolveCommit(b)
	if err != nil {
		return nil, err
	}
	bases, err := ca.MergeBase(cb)
	if err != nil {
		return nil, fmt.Errorf("merge base: %w", err)
	}
	out := make([]string, 0, len(bases))
	for _, c := range bases {
		out = append(out, c.Hash.String())
	}
	sort.Strings(out)
	return out, nil
}

// RevParse resolves a revision to the full hash it names.
func (r *Repo) RevParse(rev string) (string, error) {
	h, err := r.resolve(rev)
	if err != nil {
		return "", err
	}
	return h.String(), nil
}
