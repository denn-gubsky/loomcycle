package gitrepo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	billyutil "github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Signature is who made a commit, and when (zero is now).
type Signature struct {
	Name, Email string
	When        time.Time
}

// CommitOptions are Commit's.
type CommitOptions struct {
	Message           string // required, non-empty after trimming
	Author, Committer Signature
	All               bool // stage modified and deleted tracked files first
	AllowEmpty        bool
	Amend             bool
}

// CheckoutOptions are Checkout's. Exactly one of Branch, or Rev without
// Create, names the target.
type CheckoutOptions struct {
	Branch string
	// Rev is a detached checkout's revision, or with Create the new
	// branch's start point.
	Rev    string
	Create bool
	// Force discards local changes; Keep keeps index and worktree as they
	// are and moves HEAD only.
	Force bool
	Keep  bool
}

func (r *Repo) worktree() (*git.Worktree, error) {
	w, err := r.repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	return w, nil
}

// Add stages paths ("." or "" is everything not ignored). A deleted path is
// staged as a deletion.
func (r *Repo) Add(paths []string) error {
	specs, err := pathspecs(paths, true)
	if err != nil {
		return err
	}
	w, err := r.worktree()
	if err != nil {
		return err
	}
	if specs == nil {
		if err := w.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			return fmt.Errorf("add: %w", err)
		}
		return nil
	}
	for _, p := range specs {
		if err := w.AddWithOptions(&git.AddOptions{Path: p}); err != nil {
			if errors.Is(err, index.ErrEntryNotFound) || os.IsNotExist(err) {
				return fmt.Errorf("%w: %s matches no file", ErrInvalidArgument, p)
			}
			return fmt.Errorf("add %s: %w", p, err)
		}
	}
	return nil
}

// Remove untracks the tracked files at paths and, unless cached, deletes
// them. A file with changes not committed is refused (ErrWorktreeDirty), as
// git rm refuses one, so nothing uncommitted is lost.
func (r *Repo) Remove(paths []string, cached bool) error {
	specs, err := pathspecs(paths, false)
	if err != nil {
		return err
	}
	idx, err := r.repo.Storer.Index()
	if err != nil {
		return fmt.Errorf("read the index: %w", err)
	}
	var matched []string
	kept := idx.Entries[:0:0]
	for _, e := range idx.Entries {
		if matchPaths(specs, e.Name) {
			matched = append(matched, e.Name)
			continue
		}
		kept = append(kept, e)
	}
	if len(matched) == 0 {
		return fmt.Errorf("%w: the paths match no tracked file", ErrInvalidArgument)
	}
	if !cached {
		st, err := r.status()
		if err != nil {
			return err
		}
		var dirty []string
		for _, p := range matched {
			if fs, ok := st[p]; ok && (fs.Staging != git.Unmodified || fs.Worktree == git.Modified) {
				dirty = append(dirty, p)
			}
		}
		if len(dirty) > 0 {
			return fmt.Errorf("%w: uncommitted changes in %s", ErrWorktreeDirty, listPaths(dirty))
		}
	}
	idx.Entries = kept
	if err := r.repo.Storer.SetIndex(idx); err != nil {
		return fmt.Errorf("write the index: %w", err)
	}
	if cached {
		return nil
	}
	for _, p := range matched {
		if err := r.wt.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		r.removeEmptyParents(p)
	}
	return nil
}

// removeEmptyParents removes the directories above p that are now empty, as
// git does after removing a file. Failure leaves an empty directory, which
// is harmless.
func (r *Repo) removeEmptyParents(p string) {
	for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
		entries, err := r.wt.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if r.wt.Remove(dir) != nil {
			return
		}
	}
}

// Move renames a tracked file in the worktree and the index.
func (r *Repo) Move(from, to string) error {
	src, err := cleanPath(from)
	if err != nil {
		return err
	}
	dst, err := cleanPath(to)
	if err != nil {
		return err
	}
	w, err := r.worktree()
	if err != nil {
		return err
	}
	if _, err := w.Move(src, dst); err != nil {
		switch {
		case errors.Is(err, git.ErrDestinationExists):
			return fmt.Errorf("%w: %s exists", ErrAlreadyExists, dst)
		case errors.Is(err, index.ErrEntryNotFound), os.IsNotExist(err):
			return fmt.Errorf("%w: %s is not a tracked file", ErrInvalidArgument, src)
		}
		return fmt.Errorf("move: %w", err)
	}
	return nil
}

// Restore discards changes at paths ("." is everything): staged restores
// the index from HEAD, worktree restores the files from the index, both does
// both (the files then match HEAD). Untracked files are not touched.
func (r *Repo) Restore(paths []string, staged, worktree bool) error {
	if !staged && !worktree {
		return fmt.Errorf("%w: restore needs staged or worktree", ErrInvalidArgument)
	}
	specs, err := pathspecs(paths, false)
	if err != nil {
		return err
	}
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	src := side{}
	if head != nil {
		if src, err = r.commitSide(head); err != nil {
			return err
		}
	}
	if staged {
		n, err := r.resetIndexPaths(src, specs)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: the paths match no tracked file", ErrInvalidArgument)
		}
	}
	if worktree {
		n, err := r.restoreWorktree(specs)
		if err != nil {
			return err
		}
		if n == 0 && !staged {
			return fmt.Errorf("%w: the paths match no tracked file", ErrInvalidArgument)
		}
	}
	return nil
}

// resetIndexPaths makes the index entries under specs those of src: what
// `git reset <rev> -- paths` does, without moving HEAD (the library's
// path-limited reset moves it). It returns how many paths matched on
// either side. An entry already equal is kept with its stat data.
func (r *Repo) resetIndexPaths(src side, specs []string) (int, error) {
	idx, err := r.repo.Storer.Index()
	if err != nil {
		return 0, fmt.Errorf("read the index: %w", err)
	}
	matched := map[string]bool{}
	entries := idx.Entries[:0:0]
	for _, e := range idx.Entries {
		if !matchPaths(specs, e.Name) {
			entries = append(entries, e)
			continue
		}
		matched[e.Name] = true
		if s, ok := src[e.Name]; ok && e.Stage == mergedStage && s.hash == e.Hash && s.mode == e.Mode {
			entries = append(entries, e)
			delete(src, e.Name) // already there; not to be added again
		}
	}
	for name, s := range src {
		if !matchPaths(specs, name) {
			continue
		}
		matched[name] = true
		if s.mode == filemode.Dir {
			continue
		}
		entries = append(entries, &index.Entry{Name: name, Hash: s.hash, Mode: s.mode})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Stage < entries[j].Stage
	})
	idx.Entries = entries
	if err := r.repo.Storer.SetIndex(idx); err != nil {
		return 0, fmt.Errorf("write the index: %w", err)
	}
	return len(matched), nil
}

// restoreWorktree writes the index's content over the tracked files under
// specs that differ from it. It returns how many index entries matched.
func (r *Repo) restoreWorktree(specs []string) (int, error) {
	idx, err := r.indexSide()
	if err != nil {
		return 0, err
	}
	st, err := r.status()
	if err != nil {
		return 0, err
	}
	n := 0
	for name, e := range idx {
		if !matchPaths(specs, name) {
			continue
		}
		n++
		fs, ok := st[name]
		if !ok || (fs.Worktree != git.Modified && fs.Worktree != git.Deleted) || e.mode == filemode.Submodule {
			continue
		}
		if err := r.writeBlob(name, e); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// writeBlob writes an entry's content at p. What is at p is removed first,
// so a link left there is replaced and never written through.
func (r *Repo) writeBlob(p string, e *sideEntry) error {
	if err := r.wt.Remove(p); err != nil && !os.IsNotExist(err) {
		if err := billyutil.RemoveAll(r.wt, p); err != nil {
			return fmt.Errorf("replace %s: %w", p, err)
		}
	}
	if dir := path.Dir(p); dir != "." {
		if err := r.wt.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	blob, err := r.repo.BlobObject(e.hash)
	if err != nil {
		return fmt.Errorf("read blob %s: %w", e.hash, err)
	}
	rd, err := blob.Reader()
	if err != nil {
		return fmt.Errorf("read blob %s: %w", e.hash, err)
	}
	defer func() { _ = rd.Close() }()
	if e.mode == filemode.Symlink {
		target, err := io.ReadAll(io.LimitReader(rd, maxArgLen))
		if err != nil {
			return fmt.Errorf("read blob %s: %w", e.hash, err)
		}
		if err := r.wt.Symlink(string(target), p); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		return nil
	}
	perm := os.FileMode(0o644)
	if e.mode == filemode.Executable {
		perm = 0o755
	}
	f, err := r.wt.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	if _, err := io.Copy(f, rd); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	return nil
}

// Reset moves the current branch to rev ("" is HEAD): soft keeps index and
// worktree, mixed ("" too) resets the index, hard resets both. With paths
// (mixed only) it resets those index entries to rev's and moves nothing.
func (r *Repo) Reset(mode, rev string, paths []string) error {
	var m git.ResetMode
	switch mode {
	case "soft":
		m = git.SoftReset
	case "mixed", "":
		m = git.MixedReset
	case "hard":
		m = git.HardReset
	default:
		return fmt.Errorf("%w: reset mode must be soft, mixed or hard", ErrInvalidArgument)
	}
	if rev == "" {
		rev = "HEAD"
	}
	if len(paths) > 0 {
		if m != git.MixedReset {
			return fmt.Errorf("%w: paths go only with a mixed reset", ErrInvalidArgument)
		}
		specs, err := pathspecs(paths, false)
		if err != nil {
			return err
		}
		src := side{}
		head, err := r.headCommit()
		if err != nil {
			return err
		}
		// An unborn HEAD resets the paths to nothing: unstages them.
		if rev != "HEAD" || head != nil {
			if src, err = r.revSide(rev); err != nil {
				return err
			}
		}
		_, err = r.resetIndexPaths(src, specs)
		return err
	}
	c, err := r.resolveCommit(rev)
	if err != nil {
		return err
	}
	if m == git.HardReset {
		return r.hardReset(c.Hash)
	}
	w, err := r.worktree()
	if err != nil {
		return err
	}
	if err := w.Reset(&git.ResetOptions{Commit: c.Hash, Mode: m}); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}

// hardReset moves HEAD (its branch, or HEAD itself when detached) to commit
// and makes the index and the tracked files match it, as `git reset --hard`
// does. The library's hard reset also deletes every untracked file, which
// git never does and which would lose an agent's new work; so this is a
// mixed reset, then the files tracked before and not now are removed and
// the tracked ones are written from the index. An untracked file at a path
// the commit has is written over, as git does.
func (r *Repo) hardReset(commit plumbing.Hash) error {
	before, err := r.indexSide()
	if err != nil {
		return err
	}
	w, err := r.worktree()
	if err != nil {
		return err
	}
	if err := w.Reset(&git.ResetOptions{Commit: commit, Mode: git.MixedReset}); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	after, err := r.indexSide()
	if err != nil {
		return err
	}
	for p := range before {
		if _, ok := after[p]; ok {
			continue
		}
		if err := r.wt.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		r.removeEmptyParents(p)
	}
	_, err = r.restoreWorktree(nil)
	return err
}

func signature(s Signature, role string) (*object.Signature, error) {
	if strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.Email) == "" {
		return nil, fmt.Errorf("%w: the %s's name and email are required", ErrInvalidArgument, role)
	}
	if strings.ContainsAny(s.Name+s.Email, "<>\n\r\x00") {
		return nil, fmt.Errorf("%w: the %s's name or email has <, > or a line break", ErrInvalidArgument, role)
	}
	when := s.When
	if when.IsZero() {
		when = time.Now()
	}
	return &object.Signature{Name: s.Name, Email: s.Email, When: when}, nil
}

// Commit records the index as a new commit on HEAD and returns its hash.
// Author and committer are always given: the library would otherwise read
// them from the user's and the system's git config.
func (r *Repo) Commit(o CommitOptions) (string, error) {
	if strings.TrimSpace(o.Message) == "" {
		return "", fmt.Errorf("%w: a commit message is required", ErrInvalidArgument)
	}
	author, err := signature(o.Author, "author")
	if err != nil {
		return "", err
	}
	committer, err := signature(o.Committer, "committer")
	if err != nil {
		return "", err
	}
	w, err := r.worktree()
	if err != nil {
		return "", err
	}
	if o.Amend {
		head, err := r.headCommit()
		if err != nil {
			return "", err
		}
		if head == nil {
			return "", fmt.Errorf("%w: there is no commit to amend", ErrInvalidArgument)
		}
	}
	// The library refuses All with Amend; staging first does the same thing
	// and allows both.
	if o.All {
		st, err := r.status()
		if err != nil {
			return "", err
		}
		for p, fs := range st {
			if fs.Worktree != git.Modified && fs.Worktree != git.Deleted {
				continue
			}
			if _, err := w.Add(p); err != nil {
				return "", fmt.Errorf("stage %s: %w", p, err)
			}
		}
	}
	h, err := w.Commit(o.Message, &git.CommitOptions{
		Author:    author,
		Committer: committer,
		// Amending only the message is a commit with the parent's tree.
		AllowEmptyCommits: o.AllowEmpty || o.Amend,
		Amend:             o.Amend,
	})
	if errors.Is(err, git.ErrEmptyCommit) {
		return "", fmt.Errorf("%w: no changes are staged", ErrNothingToCommit)
	}
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return h.String(), nil
}

// checkSwitch refuses a move of the worktree to target that would lose
// work: any uncommitted change to a tracked file, staged or not, and any
// untracked file the target would write over. The library's own check
// looks only at unstaged changes, after it has already moved HEAD, and its
// reset discards staged ones; this is stricter than git, which carries a
// change across a switch that does not touch its file.
func (r *Repo) checkSwitch(target *object.Commit) error {
	st, err := r.status()
	if err != nil {
		return err
	}
	tree, err := target.Tree()
	if err != nil {
		return fmt.Errorf("read the target tree: %w", err)
	}
	var changed, clobbered []string
	for p, fs := range st {
		if fs.Worktree == git.Untracked {
			if collides(tree, p) {
				clobbered = append(clobbered, p)
			}
			continue
		}
		if fs.Staging != git.Unmodified || fs.Worktree != git.Unmodified {
			changed = append(changed, p)
		}
	}
	if len(changed) > 0 {
		return fmt.Errorf("%w: uncommitted changes in %s", ErrWorktreeDirty, listPaths(changed))
	}
	if len(clobbered) > 0 {
		return fmt.Errorf("%w: untracked files would be overwritten: %s", ErrWorktreeDirty, listPaths(clobbered))
	}
	return nil
}

// collides reports whether tree has something at p, or a file where p's
// directory would be.
func collides(tree *object.Tree, p string) bool {
	if _, err := tree.FindEntry(p); err == nil {
		return true
	}
	for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
		if e, err := tree.FindEntry(dir); err == nil && e.Mode != filemode.Dir {
			return true
		}
	}
	return false
}

// listPaths names at most a few paths for an error.
func listPaths(ps []string) string {
	sort.Strings(ps)
	const show = 5
	if len(ps) > show {
		return strings.Join(ps[:show], ", ") + fmt.Sprintf(" and %d more", len(ps)-show)
	}
	return strings.Join(ps, ", ")
}

// Checkout switches to a branch, creates one, or detaches HEAD at a
// revision.
func (r *Repo) Checkout(o CheckoutOptions) error {
	if o.Force && o.Keep {
		return fmt.Errorf("%w: force and keep do not go together", ErrInvalidArgument)
	}
	if (o.Branch == "") == (o.Rev == "" || o.Create) {
		return fmt.Errorf("%w: name a branch, or a revision without create", ErrInvalidArgument)
	}
	var target *object.Commit
	var branch plumbing.ReferenceName
	var err error
	switch {
	case o.Branch != "":
		if err := validBranchName(o.Branch); err != nil {
			return err
		}
		branch = plumbing.NewBranchReferenceName(o.Branch)
		_, rerr := r.repo.Reference(branch, false)
		if o.Create {
			if rerr == nil {
				return fmt.Errorf("%w: branch %s exists", ErrAlreadyExists, o.Branch)
			}
			start := o.Rev
			if start == "" {
				start = "HEAD"
			}
			if target, err = r.resolveCommit(start); err != nil {
				return err
			}
		} else {
			if rerr != nil {
				return fmt.Errorf("%w: no branch %s", ErrInvalidRevision, o.Branch)
			}
			if target, err = r.resolveCommit(branch.String()); err != nil {
				return err
			}
		}
	default:
		if target, err = r.resolveCommit(o.Rev); err != nil {
			return err
		}
	}
	if !o.Force && !o.Keep {
		if err := r.checkSwitch(target); err != nil {
			return err
		}
	}
	w, err := r.worktree()
	if err != nil {
		return err
	}
	if branch != "" && o.Create {
		if err := r.repo.Storer.SetReference(plumbing.NewHashReference(branch, target.Hash)); err != nil {
			return fmt.Errorf("create branch: %w", err)
		}
	}
	if o.Force {
		// The library's forced checkout is its hard reset, which deletes
		// untracked files; HEAD is moved here and hardReset does the rest.
		head := plumbing.NewHashReference(plumbing.HEAD, target.Hash)
		if branch != "" {
			head = plumbing.NewSymbolicReference(plumbing.HEAD, branch)
		}
		if err := r.repo.Storer.SetReference(head); err != nil {
			return fmt.Errorf("checkout: %w", err)
		}
		return r.hardReset(target.Hash)
	}
	opts := &git.CheckoutOptions{Keep: o.Keep}
	if branch != "" {
		opts.Branch = branch
	} else {
		opts.Hash = target.Hash
	}
	if err := w.Checkout(opts); err != nil {
		if o.Create {
			_ = r.repo.Storer.RemoveReference(branch)
		}
		return fmt.Errorf("checkout: %w", err)
	}
	return nil
}

// BranchCreate creates a branch at rev ("" is HEAD).
func (r *Repo) BranchCreate(name, rev string) error {
	if err := validBranchName(name); err != nil {
		return err
	}
	ref := plumbing.NewBranchReferenceName(name)
	if _, err := r.repo.Reference(ref, false); err == nil {
		return fmt.Errorf("%w: branch %s exists", ErrAlreadyExists, name)
	}
	if rev == "" {
		rev = "HEAD"
	}
	c, err := r.resolveCommit(rev)
	if err != nil {
		return err
	}
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(ref, c.Hash)); err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	return nil
}

// BranchDelete deletes a branch. The current branch is refused, and without
// force so is one whose commits HEAD does not contain.
func (r *Repo) BranchDelete(name string, force bool) error {
	if err := validBranchName(name); err != nil {
		return err
	}
	ref := plumbing.NewBranchReferenceName(name)
	br, err := r.repo.Reference(ref, true)
	if err != nil {
		return fmt.Errorf("%w: no branch %s", ErrInvalidArgument, name)
	}
	current, err := r.currentBranch()
	if err != nil {
		return err
	}
	if current == ref {
		return fmt.Errorf("%w: %s is the current branch", ErrInvalidArgument, name)
	}
	if !force {
		merged := false
		head, err := r.headCommit()
		if err != nil {
			return err
		}
		if head != nil {
			bc, err := r.repo.CommitObject(br.Hash())
			if err != nil {
				return fmt.Errorf("read the branch's commit: %w", err)
			}
			if bc.Hash == head.Hash {
				merged = true
			} else if merged, err = bc.IsAncestor(head); err != nil {
				return fmt.Errorf("check the branch is merged: %w", err)
			}
		}
		if !merged {
			return fmt.Errorf("%w: branch %s is not fully merged into HEAD; delete it with force", ErrInvalidArgument, name)
		}
	}
	if err := r.repo.Storer.RemoveReference(ref); err != nil {
		return fmt.Errorf("delete branch: %w", err)
	}
	if err := r.repo.DeleteBranch(name); err != nil && !errors.Is(err, git.ErrBranchNotFound) {
		return fmt.Errorf("delete branch config: %w", err)
	}
	return nil
}

// MergeFF fast-forwards the current branch (or a detached HEAD) to rev and
// returns HEAD's hash. A rev HEAD already contains changes nothing.
func (r *Repo) MergeFF(rev string) (string, error) {
	target, err := r.resolveCommit(rev)
	if err != nil {
		return "", err
	}
	return r.mergeFF(target)
}

func (r *Repo) mergeFF(target *object.Commit) (string, error) {
	head, err := r.headCommit()
	if err != nil {
		return "", err
	}
	if head != nil {
		if head.Hash == target.Hash {
			return head.Hash.String(), nil
		}
		contained, err := target.IsAncestor(head)
		if err != nil {
			return "", fmt.Errorf("merge: %w", err)
		}
		if contained {
			return head.Hash.String(), nil
		}
		ff, err := head.IsAncestor(target)
		if err != nil {
			return "", fmt.Errorf("merge: %w", err)
		}
		if !ff {
			return "", fmt.Errorf("%w: HEAD and %s have diverged", ErrNotFastForward, target.Hash)
		}
	}
	if err := r.checkSwitch(target); err != nil {
		return "", err
	}
	if head == nil {
		// An unborn branch has no ref for the reset to move: create it.
		branch, err := r.currentBranch()
		if err != nil {
			return "", err
		}
		if branch == "" {
			return "", fmt.Errorf("%w: HEAD names no branch", ErrInvalidArgument)
		}
		if err := r.repo.Storer.SetReference(plumbing.NewHashReference(branch, target.Hash)); err != nil {
			return "", fmt.Errorf("merge: %w", err)
		}
	}
	w, err := r.worktree()
	if err != nil {
		return "", err
	}
	// A merge reset moves HEAD's branch and rewrites what differs; the
	// check above made sure nothing of the agent's is among it.
	if err := w.Reset(&git.ResetOptions{Commit: target.Hash, Mode: git.MergeReset}); err != nil {
		return "", fmt.Errorf("merge: %w", err)
	}
	return target.Hash.String(), nil
}

// Clean removes untracked files that are not ignored, and with dirs the
// untracked directories too, or with dryRun only lists them. A directory is
// listed with a trailing "/". A nested repository is left alone, as git does.
func (r *Repo) Clean(dirs, dryRun bool) ([]string, error) {
	st, err := r.status()
	if err != nil {
		return nil, err
	}
	idx, err := r.indexSide()
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for name := range idx {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			tracked[dir] = true
		}
	}
	var files []string
	topDirs := map[string]bool{}
	for p, fs := range st {
		if fs.Worktree != git.Untracked {
			continue
		}
		// The highest directory above p that holds nothing tracked.
		top := ""
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			if d := strings.Join(parts[:i], "/"); !tracked[d] {
				top = d
				break
			}
		}
		switch {
		case top == "":
			files = append(files, p)
		case dirs:
			topDirs[top] = true
		}
	}
	out := append([]string(nil), files...)
	var removeDirs []string
	for d := range topDirs {
		if _, err := r.wt.Lstat(d + "/" + git.GitDirName); err == nil {
			continue
		}
		removeDirs = append(removeDirs, d)
		out = append(out, d+"/")
	}
	sort.Strings(out)
	if dryRun {
		return out, nil
	}
	for _, p := range files {
		if err := r.wt.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove %s: %w", p, err)
		}
	}
	for _, d := range removeDirs {
		if err := billyutil.RemoveAll(r.wt, d); err != nil {
			return nil, fmt.Errorf("remove %s: %w", d, err)
		}
	}
	return out, nil
}
