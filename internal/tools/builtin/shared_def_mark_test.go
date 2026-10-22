package builtin

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A tenant caller's get and list return two tiers of AgentDef, SkillDef and
// TeamDef rows: its own tenant's, and the shared "" tier it may read and fork
// but not retire or promote. Both tiers can hold a "version 1" of the same
// name, so a shared row says so with "shared": true; an own row carries no
// such field, which keeps the change additive (RFC ES C3).

// sharedMarks returns, for each def_id get returns and each def_id list
// returns for name, whether the row carries "shared": true. A row whose
// "shared" field is present but not true fails the test: the field is
// either true or absent.
func sharedMarks(t *testing.T, tool tools.Tool, ctx context.Context, name string, defIDs ...string) (byGet, byList map[string]bool) {
	t.Helper()
	marked := func(row map[string]any) bool {
		v, present := row["shared"]
		if present && v != true {
			t.Errorf("row %v carries shared=%v, want true or absent", row["def_id"], v)
		}
		return present
	}
	byGet = map[string]bool{}
	for _, id := range defIDs {
		res := execDef(t, tool, ctx, `{"op":"get","def_id":"`+id+`"}`)
		if res.IsError {
			t.Fatalf("get %s: %s", id, res.Text)
		}
		byGet[id] = marked(decodeResult(t, res.Text))
	}
	res := execDef(t, tool, ctx, `{"op":"list","name":"`+name+`"}`)
	if res.IsError {
		t.Fatalf("list %s: %s", name, res.Text)
	}
	byList = map[string]bool{}
	versions, _ := decodeResult(t, res.Text)["versions"].([]any)
	for _, v := range versions {
		row, _ := v.(map[string]any)
		id, _ := row["def_id"].(string)
		byList[id] = marked(row)
	}
	return byGet, byList
}

func TestDefTools_GetAndListMarkOnlySharedTierRows(t *testing.T) {
	callers := map[string]func(base context.Context) context.Context{
		"tenant run": func(base context.Context) context.Context {
			ctx := auth.WithPrincipal(asTenant(base, "acme"), auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant}})
			return tools.WithRunID(ctx, "r_tenant")
		},
		"admin run": func(base context.Context) context.Context {
			return inAdminRun(adminOf(asTenant(base, "acme"), "acme"))
		},
		"tenant operator off-run": func(base context.Context) context.Context {
			return auth.WithPrincipal(asTenant(base, "acme"), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})
		},
	}
	for _, k := range forkKinds()[:3] { // AgentDef, SkillDef, TeamDef
		for who, caller := range callers {
			t.Run(k.kind+" "+who, func(t *testing.T) {
				tool, base, done := k.fixture(t)
				defer done()
				// The same name in both tiers: each is that tier's version 1.
				sharedID := createDefFor(t, tool, asTenant(base, ""), "both-tiers", k.create)
				ownID := createDefFor(t, tool, asTenant(base, "acme"), "both-tiers", k.create)

				byGet, byList := sharedMarks(t, tool, caller(base), "both-tiers", sharedID, ownID)
				if !byGet[sharedID] || byGet[ownID] {
					t.Errorf("get marks = %v, want shared only on %s", byGet, sharedID)
				}
				if len(byList) != 2 || !byList[sharedID] || byList[ownID] {
					t.Errorf("list marks = %v, want both rows and shared only on %s", byList, sharedID)
				}
			})
		}
	}
}

// An off-run admin reads every tenant's rows; only the "" tier is marked, so
// another tenant's row is told apart from the shared one by tenant, not by the
// mark.
func TestDefTools_OffRunAdminSeesTheSharedMarkOnlyOnTheSharedTier(t *testing.T) {
	for _, k := range forkKinds()[:3] {
		t.Run(k.kind, func(t *testing.T) {
			tool, base, done := k.fixture(t)
			defer done()
			sharedID := createDefFor(t, tool, asTenant(base, ""), "everywhere", k.create)
			acmeID := createDefFor(t, tool, asTenant(base, "acme"), "everywhere", k.create)
			globexID := createDefFor(t, tool, asTenant(base, "globex"), "everywhere", k.create)

			admin := adminOf(asTenant(base, "acme"), "acme")
			byGet, byList := sharedMarks(t, tool, admin, "everywhere", sharedID, acmeID, globexID)
			want := map[string]bool{sharedID: true, acmeID: false, globexID: false}
			for id, w := range want {
				if byGet[id] != w {
					t.Errorf("get %s shared = %v, want %v", id, byGet[id], w)
				}
				if got, listed := byList[id]; !listed || got != w {
					t.Errorf("list %s: listed=%v shared=%v, want listed and shared=%v", id, listed, got, w)
				}
			}
		})
	}
}

// A caller whose own tenant is "" — open mode, the stdio MCP server, a run a
// yaml schedule or webhook fired — owns the "" tier: it may retire and promote
// those rows, so none of them is marked shared.
func TestDefTools_ACallerInTheSharedTenantSeesNoSharedMark(t *testing.T) {
	for _, k := range forkKinds()[:3] {
		t.Run(k.kind, func(t *testing.T) {
			tool, base, done := k.fixture(t)
			defer done()
			open := tools.WithUnauthenticatedOperator(asTenant(base, ""))
			id := createDefFor(t, tool, open, "open-mode", k.create)
			byGet, byList := sharedMarks(t, tool, open, "open-mode", id)
			if byGet[id] || byList[id] {
				t.Errorf("open-mode caller sees its own row marked shared: get=%v list=%v", byGet, byList)
			}
			if res := execDef(t, tool, open, `{"op":"promote","def_id":"`+id+`"}`); res.IsError {
				t.Errorf("open-mode caller cannot promote its own row: %s", res.Text)
			}
		})
	}
}
