package gitrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// osWorktree is a worktree on disk, confined the way the caller's is.
func osWorktree(t *testing.T) (billy.Filesystem, string) {
	t.Helper()
	// The temporary directory can be reached through a link (/var on
	// macOS); a bound filesystem refuses a base that does not resolve to
	// itself.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return osfs.New(dir, osfs.WithBoundOS()), dir
}

// assertNoToken fails when the token, or the basic-auth header value made
// from it, is in any file under dir.
func assertNoToken(t *testing.T, dir string) {
	t.Helper()
	header := base64.StdEncoding.EncodeToString([]byte(testUser + ":" + testToken))
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(testToken)) || bytes.Contains(b, []byte(header)) {
			t.Errorf("the token is on disk in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertNoTokenInError(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), testToken) {
		t.Errorf("the token is in an error: %v", err)
	}
}

func TestClone_AnonymousRecordsTheURLAndChecksOut(t *testing.T) {
	s := newServer(t, false)
	wt, _ := osWorktree(t)
	r, err := Clone(context.Background(), wt, s.network(t, ""), CloneOptions{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if h, _ := r.Head(); h.Branch != "main" || h.Hash != s.head {
		t.Errorf("Head = %+v, want main at %s", h, s.head)
	}
	if read(t, wt, "README.md") != "readme\n" || read(t, wt, "src/a.go") != "package a\n" {
		t.Error("the clone did not check out the tree")
	}
	if u, err := r.RemoteURL("origin"); err != nil || u != s.url {
		t.Errorf("RemoteURL = %q, %v; want %q", u, err, s.url)
	}
	bs, err := r.Branches()
	if err != nil || len(bs) != 1 || bs[0].Upstream != "origin/main" {
		t.Errorf("Branches = %+v, %v", bs, err)
	}
	if st := statusMap(t, r); len(st) != 0 {
		t.Errorf("status after a clone = %v", st)
	}
	// The clone can be opened again.
	if _, err := Open(wt); err != nil {
		t.Errorf("Open after Clone: %v", err)
	}
	if _, err := Clone(context.Background(), wt, s.network(t, ""), CloneOptions{}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("a clone into a repository: %v", err)
	}
}

func TestClone_AuthenticatedLeavesTheTokenOffDisk(t *testing.T) {
	s := newServer(t, true)
	wt, dir := osWorktree(t)
	r, err := Clone(context.Background(), wt, s.network(t, testToken), CloneOptions{Branch: "main", SingleBranch: true})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if h, _ := r.Head(); h.Hash != s.head {
		t.Errorf("Head = %+v", h)
	}
	if !strings.Contains(read(t, wt, ".git/config"), "url = "+s.url+"\n") {
		t.Errorf(".git/config does not record the URL as given:\n%s", read(t, wt, ".git/config"))
	}
	assertNoToken(t, dir)
}

// A failed clone leaves nothing that looks like a repository, and nothing it
// created; what was in the directory before stays.
func TestClone_FailureLeavesNoGitBehind(t *testing.T) {
	s := newServer(t, true)
	wt, dir := osWorktree(t)
	write(t, wt, "mine.txt", "keep\n")

	_, err := Clone(context.Background(), wt, s.network(t, "wrong-token"), CloneOptions{})
	if !errors.Is(err, gitsource.ErrAuthFailed) || Class(err) != "auth_failed" {
		t.Fatalf("a clone with a bad token: %v (class %s), want auth_failed", err, Class(err))
	}
	if exists(wt, ".git") || read(t, wt, "mine.txt") != "keep\n" {
		t.Errorf("after the failed clone: .git exists %v", exists(wt, ".git"))
	}

	_, err = Clone(context.Background(), wt, s.network(t, testToken), CloneOptions{Branch: "no-such-branch"})
	if err == nil || exists(wt, ".git") || exists(wt, "README.md") {
		t.Errorf("a clone of a missing branch: %v; .git exists %v", err, exists(wt, ".git"))
	}
	if Class(err) != "invalid_revision" {
		t.Errorf("a clone of a missing branch has class %s, want invalid_revision (%v)", Class(err), err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries after failed clones, want just mine.txt", len(entries))
	}
}

func TestNetwork_FailingAuthIsAuthFailedAndNeverEchoesTheToken(t *testing.T) {
	s := newServer(t, true)
	n := s.network(t, testToken+"-but-wrong")
	_, err := LsRemote(context.Background(), n)
	if Class(err) != "auth_failed" {
		t.Errorf("LsRemote with a bad token: %v, want auth_failed", err)
	}
	if strings.Contains(err.Error(), n.Token) {
		t.Errorf("the token is in the error: %v", err)
	}
	// A URL for another host than the guard's is refused before any request.
	other := s.network(t, testToken)
	other.URL = "https://elsewhere.example/acme/app.git"
	if _, err := LsRemote(context.Background(), other); !errors.Is(err, gitsource.ErrInvalidRequest) {
		t.Errorf("a URL for another host: %v", err)
	}
	withUser := s.network(t, testToken)
	withUser.URL = strings.Replace(s.url, "https://", "https://u:p@", 1)
	if _, err := LsRemote(context.Background(), withUser); !errors.Is(err, gitsource.ErrInvalidRequest) {
		t.Errorf("a URL with credentials: %v", err)
	}
}

func TestLsRemote_ListsTheServedRefs(t *testing.T) {
	s := newServer(t, false)
	refs, err := LsRemote(context.Background(), s.network(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.Name] = r.Hash
	}
	if got["HEAD"] != s.head || got["refs/heads/main"] != s.head {
		t.Errorf("LsRemote = %v", refs)
	}
}

func TestFetchPull_FastForwardsAndReportsUpToDate(t *testing.T) {
	s := newServer(t, true)
	wt, dir := osWorktree(t)
	ctx := context.Background()
	n := s.network(t, testToken)
	r, err := Clone(ctx, wt, n, CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.Pull(ctx, n, PullOptions{}); err != nil || !res.UpToDate || res.Head != s.head {
		t.Errorf("Pull with nothing new = %+v, %v", res, err)
	}

	next := s.commit(t, "new.txt", "new\n")
	res, err := r.Fetch(ctx, n, FetchOptions{})
	if err != nil || res.UpToDate {
		t.Fatalf("Fetch = %+v, %v", res, err)
	}
	want := RefUpdate{Name: "refs/remotes/origin/main", Old: s.head, New: next}
	if len(res.Updated) != 1 || res.Updated[0] != want {
		t.Errorf("Fetch updated %+v, want [%+v]", res.Updated, want)
	}
	if res, err := r.Fetch(ctx, n, FetchOptions{}); err != nil || !res.UpToDate || len(res.Updated) != 0 {
		t.Errorf("a second Fetch = %+v, %v", res, err)
	}
	if h, _ := r.Head(); h.Hash != s.head {
		t.Error("Fetch moved HEAD")
	}
	pr, err := r.Pull(ctx, n, PullOptions{})
	if err != nil || pr.UpToDate || pr.Head != next {
		t.Fatalf("Pull = %+v, %v; want head %s", pr, err, next)
	}
	if read(t, wt, "new.txt") != "new\n" || len(statusMap(t, r)) != 0 {
		t.Errorf("after the pull: status %v", statusMap(t, r))
	}
	if pr, err := r.Pull(ctx, n, PullOptions{}); err != nil || !pr.UpToDate {
		t.Errorf("a second Pull = %+v, %v", pr, err)
	}

	// A refspec that would move the checked-out branch under the worktree.
	for _, spec := range []string{"+refs/heads/*:refs/heads/*", "refs/heads/main:refs/heads/main"} {
		if _, err := r.Fetch(ctx, n, FetchOptions{RefSpecs: []string{spec}}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Fetch %s: %v, want invalid_argument", spec, err)
		}
	}
	for _, spec := range []string{"main:x", "refs/heads/a*:refs/remotes/b", ":refs/heads/x", "-main"} {
		if _, err := r.Fetch(ctx, n, FetchOptions{RefSpecs: []string{spec}}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Fetch %s: %v, want invalid_argument", spec, err)
		}
	}
	if _, err := r.Fetch(ctx, n, FetchOptions{Remote: "upstream"}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Fetch of a missing remote: %v", err)
	}
	if _, err := r.Fetch(ctx, n, FetchOptions{RefSpecs: []string{"no-such-branch"}}); Class(err) != "invalid_revision" {
		t.Errorf("Fetch of a missing branch: %v", err)
	}
	assertNoToken(t, dir)
}

func TestPull_RefusesNonFastForwardAndLocalChanges(t *testing.T) {
	s := newServer(t, false)
	ctx := context.Background()
	n := s.network(t, "")
	c := newClock()

	// Diverged: a local commit and a remote one.
	wt, _ := osWorktree(t)
	r, err := Clone(ctx, wt, n, CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	write(t, wt, "local.txt", "l\n")
	local := commitAll(t, r, c, "local")
	s.commit(t, "remote.txt", "r\n")
	if _, err := r.Pull(ctx, n, PullOptions{}); !errors.Is(err, ErrNotFastForward) {
		t.Errorf("a diverged pull: %v, want non_fast_forward", err)
	}
	if h, _ := r.Head(); h.Hash != local || exists(wt, "remote.txt") {
		t.Errorf("a refused pull changed the branch: %+v", h)
	}

	// A local change the pull would overwrite.
	wt2, _ := osWorktree(t)
	r2, err := Clone(ctx, wt2, n, CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := r2.Head()
	s.commit(t, "README.md", "remote edit\n")
	write(t, wt2, "README.md", "my edit\n")
	if _, err := r2.Pull(ctx, n, PullOptions{}); !errors.Is(err, ErrWorktreeDirty) {
		t.Errorf("a pull over a local change: %v, want worktree_dirty", err)
	}
	if h, _ := r2.Head(); h.Hash != before.Hash || read(t, wt2, "README.md") != "my edit\n" {
		t.Errorf("a refused pull lost the local change or moved HEAD: %+v", h)
	}
}

func TestPush_UpdatesRejectsNonFastForwardAndForces(t *testing.T) {
	s := newServer(t, true)
	ctx := context.Background()
	n := s.network(t, testToken)
	c := newClock()
	wtA, dirA := osWorktree(t)
	a, err := Clone(ctx, wtA, n, CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wtB, dirB := osWorktree(t)
	b, err := Clone(ctx, wtB, n, CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}

	write(t, wtA, "a.txt", "from a\n")
	aHead := commitAll(t, a, c, "a's work")
	res, err := a.Push(ctx, n, PushOptions{})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := RefUpdate{Name: "refs/heads/main", Old: s.head, New: aHead}
	if res.UpToDate || len(res.Updated) != 1 || res.Updated[0] != want {
		t.Errorf("Push = %+v, want [%+v]", res, want)
	}
	if got := run(t, s.bare, "rev-parse", "main"); got != aHead {
		t.Errorf("the server's main = %s, want %s", got, aHead)
	}
	if res, err := a.Push(ctx, n, PushOptions{}); err != nil || !res.UpToDate {
		t.Errorf("a second Push = %+v, %v", res, err)
	}

	// B is behind and has its own commit: refused, and refused again with a
	// lease on what B last saw.
	write(t, wtB, "b.txt", "from b\n")
	bHead := commitAll(t, b, c, "b's work")
	_, err = b.Push(ctx, n, PushOptions{})
	if !errors.Is(err, ErrPushRejected) || Class(err) != "push_rejected" {
		t.Fatalf("a non-fast-forward push: %v, want push_rejected", err)
	}
	assertNoTokenInError(t, err)
	if _, err := b.Push(ctx, n, PushOptions{ForceWithLease: true}); !errors.Is(err, ErrPushRejected) {
		t.Errorf("a push whose lease is stale: %v, want push_rejected", err)
	}
	if got := run(t, s.bare, "rev-parse", "main"); got != aHead {
		t.Fatalf("a rejected push moved the server's main to %s", got)
	}
	// Forced, it goes.
	res, err = b.Push(ctx, n, PushOptions{Force: true})
	if err != nil || len(res.Updated) != 1 || res.Updated[0] != (RefUpdate{Name: "refs/heads/main", Old: aHead, New: bHead}) {
		t.Fatalf("a forced push = %+v, %v", res, err)
	}
	if got := run(t, s.bare, "rev-parse", "main"); got != bHead {
		t.Errorf("the server's main = %s, want %s", got, bHead)
	}

	// A new branch, by name, and its deletion.
	if err := b.BranchCreate("topic", ""); err != nil {
		t.Fatal(err)
	}
	res, err = b.Push(ctx, n, PushOptions{RefSpecs: []string{"topic"}})
	if err != nil || len(res.Updated) != 1 || res.Updated[0] != (RefUpdate{Name: "refs/heads/topic", New: bHead}) {
		t.Errorf("pushing a new branch = %+v, %v", res, err)
	}
	res, err = b.Push(ctx, n, PushOptions{RefSpecs: []string{":refs/heads/topic"}})
	if err != nil || len(res.Updated) != 1 || res.Updated[0] != (RefUpdate{Name: "refs/heads/topic", Old: bHead}) {
		t.Errorf("deleting a branch = %+v, %v", res, err)
	}
	if _, err := b.Push(ctx, n, PushOptions{Force: true, ForceWithLease: true}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("force with force-with-lease: %v", err)
	}
	assertNoToken(t, dirA)
	assertNoToken(t, dirB)
}

// A pack is compressed, so the bytes fetched say nothing of what they
// become; an object over the limit fails the operation as too large.
func TestFetch_AnObjectOverTheLimitIsTooLarge(t *testing.T) {
	s := newServer(t, false)
	ctx := context.Background()
	n := s.network(t, "")
	wt, _ := osWorktree(t)
	r, err := Clone(ctx, wt, n, CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.commit(t, "zeros.bin", string(make([]byte, 4<<20)))

	small := n
	small.MaxObjectBytes = 1 << 20
	_, err = r.Fetch(ctx, small, FetchOptions{})
	if !errors.Is(err, gitsource.ErrTooLarge) || Class(err) != "source_too_large" {
		t.Errorf("a fetch of a 4 MiB object with a 1 MiB limit: %v (class %s)", err, Class(err))
	}
	if _, err := r.Pull(ctx, small, PullOptions{}); Class(err) != "source_too_large" {
		t.Errorf("a pull of it: %v", err)
	}
	wt2, _ := osWorktree(t)
	if _, err := Clone(ctx, wt2, small, CloneOptions{}); Class(err) != "source_too_large" || exists(wt2, ".git") {
		t.Errorf("a clone of it: %v; .git exists %v", err, exists(wt2, ".git"))
	}
	// With room, the same fetch goes.
	roomy := n
	roomy.MaxObjectBytes = 8 << 20
	if _, err := r.Fetch(ctx, roomy, FetchOptions{}); err != nil {
		t.Errorf("a fetch with room: %v", err)
	}
	// Without a limit or a spool directory nothing is dialled.
	none := n
	none.MaxObjectBytes = 0
	if _, err := r.Fetch(ctx, none, FetchOptions{}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a fetch without an object limit: %v", err)
	}
}
