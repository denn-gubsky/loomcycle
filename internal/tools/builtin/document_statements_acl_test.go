package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A statement's evidence must not reveal a document the caller cannot read:
// citing it answers like a missing chunk (verifying the quote would otherwise
// confirm the text is there), and a listing withholds evidence rows from it.
func TestStatementEvidence_UnreadableSourceIsNeitherVerifiedNorShown(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	// A statement inside the public document whose evidence quotes the secret one,
	// made by a caller who may read both.
	out, res := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+s.publicChunk+`","kind":"cites",
		"evidence":[{"chunk_id":"`+s.secretChunk+`","quote":"savepoint nesting secret"},{"chunk_id":"`+s.publicChunk+`","quote":"savepoint nesting public"}]}`)
	if res.IsError || out["evidence_written"] != float64(2) {
		t.Fatalf("staging link: %v %s", out, res.Text)
	}
	id := out["id"].(string)

	denied := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))

	_, res = docExec(t, d, denied, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+s.publicChunk+`","kind":"based_on",
		"evidence":[{"chunk_id":"`+s.secretChunk+`","quote":"savepoint nesting secret"}]}`)
	if !res.IsError || !strings.Contains(res.Text, "no such chunk") {
		t.Errorf("citing an unreadable chunk: want the missing-chunk answer, got %s", res.Text)
	}

	for _, op := range []string{
		`{"op":"get_edges","scope":"tenant","document_id":"` + s.publicDoc + `"}`,
		`{"op":"get_edge","scope":"tenant","id":"` + id + `"}`,
	} {
		_, res := docExec(t, d, denied, op)
		if res.IsError {
			t.Fatalf("%s: %s", op, res.Text)
		}
		if strings.Contains(res.Text, "nesting secret") || strings.Contains(res.Text, s.secretChunk) {
			t.Errorf("%s shows evidence from the unreadable document: %s", op, res.Text)
		}
		if !strings.Contains(res.Text, "nesting public") {
			t.Errorf("%s dropped the readable evidence: %s", op, res.Text)
		}
	}
}

// import_md reconnects an inbound statement by its source's natural key only
// when the caller may link from that source's document.
func TestImportMD_InboundSourceNeedsLinkOnItsDocument(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	if _, res := docExec(t, d, open, `{"op":"upsert_chunk","scope":"tenant","document_id":"`+s.secretDoc+`","natural_key":"plan|owner|ops","title":"Ops owns the plan","body":"ops owns the plan"}`); res.IsError {
		t.Fatalf("upsert: %s", res.Text)
	}
	md := "# Imported\n<!-- loom: {\"id\":\"old-root\"} -->\n\n## Part\n<!-- loom: {\"id\":\"old-part\"} -->\n\ntext\n\n" +
		"<!-- loom-edges:\n@plan|owner|ops -> old-part [cites]\n-->\n"
	importAs := func(ctx_ string) int {
		ctx := open
		if ctx_ == "denied" {
			ctx = tenantDocCtx(base, aclSet(t,
				authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
				authz.Rule{Effect: authz.Deny, Actions: []string{"link"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
		}
		out, res := docExec(t, d, ctx, `{"op":"import_md","scope":"tenant","path":"/kb/public/imported-`+ctx_+`","markdown":`+jsonString(md)+`}`)
		if res.IsError {
			t.Fatalf("import (%s): %s", ctx_, res.Text)
		}
		edges := edgesOf(t, d, open, `{"op":"get_edges","scope":"tenant","document_id":"`+out["document_id"].(string)+`"}`)
		return len(edges)
	}
	if n := importAs("open"); n != 1 {
		t.Fatalf("control: the inbound statement was not reconnected (%d edges)", n)
	}
	if n := importAs("denied"); n != 0 {
		t.Errorf("an import linked from a document the caller may not link: %d edges", n)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A chat-turn citation verifies its quote against the turn, so it is limited to
// the caller's own chats: otherwise "does this text occur in another user's turn"
// would be answerable by citing it.
func TestStatementEvidence_ChatTurnMustBeTheCallersOwn(t *testing.T) {
	d, ctx, s := documentFixture(t) // caller: user u1, tenant tnt
	_, a, b, _ := stmtDoc(t, d, ctx, "chat")
	cite := func(user string) (map[string]any, string) {
		sess, err := s.CreateSession(context.Background(), "tnt", "doc-agent", user)
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a", UserID: user, TenantID: "tnt"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendEvent(context.Background(), run.ID, "text", []byte(`{"text":"the salary is 90k"}`)); err != nil {
			t.Fatal(err)
		}
		events, err := s.GetTranscript(context.Background(), sess.ID)
		if err != nil || len(events) == 0 {
			t.Fatalf("transcript: %v %d", err, len(events))
		}
		seq := events[len(events)-1].Seq
		out, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"session_id":%q,"event_seq":%d,"quote":"salary is 90k"}]}`, a, b, sess.ID, seq))
		return out, res.Text
	}
	if out, text := cite("u1"); out == nil || out["evidence_written"] != float64(1) {
		t.Fatalf("control: citing the caller's own turn failed: %s", text)
	}
	if out, text := cite("u2"); out != nil || !strings.Contains(text, "no such chat turn") {
		t.Errorf("citing another user's turn: want the not-found answer, got %v %s", out, text)
	}
}
