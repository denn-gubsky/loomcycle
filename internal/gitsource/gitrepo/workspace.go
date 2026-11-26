package gitrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/go-git/go-billy/v5"
	billyutil "github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// A workspace's repository and its mirror (RFC GW §4.2, P11 plan §3.5).
//
// The worker keeps one bare mirror per (tenant, repository URL), filled only
// from the forge through the core. Each workspace on it has its own working
// tree and git directory: its refs, index and the objects its commits made.
// Everything else it reads from the mirror, layered under its own store by
// this package, never through git's alternates file. The mirror keeps a pin
// ref per workspace so a later prune never drops what a workspace rests on.

// PinPrefix is where a mirror keeps its workspaces' pins. A fetch never
// writes here: the mirror's refspecs map the forge's heads and tags only.
const PinPrefix = "refs/loom/workspaces/"

// mirrorRefSpecs are what a mirror takes from the forge. Prune removes refs
// under these destinations only, so a pin is never pruned.
var mirrorRefSpecs = []config.RefSpec{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// MirrorKey names a tenant's mirror of one repository: the first 32 hex
// digits of SHA-256 over the tenant and the URL. A worker joins it to a path
// only after ValidMirrorKey.
func MirrorKey(tenantID, url string) string {
	sum := sha256.Sum256([]byte(tenantID + "\n" + url))
	return hex.EncodeToString(sum[:16])
}

var mirrorKeyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidMirrorKey reports whether k is a key MirrorKey makes.
func ValidMirrorKey(k string) bool { return mirrorKeyRe.MatchString(k) }

// ValidBranchName reports whether name is a short branch name this package
// creates: the core checks a workspace's own branch with it before the open
// reaches the network.
func ValidBranchName(name string) bool { return validBranchName(name) == nil }

// errWorkspaceNetwork refuses a network op a workspace's repository does not
// take: its objects come from the mirror, which Mirror.Fetch fills.
var errWorkspaceNetwork = fmt.Errorf("%w: a workspace fetches through its mirror", ErrInvalidArgument)

// workspaceIDRe is a workspace id as a ref name component.
var workspaceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Mirror is a bare repository on a root-confined filesystem.
type Mirror struct {
	fs billy.Filesystem
}

// OpenMirror opens the mirror whose git directory is fs, making an empty
// bare repository there when fs holds none.
func OpenMirror(fs billy.Filesystem) (*Mirror, error) {
	if _, err := fs.Lstat("HEAD"); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %v", ErrNotARepository, err)
		}
		if _, err := git.Init(openStore(fs), nil); err != nil {
			return nil, fmt.Errorf("init the mirror: %w", err)
		}
	}
	return &Mirror{fs: fs}, nil
}

// store opens the mirror's store afresh: a handle held across a repack fails
// (spike A), so none is kept.
func (m *Mirror) store() *filesystem.Storage { return openStore(m.fs) }

// Fetch brings the forge's heads and tags into the mirror, pruning heads and
// tags the forge no longer has. Every incoming pack is checked before it is
// stored (the pack guard: n.MaxObjectBytes per object, the read limit in all).
func (m *Mirror) Fetch(ctx context.Context, n Network) (FetchResult, error) {
	if err := n.receiving(); err != nil {
		return FetchResult{}, err
	}
	ctx, sess, auth, err := n.bind(ctx)
	if err != nil {
		return FetchResult{}, err
	}
	before, err := refsOf(m.store())
	if err != nil {
		return FetchResult{}, err
	}
	g := gitsource.NewGuardedStorage(ctx, m.store(), n.SpoolDir, n.MaxObjectBytes, n.Guard.MaxReadBytes)
	sess.WatchPacks(g)
	rem := git.NewRemote(g, &config.RemoteConfig{Name: git.DefaultRemoteName, URLs: []string{n.URL}, Fetch: mirrorRefSpecs})
	ferr := rem.FetchContext(ctx, &git.FetchOptions{
		RemoteURL: n.URL, RefSpecs: mirrorRefSpecs, Auth: auth, Prune: true, Tags: git.NoTags,
	})
	upToDate := errors.Is(ferr, git.NoErrAlreadyUpToDate)
	if upToDate || errors.Is(ferr, git.NoMatchingRefSpecError{}) {
		ferr = nil // an empty repository advertises nothing to take
	}
	if err := finish(ctx, sess, ferr); err != nil {
		return FetchResult{}, err
	}
	after, err := refsOf(m.store())
	if err != nil {
		return FetchResult{}, err
	}
	updated := refUpdates(before, after)
	return FetchResult{UpToDate: len(updated) == 0, Updated: updated}, nil
}

// refsOf is every hash ref of s but the pins.
func refsOf(s *filesystem.Storage) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	it, err := s.IterReferences()
	if err != nil {
		return nil, err
	}
	out := map[plumbing.ReferenceName]plumbing.Hash{}
	err = it.ForEach(func(r *plumbing.Reference) error {
		if r.Type() == plumbing.HashReference && !strings.HasPrefix(r.Name().String(), PinPrefix) {
			out[r.Name()] = r.Hash()
		}
		return nil
	})
	return out, err
}

// Resolve is the commit a mirror ref names: a full ref under refs/heads/ or
// refs/tags/, or a full commit id the mirror holds. The forge's default
// branch is the core's to name (it reads the forge's own advertisement), so
// the mirror's HEAD, which no fetch sets, is not taken.
func (m *Mirror) Resolve(ref string) (plumbing.Hash, error) {
	s := m.store()
	if plumbing.IsHash(ref) {
		h := plumbing.NewHash(ref)
		if _, err := s.EncodedObject(plumbing.CommitObject, h); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("%w: the mirror has no commit %s", ErrInvalidRevision, ref)
		}
		return h, nil
	}
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return plumbing.ZeroHash, fmt.Errorf("%w: a base is a full ref under refs/heads/ or refs/tags/, or a commit id", ErrInvalidArgument)
	}
	if err := validRev(ref); err != nil {
		return plumbing.ZeroHash, err
	}
	name := plumbing.ReferenceName(ref)
	if err := name.Validate(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("%w: %q is not a ref name", ErrInvalidArgument, truncate(ref, 200))
	}
	r, err := storerResolve(s, name)
	if err != nil || r.IsZero() {
		return plumbing.ZeroHash, fmt.Errorf("%w: the mirror has no %s", ErrInvalidRevision, truncate(ref, 200))
	}
	// A tag may name a tag object; the base is the commit it peels to.
	c, err := peelToCommit(s, r)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return c, nil
}

// Pin writes refs/loom/workspaces/<id>/<name> at h.
func (m *Mirror) Pin(id, name string, h plumbing.Hash) error {
	ref, err := pinRef(id, name)
	if err != nil {
		return err
	}
	return m.store().SetReference(plumbing.NewHashReference(ref, h))
}

// Unpin removes every pin of workspace id.
func (m *Mirror) Unpin(id string) error {
	if !workspaceIDRe.MatchString(id) {
		return fmt.Errorf("%w: a workspace id", ErrInvalidArgument)
	}
	s := m.store()
	prefix := PinPrefix + id + "/"
	var names []plumbing.ReferenceName
	it, err := s.IterReferences()
	if err != nil {
		return err
	}
	if err := it.ForEach(func(r *plumbing.Reference) error {
		if strings.HasPrefix(r.Name().String(), prefix) {
			names = append(names, r.Name())
		}
		return nil
	}); err != nil {
		return err
	}
	for _, n := range names {
		if err := s.RemoveReference(n); err != nil {
			return err
		}
	}
	return nil
}

// Pinned is the ids of the workspaces that hold a pin in the mirror.
func (m *Mirror) Pinned() ([]string, error) {
	it, err := m.store().IterReferences()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	err = it.ForEach(func(r *plumbing.Reference) error {
		rest, ok := strings.CutPrefix(r.Name().String(), PinPrefix)
		if !ok {
			return nil
		}
		id, _, _ := strings.Cut(rest, "/")
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
		return nil
	})
	return out, err
}

func pinRef(id, name string) (plumbing.ReferenceName, error) {
	if !workspaceIDRe.MatchString(id) {
		return "", fmt.Errorf("%w: a workspace id", ErrInvalidArgument)
	}
	ref := plumbing.ReferenceName(PinPrefix + id + "/" + name)
	if name == "" || ref.Validate() != nil {
		return "", fmt.Errorf("%w: a pin name", ErrInvalidArgument)
	}
	return ref, nil
}

// WorkspaceSpec is what the core's row says of a workspace (RFC GW §8): the
// supervisor never reads these back from the workspace.
type WorkspaceSpec struct {
	// ID is the workspace's id; it names its pins.
	ID string
	// URL is the repository's URL, the remote of the configuration the
	// typed ops see.
	URL string
	// Branch is the workspace's own branch.
	Branch string
	// AlternatesPath is written into the workspace's alternates file for
	// `git` in its session: the mirror's objects as the session sees them
	// (/mirror/objects in a container). The typed ops never read it. Empty
	// writes no file.
	AlternatesPath string
	// Limits bound the tree checked out and are the returned Repo's.
	Limits Limits
}

func (s WorkspaceSpec) check() error {
	if !workspaceIDRe.MatchString(s.ID) {
		return fmt.Errorf("%w: a workspace id", ErrInvalidArgument)
	}
	if _, err := gitsource.ValidateURL(s.URL); err != nil {
		return err
	}
	if err := validBranchName(s.Branch); err != nil {
		return err
	}
	if p := s.AlternatesPath; p != "" && (!strings.HasPrefix(p, "/") || hasControl(p)) {
		return fmt.Errorf("%w: the alternates path must be absolute, on one line", ErrInvalidArgument)
	}
	return nil
}

// config is the configuration the typed ops see: the row's remote and the
// workspace's branch tracking the branch of the same name there.
func (s WorkspaceSpec) config() *config.Config {
	c := config.NewConfig()
	c.Core.IsBare = false
	c.Remotes[git.DefaultRemoteName] = &config.RemoteConfig{
		Name: git.DefaultRemoteName, URLs: []string{s.URL},
		Fetch: []config.RefSpec{config.RefSpec("+refs/heads/*:refs/remotes/" + git.DefaultRemoteName + "/*")},
	}
	c.Branches[s.Branch] = &config.Branch{
		Name: s.Branch, Remote: git.DefaultRemoteName, Merge: plumbing.NewBranchReferenceName(s.Branch),
	}
	return c
}

// CreateWorkspace makes a workspace's git directory in wt (which must have
// none) over mirror m: the branch spec.Branch at base, a pin at base in the
// mirror, and base's tree checked out once it is checked against the limits
// and for paths into .git. On failure its .git and the pin are removed; files
// a checkout wrote before it failed stay, for the caller, which made wt for
// this workspace, to remove with it.
func CreateWorkspace(wt billy.Filesystem, m *Mirror, spec WorkspaceSpec, base plumbing.Hash) (_ *Repo, err error) {
	if err := spec.check(); err != nil {
		return nil, err
	}
	exists, err := hasDotGit(wt)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if exists {
		return nil, fmt.Errorf("%w: the directory already has a .git", ErrAlreadyExists)
	}
	ms := m.store()
	if _, err := ms.EncodedObject(plumbing.CommitObject, base); err != nil {
		return nil, fmt.Errorf("%w: the mirror has no commit %s", ErrInvalidRevision, base)
	}
	defer func() {
		if err != nil {
			_ = billyutil.RemoveAll(wt, git.GitDirName)
			_ = m.Unpin(spec.ID)
		}
	}()
	if err := writeGitDir(wt, spec); err != nil {
		return nil, err
	}
	if err := m.Pin(spec.ID, "base", base); err != nil {
		return nil, err
	}
	r, err := OpenWorkspace(wt, m, spec)
	if err != nil {
		return nil, err
	}
	r.SetLimits(spec.Limits)
	branch := plumbing.NewBranchReferenceName(spec.Branch)
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(branch, base)); err != nil {
		return nil, err
	}
	head, err := r.repo.CommitObject(base)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRevision, err)
	}
	if err := r.checkTree(head); err != nil {
		return nil, err
	}
	w, err := r.worktree()
	if err != nil {
		return nil, err
	}
	if err := w.Reset(&git.ResetOptions{Commit: base, Mode: git.MergeReset}); err != nil {
		return nil, fmt.Errorf("checkout: %w", err)
	}
	return r, nil
}

// writeGitDir writes a workspace's git directory: HEAD on its branch, the
// directories git expects, the alternates file for the session's git, and
// a configuration file the session's git reads (the typed ops never do).
func writeGitDir(wt billy.Filesystem, spec WorkspaceSpec) error {
	for _, d := range []string{".git/objects/info", ".git/objects/pack", ".git/refs/heads", ".git/refs/tags"} {
		if err := wt.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	cfg, err := spec.config().Marshal()
	if err != nil {
		return err
	}
	files := map[string]string{
		".git/HEAD":   "ref: " + plumbing.NewBranchReferenceName(spec.Branch).String() + "\n",
		".git/config": string(cfg),
	}
	if spec.AlternatesPath != "" {
		files[".git/objects/info/alternates"] = spec.AlternatesPath + "\n"
	}
	for p, body := range files {
		if err := billyutil.WriteFile(wt, p, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// OpenWorkspace opens a workspace's repository over its mirror for the typed
// ops. Only a .git directory is a repository, as Open; what go-git cannot
// open is refused by name (ErrUnsupportedFeature); the configuration is
// spec's, not the file's; and objects come from the mirror m and the
// workspace's own store, never from an alternates file.
func OpenWorkspace(wt billy.Filesystem, m *Mirror, spec WorkspaceSpec) (*Repo, error) {
	if err := spec.check(); err != nil {
		return nil, err
	}
	fi, err := wt.Lstat(git.GitDirName)
	if err != nil {
		return nil, fmt.Errorf("%w: no .git directory", ErrNotARepository)
	}
	if !fi.IsDir() {
		return nil, &FeatureError{Feature: ".git that is not a directory", Undo: "make .git the workspace's own git directory again"}
	}
	dot, err := wt.Chroot(git.GitDirName)
	if err != nil {
		return nil, fmt.Errorf("open .git: %w", err)
	}
	if err := scanWorkspaceFeatures(dot); err != nil {
		return nil, err
	}
	s := &layeredStorer{Storage: openStore(dot), mirror: m.store(), cfg: &memConfig{c: spec.config()}}
	repo, err := git.Open(s, guardWorktree(wt))
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return nil, fmt.Errorf("%w: %v", ErrNotARepository, err)
		}
		return nil, fmt.Errorf("open: %w", err)
	}
	r := newRepo(wt, repo)
	r.workspace = true
	return r, nil
}

// storerResolve follows symbolic refs to a hash ref, as git does.
func storerResolve(s *filesystem.Storage, name plumbing.ReferenceName) (plumbing.Hash, error) {
	r, err := storer.ResolveReference(s, name)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return r.Hash(), nil
}

// peelToCommit follows tag objects from h to the commit they name, at most
// a few levels deep, as a tag of a tag is.
func peelToCommit(s *filesystem.Storage, h plumbing.Hash) (plumbing.Hash, error) {
	for range 8 {
		o, err := s.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("%w: the mirror has no object %s", ErrInvalidRevision, h)
		}
		switch o.Type() {
		case plumbing.CommitObject:
			return h, nil
		case plumbing.TagObject:
			t, err := object.DecodeTag(s, o)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("%w: %v", ErrInvalidRevision, err)
			}
			h = t.Target
		default:
			return plumbing.ZeroHash, fmt.Errorf("%w: %s names a %s, not a commit", ErrInvalidRevision, h, o.Type())
		}
	}
	return plumbing.ZeroHash, fmt.Errorf("%w: tags nested too deep", ErrInvalidRevision)
}
