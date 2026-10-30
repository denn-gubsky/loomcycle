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

// edgePages follows get_edges' next_cursor from body (which must not close its
// object) and returns each page's statement ids and raw answer.
func edgePages(t *testing.T, d *Document, ctx context.Context, body string) (pages [][]string, texts []string) {
	t.Helper()
	after := ""
	for i := 0; i < 10; i++ {
		req := body
		if after != "" {
			req += `,"after":"` + after + `"`
		}
		out, r := docExec(t, d, ctx, req+"}")
		if r.IsError {
			t.Fatalf("%s: %s", req, r.Text)
		}
		raw, _ := out["edges"].([]any)
		ids := make([]string, 0, len(raw))
		for _, e := range raw {
			ids = append(ids, e.(map[string]any)["id"].(string))
		}
		pages, texts = append(pages, ids), append(texts, r.Text)
		next, ok := out["next_cursor"].(string)
		if !ok {
			if _, ok := out["truncated"]; ok {
				t.Errorf("%s: truncated without a next_cursor: %s", req, r.Text)
			}
			return pages, texts
		}
		after = next
	}
	t.Fatalf("%s: more than 10 pages", body)
	return nil, nil
}

// interleavedEdges links the public chunk to itself (visible to a reader denied
// /kb/secret) and to the secret chunk (hidden from it), so that in get_edges'
// order (kind, created_at, id) the rows run visible, hidden, visible, hidden,
// visible, hidden. It returns the visible ids and the hidden ids, in that order.
func interleavedEdges(t *testing.T, d *Document, open context.Context, s aclDocs) (visible, hidden []string) {
	t.Helper()
	for i, kind := range []string{"cites", "defines", "implements"} {
		for j, to := range []string{s.publicChunk, s.secretChunk} {
			id := linkStmt(t, d, open, "tenant", s.publicChunk, to, kind, "")
			stmtSQL(t, d, open, "tenant", `UPDATE chunk_edges SET created_at = ? WHERE id = ?`, int64(10*(2*i+j+1)), id)
			if j == 0 {
				visible = append(visible, id)
			} else {
				hidden = append(hidden, id)
			}
		}
	}
	return visible, hidden
}

func secretReadDenied(t *testing.T, base context.Context) context.Context {
	return tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
}

// Rows a confined reader may not see are withheld before get_edges cuts its
// page: a page of limit N holds N visible rows when that many exist, next_cursor
// is absent exactly when no visible row remains, and every page is byte-for-byte
// what the reader would get if the hidden rows did not exist at all.
func TestGetEdges_HiddenRowsNeverShapeThePage(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		s := seedACLDocs(t, d, open)
		visible, hidden := interleavedEdges(t, d, open, s)
		denied := secretReadDenied(t, base)

		bodies := []string{}
		for _, dir := range []string{"", `,"direction":"out"`} {
			for _, limit := range []int{1, 2, 3} {
				bodies = append(bodies, fmt.Sprintf(`{"op":"get_edges","scope":"tenant","document_id":%q,"limit":%d%s`, s.publicDoc, limit, dir))
			}
		}
		withHidden := map[string][]string{}
		for _, body := range bodies {
			pages, texts := edgePages(t, d, denied, body)
			withHidden[body] = texts
			var got []string
			for i, p := range pages {
				if i < len(pages)-1 && len(p) != len(pages[0]) {
					t.Errorf("%s: page %d holds %d rows, the first %d: %v", body, i, len(p), len(pages[0]), pages)
				}
				got = append(got, p...)
			}
			if strings.Join(got, ",") != strings.Join(visible, ",") {
				t.Errorf("%s: pages %v, want the visible rows %v in order", body, pages, visible)
			}
			if len(pages[len(pages)-1]) == 0 && len(pages) > 1 {
				t.Errorf("%s: a next_cursor led to an empty page: %v", body, pages)
			}
		}

		// The same reader, once the hidden rows are gone: nothing may differ.
		for _, id := range hidden {
			stmtSQL(t, d, open, "tenant", `DELETE FROM chunk_edges WHERE id = ?`, id)
		}
		for _, body := range bodies {
			_, texts := edgePages(t, d, denied, body)
			if strings.Join(texts, "\n") != strings.Join(withHidden[body], "\n") {
				t.Errorf("%s: hidden rows change the answer:\nwith    %v\nwithout %v", body, withHidden[body], texts)
			}
		}

		// backlinks reads the whole scope, so this reader is refused outright and
		// there is no page for hidden rows to shape.
		for _, id := range []string{s.publicChunk, s.secretChunk} {
			_, r := docExec(t, d, denied, `{"op":"backlinks","scope":"tenant","id":"`+id+`","limit":1}`)
			refusedByACL(t, "backlinks "+id, r)
		}
	})
}

// A caller with no set, or a set that hides nothing here, pages get_edges as it
// always has: every row, cut at limit, the cursor on the last row of the page,
// and the same bytes either way.
func TestGetEdges_UnrestrictedCallerPagesEveryRowUnchanged(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		s := seedACLDocs(t, d, open)
		visible, hidden := interleavedEdges(t, d, open, s)
		allowAll := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}}))

		body := `{"op":"get_edges","scope":"tenant","document_id":"` + s.publicDoc + `","limit":2`
		pages, texts := edgePages(t, d, open, body)
		want := [][]string{{visible[0], hidden[0]}, {visible[1], hidden[1]}, {visible[2], hidden[2]}}
		if fmt.Sprint(pages) != fmt.Sprint(want) {
			t.Fatalf("pages %v, want %v", pages, want)
		}
		for i, text := range texts[:len(texts)-1] {
			var out struct {
				Edges []struct {
					ID   string `json:"id"`
					Kind string `json:"kind"`
				} `json:"edges"`
				Truncated  bool   `json:"truncated"`
				NextCursor string `json:"next_cursor"`
			}
			if err := json.Unmarshal([]byte(text), &out); err != nil {
				t.Fatal(err)
			}
			last := out.Edges[len(out.Edges)-1]
			c, ok := decodeEdgeCursor(out.NextCursor)
			if !out.Truncated || !ok || c.ID != last.ID || c.Kind != last.Kind || c.CreatedAt != int64(10*(2*i+2)) {
				t.Errorf("page %d: cursor %+v (truncated %v), want the page's last row %s", i, c, out.Truncated, last.ID)
			}
		}
		_, allowTexts := edgePages(t, d, allowAll, body)
		if strings.Join(allowTexts, "\n") != strings.Join(texts, "\n") {
			t.Errorf("a set that hides nothing changes the answer:\nno set    %v\nallow-all %v", texts, allowTexts)
		}
	})
}
