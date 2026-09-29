package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A promoted team whose entry is a Starter is walked by the subscription sweep
// with no caller on ctx, so the active pointer records the promoter's
// confinement for those walks. Every way of setting the pointer — create's
// default promote, fork with promote, op=promote — must capture it, from the
// same two sources a schedule's author is read from.

// activePromoter reads the capture on (tenant, name)'s active pointer.
func activePromoter(t *testing.T, st store.Store, tenant, name string) *store.TeamDefPromoter {
	t.Helper()
	names, err := st.TeamDefListNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n.TenantID == tenant && n.Name == name {
			if n.ActiveDefID == "" {
				t.Fatalf("team %s/%s has no active pointer", tenant, name)
			}
			return n.ActivePromoter
		}
	}
	t.Fatalf("team %s/%s not listed", tenant, name)
	return nil
}

func TestTeamDef_EveryPromoteCapturesThePromotersConfinement(t *testing.T) {
	// The tenant's own operator authors each team unpromoted; the case under
	// test is only ever the PROMOTER.
	author := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_author", TenantID: "acme"})
	paths := map[string]func(t *testing.T, tool *TeamDef, promoter context.Context, name string){
		"create": func(t *testing.T, tool *TeamDef, promoter context.Context, name string) {
			createTeam(t, tool, promoter, name, validTeamGraph) // create promotes by default
		},
		"fork": func(t *testing.T, tool *TeamDef, promoter context.Context, name string) {
			res, _ := tool.Execute(author, json.RawMessage(`{"op":"create","name":"`+name+`","promote":false,"overlay":`+validTeamGraph+`}`))
			if res.IsError {
				t.Fatalf("create: %s", res.Text)
			}
			parent, _ := decodeResult(t, res.Text)["def_id"].(string)
			if res, _ := tool.Execute(promoter, json.RawMessage(`{"op":"fork","name":"`+name+`","parent_def_id":"`+parent+`","promote":true,"description":"v2"}`)); res.IsError {
				t.Fatalf("fork: %s", res.Text)
			}
		},
		"promote": func(t *testing.T, tool *TeamDef, promoter context.Context, name string) {
			res, _ := tool.Execute(author, json.RawMessage(`{"op":"create","name":"`+name+`","promote":false,"overlay":`+validTeamGraph+`}`))
			if res.IsError {
				t.Fatalf("create: %s", res.Text)
			}
			defID, _ := decodeResult(t, res.Text)["def_id"].(string)
			if res, _ := tool.Execute(promoter, json.RawMessage(`{"op":"promote","def_id":"`+defID+`"}`)); res.IsError {
				t.Fatalf("promote: %s", res.Text)
			}
		},
	}
	for path, promote := range paths {
		for _, c := range authorCtxCases() {
			t.Run(path+"/"+c.name, func(t *testing.T) {
				tool, _, cleanup := teamDefFixture(t)
				defer cleanup()
				tool.OperatorKeyGate = func() bool { return true }

				promote(t, tool, c.apply(context.Background()), "armed")
				got := activePromoter(t, tool.Store, "acme", "armed")
				want := store.TeamDefPromoter{OperatorKeyRestricted: c.wantRestricted, Isolated: c.wantIsolated}
				if got == nil || *got != want {
					t.Errorf("captured %+v, want %+v", got, want)
				}
			})
		}
	}
}

// With the deployment gate off nobody is denied the operator's key, so a
// promote by a principal who would be restricted under the gate captures no
// operator-key restriction; its isolation, which has no gate, still holds.
func TestTeamDef_PromoteCaptureFollowsTheOperatorKeyGate(t *testing.T) {
	restricted := authorCtxCases()[2] // restricted principal off-run
	for _, gateOn := range []bool{false, true} {
		tool, _, cleanup := teamDefFixture(t)
		tool.OperatorKeyGate = func() bool { return gateOn }
		createTeam(t, tool, restricted.apply(context.Background()), "armed", validTeamGraph)
		got := activePromoter(t, tool.Store, "acme", "armed")
		want := store.TeamDefPromoter{OperatorKeyRestricted: gateOn, Isolated: true}
		if got == nil || *got != want {
			t.Errorf("gate on=%v: captured %+v, want %+v", gateOn, got, want)
		}
		cleanup()
	}
}

// The capture is authority, not content: promoting — by a confined caller or
// not — leaves the def's content hash exactly as authored.
func TestTeamDef_PromoteLeavesTheContentHashUnchanged(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.OperatorKeyGate = func() bool { return true }
	author := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_author", TenantID: "acme"})
	res, _ := tool.Execute(author, json.RawMessage(`{"op":"create","name":"hashed","promote":false,"overlay":`+validTeamGraph+`}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	created := decodeResult(t, res.Text)
	defID, _ := created["def_id"].(string)
	before, _ := created["content_sha256"].(string)

	for _, c := range authorCtxCases()[:3] { // the confined promoters
		if res, _ := tool.Execute(c.apply(context.Background()), json.RawMessage(`{"op":"promote","def_id":"`+defID+`"}`)); res.IsError {
			t.Fatalf("promote (%s): %s", c.name, res.Text)
		}
		row, err := tool.Store.TeamDefGet(context.Background(), defID)
		if err != nil {
			t.Fatal(err)
		}
		if row.ContentSHA256 != before || before == "" {
			t.Errorf("after a promote by %s: content_sha256 = %q, want the authored %q", c.name, row.ContentSHA256, before)
		}
	}
	if got := activePromoter(t, tool.Store, "acme", "hashed"); got == nil || !got.OperatorKeyRestricted || !got.Isolated {
		t.Errorf("the promotes did not capture a confined promoter: %+v", got)
	}
}
