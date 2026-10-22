package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-billy/v5"
	billyutil "github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// Network is how one network operation reaches the remote.
type Network struct {
	// URL is dialled as given; a remote's configured URL never is, since the
	// repository's config is the agent's to write. The caller validates it
	// (gitsource.ValidateURL); it is checked again here.
	URL string
	// Username is the basic-auth user; ignored when Token is empty.
	Username string
	// Token is the basic-auth password; empty is anonymous.
	Token string
	// Guard is bound to the operation; its Host must be URL's.
	Guard gitsource.Guard
	// MaxObjectBytes bounds any one incoming object once inflated. Required
	// for clone, fetch and pull.
	MaxObjectBytes int64
	// SpoolDir is an existing directory of the runtime's own, outside any
	// volume, where incoming packs are checked before they are stored.
	SpoolDir string
}

// CloneOptions are Clone's.
type CloneOptions struct {
	RemoteName   string // "" = origin
	Branch       string
	Depth        int
	SingleBranch bool
	NoCheckout   bool
}

// RefInfo is one ref a remote advertises.
type RefInfo struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// FetchOptions are Fetch's.
type FetchOptions struct {
	Remote string // "" = origin
	// RefSpecs default to the remote's configured ones. A bare branch name
	// is "+refs/heads/NAME:refs/remotes/REMOTE/NAME".
	RefSpecs []string
	Depth    int
	Prune    bool
	Tags     bool
}

// FetchResult reports a fetch.
type FetchResult struct {
	UpToDate bool        `json:"up_to_date"`
	Updated  []RefUpdate `json:"updated"`
}

// PullOptions are Pull's.
type PullOptions struct {
	Remote string // "" = origin
	Branch string // "" = the current branch's name
}

// PullResult reports a pull.
type PullResult struct {
	UpToDate bool   `json:"up_to_date"`
	Head     string `json:"head"`
}

// PushOptions are Push's.
type PushOptions struct {
	Remote string // "" = origin
	// RefSpecs default to the current branch to the same name. A bare
	// branch name is "refs/heads/NAME:refs/heads/NAME".
	RefSpecs       []string
	Force          bool
	ForceWithLease bool
	Tags           bool
}

// PushResult reports a push.
type PushResult struct {
	UpToDate bool        `json:"up_to_date"`
	Updated  []RefUpdate `json:"updated"`
}

// RefUpdate is one ref moved by a fetch or a push. An empty Old is a ref
// created; an empty New is one deleted.
type RefUpdate struct {
	Name string `json:"name"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// errProbe asks a session whether its limits were passed; see finish.
var errProbe = errors.New("gitrepo: limit probe")

// bind checks the URL against the guard and binds the operation.
func (n Network) bind(ctx context.Context) (context.Context, *gitsource.Session, transport.AuthMethod, error) {
	host, err := gitsource.ValidateURL(n.URL)
	if err != nil {
		return ctx, nil, nil, err
	}
	if host != n.Guard.Host {
		return ctx, nil, nil, fmt.Errorf("%w: the URL's host is not the one the operation is bound to", gitsource.ErrInvalidRequest)
	}
	ctx, sess, err := gitsource.Bind(ctx, n.Guard)
	if err != nil {
		return ctx, nil, nil, err
	}
	var auth transport.AuthMethod
	if n.Token != "" {
		auth = &githttp.BasicAuth{Username: n.Username, Password: n.Token}
	}
	return ctx, sess, auth, nil
}

// receiving checks what an operation that takes packs needs.
func (n Network) receiving() error {
	if n.MaxObjectBytes <= 0 || n.SpoolDir == "" {
		return fmt.Errorf("%w: an object size limit and a spool directory are required", ErrInvalidArgument)
	}
	return nil
}

// finish classifies the end of a bound operation. A success is checked
// against the limits too: a library step that stopped reading at the byte
// limit and did not look at the error would otherwise end "successfully",
// and the limits are the caller's promise about memory and disk. Classify
// reports a passed limit whatever error it is given, so a probe asks.
func finish(ctx context.Context, sess *gitsource.Session, err error) error {
	if err != nil {
		return sess.Classify(ctx, err)
	}
	if c := sess.Classify(ctx, errProbe); errors.Is(c, gitsource.ErrTooLarge) || errors.Is(c, gitsource.ErrUploadTooLarge) {
		return c
	}
	return nil
}

// guarded is a repository over wt whose store checks every incoming pack,
// for the length of one operation.
func guarded(ctx context.Context, sess *gitsource.Session, wt billy.Filesystem, n Network) (*gitsource.GuardedStorage, error) {
	base, err := newStorage(wt)
	if err != nil {
		return nil, err
	}
	g := gitsource.NewGuardedStorage(ctx, base, n.SpoolDir, n.MaxObjectBytes, n.Guard.MaxReadBytes)
	sess.WatchPacks(g)
	return g, nil
}

// Clone clones n.URL into wt, which must have no .git. The remote's URL is
// recorded as n.URL exactly. On failure what Clone created in wt is removed:
// no partial repository is left to be mistaken for one.
func Clone(ctx context.Context, wt billy.Filesystem, n Network, o CloneOptions) (_ *Repo, err error) {
	remote := o.RemoteName
	if remote == "" {
		remote = git.DefaultRemoteName
	}
	if err := validRemoteName(remote); err != nil {
		return nil, err
	}
	if o.Branch != "" {
		if err := validBranchName(o.Branch); err != nil {
			return nil, err
		}
	}
	if o.Depth < 0 {
		return nil, fmt.Errorf("%w: depth must be >= 0", ErrInvalidArgument)
	}
	if err := n.receiving(); err != nil {
		return nil, err
	}
	exists, err := hasDotGit(wt)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if exists {
		return nil, fmt.Errorf("%w: the directory already has a .git", ErrAlreadyExists)
	}
	ctx, sess, auth, err := n.bind(ctx)
	if err != nil {
		return nil, err
	}
	before := map[string]bool{}
	entries, err := wt.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("%w: read the directory: %v", ErrInvalidArgument, err)
	}
	for _, e := range entries {
		before[e.Name()] = true
	}
	defer func() {
		if err == nil {
			return
		}
		after, rerr := wt.ReadDir(".")
		if rerr != nil {
			return
		}
		for _, e := range after {
			if !before[e.Name()] {
				_ = billyutil.RemoveAll(wt, e.Name())
			}
		}
	}()
	g, err := guarded(ctx, sess, wt, n)
	if err != nil {
		return nil, err
	}
	co := &git.CloneOptions{
		URL:          n.URL,
		Auth:         auth,
		RemoteName:   remote,
		Depth:        o.Depth,
		SingleBranch: o.SingleBranch,
		NoCheckout:   o.NoCheckout,
	}
	if o.Branch != "" {
		co.ReferenceName = plumbing.NewBranchReferenceName(o.Branch)
	}
	_, cerr := git.CloneContext(ctx, g, guardWorktree(wt), co)
	if cerr != nil && (errors.Is(cerr, plumbing.ErrReferenceNotFound) || errors.Is(cerr, git.NoMatchingRefSpecError{})) {
		if c := finish(ctx, sess, nil); c != nil {
			return nil, c
		}
		return nil, fmt.Errorf("%w: the remote has no branch %q: %v", ErrInvalidRevision, o.Branch, cerr)
	}
	if err := finish(ctx, sess, cerr); err != nil {
		return nil, err
	}
	repo, err := openRepo(wt)
	if err != nil {
		return nil, err
	}
	return newRepo(wt, repo), nil
}

// listRemote lists the refs advertised at url. An empty repository has
// none.
func listRemote(ctx context.Context, url string, auth transport.AuthMethod) ([]*plumbing.Reference, error) {
	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: git.DefaultRemoteName, URLs: []string{url}})
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: auth})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, nil
	}
	return refs, err
}

// LsRemote lists the refs n.URL advertises, sorted by name. A symbolic ref
// (HEAD) is given the hash of what it names.
func LsRemote(ctx context.Context, n Network) ([]RefInfo, error) {
	ctx, sess, auth, err := n.bind(ctx)
	if err != nil {
		return nil, err
	}
	refs, err := listRemote(ctx, n.URL, auth)
	if err := finish(ctx, sess, err); err != nil {
		return nil, err
	}
	hashes := map[plumbing.ReferenceName]plumbing.Hash{}
	for _, ref := range refs {
		if ref.Type() == plumbing.HashReference {
			hashes[ref.Name()] = ref.Hash()
		}
	}
	out := make([]RefInfo, 0, len(refs))
	for _, ref := range refs {
		h := ref.Hash()
		if ref.Type() == plumbing.SymbolicReference {
			h = hashes[ref.Target()]
		}
		ri := RefInfo{Name: ref.Name().String()}
		if !h.IsZero() {
			ri.Hash = h.String()
		}
		out = append(out, ri)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// remoteName defaults and checks a remote's name, and checks the repository
// configures it: the fetch and push bookkeeping is the remote's.
func (r *Repo) remoteName(name string) (string, error) {
	if name == "" {
		name = git.DefaultRemoteName
	}
	if err := validRemoteName(name); err != nil {
		return "", err
	}
	if _, err := r.repo.Remote(name); err != nil {
		return "", fmt.Errorf("%w: no remote %q", ErrInvalidArgument, name)
	}
	return name, nil
}

// validRefPattern checks a full ref name that may hold one "*".
func validRefPattern(s string) bool {
	if !strings.HasPrefix(s, "refs/") || strings.Count(s, "*") > 1 || hasControl(s) {
		return false
	}
	return plumbing.ReferenceName(strings.Replace(s, "*", "x", 1)).Validate() == nil
}

// parseRefSpec checks "[+]src:dst" with full ref names. A delete (empty
// src) is allowed only when push is set.
func parseRefSpec(s string, push bool) (config.RefSpec, error) {
	bad := fmt.Errorf("%w: invalid refspec %q", ErrInvalidArgument, truncate(s, 200))
	if len(s) > maxArgLen {
		return "", bad
	}
	body := strings.TrimPrefix(s, "+")
	src, dst, ok := strings.Cut(body, ":")
	if !ok || strings.Contains(dst, ":") || !validRefPattern(dst) {
		return "", bad
	}
	if src == "" {
		if !push || strings.HasPrefix(s, "+") || strings.Contains(dst, "*") {
			return "", bad
		}
	} else if !validRefPattern(src) || strings.Contains(src, "*") != strings.Contains(dst, "*") {
		return "", bad
	}
	rs := config.RefSpec(s)
	if rs.Validate() != nil {
		return "", bad
	}
	return rs, nil
}

// fetchRefSpecs turns Fetch's refspecs into the library's. None of them may
// write to the checked-out branch, which would move it under the worktree.
func (r *Repo) fetchRefSpecs(remote string, specs []string) ([]config.RefSpec, error) {
	current, err := r.currentBranch()
	if err != nil {
		return nil, err
	}
	out := make([]config.RefSpec, 0, len(specs))
	for _, s := range specs {
		var rs config.RefSpec
		if !strings.Contains(s, ":") && !strings.HasPrefix(s, "refs/") && !strings.HasPrefix(s, "+") {
			if err := validBranchName(s); err != nil {
				return nil, err
			}
			rs = config.RefSpec("+refs/heads/" + s + ":refs/remotes/" + remote + "/" + s)
		} else {
			if rs, err = parseRefSpec(s, false); err != nil {
				return nil, err
			}
		}
		if current != "" && dstMayBe(rs, current) {
			return nil, fmt.Errorf("%w: refspec %q writes to the checked-out branch", ErrInvalidArgument, truncate(s, 200))
		}
		out = append(out, rs)
	}
	return out, nil
}

// dstMayBe reports whether rs could write ref.
func dstMayBe(rs config.RefSpec, ref plumbing.ReferenceName) bool {
	_, dst, _ := strings.Cut(strings.TrimPrefix(string(rs), "+"), ":")
	pre, post, wild := strings.Cut(dst, "*")
	if !wild {
		return dst == ref.String()
	}
	name := ref.String()
	return len(name) >= len(pre)+len(post) && strings.HasPrefix(name, pre) && strings.HasSuffix(name, post)
}

// refSnapshot is every ref with a hash, HEAD aside.
func (r *Repo) refSnapshot() (map[plumbing.ReferenceName]plumbing.Hash, error) {
	it, err := r.repo.Storer.IterReferences()
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	out := map[plumbing.ReferenceName]plumbing.Hash{}
	err = it.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference && ref.Name() != plumbing.HEAD {
			out[ref.Name()] = ref.Hash()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	return out, nil
}

// refUpdates is what moved between two snapshots, sorted by name.
func refUpdates(before, after map[plumbing.ReferenceName]plumbing.Hash) []RefUpdate {
	out := []RefUpdate{}
	for name, h := range after {
		if old, ok := before[name]; !ok || old != h {
			u := RefUpdate{Name: name.String(), New: h.String()}
			if ok {
				u.Old = old.String()
			}
			out = append(out, u)
		}
	}
	for name, h := range before {
		if _, ok := after[name]; !ok {
			out = append(out, RefUpdate{Name: name.String(), Old: h.String()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Fetch fetches from n.URL, keeping the books of the named remote.
func (r *Repo) Fetch(ctx context.Context, n Network, o FetchOptions) (FetchResult, error) {
	remote, err := r.remoteName(o.Remote)
	if err != nil {
		return FetchResult{}, err
	}
	specs, err := r.fetchRefSpecs(remote, o.RefSpecs)
	if err != nil {
		return FetchResult{}, err
	}
	if o.Depth < 0 {
		return FetchResult{}, fmt.Errorf("%w: depth must be >= 0", ErrInvalidArgument)
	}
	if err := n.receiving(); err != nil {
		return FetchResult{}, err
	}
	ctx, sess, auth, err := n.bind(ctx)
	if err != nil {
		return FetchResult{}, err
	}
	tags := git.TagFollowing
	if o.Tags {
		tags = git.AllTags
	}
	return r.fetch(ctx, sess, n, &git.FetchOptions{
		RemoteName: remote,
		RemoteURL:  n.URL,
		RefSpecs:   specs,
		Depth:      o.Depth,
		Auth:       auth,
		Prune:      o.Prune,
		Tags:       tags,
	})
}

func (r *Repo) fetch(ctx context.Context, sess *gitsource.Session, n Network, fo *git.FetchOptions) (FetchResult, error) {
	before, err := r.refSnapshot()
	if err != nil {
		return FetchResult{}, err
	}
	g, err := guarded(ctx, sess, r.base, n)
	if err != nil {
		return FetchResult{}, err
	}
	gr, err := git.Open(g, r.wt)
	if err != nil {
		return FetchResult{}, fmt.Errorf("open: %w", err)
	}
	rem, err := gr.Remote(fo.RemoteName)
	if err != nil {
		return FetchResult{}, fmt.Errorf("%w: no remote %q", ErrInvalidArgument, fo.RemoteName)
	}
	ferr := rem.FetchContext(ctx, fo)
	// The fetch wrote objects and refs through another store; this one's
	// caches (the list of packs among them) do not know of them.
	repo, oerr := openRepo(r.base)
	if oerr != nil {
		return FetchResult{}, oerr
	}
	r.repo = repo
	upToDate := errors.Is(ferr, git.NoErrAlreadyUpToDate)
	if upToDate {
		ferr = nil
	}
	switch {
	case ferr == nil:
	case errors.Is(ferr, git.ErrForceNeeded):
		if c := finish(ctx, sess, nil); c != nil {
			return FetchResult{}, c
		}
		return FetchResult{}, fmt.Errorf("%w: a ref the remote moved is not a fast-forward of the local one", ErrNotFastForward)
	case errors.Is(ferr, git.NoMatchingRefSpecError{}):
		if c := finish(ctx, sess, nil); c != nil {
			return FetchResult{}, c
		}
		return FetchResult{}, fmt.Errorf("%w: the remote has no ref the refspecs name", ErrInvalidRevision)
	}
	if err := finish(ctx, sess, ferr); err != nil {
		return FetchResult{}, err
	}
	after, err := r.refSnapshot()
	if err != nil {
		return FetchResult{}, err
	}
	updated := refUpdates(before, after)
	return FetchResult{UpToDate: upToDate && len(updated) == 0, Updated: updated}, nil
}

// Pull fetches a branch of the remote and fast-forwards the current branch
// to it. A pull that is not a fast-forward is refused (ErrNotFastForward),
// as is one that would overwrite local changes (ErrWorktreeDirty); the
// fetched commits are kept either way.
func (r *Repo) Pull(ctx context.Context, n Network, o PullOptions) (PullResult, error) {
	remote, err := r.remoteName(o.Remote)
	if err != nil {
		return PullResult{}, err
	}
	branch := o.Branch
	if branch == "" {
		current, err := r.currentBranch()
		if err != nil {
			return PullResult{}, err
		}
		if current == "" {
			return PullResult{}, fmt.Errorf("%w: HEAD is detached; name the branch to pull", ErrInvalidArgument)
		}
		branch = current.Short()
	}
	if err := validBranchName(branch); err != nil {
		return PullResult{}, err
	}
	if err := n.receiving(); err != nil {
		return PullResult{}, err
	}
	head, err := r.Head()
	if err != nil {
		return PullResult{}, err
	}
	ctx, sess, auth, err := n.bind(ctx)
	if err != nil {
		return PullResult{}, err
	}
	tracking := plumbing.NewRemoteReferenceName(remote, branch)
	if _, err := r.fetch(ctx, sess, n, &git.FetchOptions{
		RemoteName: remote,
		RemoteURL:  n.URL,
		RefSpecs:   []config.RefSpec{config.RefSpec("+" + plumbing.NewBranchReferenceName(branch).String() + ":" + tracking.String())},
		Auth:       auth,
	}); err != nil {
		return PullResult{}, err
	}
	tip, err := r.repo.Reference(tracking, true)
	if err != nil {
		return PullResult{}, fmt.Errorf("%w: the remote has no branch %s", ErrInvalidRevision, branch)
	}
	target, err := r.repo.CommitObject(tip.Hash())
	if err != nil {
		return PullResult{}, fmt.Errorf("read %s: %w", tracking, err)
	}
	newHead, err := r.mergeFF(target)
	if err != nil {
		return PullResult{}, err
	}
	return PullResult{UpToDate: newHead == head.Hash, Head: newHead}, nil
}

// pushRefSpecs turns Push's refspecs into the library's.
func (r *Repo) pushRefSpecs(o PushOptions) ([]config.RefSpec, error) {
	var out []config.RefSpec
	if len(o.RefSpecs) == 0 {
		current, err := r.currentBranch()
		if err != nil {
			return nil, err
		}
		if current == "" {
			return nil, fmt.Errorf("%w: HEAD is detached; name what to push", ErrInvalidArgument)
		}
		out = append(out, config.RefSpec(current.String()+":"+current.String()))
	}
	for _, s := range o.RefSpecs {
		if !strings.Contains(s, ":") && !strings.HasPrefix(s, "refs/") && !strings.HasPrefix(s, "+") {
			if err := validBranchName(s); err != nil {
				return nil, err
			}
			b := plumbing.NewBranchReferenceName(s).String()
			out = append(out, config.RefSpec(b+":"+b))
			continue
		}
		rs, err := parseRefSpec(s, true)
		if err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	if o.Tags {
		out = append(out, config.RefSpec("refs/tags/*:refs/tags/*"))
	}
	return out, nil
}

// Push pushes to n.URL, keeping the books of the named remote. A ref the
// remote refuses (not a fast-forward, protected, ...) fails the push with
// ErrPushRejected.
func (r *Repo) Push(ctx context.Context, n Network, o PushOptions) (PushResult, error) {
	remote, err := r.remoteName(o.Remote)
	if err != nil {
		return PushResult{}, err
	}
	if o.Force && o.ForceWithLease {
		return PushResult{}, fmt.Errorf("%w: force and force-with-lease do not go together", ErrInvalidArgument)
	}
	specs, err := r.pushRefSpecs(o)
	if err != nil {
		return PushResult{}, err
	}
	rem, err := r.repo.Remote(remote)
	if err != nil {
		return PushResult{}, fmt.Errorf("%w: no remote %q", ErrInvalidArgument, remote)
	}
	ctx, sess, auth, err := n.bind(ctx)
	if err != nil {
		return PushResult{}, err
	}
	// The library does not say what a push moved, so the remote's refs are
	// read first, from the same URL, to report each update's old value.
	advertised, err := listRemote(ctx, n.URL, auth)
	if err := finish(ctx, sess, err); err != nil {
		return PushResult{}, err
	}
	remoteRefs := map[plumbing.ReferenceName]plumbing.Hash{}
	for _, ref := range advertised {
		if ref.Type() == plumbing.HashReference {
			remoteRefs[ref.Name()] = ref.Hash()
		}
	}
	po := &git.PushOptions{RemoteName: remote, RemoteURL: n.URL, RefSpecs: specs, Auth: auth, Force: o.Force}
	if o.ForceWithLease {
		po.ForceWithLease = &git.ForceWithLease{}
	}
	perr := rem.PushContext(ctx, po)
	if errors.Is(perr, git.NoErrAlreadyUpToDate) {
		if err := finish(ctx, sess, nil); err != nil {
			return PushResult{}, err
		}
		return PushResult{UpToDate: true, Updated: []RefUpdate{}}, nil
	}
	if perr != nil {
		if c := finish(ctx, sess, nil); c != nil {
			return PushResult{}, c
		}
		if pushRejected(perr) {
			return PushResult{}, fmt.Errorf("%w: %s", ErrPushRejected, refusal(perr))
		}
		return PushResult{}, sess.Classify(ctx, perr)
	}
	if err := finish(ctx, sess, nil); err != nil {
		return PushResult{}, err
	}
	updated, err := r.pushed(specs, remoteRefs)
	if err != nil {
		return PushResult{}, err
	}
	return PushResult{Updated: updated}, nil
}

// pushed is what a successful push moved: each local ref the refspecs
// matched whose remote value differed, and each deletion of a ref the
// remote had.
func (r *Repo) pushed(specs []config.RefSpec, remoteRefs map[plumbing.ReferenceName]plumbing.Hash) ([]RefUpdate, error) {
	local, err := r.refSnapshot()
	if err != nil {
		return nil, err
	}
	seen := map[plumbing.ReferenceName]bool{}
	out := []RefUpdate{}
	for _, rs := range specs {
		if rs.IsDelete() {
			dst := rs.Dst("")
			if old, ok := remoteRefs[dst]; ok && !seen[dst] {
				seen[dst] = true
				out = append(out, RefUpdate{Name: dst.String(), Old: old.String()})
			}
			continue
		}
		for name, h := range local {
			if !rs.Match(name) {
				continue
			}
			dst := rs.Dst(name)
			old, ok := remoteRefs[dst]
			if (ok && old == h) || seen[dst] {
				continue
			}
			seen[dst] = true
			u := RefUpdate{Name: dst.String(), New: h.String()}
			if ok {
				u.Old = old.String()
			}
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// pushRejected reports a ref update refused, by the library's own
// fast-forward check (which has no error value, only this text) or by the
// server's report.
func pushRejected(err error) bool {
	s := err.Error()
	return strings.HasPrefix(s, "non-fast-forward update") || strings.HasPrefix(s, "command error on ") || errors.Is(err, git.ErrForceNeeded)
}

// maxRefusal bounds what is kept of a refusal's text, which can be the
// server's own words.
const maxRefusal = 160

// refusal is a refusal's text, bounded and on one line, for the operator.
func refusal(err error) string {
	s := strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return ' '
		}
		return c
	}, err.Error())
	return truncate(s, maxRefusal)
}
