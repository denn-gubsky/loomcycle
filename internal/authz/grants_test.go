package authz

import (
	"context"
	"testing"
)

// `review` is a document verb no baseline but the operators' holds: a member
// holds it only by a rule naming it, and the per-statement question is answered
// by that rule alone, whatever the rollout mode.

func TestCatalog_ReviewIsADocumentVerb(t *testing.T) {
	if !ValidVerb("document", "review") {
		t.Fatal("review is not a document verb")
	}
}

func TestBaselines_ReviewOnlyForOperators(t *testing.T) {
	member := Subject{TenantID: "acme", ID: "alice"}
	for name, want := range map[string]Outcome{
		BaselineAdmin: Allowed, BaselineTenantOperator: Allowed,
		BaselineMember: NoMatch, BaselineIsolated: Denied,
	} {
		acl := Baselines()[name]
		e := compile(t, member, []Binding{{ACL: name, SubjectKind: SubjectUser, SubjectID: "alice"}}, acl)
		if got := ask(e, "review", "document", "tenant:/board/a"); got != want {
			t.Errorf("%s: review document:tenant:/board/a = %v, want %v", name, got, want)
		}
	}
}

func TestGrants_OnlyAnExplicitAllowGrantsInEveryMode(t *testing.T) {
	alice := Subject{TenantID: "acme", ID: "alice"}
	board := mustACL(t, `{"name":"board-review","rules":[
		{"effect":"allow","actions":["review"],"resources":["document:tenant:/board/**"]},
		{"effect":"deny","actions":["review"],"resources":["document:tenant:/board/secret/**"]}]}`)
	e := compile(t, alice, []Binding{{ACL: "board-review", SubjectKind: SubjectUser, SubjectID: "alice"}}, board)
	for _, mode := range []Mode{ModeCompat, ModeShadow, ModeEnforce} {
		ctx := WithMode(context.Background(), mode)
		for name, want := range map[string]bool{
			"tenant:/board/reg/a":    true,
			"tenant:/board/secret/x": false, // the deny wins
			"tenant:/kb/notes":       false, // no rule: compat's scope answer does not grant it
		} {
			if got := Grants(ctx, e, "review", Resource{Type: "document", Name: name}); got != want {
				t.Errorf("%s: Grants(review %s) = %v, want %v", mode, name, got, want)
			}
		}
		if Grants(ctx, nil, "review", Resource{Type: "document", Name: "tenant"}) {
			t.Errorf("%s: no set granted review", mode)
		}
	}
	// Another tenant's document is never granted.
	if Grants(context.Background(), e, "review", Resource{Type: "document", Name: "tenant:/board/a", TenantID: "other"}) {
		t.Error("review was granted across tenants")
	}
}

func TestGrantsSomewhere_NeedsAnAllowOrAConditionalRule(t *testing.T) {
	alice := Subject{TenantID: "acme", ID: "alice"}
	none := mustACL(t, `{"name":"read-only","rules":[{"effect":"allow","actions":["read"],"resources":["document:tenant"]}]}`)
	some := mustACL(t, `{"name":"board","rules":[{"effect":"allow","actions":["review"],"resources":["document:tenant:/board/**"]}]}`)
	if GrantsSomewhere(compile(t, alice, []Binding{{ACL: "read-only", SubjectKind: SubjectUser, SubjectID: "alice"}}, none), "document", "review") {
		t.Error("a set with no review rule holds review somewhere")
	}
	if !GrantsSomewhere(compile(t, alice, []Binding{{ACL: "board", SubjectKind: SubjectUser, SubjectID: "alice"}}, some), "document", "review") {
		t.Error("a set granting review on a path holds it nowhere")
	}
	if GrantsSomewhere(nil, "document", "review") {
		t.Error("no set holds review somewhere")
	}
}
