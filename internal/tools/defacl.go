package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EZ §1.2 — a definition write asks its own verb of the definition it
// names. Every `_<family>def` route and RPC carries the op in its body, so
// the transport gates can only ask the family's weakest verb (`read
// def:<fam>/*`); the tool, which every transport and every in-run call
// reaches, asks the op's question here. Without it no ACL could stop a
// definition write.

// DefOpAction is the verb op asks of the definition it names: read for the
// ops that only look, retire for the ones that take a definition out of
// service, and author for every other op — fail closed, so an op added to a
// tool later is asked author even if nobody adds it here. "" for an op that
// names no definition: a team walk's run, poll and cancel, which are run
// operations, not definition ones.
func DefOpAction(family, op string) string {
	switch op {
	case "get", "list", "verify", "render_diagram":
		return "read"
	case "retire", "delete", "purge":
		return "retire"
	case "run", "poll", "cancel":
		if family == "team" {
			return ""
		}
	}
	return "author"
}

// ACLRecordFunc records a refusal or a shadow divergence of a question a
// tool asked: the audit sink of the surface that stamped it.
type ACLRecordFunc func(ctx context.Context, event string, s authz.Surface, d authz.Decision)

type ctxKeyACLRecorder struct{}

// WithACLRecorder stamps the recorder a tool's ACL question reports to.
func WithACLRecorder(ctx context.Context, fn ACLRecordFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyACLRecorder{}, fn)
}

// defOpSet is the compiled set a definition op is judged by, and whether
// there is anyone to judge. Inside a run it is the run's own (RunPolicy.ACL —
// the originating principal's, inherited by every hop), whatever principal
// the ctx also carries; nil there is decided by the mode (enforce refuses).
// Off a run it is the calling principal's; no principal is open mode or the
// stdio MCP launcher — the operator — and nothing is asked, as the acldef
// operations do.
func defOpSet(ctx context.Context) (*authz.Effective, bool) {
	if RunID(ctx) != "" {
		return PolicyOf(ctx).ACL, true
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil, false
	}
	return p.Effective, true
}

// AuthorizeDefOp asks op's verb (DefOpAction) of def:<family>/<name> for
// each name, and of def:<family>/* when a name is "" or none is given — a
// definition the caller could not name is judged by the family-wide
// question, which only a rule covering the whole family answers. It
// returns the refusal and true, or a zero Result and false when the op may
// proceed.
func AuthorizeDefOp(ctx context.Context, family, op string, names ...string) (Result, bool) {
	action := DefOpAction(family, op)
	if action == "" {
		return Result{}, false
	}
	return authorizeInstances(ctx, action, "def", family+"/", names)
}

// authorizeInstances asks action of typ:<prefix><name> for each name, and of
// typ:<prefix>* for a name that is "" or when none is given, against the set
// defOpSet picks. It returns the first refusal and true, or a zero Result and
// false when every question is admitted.
func authorizeInstances(ctx context.Context, action, typ, prefix string, names []string) (Result, bool) {
	eff, judged := defOpSet(ctx)
	if !judged {
		return Result{}, false
	}
	if len(names) == 0 {
		names = []string{""}
	}
	asked := map[string]bool{}
	for _, name := range names {
		if name == "" {
			name = "*"
		}
		if asked[name] {
			continue
		}
		asked[name] = true
		surface := authz.Surface{Action: action, Type: typ, Name: prefix + name}
		if r, refused := judgeDefOp(ctx, eff, surface); refused {
			return r, true
		}
	}
	return Result{}, false
}

// aclRefusal is the refusal body, field for field the one the HTTP gate
// writes, so every surface carries one code: acl_denied, or acl_unavailable
// when enforce mode had no set to decide with.
type aclRefusal struct {
	Code     string         `json:"code"`
	Error    string         `json:"error"`
	Action   string         `json:"action,omitempty"`
	Resource string         `json:"resource,omitempty"`
	Reasons  []authz.Reason `json:"reasons,omitempty"`
}

func judgeDefOp(ctx context.Context, eff *authz.Effective, s authz.Surface) (Result, bool) {
	res := authz.EvaluateSurface(ctx, eff, s, true)
	switch {
	case res.Unavailable:
		recordDefOp(ctx, eff, "acl_unavailable", s, res.Decision)
		msg := "access control could not be evaluated; retry"
		return aclRefusalResult(aclRefusal{Code: "acl_unavailable", Error: msg},
			&ErrorInfo{Category: CategoryTransient, Retryable: true, Description: msg}), true
	case res.Diverged:
		recordDefOp(ctx, eff, "acl_shadow_diverged", s, res.Decision)
	case !res.Allowed:
		recordDefOp(ctx, eff, "acl_denied", s, res.Decision)
		msg := aclDeniedText(s, res.Decision)
		return aclRefusalResult(aclRefusal{Code: "acl_denied", Error: msg, Action: s.Action, Resource: s.Type + ":" + s.Name, Reasons: res.Decision.Reasons},
			&ErrorInfo{Category: CategoryPermission, Description: msg}), true
	}
	return Result{}, false
}

// ACLRefusalCode reports whether a failed result is a refusal of
// AuthorizeDefOp, and its code: acl_denied or acl_unavailable. A transport
// that answers in status codes (HTTP) answers it as the route gate does.
func ACLRefusalCode(text string, info *ErrorInfo) (string, bool) {
	if info == nil || (info.Category != CategoryPermission && info.Category != CategoryTransient) {
		return "", false
	}
	var b aclRefusal
	if json.Unmarshal([]byte(text), &b) != nil {
		return "", false
	}
	switch b.Code {
	case "acl_denied", "acl_unavailable":
		return b.Code, true
	}
	return "", false
}

func aclRefusalResult(body aclRefusal, info *ErrorInfo) Result {
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(body.Error)
	}
	return Result{Text: string(text), IsError: true, Error: info}
}

// aclDeniedText is the HTTP gate's wording of a refusal.
func aclDeniedText(s authz.Surface, d authz.Decision) string {
	if d.Outcome == authz.Denied && len(d.Reasons) > 0 {
		return fmt.Sprintf("%s %s:%s is denied by %s rule %d", s.Action, s.Type, s.Name, d.Reasons[0].ACL, d.Reasons[0].RuleIndex)
	}
	return fmt.Sprintf("no rule allows %s %s:%s", s.Action, s.Type, s.Name)
}

// recordDefOp reports to the stamped recorder, or logs when none is (a run
// started where no recorder was stamped). Names and ids only.
func recordDefOp(ctx context.Context, eff *authz.Effective, event string, s authz.Surface, d authz.Decision) {
	if fn, ok := ctx.Value(ctxKeyACLRecorder{}).(ACLRecordFunc); ok {
		fn(ctx, event, s, d)
		return
	}
	var tenant, subject string
	if eff != nil {
		tenant, subject = eff.Subject.TenantID, eff.Subject.ID
	}
	slog.Warn("authz: "+event, "tenant", tenant, "subject", subject, "action", s.Action,
		"resource", s.Type+":"+s.Name, "outcome", d.Outcome.String())
}
