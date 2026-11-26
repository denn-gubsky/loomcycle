package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
)

// The network ops of a git workspace (RFC GW P11 plan §3.7, PR 7), each the
// whole way to a real forge, under a ticket of its own.

func gitNet(token string) GitNet {
	return GitNet{Username: forgeUser, Token: token, MaxObjectBytes: 1 << 20, MaxReadBytes: 64 << 20,
		MaxPushBytes: 16 << 20, Timeout: time.Minute, Push: config.WorkspacePushConfig{}.Resolved()}
}

// forgeCommit commits a file on branch in the forge's working copy and
// pushes it to the served repository.
func (f *realForge) commit(t *testing.T, branch, file, body string) string {
	t.Helper()
	gitRun(t, f.work, "checkout", "-q", branch)
	if err := os.WriteFile(filepath.Join(f.work, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.work, "add", file)
	gitRun(t, f.work, "commit", "-q", "-m", "forge: "+file)
	gitRun(t, f.work, "push", "-q", f.bare, branch+":"+branch)
	return gitRun(t, f.work, "rev-parse", "HEAD")
}

// wsCommit commits a file in the workspace with the session's own git.
func wsCommit(t *testing.T, dir, file, body string, amend bool) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", file)
	args := []string{"commit", "-q", "-m", "workspace: " + file}
	if amend {
		args = append(args, "--amend")
	}
	gitRun(t, dir, args...)
	return gitRun(t, dir, "rev-parse", "HEAD")
}

func openGitWS(t *testing.T, f *fixture, forge *realForge, name, branch string, adopt bool) store.WorkspaceRow {
	t.Helper()
	req := gitOpenReq(forge, name, branch, forgeTokenA)
	req.Git.Push.AdoptExisting = adopt
	row, err := f.m.Open(context.Background(), f.run(t, "alice"), req)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return row
}

func TestGitFetch_BringsTheForgesCommitsAndPinsThem(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	row := openGitWS(t, f, forge, "app", "", false)
	tip := forge.commit(t, "main", "news.txt", "news\n")
	res, err := f.m.GitFetch(context.Background(), row, gitNet(forgeTokenA))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(strings.Join(res.Updated, " "), "refs/remotes/origin/main") {
		t.Fatalf("updated = %v", res.Updated)
	}
	dir := filepath.Join(f.h.Root, row.ID)
	if got := gitRun(t, dir, "rev-parse", "refs/remotes/origin/main"); got != tip {
		t.Fatalf("origin/main = %s, want %s", got, tip)
	}
	mirror := filepath.Join(f.h.Root, ".loomcycle-mirrors", row.Git.Mirror)
	if got := gitRun(t, mirror, "--git-dir=.", "rev-parse", "refs/loom/workspaces/"+row.ID+"/remotes/main"); got != tip {
		t.Fatalf("pin = %s, want %s", got, tip)
	}
}

func TestGitPull_FastForwardsTheOwnBranch(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	row := openGitWS(t, f, forge, "app", "feature", true)
	tip := forge.commit(t, "feature", "pulled.txt", "pulled\n")
	res, err := f.m.GitPull(context.Background(), row, gitNet(forgeTokenA))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if res.Head != tip {
		t.Fatalf("head = %s, want %s", res.Head, tip)
	}
	if b, err := os.ReadFile(filepath.Join(f.h.Root, row.ID, "pulled.txt")); err != nil || string(b) != "pulled\n" {
		t.Fatalf("pulled.txt = %q, %v", b, err)
	}
}

// A pull that is no fast-forward is refused with its class; the fetched
// commits are kept, so the agent can merge or rebase in the shell.
func TestGitPull_RefusesANonFastForward(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	row := openGitWS(t, f, forge, "app", "feature", true)
	dir := filepath.Join(f.h.Root, row.ID)
	wsCommit(t, dir, "mine.txt", "mine\n", false)
	theirs := forge.commit(t, "feature", "theirs.txt", "theirs\n")
	_, err := f.m.GitPull(context.Background(), row, gitNet(forgeTokenA))
	if dispatch.ClassOf(err) != "non_fast_forward" {
		t.Fatalf("pull = %v, want non_fast_forward", err)
	}
	if got := gitRun(t, dir, "rev-parse", "refs/remotes/origin/feature"); got != theirs {
		t.Fatalf("origin/feature = %s, want the fetched %s", got, theirs)
	}
}

func TestGitPush_PushesTheOwnBranch(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	row := openGitWS(t, f, forge, "app", "", false)
	c := wsCommit(t, filepath.Join(f.h.Root, row.ID), "work.txt", "work\n", false)
	if _, err := f.m.GitPush(context.Background(), row, gitNet(forgeTokenA), false); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := gitRun(t, forge.bare, "rev-parse", "refs/heads/"+row.Git.Branch); got != c {
		t.Fatalf("the forge has %s, want %s", got, c)
	}
}

// Without force a rewrite of the own branch is refused before it is sent;
// with force it is accepted (D11).
func TestGitPush_ARewriteNeedsForce(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	row := openGitWS(t, f, forge, "app", "", false)
	dir := filepath.Join(f.h.Root, row.ID)
	first := wsCommit(t, dir, "work.txt", "work\n", false)
	if _, err := f.m.GitPush(context.Background(), row, gitNet(forgeTokenA), false); err != nil {
		t.Fatal(err)
	}
	amended := wsCommit(t, dir, "work.txt", "rewritten\n", true)
	if _, err := f.m.GitPush(context.Background(), row, gitNet(forgeTokenA), false); err == nil {
		t.Fatal("a rewrite without force was pushed")
	}
	if got := gitRun(t, forge.bare, "rev-parse", "refs/heads/"+row.Git.Branch); got != first {
		t.Fatalf("a refused push moved the forge's branch to %s", got)
	}
	if _, err := f.m.GitPush(context.Background(), row, gitNet(forgeTokenA), true); err != nil {
		t.Fatalf("forced push: %v", err)
	}
	if got := gitRun(t, forge.bare, "rev-parse", "refs/heads/"+row.Git.Branch); got != amended {
		t.Fatalf("the forge has %s after a forced push, want %s", got, amended)
	}
}

// A push ticket covers the own branch only: a worker that sends a push of
// another ref (here, the forge's default branch) is refused by the core and
// nothing reaches the forge's receive-pack.
func TestGitPush_TheCoreRefusesARefOutsideTheOwnBranch(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	row := openGitWS(t, f, forge, "app", "", false)
	main := gitRun(t, forge.bare, "rev-parse", "refs/heads/main")
	var tk *GitTicket
	f.m.onGitTicket = func(t *GitTicket) { tk = t }
	tkRow, err := f.st.WorkspaceGet(context.Background(), "acme", row.ID)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.m.mintGitOp(tkRow, gitNet(forgeTokenA), gitPushShapes)
	if err != nil {
		t.Fatal(err)
	}
	defer ticket.End()
	if tk != ticket {
		t.Fatal("the hook did not see the op's ticket")
	}
	before := forge.receives.Load()
	for _, ref := range []string{"refs/heads/main", "refs/heads/other", "refs/tags/v1"} {
		body := pktLine(fmt.Sprintf("%s %s %s\x00report-status\n", strings.Repeat("0", 40), main, ref)) + "0000"
		req := egress.GitRequest{Shape: egress.GitReceivePack, Header: http.Header{"Content-Type": {"application/x-git-receive-pack-request"}},
			Body: strings.NewReader(body), ContentLength: int64(len(body))}
		err := gitEgress(t, f.m, tkRow, ticket.Secret(), req)
		if err == nil {
			t.Fatalf("a push of %s went out", ref)
		}
	}
	if n := forge.receives.Load() - before; n != 0 {
		t.Fatalf("the forge's receive-pack saw %d requests", n)
	}
}

// A network op's ticket ends with the op: replaying its bytes afterwards
// reaches nothing.
func TestGitOps_ATicketCannotBeReusedAfterItsOp(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	row := openGitWS(t, f, forge, "app", "", false)
	for name, op := range map[string]func() error{
		"fetch": func() error { _, err := f.m.GitFetch(context.Background(), row, gitNet(forgeTokenA)); return err },
		"pull":  func() error { _, err := f.m.GitPull(context.Background(), row, gitNet(forgeTokenA)); return err },
		"push":  func() error { _, err := f.m.GitPush(context.Background(), row, gitNet(forgeTokenA), false); return err },
	} {
		t.Run(name, func(t *testing.T) {
			var tk *GitTicket
			f.m.onGitTicket = func(t *GitTicket) { tk = t }
			_ = op() // pull of a branch the forge lacks fails; the ticket must end either way
			if tk == nil {
				t.Fatal("no ticket minted")
			}
			ready, err := f.st.WorkspaceGet(context.Background(), "acme", row.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := forge.requests.Load()
			wantTicketRefusal(t, gitEgress(t, f.m, ready, tk.Secret(), advertise()))
			if forge.requests.Load() != before {
				t.Fatal("a replayed ticket reached the forge")
			}
		})
	}
}

// A network op needs a ready git workspace.
func TestGitOps_RefuseAWorkspaceThatIsNotGit(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	plain := f.open(t, f.run(t, "alice"), "plain")
	if _, err := f.m.GitFetch(context.Background(), plain, gitNet(forgeTokenA)); err == nil {
		t.Fatal("a fetch of a general workspace ran")
	}
}

func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
