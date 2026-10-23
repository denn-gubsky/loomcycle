package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The management operations hold their own gates (RFC EZ §3): the route and
// RPC gates in front of them are not what keeps a member, a run, a
// restricted credential or an ACL deny out, because a transport that forgets
// one would otherwise reach the logic. These tests call the operations with
// no transport in front.

// aclCaller is a principal resolved the way the auth middleware resolves one,
// with its compiled access-control set.
func aclCaller(t *testing.T, srv *Server, tenant, subject string, scopes ...string) context.Context {
	t.Helper()
	p, err := srv.aclResolver.Resolve(context.Background(), auth.Principal{TenantID: tenant, Subject: subject, Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithTransport(authz.WithMode(context.Background(), srv.aclMode()), "grpc")
	return auth.WithPrincipal(ctx, p)
}

// aclOps is every management and authz operation, as a name and a call.
func aclOps(srv *Server) []struct {
	name string
	call func(context.Context) aclReply
	mgmt bool // false for the authz questions, which any principal may ask about itself
} {
	ask := authzEvaluationRequest{Action: &authzActionRef{Name: "run"}, Resource: &authzResourceRef{Type: "agent", ID: "chat"}}
	return []struct {
		name string
		call func(context.Context) aclReply
		mgmt bool
	}{
		{"groupdef list", func(c context.Context) aclReply { return srv.aclGroupList(c, "") }, true},
		{"groupdef get", func(c context.Context) aclReply { return srv.aclGroupGet(c, "", "support") }, true},
		{"groupdef create", func(c context.Context) aclReply { return srv.aclGroupPut(c, "", groupRequest{Name: "x"}) }, true},
		{"groupdef delete", func(c context.Context) aclReply { return srv.aclGroupDelete(c, "", "support") }, true},
		{"groupdef add_member", func(c context.Context) aclReply {
			return srv.aclGroupAddMember(c, "", "support", groupMemberRequest{Kind: "user", ID: "bob"})
		}, true},
		{"groupdef remove_member", func(c context.Context) aclReply { return srv.aclGroupRemoveMember(c, "", "support", "user", "bob") }, true},
		{"acldef list", func(c context.Context) aclReply { return srv.aclDefOp(c, "", aclDefRequest{Op: "list"}) }, true},
		{"acldef create", func(c context.Context) aclReply {
			return srv.aclDefOp(c, "", aclDefRequest{Op: "create", Name: "x", Rules: json.RawMessage(`[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`)})
		}, true},
		{"acldef names", func(c context.Context) aclReply { return srv.aclDefNames(c, "") }, true},
		{"aclbinding list", func(c context.Context) aclReply { return srv.aclBindingList(c, "", false) }, true},
		{"aclbinding create", func(c context.Context) aclReply {
			return srv.aclBindingCreate(c, "", bindingRequest{ACL: "x", SubjectKind: "user", SubjectID: "bob"})
		}, true},
		{"aclbinding delete", func(c context.Context) aclReply { return srv.aclBindingRetire(c, "", "b_none") }, true},
		{"authz evaluation", func(c context.Context) aclReply { return srv.authzEvaluation(c, "", ask) }, false},
		{"authz evaluations", func(c context.Context) aclReply {
			return srv.authzEvaluations(c, "", authzEvaluationsRequest{Evaluations: []authzEvaluationRequest{ask}})
		}, false},
		{"authz explain", func(c context.Context) aclReply { return srv.authzExplain(c, "", ask) }, false},
		{"authz simulate", func(c context.Context) aclReply {
			return srv.authzSimulate(c, "", authzSimulateRequest{Requests: []authzEvaluationRequest{ask}})
		}, false},
	}
}

func replyCode(r aclReply) string {
	raw, _ := json.Marshal(r.body)
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &b)
	return b.Code
}

func TestACLOperations_RefuseAMemberManagementButAnswerItsOwnQuestions(t *testing.T) {
	srv, _, _ := aclMgmtServer(t, authz.ModeCompat)
	member := aclCaller(t, srv, "acme", "alice", auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead)
	for _, op := range aclOps(srv) {
		r := op.call(member)
		if op.mgmt {
			if r.status != 403 || replyCode(r) != "insufficient_scope" {
				t.Errorf("%s as a member: %d %s, want 403 insufficient_scope", op.name, r.status, replyCode(r))
			}
			continue
		}
		if r.status != 200 {
			t.Errorf("%s about itself as a member: %d %+v, want 200", op.name, r.status, r.body)
		}
	}
}

func TestACLOperations_RefuseACallFromInsideARun(t *testing.T) {
	srv, _, _ := aclMgmtServer(t, authz.ModeCompat)
	inRun := tools.WithRunID(aclCaller(t, srv, "acme", "ops", auth.ScopeTenant), "r_agent")
	for _, op := range aclOps(srv) {
		if r := op.call(inRun); r.status != 403 || replyCode(r) != "operator_plane" {
			t.Errorf("%s from inside a run: %d %s, want 403 operator_plane", op.name, r.status, replyCode(r))
		}
	}
}

func TestACLOperations_RefuseACredentialLimitedToTheRunPlane(t *testing.T) {
	srv, _, _ := aclMgmtServer(t, authz.ModeCompat)
	p, err := srv.aclResolver.Resolve(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops",
		Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Agents: []string{"chat"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithPrincipal(context.Background(), p)
	for _, op := range aclOps(srv) {
		if r := op.call(ctx); r.status != 403 || replyCode(r) != "restricted_token" {
			t.Errorf("%s with a run-plane credential: %d %s, want 403 restricted_token", op.name, r.status, replyCode(r))
		}
	}
}

// The route's own question moves with the logic: a tenant operator whose ACL
// denies managing groups and authoring lists is refused those operations
// with no route gate in front, and may still read.
func TestACLOperations_AskTheRouteQuestionThemselves(t *testing.T) {
	srv, ts, st := aclMgmtServer(t, authz.ModeCompat)
	addDelegateOperator(t, st)
	lock := `[{"effect":"deny","actions":["manage"],"resources":["principal:group:*"]},{"effect":"deny","actions":["author"],"resources":["def:acl/*"]}]`
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"lock","rules":`+lock+`}`); code != 201 {
		t.Fatalf("create lock: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"lock","subject_kind":"user","subject_id":"delegate"}`); code != 201 {
		t.Fatalf("bind lock: %d %s", code, body)
	}
	delegate := aclCaller(t, srv, "acme", "delegate", auth.ScopeTenant)
	for _, c := range []struct {
		name   string
		reply  aclReply
		action string
	}{
		{"groupdef create", srv.aclGroupPut(delegate, "", groupRequest{Name: "x"}), "manage"},
		{"groupdef add_member", srv.aclGroupAddMember(delegate, "", "support", groupMemberRequest{Kind: "user", ID: "bob"}), "manage"},
		{"aclbinding create", srv.aclBindingCreate(delegate, "", bindingRequest{ACL: "x", SubjectKind: "user", SubjectID: "bob"}), "author"},
		{"aclbinding delete", srv.aclBindingRetire(delegate, "", "b_none"), "author"},
	} {
		raw, _ := json.Marshal(c.reply.body)
		if c.reply.status != 403 || replyCode(c.reply) != "acl_denied" || !strings.Contains(string(raw), `"action":"`+c.action+`"`) {
			t.Errorf("%s as a delegate denied it: %d %s, want 403 acl_denied on %s", c.name, c.reply.status, raw, c.action)
		}
	}
	if r := srv.aclGroupList(delegate, ""); r.status != 200 {
		t.Errorf("groupdef list as a delegate who may read: %d %+v", r.status, r.body)
	}
}

// On HTTP the route gate already asked the same question; the operation
// does not ask it again, so a shadow divergence is recorded once per
// request, not twice.
func TestACLRoute_AQuestionTheRouteAnsweredIsNotAskedAgain(t *testing.T) {
	srv, ts, st := aclMgmtServer(t, authz.ModeShadow)
	sink := &recordingSink{}
	srv.SetTokenAudit(sink)
	addDelegateOperator(t, st)
	lock := `[{"effect":"deny","actions":["manage"],"resources":["principal:group:*"]}]`
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"lock","rules":`+lock+`}`); code != 201 {
		t.Fatalf("create lock: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"lock","subject_kind":"user","subject_id":"delegate"}`); code != 201 {
		t.Fatalf("bind lock: %d %s", code, body)
	}
	sink.events = nil
	if code, body := bearerCall(t, ts, delegateBearer, "POST", "/v1/_groups", `{"name":"support"}`); code != 201 {
		t.Fatalf("shadow mode lets the write through: %d %s", code, body)
	}
	diverged := 0
	for _, ev := range sink.events {
		if ev.Action == "acl_shadow_diverged" {
			diverged++
		}
	}
	if diverged != 1 {
		t.Errorf("one request recorded %d shadow divergences, want 1: %+v", diverged, sink.events)
	}
}
