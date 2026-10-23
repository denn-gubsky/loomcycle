package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// libRepo is a second repository served next to s's: acme/lib.git. It
// returns the working copy that pushes to it and its first commit.
func libRepo(t *testing.T, s *server) (work, first string) {
	t.Helper()
	base := filepath.Dir(filepath.Dir(s.bare)) // the served root
	work = filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, work, "init", "-q", "-b", "main")
	writeOS(t, work, "lib.txt", "lib one\n")
	run(t, work, "add", ".")
	run(t, work, "commit", "-q", "-m", "lib one")
	bare := filepath.Join(base, "acme", "lib.git")
	run(t, base, "clone", "-q", "--bare", work, bare)
	run(t, work, "remote", "add", "served", bare)
	return work, run(t, work, "rev-parse", "HEAD")
}

// libCommit commits content to lib.txt and pushes it to the served lib.
func libCommit(t *testing.T, work, content string) string {
	t.Helper()
	writeOS(t, work, "lib.txt", content)
	run(t, work, "commit", "-q", "-am", content)
	run(t, work, "push", "-q", "served", "HEAD:refs/heads/main")
	return run(t, work, "rev-parse", "HEAD")
}

// setGitlinks records lib at commit in s's superproject, with .gitmodules
// naming it by the relative URL and a second module by an absolute one,
// and pushes it.
func setGitlinks(t *testing.T, s *server, commit, absURL string) {
	t.Helper()
	writeOS(t, s.work, ".gitmodules", "[submodule \"lib\"]\n\tpath = lib\n\turl = ../lib.git\n"+
		"[submodule \"abs\"]\n\tpath = vendor/abs\n\turl = "+absURL+"\n")
	run(t, s.work, "add", ".gitmodules")
	run(t, s.work, "update-index", "--add", "--cacheinfo", "160000,"+commit+",lib")
	run(t, s.work, "update-index", "--add", "--cacheinfo", "160000,"+commit+",vendor/abs")
	run(t, s.work, "commit", "-q", "-m", "lib at "+commit[:8])
	run(t, s.work, "push", "-q", "served", "HEAD:refs/heads/main")
}

func TestSubmoduleUpdate_ClonesFetchesAndChecksOutTheRecordedCommit(t *testing.T) {
	s := newServer(t, true)
	libWork, lib1 := libRepo(t, s)
	libURL := strings.TrimSuffix(s.url, "/app.git") + "/lib.git"
	setGitlinks(t, s, lib1, libURL)

	wt, dir := osWorktree(t)
	ctx := context.Background()
	r, err := Clone(ctx, wt, s.network(t, testToken), CloneOptions{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	subs, err := r.Submodules()
	if err != nil {
		t.Fatal(err)
	}
	want := []SubmoduleInfo{
		{Name: "abs", Path: "vendor/abs", URL: libURL, Commit: lib1},
		{Name: "lib", Path: "lib", URL: libURL, Commit: lib1},
	}
	if len(subs) != 2 || subs[0] != want[0] || subs[1] != want[1] {
		t.Fatalf("Submodules = %+v, want %+v", subs, want)
	}
	libNet := func() Network {
		n := s.network(t, testToken)
		n.URL = subs[1].URL
		return n
	}

	// An empty path: cloned, then checked out at the recorded commit.
	info, err := r.SubmoduleUpdate(ctx, "lib", libNet())
	if err != nil {
		t.Fatalf("SubmoduleUpdate: %v", err)
	}
	if info != (SubmoduleInfo{Name: "lib", Path: "lib", URL: libURL, Commit: lib1, Initialized: true}) {
		t.Errorf("SubmoduleUpdate = %+v", info)
	}
	if read(t, wt, "lib/lib.txt") != "lib one\n" {
		t.Errorf("lib/lib.txt = %q", read(t, wt, "lib/lib.txt"))
	}
	sub, err := r.openSubmodule("lib")
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := sub.Head(); !h.Detached || h.Hash != lib1 {
		t.Errorf("the submodule's HEAD = %+v, want detached at %s", h, lib1)
	}
	if u, _ := sub.RemoteURL("origin"); u != libURL {
		t.Errorf("the submodule's origin = %q", u)
	}
	// The superproject sees the submodule as it records it, and nothing
	// inside it as its own.
	if st := statusMap(t, r); len(st) != 0 {
		t.Errorf("the superproject's status = %v", st)
	}
	if subs, _ := r.Submodules(); !subs[1].Initialized || subs[0].Initialized {
		t.Errorf("Submodules after the update = %+v", subs)
	}

	// The superproject moves the gitlink to a commit the submodule does not
	// have; the server will not serve a commit by sha, so the update falls
	// back to fetching the branches.
	lib2 := libCommit(t, libWork, "lib two\n")
	setGitlinks(t, s, lib2, libURL)
	if _, err := r.Pull(ctx, s.network(t, testToken), PullOptions{}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if info, err = r.SubmoduleUpdate(ctx, "lib", libNet()); err != nil {
		t.Fatalf("the second SubmoduleUpdate: %v", err)
	}
	if info.Commit != lib2 || !info.Initialized || read(t, wt, "lib/lib.txt") != "lib two\n" {
		t.Errorf("after the second update: %+v, lib.txt %q", info, read(t, wt, "lib/lib.txt"))
	}

	// Now the server serves reachable commits by sha: only that commit is
	// fetched, and the branch the fallback fetched stays where it was.
	run(t, filepath.Join(filepath.Dir(s.bare), "lib.git"), "config", "uploadpack.allowReachableSHA1InWant", "true")
	lib3 := libCommit(t, libWork, "lib three\n")
	setGitlinks(t, s, lib3, libURL)
	if _, err := r.Pull(ctx, s.network(t, testToken), PullOptions{}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if info, err = r.SubmoduleUpdate(ctx, "lib", libNet()); err != nil {
		t.Fatalf("the third SubmoduleUpdate: %v", err)
	}
	if info.Commit != lib3 || !info.Initialized || read(t, wt, "lib/lib.txt") != "lib three\n" {
		t.Errorf("after the third update: %+v, lib.txt %q", info, read(t, wt, "lib/lib.txt"))
	}
	sub, _ = r.openSubmodule("lib")
	if ref, err := sub.repo.Reference(plumbing.NewRemoteReferenceName("origin", "main"), true); err != nil || ref.Hash().String() != lib2 {
		t.Errorf("origin/main in the submodule = %v, %v; want it left at %s by a fetch by sha", ref, err, lib2)
	}
	if _, err := sub.repo.Reference(gitlinkRef, false); err == nil {
		t.Error("the fetch-by-sha ref was left behind")
	}

	// A submodule with local changes is not moved.
	write(t, wt, "lib/lib.txt", "my change\n")
	run(t, s.work, "update-index", "--cacheinfo", "160000,"+lib2+",lib")
	run(t, s.work, "commit", "-q", "-m", "lib back")
	run(t, s.work, "push", "-q", "served", "HEAD:refs/heads/main")
	if _, err := r.Pull(ctx, s.network(t, testToken), PullOptions{}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := r.SubmoduleUpdate(ctx, "lib", libNet()); !errors.Is(err, ErrWorktreeDirty) {
		t.Errorf("SubmoduleUpdate over a local change: %v, want worktree_dirty", err)
	}
	if read(t, wt, "lib/lib.txt") != "my change\n" {
		t.Error("the refused update touched the change")
	}

	// The update dials n.URL, bound to its host: a URL of another host is
	// refused before anything is sent.
	n := libNet()
	n.URL = "https://elsewhere.example/acme/lib.git"
	if _, err := r.SubmoduleUpdate(ctx, "abs", n); !errors.Is(err, gitsource.ErrInvalidRequest) {
		t.Errorf("SubmoduleUpdate to another host: %v, want invalid_request", err)
	}
	assertNoToken(t, dir)
}

// A failed update into an empty path leaves no repository behind: not when
// the clone fails, and not when the clone works but the commit is not there.
func TestSubmoduleUpdate_AFailureLeavesNoRepository(t *testing.T) {
	s := newServer(t, true)
	_, lib1 := libRepo(t, s)
	libURL := strings.TrimSuffix(s.url, "/app.git") + "/lib.git"
	setGitlinks(t, s, lib1, libURL)
	wt, dir := osWorktree(t)
	r, err := Clone(context.Background(), wt, s.network(t, testToken), CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := s.network(t, "wrong-token")
	n.URL = libURL
	_, err = r.SubmoduleUpdate(context.Background(), "lib", n)
	if !errors.Is(err, gitsource.ErrAuthFailed) {
		t.Fatalf("SubmoduleUpdate with a bad token: %v, want auth_failed", err)
	}
	assertNoTokenInError(t, err)
	if exists(wt, "lib/.git") {
		t.Error("a failed clone left lib/.git")
	}
	if subs, _ := r.Submodules(); subs[1].Initialized {
		t.Errorf("Submodules after a failed update = %+v", subs)
	}

	// HEAD records a commit the served lib does not have.
	const missing = "2222222222222222222222222222222222222222"
	setGitlinks(t, s, missing, libURL)
	if _, err := r.Pull(context.Background(), s.network(t, testToken), PullOptions{}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	n.Token = testToken
	_, err = r.SubmoduleUpdate(context.Background(), "lib", n)
	if !errors.Is(err, gitsource.ErrCommitNotFound) || Class(err) != "commit_not_found" {
		t.Fatalf("SubmoduleUpdate to a missing commit: %v (class %s), want commit_not_found", err, Class(err))
	}
	if entries, _ := wt.ReadDir("lib"); len(entries) != 0 {
		t.Errorf("lib holds %d entries after the failed update, want none", len(entries))
	}
	// The next update starts afresh rather than finding a broken clone.
	setGitlinks(t, s, lib1, libURL)
	if _, err := r.Pull(context.Background(), s.network(t, testToken), PullOptions{}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if info, err := r.SubmoduleUpdate(context.Background(), "lib", n); err != nil || !info.Initialized {
		t.Errorf("SubmoduleUpdate after the failures = %+v, %v", info, err)
	}
	assertNoToken(t, dir)
}
