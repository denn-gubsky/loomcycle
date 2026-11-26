package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// workspace_git.go — the checks of a git workspace's open (RFC GW P11 plan
// §3.3, §3.6), run here, in the run, as the Git tool runs them for a fetch:
// the agent's git_remote, the operator's host list, the user's access-control
// rules on git:<host>/<path>, then the credential, resolved for this run and
// this repository only. The resolved token goes on the open's ticket in the
// core's memory; the worker never holds it.

// gitOpen checks an open's repository and resolves its credential. release
// frees the git slot the open holds; call it when the open returns.
func (w *Workspace) gitOpen(ctx context.Context, o lifecycle.Owner, in workspaceInput) (lifecycle.GitOpen, func(), tools.Result, bool) {
	no := func(class, detail string) (lifecycle.GitOpen, func(), tools.Result, bool) {
		return lifecycle.GitOpen{}, nil, workspaceRefusal(class, detail), false
	}
	// A principal has no run, no git_remote and no agent for the
	// resolver (P11 plan G15).
	if o.IsPrincipal() {
		return no("not_over_mcp", "a git workspace is opened inside a run; over the MCP server only general workspaces")
	}
	if !tools.PolicyOf(ctx).GitRemote.Allows("fetch") {
		return no("git_remote_not_allowed", `this agent's git_remote does not include "fetch"`)
	}
	src := w.GitSource
	if src == nil || len(src.Hosts) == 0 {
		return no("git_network_disabled", "the operator has not enabled git hosts (LOOMCYCLE_GIT_SOURCE_HOSTS)")
	}
	host, err := gitsource.ValidateURL(in.Repo)
	if err != nil {
		return no("invalid_argument", "repo: "+strings.TrimPrefix(err.Error(), gitsource.ErrInvalidRequest.Error()+": "))
	}
	if !hostListed(src.Hosts, host) {
		return no("host_not_allowed", "")
	}
	if res := gitACLResource(host, in.Repo); !tools.ACLAllowsInstance(ctx, tools.CallerACL(ctx), "fetch", res) {
		return no("acl_denied", fmt.Sprintf("the access-control rules of the user who started this run do not allow fetch of %s", res.Name))
	}
	var username, token string
	if in.Credential != "" {
		var class, detail string
		if username, token, class, detail = resolveGitCredential(ctx, src.Credential, in.Credential, in.Repo, host); class != "" {
			return no(class, detail)
		}
	}
	release := func() {}
	if src.Slots != nil {
		select {
		case src.Slots <- struct{}{}:
			release = func() { <-src.Slots }
		case <-ctx.Done():
			return no("timeout", "waiting for a free git slot")
		}
	}
	timeout := src.Timeout
	if timeout <= 0 {
		timeout = gitOpTimeoutFallback
	}
	return lifecycle.GitOpen{URL: in.Repo, Branch: in.Branch, BaseRef: in.Base, Credential: in.Credential,
		Username: username, Token: token, MaxObjectBytes: src.MaxObjectBytes, MaxReadBytes: src.MaxBytes,
		Timeout: timeout, Push: w.GitPush}, release, tools.Result{}, true
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
