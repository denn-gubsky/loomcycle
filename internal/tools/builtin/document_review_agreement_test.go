package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The agreement report counts every acceptance the judge made, but only the
// audit sample toward the rate: an acceptance a person settled unprompted is
// accepted, not sampled. Settled sample entries keep counting once a person's
// ruling has re-attributed the statement.
func TestJudgeAgreement_CountsTheAuditSampleOnly(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		md := strings.Replace(judgeOntologyMD, "judge_floor: 0.85\n", "judge_floor: 0.85\njudge_audit: 1\n", 1)
		f := newPolicyFixture(t, d, base, md)
		a := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		b := f.propose(f.oneB, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		c := f.propose(f.two, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		r := f.propose(f.one, f.two, "repeals", `,"method":"extractor:relate"`)["id"].(string)
		if _, res := judge(t, d, judgeRunCtx(base, nil), "", judgement(a, "accept", 0.95), judgement(b, "accept", 0.95),
			judgement(c, "accept", 0.95), judgement(r, "accept", 0.95)); res.IsError {
			t.Fatal(res.Text)
		}
		// The repeal stands as an acceptance nobody sampled.
		key, _, err := d.resolveScope(f.op, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		if err := d.exec(f.op, key, `UPDATE chunk_edges SET attributes = ? WHERE id = ?`, `{"judge":{"verdict":"accept","confidence":0.95}}`, r); err != nil {
			t.Fatal(err)
		}
		op := operatorCtx(f.op)
		for _, body := range []string{
			fmt.Sprintf(`{"op":"accept_statement","scope":"tenant","id":%q}`, a),
			fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"another article"}`, b),
			fmt.Sprintf(`{"op":"accept_statement","scope":"tenant","id":%q}`, r),
		} {
			if _, res := docExec(t, d, op, body); res.IsError {
				t.Fatalf("%s: %s", body, res.Text)
			}
		}

		got, err := d.JudgeAgreement(op, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Kinds) != 2 || got.Kinds[0].Kind != "elaborates" || got.Kinds[1].Kind != "repeals" {
			t.Fatalf("kinds = %+v", got.Kinds)
		}
		el, rp := got.Kinds[0], got.Kinds[1]
		if el.Accepted != 3 || el.Sampled != 3 || el.Pending != 1 || el.Confirmed != 1 || el.Overturned != 1 || el.Agreement == nil || *el.Agreement != 0.5 {
			t.Errorf("elaborates = %+v, want 3 accepted, all sampled: 1 pending, 1 confirmed, 1 overturned, agreement 0.5", el)
		}
		if rp.Accepted != 1 || rp.Sampled != 0 || rp.Confirmed != 0 || rp.Agreement != nil {
			t.Errorf("repeals = %+v, want 1 accepted and nothing sampled: a confirmation nobody was asked for is not counted", rp)
		}
		if got.Total.Accepted != 4 || got.Total.Sampled != 3 || got.Total.Agreement == nil || *got.Total.Agreement != 0.5 {
			t.Errorf("total = %+v", got.Total)
		}
		if _, err := d.JudgeAgreement(f.run, "tenant"); err == nil {
			t.Error("a run read the agreement report")
		}
	})
}
