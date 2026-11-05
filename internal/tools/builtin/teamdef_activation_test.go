package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A member may not make its version of an operator's team the active one, nor
// retire or delete the operator's (which clears the pointer for a create to
// take): the team an operator wrote keeps running as the operator wrote it. A
// member's own team is unaffected.
func TestTeamDef_AMemberCannotTakeOverAnOperatorsTeam(t *testing.T) {
	tool, _ := localTeamFixture(t)
	operator := tools.WithCallerTier(localAuthorCtx("", []string{"any"}, []string{"*"}), tools.TierOperatorPlane)
	member := tools.WithCallerTier(localAuthorCtx("", []string{"any"}, []string{"*"}), tools.TierMember)
	body := localTeam(`{"system_prompt":"review","tools":["Read"]}`)
	if res := teamOp(t, tool, operator, "create", "review", body); res.IsError {
		t.Fatalf("operator create: %s", res.Text)
	}
	before, err := tool.Store.TeamDefGetActive(context.Background(), "", "review")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(ctx context.Context, body string) tools.Result {
		res, err := tool.Execute(ctx, json.RawMessage(body))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := teamOp(t, tool, member, "create", "review", body); !res.IsError {
		t.Errorf("a member's create took over the operator's team: %s", res.Text)
	}
	if res := exec(member, `{"op":"fork","name":"review","promote":true,"overlay":{}}`); !res.IsError {
		t.Errorf("a member's fork was promoted over the operator's team: %s", res.Text)
	}
	fork := exec(member, `{"op":"fork","name":"review","overlay":{}}`)
	if fork.IsError {
		t.Fatalf("a member's inactive fork was refused: %s", fork.Text)
	}
	var f struct {
		DefID string `json:"def_id"`
	}
	_ = json.Unmarshal([]byte(fork.Text), &f)
	if res := exec(member, `{"op":"promote","def_id":"`+f.DefID+`"}`); !res.IsError {
		t.Errorf("a member promoted its fork over the operator's team: %s", res.Text)
	}
	if res := exec(member, `{"op":"retire","def_id":"`+before.DefID+`","retired":true}`); !res.IsError {
		t.Errorf("a member retired the operator's team: %s", res.Text)
	}
	if res := exec(member, `{"op":"delete","name":"review"}`); !res.IsError {
		t.Errorf("a member deleted the operator's team: %s", res.Text)
	}
	if after, err := tool.Store.TeamDefGetActive(context.Background(), "", "review"); err != nil || after.DefID != before.DefID {
		t.Errorf("the active team moved: %s → %s (%v)", before.DefID, after.DefID, err)
	}
	// The member's own team: create, fork, promote as before.
	if res := teamOp(t, tool, member, "create", "mine", body); res.IsError {
		t.Fatalf("member create: %s", res.Text)
	}
	if res := exec(member, `{"op":"fork","name":"mine","promote":true,"overlay":{}}`); res.IsError {
		t.Errorf("a member could not fork and promote its own team: %s", res.Text)
	}
}
