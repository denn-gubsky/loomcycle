package authz

import (
	"math/rand"
	"testing"
)

// A set held to a ceiling never allows what the ceiling does not: for random
// pairs of worlds, every answer of cur.Within(start) is the meet of the two —
// a deny from either, an allow only from both, and the same through Quick,
// DecideScope and under every mode's Resolve. This is the narrow-only rule a
// live run (LOOMCYCLE_ACL_LIVE) relies on.
func TestWithin_IsTheMeetOfBothSets(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	subj := Subject{TenantID: "t", ID: "alice", Groups: []string{"g1"}}
	for iter := 0; iter < 300; iter++ {
		startACLs, startB := randomWorld(r)
		curACLs, curB := randomWorld(r)
		start := compile(t, subj, startB, startACLs...)
		cur := compile(t, subj, curB, curACLs...)
		live := cur.Within(start)
		for k, req := range propertyRequests {
			req.Subject = subj
			req.Context = Context{Transport: "http", Now: testNow}
			a, b, got := start.Decide(req), cur.Decide(req), live.Decide(req)
			want := NoMatch
			switch {
			case a.Outcome == Denied || b.Outcome == Denied:
				want = Denied
			case a.Outcome == Allowed && b.Outcome == Allowed:
				want = Allowed
			}
			if got.Outcome != want {
				t.Fatalf("iteration %d request %d: Within = %v, start %v, current %v", iter, k, got.Outcome, a.Outcome, b.Outcome)
			}
			for _, mode := range []Mode{ModeCompat, ModeEnforce} {
				if Resolve(mode, got, true) && !(Resolve(mode, a, true) && Resolve(mode, b, true)) {
					t.Fatalf("iteration %d request %d %s: Within allows what one set refuses", iter, k, mode)
				}
			}
			// Quick builds an empty context; compare against Decide with one.
			qreq := req
			qreq.Context = Context{}
			if q, d := live.Quick(req.Resource.Type, req.Action, req.Resource.Name), live.Decide(qreq).Outcome; q != d {
				t.Fatalf("iteration %d request %d: Quick %v, Decide %v", iter, k, q, d)
			}
		}
	}
}

// The live set reports the CURRENT subject and version (what Context op=self
// shows) while its denies — both sets' — are what DenyPatterns hands the
// skill matcher.
func TestWithin_KeepsTheCurrentIdentityAndBothSetsDenies(t *testing.T) {
	subj := Subject{TenantID: "t", ID: "alice"}
	startA := mustACL(t, `{"name":"s","rules":[{"effect":"deny","actions":["invoke"],"resources":["skill:old-*"]}]}`)
	curA := mustACL(t, `{"name":"c","rules":[{"effect":"deny","actions":["invoke"],"resources":["skill:new-*"]}]}`)
	start := compile(t, subj, []Binding{{ACL: "s", SubjectKind: SubjectTenant}}, startA)
	cur := compile(t, Subject{TenantID: "t", ID: "alice", Groups: []string{"g9"}}, []Binding{{ACL: "c", SubjectKind: SubjectTenant}}, curA)
	cur.Version = 9
	live := cur.Within(start)
	if live.Version != 9 || len(live.Subject.Groups) != 1 {
		t.Errorf("live set reports version %d groups %v, want the current set's", live.Version, live.Subject.Groups)
	}
	got := map[string]bool{}
	for _, p := range live.DenyPatterns("skill", "invoke") {
		got[p] = true
	}
	if !got["old-*"] || !got["new-*"] {
		t.Errorf("DenyPatterns = %v, want both sets' denies", got)
	}
	if cur.Within(nil) != cur || cur.Within(cur) != cur {
		t.Error("Within(nil) and Within(self) must return the set itself")
	}
	// Held twice, a set answers to both ceilings.
	denyRead := compile(t, subj, []Binding{{ACL: "r", SubjectKind: SubjectTenant}},
		mustACL(t, `{"name":"r","rules":[{"effect":"deny","actions":["invoke"],"resources":["tool:Read"]}]}`))
	if ask(cur.Within(start).Within(denyRead), "invoke", "skill", "old-x") != Denied ||
		ask(cur.Within(start).Within(denyRead), "invoke", "tool", "Read") != Denied {
		t.Error("a set held twice lost one of its ceilings")
	}
	// A scope question: the ceiling's deny on part of the scope refuses it.
	memStart := compile(t, subj, []Binding{{ACL: "m", SubjectKind: SubjectTenant}},
		mustACL(t, `{"name":"m","rules":[{"effect":"deny","actions":["read"],"resources":["document:tenant:/hr/**"]}]}`))
	memCur := compile(t, subj, []Binding{{ACL: "a", SubjectKind: SubjectTenant}},
		mustACL(t, `{"name":"a","rules":[{"effect":"allow","actions":["read"],"resources":["document:tenant"]}]}`))
	req := Request{Subject: subj, Action: "read", Resource: Resource{Type: "document", Name: "tenant", TenantID: "t"}, Context: Context{Now: testNow}}
	if d := memCur.Within(memStart).DecideScope(req); d.Outcome != Denied {
		t.Errorf("DecideScope through the ceiling = %v, want the ceiling's deny", d.Outcome)
	}
	if !memCur.Within(memStart).HasInstanceRules("document", "tenant") {
		t.Error("HasInstanceRules must see the ceiling's instance rule")
	}
}
