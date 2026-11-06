package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A review policy or a judge rules only on a proposal. A machine ruling that read
// its candidates before an operator rejected one must not accept that rejection
// afterwards: an operator's rejection is a tombstone, and only a person reverses it.
func TestApplyRuling_APolicyCannotOverturnAnOperatorsRejection(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		id := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		if e := f.edge(id); e["status"] != edgeStatusProposed {
			t.Fatalf("filed = %v, want proposed", e)
		}
		if _, r := docExec(t, d, operatorCtx(f.op), fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"not an elaboration"}`, id)); r.IsError {
			t.Fatalf("reject: %s", r.Text)
		}
		key, mscope, err := d.resolveScope(f.op, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		for _, by := range []string{"policy:1@3", "judge:run-judge"} {
			r, err := d.applyRuling(f.op, key, mscope, "accept_statement", id, true, "", by)
			if err != nil || r.code != rulingNotProposal {
				t.Errorf("%s accepting an operator's rejection = %+v %v, want not_a_proposal", by, r, err)
			}
		}
		e := f.edge(id)
		if e["status"] != edgeStatusRejected || e["operator_ruled_at"] == nil || !strings.HasPrefix(asStr(e["ruled_by"]), "operator") {
			t.Errorf("after the machine rulings = %v, want the operator's rejection kept", e)
		}
		// A person may still reverse it.
		if _, r := docExec(t, d, operatorCtx(f.op), fmt.Sprintf(`{"op":"accept_statement","scope":"tenant","id":%q}`, id)); r.IsError {
			t.Errorf("an operator's accept of a rejection was refused: %s", r.Text)
		}
	})
}
