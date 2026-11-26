package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/gitsource"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// workspace_git.go — a git workspace's network ops in the Workspace tool
// (RFC GW P11 plan §3.3, §3.6, §3.7): open with a repo, git_fetch, git_pull
// and git_push. Their checks run here, in the run, as the Git tool runs them:
// the agent's git_remote, the operator's host list (the push list for a
// push), the user's access-control rules on git:<host>/<path>, then the
// credential, resolved for this run and this repository only. The resolved
// token goes on the op's ticket in the core's memory; the worker never holds
// it. Every call of one writes one audit line, a refused one too.

// gitNetworkCall reports whether in is a network op of a git workspace.
func gitNetworkCall(in workspaceInput) bool {
	switch in.Op {
	case "git_fetch", "git_pull", "git_push":
		return true
	case "open":
		return in.Repo != ""
	}
	return false
}

// gitNetCheck is what the checks of one network op settle.
type gitNetCheck struct {
	username, token string
	release         func()
	timeout         time.Duration
}

// checkGitNet runs a network op's checks on rawURL with the credential
// reference cred: fetch for open, git_fetch and git_pull, push for git_push.
// release frees the git slot it holds.
func (w *Workspace) checkGitNet(ctx context.Context, o lifecycle.Owner, push bool, rawURL, cred string) (gitNetCheck, tools.Result, bool) {
	no := func(class, detail string) (gitNetCheck, tools.Result, bool) {
		return gitNetCheck{}, workspaceRefusal(class, detail), false
	}
	if ev := gitAuditOf(ctx); ev != nil {
		ev.GitCredential = cred
		if host, err := gitsource.ValidateURL(rawURL); err == nil {
			ev.GitHost, ev.GitRepository = host, redactURLPath(rawURL)
		}
	}
	// A principal has no run, no git_remote and no agent for the
	// resolver (P11 plan G15).
	if o.IsPrincipal() {
		return no("not_over_mcp", "a git workspace's network ops run inside a run; over the MCP server only general workspaces")
	}
	need := "fetch"
	if push {
		need = "push"
	}
	if !tools.PolicyOf(ctx).GitRemote.Allows(need) {
		return no("git_remote_not_allowed", fmt.Sprintf("this agent's git_remote does not include %q", need))
	}
	src := w.GitSource
	if src == nil || len(src.Hosts) == 0 {
		return no("git_network_disabled", "the operator has not enabled git hosts (LOOMCYCLE_GIT_SOURCE_HOSTS)")
	}
	hosts, resolve := src.Hosts, src.Credential
	if push {
		if len(w.GitPushHosts) == 0 {
			return no("git_push_disabled", "the operator has not enabled push (LOOMCYCLE_GIT_PUSH_HOSTS)")
		}
		hosts, resolve = w.GitPushHosts, w.GitPushCredential
	}
	host, err := gitsource.ValidateURL(rawURL)
	if err != nil {
		return no("invalid_argument", "repo: "+strings.TrimPrefix(err.Error(), gitsource.ErrInvalidRequest.Error()+": "))
	}
	if !hostListed(hosts, host) {
		return no("host_not_allowed", "")
	}
	if res := gitACLResource(host, rawURL); !tools.ACLAllowsInstance(ctx, tools.CallerACL(ctx), need, res) {
		return no("acl_denied", fmt.Sprintf("the access-control rules of the user who started this run do not allow %s of %s", need, res.Name))
	}
	var c gitNetCheck
	if cred != "" {
		var class, detail string
		if c.username, c.token, class, detail = resolveGitCredential(ctx, resolve, cred, rawURL, host); class != "" {
			return no(class, detail)
		}
	}
	c.release = func() {}
	if src.Slots != nil {
		select {
		case src.Slots <- struct{}{}:
			c.release = func() { <-src.Slots }
		case <-ctx.Done():
			return no("timeout", "waiting for a free git slot")
		}
	}
	c.timeout = src.Timeout
	if c.timeout <= 0 {
		c.timeout = gitOpTimeoutFallback
	}
	return c, tools.Result{}, true
}

// gitOpen checks an open's repository and resolves its credential. release
// frees the git slot the open holds; call it when the open returns.
func (w *Workspace) gitOpen(ctx context.Context, o lifecycle.Owner, in workspaceInput) (lifecycle.GitOpen, func(), tools.Result, bool) {
	c, res, ok := w.checkGitNet(ctx, o, false, in.Repo, in.Credential)
	if !ok {
		return lifecycle.GitOpen{}, nil, res, false
	}
	src := w.GitSource
	return lifecycle.GitOpen{URL: in.Repo, Branch: in.Branch, BaseRef: in.Base, Credential: in.Credential,
		Username: c.username, Token: c.token, MaxObjectBytes: src.MaxObjectBytes, MaxReadBytes: src.MaxBytes,
		Timeout: c.timeout, Push: w.GitPush}, c.release, tools.Result{}, true
}

// gitNetOp runs git_fetch, git_pull or git_push on the run's own workspace
// row, with the row's repository and credential reference.
func (w *Workspace) gitNetOp(ctx context.Context, o lifecycle.Owner, row store.WorkspaceRow, in workspaceInput) tools.Result {
	ev := gitAuditOf(ctx)
	if ev != nil {
		ev.WorkspaceID, ev.WorkspaceOwnerRunID = row.ID, row.OwnerRunID
	}
	if row.Kind != store.WorkspaceKindGit || row.Git == nil {
		return workspaceRefusal("not_a_git_workspace", fmt.Sprintf("%q is no git workspace; open one with repo", row.Name))
	}
	push := in.Op == "git_push"
	if in.Force && !push {
		return workspaceRefusal("invalid_argument", "force is for git_push")
	}
	c, res, ok := w.checkGitNet(ctx, o, push, row.Git.URL, row.Git.Credential)
	if !ok {
		return res
	}
	defer c.release()
	n := lifecycle.GitNet{Username: c.username, Token: c.token, MaxObjectBytes: w.GitSource.MaxObjectBytes,
		MaxReadBytes: w.GitSource.MaxBytes, Timeout: c.timeout, Push: w.GitPush}
	var out lifecycle.GitNetResult
	var err error
	switch in.Op {
	case "git_fetch":
		out, err = w.Manager.GitFetch(ctx, row, n)
	case "git_pull":
		out, err = w.Manager.GitPull(ctx, row, n)
	default:
		out, err = w.Manager.GitPush(ctx, row, n, in.Force)
	}
	if err != nil {
		var oe *dispatch.OpError
		if errors.As(err, &oe) && oe.Class == "non_fast_forward" && in.Op == "git_pull" {
			return workspaceRefusal(oe.Class, oe.Detail+"; the fetched commits are in origin/*: merge or rebase with exec (git merge, git rebase), then push")
		}
		return workspaceError(err, row.Name)
	}
	if ev != nil {
		ev.GitRefs = out.Updated
	}
	body := map[string]any{"name": row.Name, "op": in.Op, "up_to_date": out.UpToDate, "updated": out.Updated}
	if out.Head != "" {
		body["head"] = out.Head
	}
	return workspaceOK(body)
}

// withGit names a git workspace's repository in an answer: its URL, its own
// branch, the commit it started at and the forge's default branch. Never the
// credential's value; its reference is the caller's own.
func withGit(out map[string]any, g *store.WorkspaceGit) {
	if g == nil {
		return
	}
	out["repo"], out["branch"], out["base"] = g.URL, g.Branch, g.Base
	if g.DefaultBranch != "" {
		out["default_branch"] = g.DefaultBranch
	}
}

// ---- the audit line ----

type gitAuditKey struct{}

func withGitAudit(ctx context.Context, ev *audit.Event) context.Context {
	return context.WithValue(ctx, gitAuditKey{}, ev)
}

// gitAuditOf is the audit line the call fills in, nil outside a network op.
func gitAuditOf(ctx context.Context) *audit.Event {
	ev, _ := ctx.Value(gitAuditKey{}).(*audit.Event)
	return ev
}

// gitAuditEvent starts a network op's line: who, which op, which run, which
// workspace by name. Names and references only, never a credential's value.
func (w *Workspace) gitAuditEvent(ctx context.Context, in workspaceInput) audit.Event {
	ident := tools.RunIdentity(ctx)
	ev := audit.Event{
		TS: time.Now().UTC(), Action: "workspace_git", WorkspaceOp: in.Op,
		ActorTenant: ident.TenantID, ActorSubject: ident.UserID, TargetTenant: ident.TenantID,
		TargetName: "workspace:" + in.Name, GitRunID: tools.RunID(ctx), GitAgent: tools.AgentName(ctx),
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}
	return ev
}

// recordGit writes the line with the call's outcome: ok, or its refusal's
// class.
func (w *Workspace) recordGit(ev audit.Event, res tools.Result) {
	ev.GitOutcome = "ok"
	if res.IsError {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(res.Text), &e)
		ev.GitOutcome = e.Error
	}
	if w.GitAudit == nil {
		return
	}
	if err := w.GitAudit.Record(ev); err != nil {
		slog.Error("workspace: git audit record failed", "err", err)
	}
}

// ---- the local ops ----

// The bounds of the local ops' answers (P11 plan §3.7).
const (
	gitDiffMaxBytes = 1 << 20
	gitLogDefault   = 50
	gitLogCeiling   = 200
)

// gitLogMax is a log's commit count: the default when none is asked, at
// most the ceiling.
func gitLogMax(n int) int {
	switch {
	case n <= 0:
		return gitLogDefault
	case n > gitLogCeiling:
		return gitLogCeiling
	}
	return n
}

// gitLocal runs git_status, git_diff, git_log, git_commit, git_branch or
// git_checkout on row's repository. mayWrite is whether the caller may
// change it: the owner with the write grant. A commit, a checkout and a new
// branch are refused without it here too, whatever let the call this far
// (a share reaches this with mayWrite false).
func (w *Workspace) gitLocal(ctx context.Context, row store.WorkspaceRow, in workspaceInput, mayWrite bool) tools.Result {
	if row.Kind != store.WorkspaceKindGit || row.Git == nil {
		return workspaceRefusal("not_a_git_workspace", fmt.Sprintf("%q is no git workspace; open one with repo", row.Name))
	}
	changes := in.Op == "git_commit" || in.Op == "git_checkout" || (in.Op == "git_branch" && in.Create != "")
	if changes && !mayWrite {
		return workspaceRefusal("not_granted", in.Op+" changes the repository: it needs the owner's workspace write grant")
	}
	a := workspace.GitLocalArgs{Mirror: row.Git.Mirror, URL: row.Git.URL, Branch: row.Git.Branch, Op: strings.TrimPrefix(in.Op, "git_")}
	switch in.Op {
	case "git_diff":
		a.Path, a.Staged, a.Rev, a.MaxBytes = in.Path, in.Staged, in.Rev, gitDiffMaxBytes
	case "git_log":
		a.Rev, a.Max = in.Rev, gitLogMax(in.Max)
	case "git_commit":
		sig := commitSignature(ctx, w.GitAuthorName, w.GitAuthorEmail)
		a.Message, a.Paths, a.All = withRunTrailer(in.Message, tools.RunID(ctx)), in.Paths, in.All
		a.AuthorName, a.AuthorEmail, a.When = sig.Name, sig.Email, sig.When.Unix()
	case "git_branch":
		a.Create, a.From = in.Create, in.From
	case "git_checkout":
		if (in.Branch == "") == (in.Rev == "") {
			return workspaceRefusal("invalid_argument", "git_checkout takes branch, or rev for a detached checkout")
		}
		a.To, a.Detach = in.Branch, in.Rev != ""
		if a.Detach {
			a.To = in.Rev
		}
	}
	b, err := json.Marshal(a)
	if err != nil {
		return workspaceRefusal("failed", "could not encode the op")
	}
	res, err := w.Manager.Do(ctx, row, dispatch.Request{Op: workspace.OpGitLocal, Args: b, MaxOutput: 2 * gitDiffMaxBytes})
	if err != nil {
		return workspaceError(err, row.Name)
	}
	return tools.Result{Text: string(res.Output)}
}
