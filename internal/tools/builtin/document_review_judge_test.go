package builtin

import (
	"context"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A judge's ruling (ruled_by judge:<run>) is a machine's verdict, as a review
// policy's is: judged_by judge and no operator mark, for the accept as for the
// reject. Its rejection is no tombstone: a later write of the relation reopens
// it as a proposal, and the verdict it carried — a verdict on the proposal as
// it was — is dropped, so the reopened one is judged afresh.
func TestJudgeRuling_IsAVerdictNotAnOperatorRuling(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, reviewOntologyMD)
		key, mscope, err := d.resolveScope(f.op, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		acc := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate","confidence":0.7`)["id"].(string)
		rej := f.propose(f.notes, f.two, "elaborates", `,"method":"extractor:relate","confidence":0.7`)["id"].(string)
		if err := d.exec(f.op, key, `UPDATE chunk_edges SET attributes = ? WHERE id = ?`,
			`{"judge":{"verdict":"reject","confidence":0.95}}`, rej); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			id     string
			accept bool
			want   string
		}{{acc, true, edgeStatusAsserted}, {rej, false, edgeStatusRejected}} {
			op := "reject_statement"
			if c.accept {
				op = "accept_statement"
			}
			r, err := d.applyRuling(f.op, key, mscope, op, c.id, c.accept, "the judge found it unsupported", "judge:run-judge")
			if err != nil || r.refusal != nil {
				t.Fatalf("%s: %v %v", op, err, r.refusal)
			}
			e := f.edge(c.id)
			if e["status"] != c.want || e["ruled_by"] != "judge:run-judge" || e["judged_by"] != "judge" || e["operator_ruled_at"] != nil {
				t.Errorf("%s by the judge = %v, want %s, judged_by judge, no operator mark", op, e, c.want)
			}
		}
		later, res := docExec(t, d, tools.WithRunID(f.run, "run-later"), fmt.Sprintf(
			`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"elaborates","method":"extractor:relate","confidence":0.7}`, f.notes, f.two))
		if res.IsError {
			t.Fatalf("a judge's rejection refused a later proposal: %s", res.Text)
		}
		e := f.edge(rej)
		if later["id"] != rej || e["status"] != edgeStatusProposed || e["judged_by"] != nil {
			t.Errorf("the later proposal = %v, statement %v: want the judged one reopened as a proposal", later, e)
		}
		if attrs, _ := e["attributes"].(map[string]any); attrs[attrJudge] != nil {
			t.Errorf("the reopened proposal kept the old verdict: %v", attrs)
		}
	})
}
