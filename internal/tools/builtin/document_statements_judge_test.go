package builtin

import (
	"fmt"
	"strings"
	"testing"
)

// A reference into another scope is stored as written, unverified, so it is no
// ground for a run to judge a statement supported.
func TestJudgeFact_CrossScopeEvidenceIsNoGroundForARun(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, a, b, _ := stmtDoc(t, d, ctx, "xscope")
	l, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
		"evidence":[{"scope":"tenant","natural_key":"anything","quote":"words nobody checked"}]}`, a, b))
	if r.IsError {
		t.Fatal(r.Text)
	}
	_, r = docExec(t, d, inRunCtx(ctx), fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"supported","reason":"the quote says so"}`, l["id"]))
	if !r.IsError || !strings.Contains(r.Text, "no evidence") {
		t.Errorf("a run judged supported on unverified evidence: %s", r.Text)
	}
}
