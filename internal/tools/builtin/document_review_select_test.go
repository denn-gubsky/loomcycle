package builtin

import (
	"context"
	"testing"
)

// Selecting a summary group exactly: what the queue's summary counts, a
// filter must select, statement for statement.

// setJudgeAttributes writes a statement's attributes as the runtime's judge
// write would (a writer may not send attributes.judge).
func setJudgeAttributes(t *testing.T, d *Document, ctx context.Context, id, attrs string) {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "tenant")
	if err == nil {
		err = d.exec(ctx, key, `UPDATE chunk_edges SET attributes = ? WHERE id = ?`, attrs, id)
	}
	if err != nil {
		t.Fatalf("set attributes of %s: %v", id, err)
	}
}

// A stored judge verdict of "" is no verdict on both tiers: the summary files
// it under "" (no verdict), so has_judge_verdict=false must select it and
// has_judge_verdict=true must not.
func TestListStatements_EmptyJudgeVerdictIsNoVerdict(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedReviewCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		setJudgeAttributes(t, d, op, c.repeals, `{"judge":{"verdict":""}}`)
		none := listIDs(t, d, op, `{"op":"list_statements","scope":"tenant","has_judge_verdict":false}`)
		if !none[c.repeals] || len(none) != 3 {
			t.Errorf("has_judge_verdict=false = %v, want the empty verdict (%s) among the 3 without one", none, c.repeals)
		}
		some := listIDs(t, d, op, `{"op":"list_statements","scope":"tenant","has_judge_verdict":true}`)
		if some[c.repeals] || len(some) != 1 {
			t.Errorf("has_judge_verdict=true = %v, want only the recorded verdict", some)
		}
		out, res := docExec(t, d, op, `{"op":"list_statements","scope":"tenant","summarize":true}`)
		if res.IsError {
			t.Fatal(res.Text)
		}
		noVerdict := 0
		for _, g := range out["summary"].([]any) {
			if m := g.(map[string]any); m["judge_verdict"] == "" {
				noVerdict += int(m["count"].(float64))
			}
		}
		if noVerdict != len(none) {
			t.Errorf("the summary counts %d without a verdict, the filter selects %d", noVerdict, len(none))
		}
	})
}
