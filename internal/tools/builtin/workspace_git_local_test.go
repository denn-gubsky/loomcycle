package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A git workspace's local ops (RFC GW P11 plan §3.7, PR 8): status, diff and
// log read the repository; commit, branch and checkout change it, and only
// for the owner with the write grant.

func gitLocalRun(t *testing.T, forge *wsForge) (*wsFixture, context.Context) {
	t.Helper()
	f := newWSGitFixture(t, forge)
	f.tool.GitAuthorName, f.tool.GitAuthorEmail = "Loom Bot", "bot@example.com"
	ctx := withGitRemote(f.runCtx(t, "alice", "", tools.WorkspacePolicyValue{Open: true, Read: true, Write: true, Share: true}), true)
	f.mustOK(t, ctx, gitOpenInput(forge, nil))
	return f, ctx
}

func decodeRes(t *testing.T, res tools.Result, into any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("refused: %s", res.Text)
	}
	if err := json.Unmarshal([]byte(res.Text), into); err != nil {
		t.Fatalf("%v: %s", err, res.Text)
	}
}

func TestWorkspaceGitLocal_EachOpWorksOnAGitWorkspace(t *testing.T) {
	forge := newWSForge(t)
	f, ctx := gitLocalRun(t, forge)
	f.mustOK(t, ctx, `{"op":"write","name":"app","path":"README.md","content":"changed\n"}`)

	var st struct {
		Branch string `json:"branch"`
		Files  []struct{ Path, Worktree string }
	}
	decodeRes(t, f.call(t, ctx, `{"op":"git_status","name":"app"}`), &st)
	if !strings.HasPrefix(st.Branch, "loom/app-") || len(st.Files) != 1 || st.Files[0].Path != "README.md" {
		t.Fatalf("status = %+v", st)
	}
	var diff struct {
		Patch     string
		Truncated bool
	}
	decodeRes(t, f.call(t, ctx, `{"op":"git_diff","name":"app"}`), &diff)
	if !strings.Contains(diff.Patch, "-hello") || !strings.Contains(diff.Patch, "+changed") {
		t.Fatalf("diff = %q", diff.Patch)
	}
	var commit struct{ Commit string }
	decodeRes(t, f.call(t, ctx, `{"op":"git_commit","name":"app","message":"change the readme","all":true}`), &commit)
	if len(commit.Commit) != 40 {
		t.Fatalf("commit = %+v", commit)
	}
	var log struct {
		Commits []struct {
			Hash, Author, AuthorEmail, Message string
		}
	}
	decodeRes(t, f.call(t, ctx, `{"op":"git_log","name":"app","max":1}`), &log)
	c := log.Commits
	if len(c) != 1 || c[0].Hash != commit.Commit || c[0].Author != "Loom Bot" || !strings.Contains(c[0].Message, "Loomcycle-Run: "+tools.RunID(ctx)) {
		t.Fatalf("log = %+v", c)
	}
	f.mustOK(t, ctx, `{"op":"git_branch","name":"app","create":"topic"}`)
	var br struct{ Branches []struct{ Name string } }
	decodeRes(t, f.call(t, ctx, `{"op":"git_branch","name":"app"}`), &br)
	if len(br.Branches) < 2 {
		t.Fatalf("branches = %+v", br)
	}
	f.mustOK(t, ctx, `{"op":"git_checkout","name":"app","branch":"topic"}`)
	decodeRes(t, f.call(t, ctx, `{"op":"git_status","name":"app"}`), &st)
	if st.Branch != "topic" {
		t.Fatalf("after checkout the branch is %q", st.Branch)
	}
}

// The author is the operator's or the agent's, never the model's: the ops
// take no author, and a commit message cannot drop the run's trailer.
func TestWorkspaceGitLocal_ACommitCannotNameItsAuthor(t *testing.T) {
	forge := newWSForge(t)
	f, ctx := gitLocalRun(t, forge)
	res := f.call(t, ctx, `{"op":"git_commit","name":"app","message":"x","all":true,"author":"Someone Else"}`)
	if refusalClass(res) != "invalid_argument" {
		t.Fatalf("a commit naming an author = %s", res.Text)
	}
}

// A read share may read the repository (status, diff, log) and do nothing
// else with it; changing it is the owner's alone.
func TestWorkspaceGitLocal_AReadShareMayOnlyRead(t *testing.T) {
	forge := newWSForge(t)
	f, ctx := gitLocalRun(t, forge)
	r, err := f.st.CreateRun(context.Background(), f.sess, store.RunIdentity{AgentID: "a_peer", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	peer := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "alice", AgentID: "a_peer", TenantID: "acme", RootRunID: r.ID})
	peer = tools.WithRunID(peer, r.ID)
	peer = withPushGrant(tools.WithRunPolicy(peer, tools.RunPolicy{Tools: []string{"Workspace"},
		Workspace: tools.WorkspacePolicyValue{Read: true, Write: true}}))
	f.mustOK(t, ctx, `{"op":"share","name":"app","with":"a_peer","access":"read_write"}`)
	for _, op := range []string{"git_status", "git_diff", "git_log"} {
		if res := f.call(t, peer, `{"op":"`+op+`","name":"app@a_alice"}`); res.IsError {
			t.Fatalf("%s through a share: %s", op, res.Text)
		}
	}
	for _, in := range []string{
		`{"op":"git_commit","name":"app@a_alice","message":"x","all":true}`,
		`{"op":"git_branch","name":"app@a_alice","create":"theirs"}`,
		`{"op":"git_checkout","name":"app@a_alice","branch":"main"}`,
		`{"op":"git_fetch","name":"app@a_alice"}`,
		`{"op":"git_push","name":"app@a_alice"}`,
	} {
		if res := f.call(t, peer, in); refusalClass(res) != "not_shared_op" {
			t.Fatalf("%s through a read_write share = %s", in, res.Text)
		}
	}
}

// Changing the repository needs the write grant; reading it, read.
func TestWorkspaceGitLocal_TheGrantsHold(t *testing.T) {
	forge := newWSForge(t)
	f, owner := gitLocalRun(t, forge)
	readOnly := tools.UpdateRunPolicy(owner, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{Read: true} })
	for in, by := range map[string]string{
		// The tool's grant check answers commit and checkout; listing
		// branches is a read, so creating one is refused by the op itself.
		`{"op":"git_commit","name":"app","message":"x","all":true}`: "grant does not allow",
		`{"op":"git_checkout","name":"app","branch":"main"}`:        "grant does not allow",
		`{"op":"git_branch","name":"app","create":"topic"}`:         "changes the repository",
	} {
		if res := f.call(t, readOnly, in); refusalClass(res) != "not_granted" || !strings.Contains(res.Text, by) {
			t.Fatalf("%s with read only = %s", in, res.Text)
		}
	}
	for _, in := range []string{`{"op":"git_status","name":"app"}`, `{"op":"git_log","name":"app"}`, `{"op":"git_branch","name":"app"}`} {
		if res := f.call(t, readOnly, in); res.IsError {
			t.Fatalf("%s with read = %s", in, res.Text)
		}
	}
	none := tools.UpdateRunPolicy(owner, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{Write: true} })
	if res := f.call(t, none, `{"op":"git_status","name":"app"}`); refusalClass(res) != "not_granted" {
		t.Fatalf("status without read = %s", res.Text)
	}
}

// A repository feature the session's git turned on that the typed ops
// cannot honour is refused by name; the shell's git still works.
func TestWorkspaceGitLocal_AFeatureTurnedOnInTheShellIsRefusedByName(t *testing.T) {
	forge := newWSForge(t)
	f, ctx := gitLocalRun(t, forge)
	dir := gitWSDir(t, f, ctx, "app")
	git := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		return cmd.Run()
	}
	if err := git("sparse-checkout", "set", "--no-cone", "/README.md"); err != nil {
		t.Skipf("this git has no sparse-checkout: %v", err)
	}
	for _, op := range []string{"git_status", "git_log", "git_commit"} {
		in := `{"op":"` + op + `","name":"app"}`
		if op == "git_commit" {
			in = `{"op":"git_commit","name":"app","message":"x","all":true}`
		}
		if res := f.call(t, ctx, in); refusalClass(res) != "unsupported_repository_feature" {
			t.Fatalf("%s after a sparse checkout in the shell = %s", op, res.Text)
		}
	}
	if err := git("status"); err != nil {
		t.Fatalf("the shell's git status: %v", err)
	}
}

// A diff is cut at 1 MiB and says so; a log answers at most 200 commits.
func TestWorkspaceGitLocal_ADiffAndALogAreBounded(t *testing.T) {
	forge := newWSForge(t)
	f, ctx := gitLocalRun(t, forge)
	dir := gitWSDir(t, f, ctx, "app")
	// Many files of 24 KB, each wholly rewritten: a patch of about 2 MB.
	body := strings.Repeat("a line of a file to diff\n", 1000)
	var paths []string
	for i := range 80 {
		p := fmt.Sprintf("f%02d.txt", i)
		paths = append(paths, `"`+p+`"`)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.mustOK(t, ctx, `{"op":"git_commit","name":"app","message":"files","paths":[`+strings.Join(paths, ",")+`]}`)
	for i := range 80 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte(strings.ToUpper(body)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var diff struct {
		Patch     string
		Truncated bool
	}
	decodeRes(t, f.call(t, ctx, `{"op":"git_diff","name":"app"}`), &diff)
	if !diff.Truncated || len(diff.Patch) > gitDiffMaxBytes {
		t.Fatalf("diff of %d bytes, truncated %v", len(diff.Patch), diff.Truncated)
	}
	if gitLogMax(0) != gitLogDefault || gitLogMax(1000) != gitLogCeiling || gitLogMax(3) != 3 {
		t.Fatalf("log bounds: %d %d %d", gitLogMax(0), gitLogMax(1000), gitLogMax(3))
	}
}

func TestWorkspaceGitLocal_RefusesAGeneralWorkspace(t *testing.T) {
	forge := newWSForge(t)
	f, ctx := gitLocalRun(t, forge)
	f.mustOK(t, ctx, `{"op":"open","name":"plain"}`)
	for _, op := range []string{"git_status", "git_diff", "git_log", "git_branch"} {
		if res := f.call(t, ctx, `{"op":"`+op+`","name":"plain"}`); refusalClass(res) != "not_a_git_workspace" {
			t.Fatalf("%s on a general workspace = %s", op, res.Text)
		}
	}
}

// Past the grant check (a share reaches gitLocal with no write), a change
// to the repository is still refused: each guard on its own.
func TestWorkspaceGitLocal_ChangesNeedWriteEvenPastTheGrant(t *testing.T) {
	forge := newWSForge(t)
	f, ctx := gitLocalRun(t, forge)
	row := gitWSRow(t, f, ctx, "app")
	for _, in := range []workspaceInput{
		{Op: "git_commit", Name: "app", Message: "x", All: true},
		{Op: "git_checkout", Name: "app", Branch: "main"},
		{Op: "git_branch", Name: "app", Create: "topic"},
	} {
		if res := f.tool.gitLocal(ctx, row, in, false); refusalClass(res) != "not_granted" {
			t.Fatalf("%s without write = %s", in.Op, res.Text)
		}
	}
}

// The worker opens only the mirror the workspace was made over: a row
// naming another is refused there too.
func TestWorkspaceGitLocal_AWorkerRefusesAnotherMirror(t *testing.T) {
	forge := newWSForge(t)
	f, ctx := gitLocalRun(t, forge)
	row := gitWSRow(t, f, ctx, "app")
	other := *row.Git
	other.Mirror = strings.Repeat("0", 32)
	row.Git = &other
	if res := f.tool.gitLocal(ctx, row, workspaceInput{Op: "git_status", Name: "app"}, true); refusalClass(res) != "invalid" {
		t.Fatalf("status over another mirror = %s", res.Text)
	}
}

func gitWSRow(t *testing.T, f *wsFixture, ctx context.Context, name string) store.WorkspaceRow {
	t.Helper()
	rows, err := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", tools.RunID(ctx))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no workspace %s", name)
	return store.WorkspaceRow{}
}
