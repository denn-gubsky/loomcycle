package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

func TestDefOpAction_FailsClosedToAuthor(t *testing.T) {
	cases := []struct{ family, op, want string }{
		{"agent", "create", "author"},
		{"agent", "fork", "author"},
		{"agent", "promote", "author"},
		{"schedule", "add_hook", "author"},
		{"mcpserver", "rediscover", "author"},
		{"volume", "set_mode", "author"},
		{"agent", "get", "read"},
		{"agent", "list", "read"},
		{"hook", "verify", "read"},
		{"team", "render_diagram", "read"},
		{"agent", "retire", "retire"},
		{"team", "delete", "retire"},
		{"volume", "purge", "retire"},
		{"team", "run", ""},
		{"team", "poll", ""},
		{"team", "cancel", ""},
		// Only the team family has walk ops; anywhere else an op of that name
		// is unknown and asks author.
		{"agent", "run", "author"},
		{"agent", "frobnicate", "author"},
		{"agent", "", "author"},
	}
	for _, c := range cases {
		if got := DefOpAction(c.family, c.op); got != c.want {
			t.Errorf("DefOpAction(%q, %q) = %q, want %q", c.family, c.op, got, c.want)
		}
	}
}

func refusalCode(t *testing.T, r Result) string {
	t.Helper()
	var b struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(r.Text), &b); err != nil {
		t.Fatalf("refusal is not JSON: %s", r.Text)
	}
	return b.Code
}

func TestAuthorizeDefOp_AsksTheOpsVerbOfTheNamedDefinition(t *testing.T) {
	eff := compiledSet(t,
		authz.Rule{Effect: authz.Deny, Actions: []string{"author"}, Resources: []string{"def:agent/locked"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"retire"}, Resources: []string{"def:skill/*"}})
	run := WithRunID(UpdateRunPolicy(modeCtx(authz.ModeCompat), func(p *RunPolicy) { p.ACL = eff }), "run_1")

	if r, refused := AuthorizeDefOp(run, "agent", "fork", "locked"); !refused || refusalCode(t, r) != "acl_denied" || r.Error == nil || r.Error.Category != CategoryPermission {
		t.Errorf("fork of a definition denied author: refused=%v %s", refused, r.Text)
	}
	if _, refused := AuthorizeDefOp(run, "agent", "fork", "open"); refused {
		t.Error("a deny on one definition refused another")
	}
	if _, refused := AuthorizeDefOp(run, "agent", "get", "locked"); refused {
		t.Error("a deny on author refused a read")
	}
	// Every name the call gives is asked: an allowed name beside a denied
	// one does not carry it.
	if _, refused := AuthorizeDefOp(run, "agent", "retire", "open", "locked"); refused {
		t.Error("retire is not author: the deny on author must not refuse it")
	}
	if _, refused := AuthorizeDefOp(run, "agent", "promote", "open", "locked"); !refused {
		t.Error("promote naming a denied definition beside an allowed one was let through")
	}
	if _, refused := AuthorizeDefOp(run, "skill", "retire", "any"); !refused {
		t.Error("retire under a family-wide deny on retire was let through")
	}
	if _, refused := AuthorizeDefOp(run, "skill", "create", "any"); refused {
		t.Error("a deny on retire refused a create")
	}
	if _, refused := AuthorizeDefOp(run, "agent", "frobnicate", "locked"); !refused {
		t.Error("an unknown op was not asked author")
	}
	// A team walk's ops name no definition and are not asked here.
	teamDeny := compiledSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"author", "read", "retire"}, Resources: []string{"def:team/*"}})
	teamRun := WithRunID(UpdateRunPolicy(modeCtx(authz.ModeCompat), func(p *RunPolicy) { p.ACL = teamDeny }), "run_1")
	if _, refused := AuthorizeDefOp(teamRun, "team", "poll"); refused {
		t.Error("a team walk op was asked a definition question")
	}
}

func TestAuthorizeDefOp_UnnamedDefinitionAsksTheFamily(t *testing.T) {
	one := compiledSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"author"}, Resources: []string{"def:agent/locked"}})
	all := compiledSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"author"}, Resources: []string{"def:agent/*"}})
	ctxWith := func(eff *authz.Effective) context.Context {
		return WithRunID(UpdateRunPolicy(modeCtx(authz.ModeCompat), func(p *RunPolicy) { p.ACL = eff }), "run_1")
	}
	// A deny on one name does not answer the family-wide question; a deny on
	// the family does.
	if _, refused := AuthorizeDefOp(ctxWith(one), "agent", "create", ""); refused {
		t.Error("a deny on one definition refused the family-wide question")
	}
	r, refused := AuthorizeDefOp(ctxWith(all), "agent", "create", "")
	if !refused || refusalCode(t, r) != "acl_denied" {
		t.Errorf("a family-wide deny did not refuse an unnamed write: %s", r.Text)
	}
	if !json.Valid([]byte(r.Text)) || !strings.Contains(r.Text, `"resource":"def:agent/*"`) {
		t.Errorf("refusal names %s, want def:agent/*", r.Text)
	}
}

func TestAuthorizeDefOp_JudgesTheRunsSetOrTheCallersPrincipal(t *testing.T) {
	deny := compiledSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"author"}, Resources: []string{"def:agent/*"}})
	compat := modeCtx(authz.ModeCompat)

	// Off a run: the calling principal's set.
	caller := auth.WithPrincipal(compat, auth.Principal{TenantID: "t", Subject: "u", Effective: deny})
	if _, refused := AuthorizeDefOp(caller, "agent", "create", "x"); !refused {
		t.Error("the caller's deny did not refuse an off-run write")
	}
	// No principal off a run is the operator (open mode, the stdio launcher).
	if _, refused := AuthorizeDefOp(compat, "agent", "create", "x"); refused {
		t.Error("open mode was refused")
	}
	// Inside a run: the run's set, not whatever principal the ctx carries.
	inRun := WithRunID(UpdateRunPolicy(auth.WithPrincipal(compat, auth.Principal{TenantID: "t", Subject: "u"}),
		func(p *RunPolicy) { p.ACL = deny }), "run_1")
	if _, refused := AuthorizeDefOp(inRun, "agent", "create", "x"); !refused {
		t.Error("the run's ACL did not refuse an in-run write")
	}
	// A run with no set: compat decides as before, enforce refuses as
	// unavailable.
	bare := WithRunID(compat, "run_1")
	if _, refused := AuthorizeDefOp(bare, "agent", "create", "x"); refused {
		t.Error("compat with no set refused")
	}
	enforced := WithRunID(modeCtx(authz.ModeEnforce), "run_1")
	r, refused := AuthorizeDefOp(enforced, "agent", "create", "x")
	if !refused || refusalCode(t, r) != "acl_unavailable" || r.Error == nil || !r.Error.Retryable {
		t.Errorf("enforce with no set: refused=%v %s", refused, r.Text)
	}
}

func TestAuthorizeDefOp_ReportsToTheStampedRecorder(t *testing.T) {
	deny := compiledSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"retire"}, Resources: []string{"def:hook/h1"}})
	var got []string
	ctx := WithACLRecorder(WithRunID(UpdateRunPolicy(modeCtx(authz.ModeCompat), func(p *RunPolicy) { p.ACL = deny }), "run_1"),
		func(_ context.Context, event string, s authz.Surface, _ authz.Decision) {
			got = append(got, event+" "+s.Action+" "+s.Type+":"+s.Name)
		})
	if _, refused := AuthorizeDefOp(ctx, "hook", "retire", "h1"); !refused {
		t.Fatal("not refused")
	}
	if len(got) != 1 || got[0] != "acl_denied retire def:hook/h1" {
		t.Errorf("recorded %v", got)
	}
}
