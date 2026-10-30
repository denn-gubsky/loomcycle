package builtin

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// evidence_count is what the caller may read, not what the table holds.
func TestGetEdges_EvidenceCountLeavesOutWithheldRows(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	out, res := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+s.publicChunk+`","kind":"cites",
		"evidence":[{"chunk_id":"`+s.secretChunk+`","quote":"savepoint nesting secret"}]}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	id := out["id"].(string)
	denied := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	for _, op := range []string{
		`{"op":"get_edges","scope":"tenant","document_id":"` + s.publicDoc + `"}`,
		`{"op":"get_edge","scope":"tenant","id":"` + id + `"}`,
		`{"op":"list_statements","scope":"tenant","status":"asserted"}`,
	} {
		_, res := docExec(t, d, denied, op)
		if res.IsError {
			t.Fatalf("%s: %s", op, res.Text)
		}
		if strings.Contains(res.Text, `"evidence_count":1`) {
			t.Errorf("evidence_count counts a withheld row: %s -> %s", op, res.Text)
		}
	}
}
