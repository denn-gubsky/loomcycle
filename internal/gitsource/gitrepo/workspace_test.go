package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// The workspace tests read repositories an attacker controls twice over: the
// forge's content (the tree, its refs, its packs) and the workspace's own
// .git, which any command in the session can write. Nothing either holds may
// run a program, reach another store, or carry a write out of the tree.

// realTemp is a fresh directory reached by no link (macOS's /var is one).
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// bound is a filesystem confined to dir, as the worker's are.
func bound(dir string) billy.Filesystem { return osfs.New(dir, osfs.WithBoundOS()) }

// fixture is a served repository, a mirror of it fetched once, and the
// directories a workspace and anything "outside" live in.
type fixture struct {
	srv     *server
	mirror  *Mirror
	mdir    string
	outside string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{srv: newServer(t, false), mdir: realTemp(t), outside: realTemp(t)}
	m, err := OpenMirror(bound(f.mdir))
	if err != nil {
		t.Fatal(err)
	}
	f.mirror = m
	f.fetch(t)
	return f
}

func (f *fixture) fetch(t *testing.T) FetchResult {
	t.Helper()
	res, err := f.mirror.Fetch(context.Background(), f.srv.network(t, ""))
	if err != nil {
		t.Fatalf("mirror fetch: %v", err)
	}
	return res
}

func (f *fixture) spec(id string) WorkspaceSpec {
	return WorkspaceSpec{ID: id, URL: f.srv.url, Branch: "loom/" + id, AlternatesPath: filepath.Join(f.mdir, "objects")}
}

// workspace creates workspace id at base ("" for the forge's main) and
// returns it with its directory.
func (f *fixture) workspace(t *testing.T, id, base string) (*Repo, string) {
	t.Helper()
	if base == "" {
		base = "refs/heads/main"
	}
	h, err := f.mirror.Resolve(base)
	if err != nil {
		t.Fatalf("resolve %s: %v", base, err)
	}
	dir := realTemp(t)
	r, err := CreateWorkspace(bound(dir), f.mirror, f.spec(id), h)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return r, dir
}

// served runs git in the served repository's working copy and pushes main.
func (f *fixture) servedCommit(t *testing.T, files map[string]string, msg string) string {
	t.Helper()
	for p, c := range files {
		writeOS(t, f.srv.work, p, c)
	}
	run(t, f.srv.work, "add", "-A")
	run(t, f.srv.work, "commit", "-q", "-m", msg)
	run(t, f.srv.work, "push", "-q", "served", "HEAD:refs/heads/main")
	return run(t, f.srv.work, "rev-parse", "HEAD")
}

func wsCommit(t *testing.T, r *Repo, msg string) string {
	t.Helper()
	s := newClock().sig()
	h, err := r.Commit(CommitOptions{Message: msg, Author: s, Committer: s, All: true})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return h
}

func TestWorkspace_CreateChecksOutTheBaseOnItsOwnBranchAndPinsIt(t *testing.T) {
	f := newFixture(t)
	r, dir := f.workspace(t, "ws1", "")
	if got := readFile(t, filepath.Join(dir, "README.md")); got != "readme\n" {
		t.Fatalf("README.md = %q", got)
	}
	head, err := r.Head()
	if err != nil || head.Branch != "loom/ws1" || head.Hash != f.srv.head {
		t.Fatalf("head = %+v, %v; want loom/ws1 at %s", head, err, f.srv.head)
	}
	pinned, err := f.mirror.Pinned()
	if err != nil || len(pinned) != 1 || pinned[0] != "ws1" {
		t.Fatalf("pinned = %v, %v", pinned, err)
	}
	// The workspace's own store holds no object of the forge's: it reads
	// them from the mirror.
	ents, _ := os.ReadDir(filepath.Join(dir, ".git", "objects", "pack"))
	if len(ents) != 0 {
		t.Fatalf("the workspace copied packs: %v", ents)
	}
}

// Two workspaces of one repository change one file; neither sees the
// other's change, and the mirror gains no object of either (RFC GW §11
// "Isolation").
func TestWorkspace_TwoWorkspacesDoNotSeeEachOther(t *testing.T) {
	f := newFixture(t)
	r1, d1 := f.workspace(t, "ws1", "")
	r2, d2 := f.workspace(t, "ws2", "")
	writeOS(t, d1, "README.md", "one\n")
	writeOS(t, d2, "README.md", "two\n")
	c1, c2 := wsCommit(t, r1, "one"), wsCommit(t, r2, "two")
	if got := readFile(t, filepath.Join(d2, "README.md")); got != "two\n" {
		t.Fatalf("ws2 sees %q", got)
	}
	if _, err := r2.repo.CommitObject(plumbing.NewHash(c1)); err == nil {
		t.Fatal("ws2 reads ws1's commit")
	}
	for _, c := range []string{c1, c2} {
		if err := f.mirror.store().HasEncodedObject(plumbing.NewHash(c)); err == nil {
			t.Fatalf("a workspace's commit %s is in the mirror", c)
		}
	}
}

// The typed ops' configuration is the row's: what a session writes into
// .git/config (a remote, an insteadOf rewrite, a pushurl) is not read.
func TestWorkspace_ConfigurationIsTheRowsNotTheFile(t *testing.T) {
	f := newFixture(t)
	_, dir := f.workspace(t, "ws1", "")
	writeOS(t, dir, ".git/config", `[core]
	repositoryformatversion = 0
	bare = false
[remote "origin"]
	url = https://evil.example/x.git
	pushurl = https://evil.example/push.git
[url "https://evil.example/"]
	insteadOf = `+f.srv.url+`
`)
	r, err := OpenWorkspace(bound(dir), f.mirror, f.spec("ws1"))
	if err != nil {
		t.Fatal(err)
	}
	if u, err := r.RemoteURL("origin"); err != nil || u != f.srv.url {
		t.Fatalf("origin = %q, %v; want the row's %q", u, err, f.srv.url)
	}
	cfg, err := r.repo.Config()
	if err != nil || len(cfg.URLs) != 0 || len(cfg.Remotes["origin"].URLs) != 1 {
		t.Fatalf("the file's config was read: %+v %v", cfg, err)
	}
}

// What a workspace's session sets up to run programs (hooks, a hooks path,
// filter drivers named by the tree's .gitattributes) runs nothing when the
// typed ops check out and commit, and no filter changes a file's bytes.
func TestWorkspace_RunsNoHookAndNoFilter(t *testing.T) {
	f := newFixture(t)
	f.servedCommit(t, map[string]string{".gitattributes": "* filter=evil\n", "data.txt": "plain bytes\n"}, "attributes")
	f.fetch(t)
	marker := filepath.Join(f.outside, "ran")
	script := "#!/bin/sh\necho ran >> '" + marker + "'\n"
	hooks := filepath.Join(f.outside, "hooks")
	for _, d := range []string{hooks} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r, dir := f.workspace(t, "ws1", "")
	for _, h := range []string{"pre-commit", "post-commit", "post-checkout", "commit-msg", "reference-transaction"} {
		for _, d := range []string{hooks, filepath.Join(dir, ".git", "hooks")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, h), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeOS(t, dir, ".git/config", `[core]
	repositoryformatversion = 0
	hooksPath = `+hooks+`
	fsmonitor = `+filepath.Join(hooks, "post-commit")+`
[filter "evil"]
	smudge = sh -c 'echo ran >> `+marker+`; echo SMUDGED'
	clean = sh -c 'echo ran >> `+marker+`; echo CLEANED'
	required = true
`)
	r, err := OpenWorkspace(bound(dir), f.mirror, f.spec("ws1"))
	if err != nil {
		t.Fatal(err)
	}
	writeOS(t, dir, "data.txt", "changed bytes\n")
	c := wsCommit(t, r, "change")
	if err := r.Checkout(CheckoutOptions{Branch: "side", Create: true, Rev: f.srv.head}); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := r.Checkout(CheckoutOptions{Branch: "loom/ws1"}); err != nil {
		t.Fatalf("Checkout back: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook or a filter ran")
	}
	if got := readFile(t, filepath.Join(dir, "data.txt")); got != "changed bytes\n" {
		t.Fatalf("a filter changed the file: %q", got)
	}
	blob := run(t, dir, "--git-dir=.git", "cat-file", "-p", c+":data.txt")
	if blob != "changed bytes" {
		t.Fatalf("the commit stored %q", blob)
	}
}

// The alternates file names the mirror for the session's git only. The
// typed ops never read it, so a session that points it at another store
// (another tenant's mirror) reads nothing from there.
func TestWorkspace_IgnoresTheAlternatesFile(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	secret := other.servedCommit(t, map[string]string{"secret.txt": "another tenant's\n"}, "secret")
	other.fetch(t)
	_, dir := f.workspace(t, "ws1", "")
	writeOS(t, dir, ".git/objects/info/alternates", filepath.Join(other.mdir, "objects")+"\n")
	// An unconfined filesystem, so only the store's own refusal stands
	// between the file and the other mirror.
	r, err := OpenWorkspace(osfs.New(dir), f.mirror, f.spec("ws1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.repo.CommitObject(plumbing.NewHash(secret)); err == nil {
		t.Fatal("the workspace read another mirror's commit through its alternates file")
	}
}

// The same with the other store copied inside the workspace's own .git,
// where no filesystem confinement stops the path: only the store's refusal
// to read an alternates file does (a fixture per guard, so the two cannot
// mask each other).
func TestWorkspace_IgnoresAnAlternatesFileInsideItsGitDir(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	secret := other.servedCommit(t, map[string]string{"secret.txt": "another tenant's\n"}, "secret")
	other.fetch(t)
	_, dir := f.workspace(t, "ws1", "")
	run(t, dir, "--git-dir=.git", "init", "-q", "--bare", "smuggled")
	if err := os.RemoveAll(filepath.Join(dir, ".git", "smuggled", "objects")); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(dir, ".git", "smuggled", "objects"), os.DirFS(filepath.Join(other.mdir, "objects"))); err != nil {
		t.Fatal(err)
	}
	writeOS(t, dir, ".git/objects/info/alternates", "/smuggled/objects\n")
	r, err := OpenWorkspace(bound(dir), f.mirror, f.spec("ws1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.repo.CommitObject(plumbing.NewHash(secret)); err == nil {
		t.Fatal("the store followed the workspace's alternates file")
	}
}

// What go-git cannot open is refused by name, with the command that undoes
// it; nothing of the file is applied.
func TestOpenWorkspace_RefusesWhatGoGitCannotOpenByName(t *testing.T) {
	cases := []struct {
		name, feature string
		set           func(t *testing.T, dir string)
	}{
		{"sparse in config", "sparse checkout", cfg("[core]\n\tsparseCheckout = true\n")},
		{"sparse, bare key", "sparse checkout", cfg("[core]\n\tsparseCheckout\n")},
		{"sparse file", "sparse checkout", file(".git/info/sparse-checkout", "/src/\n")},
		{"extensions", "extensions.objectformat", cfg("[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = sha256\n")},
		{"worktree config", "extensions.worktreeconfig", cfg("[extensions]\n\tworktreeConfig = true\n")},
		{"format version", "core.repositoryformatversion 2", cfg("[core]\n\trepositoryformatversion = 2\n")},
		{"include", "an included config file", cfg("[include]\n\tpath = /tmp/elsewhere\n")},
		{"includeIf", "an included config file", cfg("[includeIf \"gitdir:/\"]\n\tpath = /tmp/elsewhere\n")},
		{"split index config", "a split index", cfg("[core]\n\tsplitIndex = true\n")},
		{"split index file", "a split index", file(".git/sharedindex.0123456789abcdef0123456789abcdef01234567", "x")},
		{"reftable", "the reftable ref store", mkdir(".git/reftable")},
		{"commondir", "a linked worktree", file(".git/commondir", "../other/.git\n")},
		{"core.worktree", "core.worktree", cfg("[core]\n\tworktree = /\n")},
		{"unreadable config", ".git/config that cannot be read", cfg("[core\n")},
		{"objects link", ".git/objects is a symbolic link", func(t *testing.T, dir string) {
			p := filepath.Join(dir, ".git", "objects")
			if err := os.Rename(p, p+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(p+".real", p); err != nil {
				t.Fatal(err)
			}
		}},
		{".git file", ".git that is not a directory", func(t *testing.T, dir string) {
			if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
			writeOS(t, dir, ".git", "gitdir: /elsewhere\n")
		}},
	}
	f := newFixture(t)
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := "f" + string(rune('a'+i))
			_, dir := f.workspace(t, id, "")
			c.set(t, dir)
			_, err := OpenWorkspace(bound(dir), f.mirror, f.spec(id))
			var fe *FeatureError
			if !errors.As(err, &fe) || !strings.Contains(strings.ToLower(fe.Feature), strings.ToLower(c.feature)) || fe.Undo == "" {
				t.Fatalf("err = %v, want the feature %q named with an undo", err, c.feature)
			}
			if Class(err) != "unsupported_repository_feature" {
				t.Fatalf("class = %q", Class(err))
			}
		})
	}
}

func cfg(body string) func(*testing.T, string) {
	return func(t *testing.T, dir string) { writeOS(t, dir, ".git/config", body) }
}

func file(rel, body string) func(*testing.T, string) {
	return func(t *testing.T, dir string) { writeOS(t, dir, rel, body) }
}

func mkdir(rel string) func(*testing.T, string) {
	return func(t *testing.T, dir string) {
		if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// RFC GW §11 "Repository features": after `git sparse-checkout set` in the
// session the typed ops refuse and name it; `git status` still works.
func TestOpenWorkspace_RefusesWhatTheSessionsGitTurnedOn(t *testing.T) {
	f := newFixture(t)
	_, dir := f.workspace(t, "ws1", "")
	run(t, dir, "sparse-checkout", "set", "src")
	_, err := OpenWorkspace(bound(dir), f.mirror, f.spec("ws1"))
	if !errors.Is(err, ErrUnsupportedFeature) || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("err = %v, want unsupported sparse checkout", err)
	}
	run(t, dir, "status", "--short")
}

// A tree that makes a path a link out of the workspace in one commit and a
// directory with a file under it in the next never carries the file out.
func TestWorkspace_ACheckoutNeverWritesThroughALinkOutOfTheTree(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink(f.outside, filepath.Join(f.srv.work, "d")); err != nil {
		t.Fatal(err)
	}
	withLink := f.servedCommit(t, nil, "a link out")
	if err := os.Remove(filepath.Join(f.srv.work, "d")); err != nil {
		t.Fatal(err)
	}
	withDir := f.servedCommit(t, map[string]string{"d/planted": "planted\n"}, "a directory")
	f.fetch(t)
	r, dir := f.workspace(t, "ws1", withLink)
	if fi, err := os.Lstat(filepath.Join(dir, "d")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("d is not a link: %v %v", fi, err)
	}
	_ = r.Checkout(CheckoutOptions{Rev: withDir, Force: true})
	if _, err := os.Stat(filepath.Join(f.outside, "planted")); err == nil {
		t.Fatal("the checkout wrote through the link out of the tree")
	}
}

// A submodule the tree names is not followed: nothing is fetched for it,
// the typed submodule ops are refused, and its directory stays empty.
func TestWorkspace_DoesNotFollowSubmodules(t *testing.T) {
	f := newFixture(t)
	writeOS(t, f.srv.work, ".gitmodules", "[submodule \"sub\"]\n\tpath = sub\n\turl = https://evil.example/sub.git\n")
	run(t, f.srv.work, "update-index", "--add", "--cacheinfo", "160000,"+f.srv.first+",sub")
	f.servedCommit(t, nil, "a submodule")
	f.fetch(t)
	r, dir := f.workspace(t, "ws1", "")
	if ents, err := os.ReadDir(filepath.Join(dir, "sub")); err == nil && len(ents) > 0 {
		t.Fatalf("the submodule was filled: %v", ents)
	}
	if _, err := r.SubmoduleUpdate(context.Background(), "sub", f.srv.network(t, "")); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("SubmoduleUpdate = %v, want refused", err)
	}
	if _, err := r.Fetch(context.Background(), f.srv.network(t, ""), FetchOptions{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Fetch = %v, want refused: a workspace fetches through its mirror", err)
	}
}

// A pack holding an object over the limit is refused before it is stored:
// the mirror keeps no ref and no pack of it.
func TestMirror_RefusesAPackOverTheObjectLimit(t *testing.T) {
	srv := newServer(t, false)
	writeOS(t, srv.work, "big.bin", strings.Repeat("x", 2<<20))
	run(t, srv.work, "add", "big.bin")
	run(t, srv.work, "commit", "-q", "-m", "big")
	run(t, srv.work, "push", "-q", "served", "HEAD:refs/heads/main")
	mdir := realTemp(t)
	m, err := OpenMirror(bound(mdir))
	if err != nil {
		t.Fatal(err)
	}
	n := srv.network(t, "")
	n.MaxObjectBytes = 1 << 20
	_, err = m.Fetch(context.Background(), n)
	if !errors.Is(err, gitsource.ErrTooLarge) {
		t.Fatalf("err = %v, want too large", err)
	}
	refs, _ := refsOf(m.store())
	if len(refs) != 0 {
		t.Fatalf("the refused fetch left refs: %v", refs)
	}
	packs, _ := filepath.Glob(filepath.Join(mdir, "objects", "pack", "*.pack"))
	if len(packs) != 0 {
		t.Fatalf("the refused pack was stored: %v", packs)
	}
}

// A fetch whose bytes pass the read limit is refused too.
func TestMirror_RefusesAFetchOverTheReadLimit(t *testing.T) {
	srv := newServer(t, false)
	m, err := OpenMirror(bound(realTemp(t)))
	if err != nil {
		t.Fatal(err)
	}
	n := srv.network(t, "")
	n.Guard.MaxReadBytes = 64
	if _, err := m.Fetch(context.Background(), n); !errors.Is(err, gitsource.ErrTooLarge) {
		t.Fatalf("err = %v, want too large", err)
	}
}

// The forge's refs outside its heads and tags are not taken, so a forge ref
// named like a pin (or anything else) cannot overwrite or forge one; a
// pruning fetch removes deleted heads and never a pin.
func TestMirror_TakesOnlyHeadsAndTagsAndNeverTouchesAPin(t *testing.T) {
	f := newFixture(t)
	_, _ = f.workspace(t, "ws1", "")
	pin := plumbing.ReferenceName(PinPrefix + "ws1/base")
	before, err := f.mirror.store().Reference(pin)
	if err != nil {
		t.Fatal(err)
	}
	other := f.servedCommit(t, map[string]string{"x": "x\n"}, "elsewhere")
	for _, ref := range []string{"HEAD", PinPrefix + "ws1/base", PinPrefix + "evil/base", "refs/pull/1/head", "refs/notes/commits", "refs/remotes/origin/main"} {
		run(t, f.srv.bare, "update-ref", ref, f.srv.first)
	}
	run(t, f.srv.bare, "update-ref", "refs/heads/gone", other)
	f.fetch(t)
	run(t, f.srv.bare, "update-ref", "-d", "refs/heads/gone")
	f.fetch(t)
	after, err := f.mirror.store().Reference(pin)
	if err != nil || after.Hash() != before.Hash() {
		t.Fatalf("the pin moved: %v -> %v (%v)", before, after, err)
	}
	it, err := f.mirror.store().IterReferences()
	if err != nil {
		t.Fatal(err)
	}
	_ = it.ForEach(func(r *plumbing.Reference) error {
		n := r.Name().String()
		switch {
		case n == "HEAD", strings.HasPrefix(n, "refs/heads/"), strings.HasPrefix(n, "refs/tags/"), n == pin.String():
		default:
			t.Errorf("the mirror took %s", n)
		}
		if n == "refs/heads/gone" {
			t.Error("a deleted branch was not pruned")
		}
		return nil
	})
	if ids, _ := f.mirror.Pinned(); len(ids) != 1 || ids[0] != "ws1" {
		t.Fatalf("pinned = %v", ids)
	}
}

// RFC GW §11 "Mirror retention" (spike A): a branch deleted upstream, a
// pruning fetch and a repack that drops unreachable objects: a workspace
// based on that branch still opens, reads its history and commits. The
// control without the pin loses the commit.
func TestMirror_APinKeepsTheBaseAfterPruneAndRepack(t *testing.T) {
	f := newFixture(t)
	run(t, f.srv.work, "checkout", "-q", "-b", "feature")
	writeOS(t, f.srv.work, "feature.txt", "feature\n")
	run(t, f.srv.work, "add", "feature.txt")
	run(t, f.srv.work, "commit", "-q", "-m", "feature")
	feature := run(t, f.srv.work, "rev-parse", "HEAD")
	run(t, f.srv.work, "push", "-q", "served", "feature")
	f.fetch(t)
	_, dir := f.workspace(t, "ws1", "refs/heads/feature")
	run(t, f.srv.bare, "update-ref", "-d", "refs/heads/feature")
	f.fetch(t)
	repack := func() {
		run(t, f.mdir, "--git-dir=.", "repack", "-a", "-d", "-q")
		run(t, f.mdir, "--git-dir=.", "prune", "--expire=now")
	}
	repack()
	r, err := OpenWorkspace(bound(dir), f.mirror, f.spec("ws1"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := r.Log(LogOptions{Max: 10})
	if err != nil || len(log) < 3 || log[0].Hash != feature {
		t.Fatalf("log = %v, %v", log, err)
	}
	writeOS(t, dir, "feature.txt", "more\n")
	wsCommit(t, r, "more")

	// The control: without the pin the commit is gone after a repack.
	if err := f.mirror.Unpin("ws1"); err != nil {
		t.Fatal(err)
	}
	repack()
	if err := f.mirror.store().HasEncodedObject(plumbing.NewHash(feature)); err == nil {
		t.Fatal("the control kept the commit without a pin: the test proves nothing")
	}
}

// A commit made by the session's own git, through the alternates file, is
// read by the typed ops and pushed with go-git (spike A), exactly its new
// objects going to the forge.
func TestWorkspace_ACommitBySessionGitIsPushedByTheTypedOps(t *testing.T) {
	f := newFixture(t)
	_, dir := f.workspace(t, "ws1", "")
	writeOS(t, dir, "session.txt", "from the shell\n")
	run(t, dir, "add", "session.txt")
	run(t, dir, "commit", "-q", "-m", "from the shell")
	c := run(t, dir, "rev-parse", "HEAD")
	r, err := OpenWorkspace(bound(dir), f.mirror, f.spec("ws1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Push(context.Background(), f.srv.network(t, ""), PushOptions{RefSpecs: []string{"refs/heads/loom/ws1:refs/heads/loom/ws1"}}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := run(t, f.srv.bare, "rev-parse", "refs/heads/loom/ws1"); got != c {
		t.Fatalf("the forge has %s, want %s", got, c)
	}
}

func TestWorkspaceSpec_RefusesWhatCouldNameAnotherPlace(t *testing.T) {
	f := newFixture(t)
	h, _ := f.mirror.Resolve("refs/heads/main")
	good := f.spec("ws1")
	for name, mod := range map[string]func(*WorkspaceSpec){
		"id with a slash":       func(s *WorkspaceSpec) { s.ID = "a/b" },
		"id with dots":          func(s *WorkspaceSpec) { s.ID = ".." },
		"empty id":              func(s *WorkspaceSpec) { s.ID = "" },
		"branch climbing":       func(s *WorkspaceSpec) { s.Branch = "../x" },
		"branch with ..":        func(s *WorkspaceSpec) { s.Branch = "a..b" },
		"branch option":         func(s *WorkspaceSpec) { s.Branch = "-x" },
		"http url":              func(s *WorkspaceSpec) { s.URL = "http://forge.example/x.git" },
		"url with user":         func(s *WorkspaceSpec) { s.URL = "https://u:p@forge.example/x.git" },
		"relative alternates":   func(s *WorkspaceSpec) { s.AlternatesPath = "mirror/objects" },
		"alternates on 2 lines": func(s *WorkspaceSpec) { s.AlternatesPath = "/m/objects\n/other/objects" },
	} {
		t.Run(name, func(t *testing.T) {
			s := good
			mod(&s)
			dir := realTemp(t)
			if _, err := CreateWorkspace(bound(dir), f.mirror, s, h); err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				t.Fatal("a refused workspace left a .git")
			}
		})
	}
	// Every ref named here exists in the mirror, so a refusal is the
	// rule's and not a ref's absence.
	_, _ = f.workspace(t, "pinned", "")
	run(t, f.mdir, "--git-dir=.", "update-ref", "refs/remotes/origin/main", h.String())
	run(t, f.mdir, "--git-dir=.", "symbolic-ref", "HEAD", "refs/heads/main")
	for _, ref := range []string{"HEAD", PinPrefix + "pinned/base", "refs/remotes/origin/main", "refs/heads/../../x", "main", "refs/heads/a..b"} {
		if _, err := f.mirror.Resolve(ref); err == nil {
			t.Errorf("Resolve(%q) accepted", ref)
		}
	}
	if err := f.mirror.Pin("ws1", "../x", h); err == nil {
		t.Error("a pin name climbing out was accepted")
	}
	if pinned, _ := f.mirror.Pinned(); len(pinned) != 1 || pinned[0] != "pinned" {
		t.Fatalf("a refused workspace left a pin: %v", pinned)
	}
}

func TestMirrorKey_IsPerTenantAndURL(t *testing.T) {
	a, b, c := MirrorKey("t1", "https://f/x.git"), MirrorKey("t2", "https://f/x.git"), MirrorKey("t1", "https://f/y.git")
	if a == b || a == c || !ValidMirrorKey(a) {
		t.Fatalf("%s %s %s", a, b, c)
	}
	for _, k := range []string{"", "../x", strings.Repeat("A", 32), a + "0"} {
		if ValidMirrorKey(k) {
			t.Errorf("ValidMirrorKey(%q)", k)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
