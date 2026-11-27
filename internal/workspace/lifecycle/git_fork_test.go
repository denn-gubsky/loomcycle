package lifecycle

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Forks and checkpoints of a git workspace (RFC GW P11 plan PR 9): a fork is
// a git workspace of its own, with its own branch and pins; a restore keeps
// what the workspace's history names pinned.

func mirrorRefs(t *testing.T, f *fixture, row store.WorkspaceRow, prefix string) string {
	t.Helper()
	return gitRun(t, filepath.Join(f.h.Root, ".loomcycle-mirrors", row.Git.Mirror), "--git-dir=.", "for-each-ref", "--format=%(refname) %(objectname)", prefix)
}

// Two forks of one git workspace each push their own branch.
func TestForkGit_TwoForksPushTwoBranches(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	o := f.run(t, "alice")
	src, err := f.m.Open(context.Background(), o, gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil {
		t.Fatal(err)
	}
	wsCommit(t, filepath.Join(f.h.Root, src.ID), "src.txt", "from the source\n", false)
	pushed := map[string]string{}
	for _, as := range []string{"one", "two"} {
		fork, _, err := f.m.ForkBranch(context.Background(), o, src, as, "", "")
		if err != nil {
			t.Fatalf("fork %s: %v", as, err)
		}
		if fork.Kind != store.WorkspaceKindGit || fork.Git == nil || !strings.HasPrefix(fork.Git.Branch, "loom/"+as+"-") || fork.Git.Branch == src.Git.Branch {
			t.Fatalf("fork %s row = %+v", as, fork.Git)
		}
		dir := filepath.Join(f.h.Root, fork.ID)
		if got := gitRun(t, dir, "symbolic-ref", "HEAD"); got != "refs/heads/"+fork.Git.Branch {
			t.Fatalf("fork %s is on %s", as, got)
		}
		c := wsCommit(t, dir, as+".txt", as+"\n", false)
		if _, err := f.m.GitPush(context.Background(), fork, gitNet(forgeTokenA), false); err != nil {
			t.Fatalf("push from fork %s: %v", as, err)
		}
		pushed[fork.Git.Branch] = c
	}
	for b, c := range pushed {
		if got := gitRun(t, forge.bare, "rev-parse", "refs/heads/"+b); got != c {
			t.Fatalf("the forge's %s = %s, want %s", b, got, c)
		}
	}
}

// A fork's branch is held to the open's rules: never the forge's default
// branch nor its source's own; a named one only where the operator lets a
// workspace adopt an existing branch, since a fork asks the forge nothing.
func TestForkGit_ABranchAPushMustNotMoveIsRefused(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	o := f.run(t, "alice")
	src, err := f.m.Open(context.Background(), o, gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"main", src.Git.Branch, "mine", "a..b"} {
		if _, _, err := f.m.ForkBranch(context.Background(), o, src, "f1", "", b); !errors.Is(err, ErrGitBranch) {
			t.Fatalf("fork on %q = %v, want ErrGitBranch", b, err)
		}
	}
	f.m.SetGitPush(config.WorkspacePushConfig{AdoptExisting: true}.Resolved())
	fork, _, err := f.m.ForkBranch(context.Background(), o, src, "f1", "", "mine")
	if err != nil || fork.Git.Branch != "mine" {
		t.Fatalf("a named branch with adopt_existing: %+v, %v", fork.Git, err)
	}
	for _, b := range []string{"main", src.Git.Branch, "a..b"} {
		if _, _, err := f.m.ForkBranch(context.Background(), o, src, "f2", "", b); !errors.Is(err, ErrGitBranch) {
			t.Fatalf("%q with adopt_existing = %v", b, err)
		}
	}
	// No heads prefix the operator lists can hold a branch named for the
	// fork: refused, not made outside the prefixes.
	f.m.SetGitPush(config.WorkspacePushConfig{RefPrefixes: []string{"refs/tags/"}}.Resolved())
	if _, _, err := f.m.ForkBranch(context.Background(), o, src, "f3", "", ""); !errors.Is(err, ErrGitBranch) {
		t.Fatalf("a fork with no prefix for its branch = %v", err)
	}
}

// A fork pins its base and its tracking refs under its own id: releasing
// the source leaves the fork's pins, and the fork still works.
func TestForkGit_TheForkHasPinsOfItsOwn(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	o := f.run(t, "alice")
	src, err := f.m.Open(context.Background(), o, gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.GitFetch(context.Background(), src, gitNet(forgeTokenA)); err != nil {
		t.Fatal(err)
	}
	fork, _, err := f.m.ForkBranch(context.Background(), o, src, "copy", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Release(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	pins := mirrorRefs(t, f, fork, "refs/loom/")
	if strings.Contains(pins, src.ID) || !strings.Contains(pins, fork.ID+"/base") || !strings.Contains(pins, fork.ID+"/remotes/main") {
		t.Fatalf("pins after the source's release: %q", pins)
	}
	if _, err := f.m.GitFetch(context.Background(), fork, gitNet(forgeTokenA)); err != nil {
		t.Fatalf("the fork's fetch: %v", err)
	}
}

// A fork for a child at spawn is a git workspace with a default branch.
func TestForkGit_AtSpawnTheChildGetsItsOwnBranch(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	parent := f.run(t, "alice")
	src, err := f.m.Open(context.Background(), parent, gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil {
		t.Fatal(err)
	}
	child := f.run(t, "alice")
	row, err := f.m.ForkForChild(context.Background(), parent, ChildFork{Child: child, Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if row.Git == nil || row.Git.Branch == src.Git.Branch || !strings.HasPrefix(row.Git.Branch, "loom/app-") {
		t.Fatalf("the child's fork: %+v", row.Git)
	}
}

// A restore puts the git directory back as it was; what its tracking refs
// name is pinned again, so a commit the workspace's history names stays in
// the mirror whatever a later fetch moved.
func TestRestoreGit_PinsWhatTheRestoredHistoryNames(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.GitFetch(context.Background(), row, gitNet(forgeTokenA)); err != nil {
		t.Fatal(err)
	}
	old := gitRun(t, filepath.Join(f.h.Root, row.ID), "rev-parse", "refs/remotes/origin/main")
	ck, err := f.m.Checkpoint(context.Background(), row, "before")
	if err != nil {
		t.Fatal(err)
	}
	forge.commit(t, "main", "later.txt", "later\n")
	if _, err := f.m.GitFetch(context.Background(), row, gitNet(forgeTokenA)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Restore(context.Background(), row, ck.ID); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.h.Root, row.ID)
	if got := gitRun(t, dir, "rev-parse", "refs/remotes/origin/main"); got != old {
		t.Fatalf("restored origin/main = %s, want %s", got, old)
	}
	if pins := mirrorRefs(t, f, row, "refs/loom/workspaces/"+row.ID+"/remotes/main"); !strings.Contains(pins, old) {
		t.Fatalf("the restored origin/main is not pinned: %q", pins)
	}
	gitRun(t, dir, "log", "--oneline", "-1", "refs/remotes/origin/main")
}
