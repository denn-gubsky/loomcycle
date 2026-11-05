package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The remaining doors around a document read-deny (security re-audit
// 2026-11-04, B7 a-e / D14 / D16; RFC FY Part G): each test is a caller whose
// tenant ACL denies it the target, going in through a door other than the op
// the deny was written for.

// withACL is ctx with eff as the run's set, everything else kept.
func withACL(ctx context.Context, eff *authz.Effective) context.Context {
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.ACL = eff })
}

func denyRule(verb, resource string) authz.Rule {
	return authz.Rule{Effect: authz.Deny, Actions: []string{verb}, Resources: []string{resource}}
}

// TestExportMD_TransclusionRespectsTheReadersACL: a readable document that
// embeds a part of an unreadable one renders the embed as its literal text, the
// same text a missing target leaves, never the hidden body.
func TestExportMD_TransclusionRespectsTheReadersACL(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	embeds := "Before. ![[/kb/secret/plan#Plan part]] Middle. ![[" + s.secretChunk + "]] After. ![[/kb/missing/doc#Nothing]]"
	b, _ := json.Marshal(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": s.publicDoc, "title": "Embeds", "body": embeds})
	if _, r := docExec(t, d, open, string(b)); r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	export := `{"op":"export_md","scope":"tenant","document_id":"` + s.publicDoc + `","include_metadata":false}`

	// The fixture is live: with no set the embeds expand to the hidden body.
	if out := copyExec(t, d, open, export); !strings.Contains(asStr(out["markdown"]), "savepoint nesting secret") {
		t.Fatalf("fixture: with no set the embed should expand: %s", out["markdown"])
	}

	denied := tenantDocCtx(base, aclSet(t, denyRule("read", "document:tenant:/kb/secret/**")))
	md := asStr(copyExec(t, d, denied, export)["markdown"])
	if strings.Contains(md, "savepoint nesting secret") {
		t.Errorf("export_md inlined the body of a document the reader may not read:\n%s", md)
	}
	for _, literal := range []string{"![[/kb/secret/plan#Plan part]]", "![[" + s.secretChunk + "]]", "![[/kb/missing/doc#Nothing]]"} {
		if !strings.Contains(md, literal) {
			t.Errorf("the embed %s should stay as written, as a missing target does:\n%s", literal, md)
		}
	}
	if !strings.Contains(md, "savepoint nesting public") {
		t.Errorf("the readable document's own text is missing:\n%s", md)
	}
}

// TestAssetGET_AsksTheDocumentsRead: the asset GET reads through
// Document.ReadAsset, which answers an image of an unreadable document exactly
// as it answers a chunk with no asset.
func TestAssetGET_AsksTheDocumentsRead(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0xfe, 0x01}
	var img string
	for _, doc := range []string{s.secretDoc, s.publicDoc} {
		c := copyExec(t, d, open, `{"op":"create_chunk","scope":"tenant","document_id":"`+doc+`","title":"Diagram"}`)
		copyExec(t, d, open, `{"op":"set_asset","scope":"tenant","id":"`+asStr(c["id"])+`","media_type":"image/png","data":"`+base64.StdEncoding.EncodeToString(png)+`"}`)
		if doc == s.secretDoc {
			img = asStr(c["id"])
		}
	}
	if _, _, ok, err := d.ReadAsset(open, "tenant", img); err != nil || !ok {
		t.Fatalf("fixture: with no set the asset should be served: ok=%v err=%v", ok, err)
	}

	denied := tenantDocCtx(base, aclSet(t, denyRule("read", "document:tenant:/kb/secret/**")))
	mt, data, ok, err := d.ReadAsset(denied, "tenant", img)
	if err != nil || ok || len(data) != 0 || mt != "" {
		t.Errorf("the asset of an unreadable document: ok=%v type=%q bytes=%d err=%v, want the answer a missing asset gets", ok, mt, len(data), err)
	}
	_, _, okMissing, errMissing := d.ReadAsset(denied, "tenant", "no-such-chunk")
	if okMissing || errMissing != nil {
		t.Fatalf("a missing asset: ok=%v err=%v", okMissing, errMissing)
	}
}

// TestMoveDocuments_AsksDelete: a move removes the source, so a caller whose
// rules deny deleting a document cannot move it away.
func TestMoveDocuments_AsksDelete(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := grantedTenantCtx(base)
	s := seedCopyDocs(t, d, open)
	ctx := withACL(open, aclSet(t, denyRule("delete", "document:user:/proj/rfcs/**")))

	dry := copyExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant","under_path":"/proj"}`)
	if !refusedAs(dry, s.planDoc, copyForbidden) {
		t.Errorf("the dry run should refuse the plan, whose deletion the rules deny: %v", dry)
	}
	n, _ := json.Marshal(dry["counts"].(map[string]any)["documents"])
	copyExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant","under_path":"/proj","dangling":"drop","dry_run":false,"expect_count":`+string(n)+`}`)
	if got := copyExec(t, d, open, `{"op":"get_document","scope":"user","id":"`+s.planDoc+`"}`); asStr(got["document_id"]) != s.planDoc {
		t.Errorf("the plan was moved away though deleting it is denied: %v", got)
	}
	if _, r := docExec(t, d, open, `{"op":"get_document","scope":"tenant","id":"`+s.planDoc+`"}`); !r.IsError {
		t.Error("the plan should not be in the tenant scope")
	}
}

// TestMoveDocuments_DanglingDropAsksLinkOnBothEnds: dangling:drop deletes the
// relations that leave the set, which is an unlink: a caller denied link on the
// far end cannot drop them, and nothing moves.
func TestMoveDocuments_DanglingDropAsksLinkOnBothEnds(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := grantedTenantCtx(base)
	s := seedCopyDocs(t, d, open)
	ctx := withACL(open, aclSet(t, denyRule("link", "document:user:/proj/docs/**")))
	set := `"document_ids":["` + s.planDoc + `"],"dangling":"drop"`

	dry := copyExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant",`+set+`}`)
	if dry["blocked_by"] != "forbidden_relations" {
		t.Errorf("the dry run should say the move is blocked by a relation the caller may not remove: %v", dry)
	}
	_, r := docExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant",`+set+`,"dry_run":false,"expect_count":1}`)
	if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission {
		t.Errorf("dropping a relation the caller may not unlink should be refused: %v %s", r.IsError, r.Text)
	}
	if got := copyExec(t, d, open, `{"op":"get_document","scope":"user","id":"`+s.planDoc+`"}`); asStr(got["document_id"]) != s.planDoc {
		t.Errorf("the plan should stay in the user scope: %v", got)
	}
	if edges := copyExec(t, d, open, `{"op":"get_edges","scope":"user","document_id":"`+s.guideDoc+`"}`); len(edges["edges"].([]any)) != 1 {
		t.Errorf("the relation to the guide should still be there: %v", edges)
	}
}

// TestCopyMoveDryRun_DoesNotRevealUnreadableDocuments: a document the caller may
// not read is absent from a copy or move plan, not refused, classified or listed
// as a relation's far end; and a target name the caller may not write is
// refused for that, before the target is probed for what holds the name.
func TestCopyMoveDryRun_DoesNotRevealUnreadableDocuments(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := grantedTenantCtx(base)
	s := seedCopyDocs(t, d, open)
	// A third hidden document that holds a fact: the old plan classified it
	// holds_facts before it asked whether the caller may read it.
	facts := copyExec(t, d, open, `{"op":"create_document","scope":"user","title":"Payroll","path":"/proj/rfcs/payroll"}`)
	copyExec(t, d, open, `{"op":"upsert_chunk","scope":"user","document_id":"`+asStr(facts["document_id"])+`","natural_key":"memory/fact/pay","title":"Pay","body":"Pay is monthly.","type":"fact","subject":"Payroll"}`)
	hidden := []string{s.planDoc, s.planChunk, asStr(facts["document_id"])}
	ctx := withACL(open, aclSet(t, denyRule("read", "document:user:/proj/rfcs/**")))

	for _, call := range []string{
		`{"op":"copy_documents","scope":"user","to_scope":"tenant","under_path":"/proj"}`,
		`{"op":"copy_documents","scope":"user","to_scope":"tenant","document_ids":["` + s.planDoc + `","` + s.guideDoc + `","` + asStr(facts["document_id"]) + `"]}`,
		`{"op":"move_documents","scope":"user","to_scope":"tenant","under_path":"/proj"}`,
		`{"op":"move_documents","scope":"user","to_scope":"tenant","under_path":"/proj","dangling":"drop"}`,
	} {
		out := copyExec(t, d, ctx, call)
		b, _ := json.Marshal(out)
		for _, id := range hidden {
			if strings.Contains(string(b), id) {
				t.Errorf("%s names %s, which the caller may not read: %s", call, id, b)
			}
		}
		if out["dangling_count"] != float64(0) {
			t.Errorf("%s counts the relation into the unreadable plan: %s", call, b)
		}
		if c := out["counts"].(map[string]any); c["documents"] != float64(1) {
			t.Errorf("%s: only the guide should be in the plan: %s", call, b)
		}
	}
	// The move would delete the hidden relation with the guide's source: it is
	// blocked, without saying which relation or document.
	_, r := docExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant","under_path":"/proj","dangling":"drop","dry_run":false,"expect_count":1}`)
	if !r.IsError || strings.Contains(r.Text, s.planChunk) || strings.Contains(r.Text, s.planDoc) {
		t.Errorf("a move that would delete a relation into an unreadable document should be refused without naming it: %s", r.Text)
	}
	if edges := copyExec(t, d, open, `{"op":"get_edges","scope":"user","document_id":"`+s.planDoc+`"}`); len(edges["edges"].([]any)) != 1 {
		t.Errorf("the hidden relation should survive: %v", edges)
	}
}

// TestCopyDocuments_ATargetNameIsAskedBeforeItIsProbed: a target name the
// caller may not write is refused for that, whatever already holds the name in
// the target scope, so the refusal says nothing about the target's contents.
func TestCopyDocuments_ATargetNameIsAskedBeforeItIsProbed(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := grantedTenantCtx(base)
	s := seedCopyDocs(t, d, open)
	copyExec(t, d, open, `{"op":"create_document","scope":"tenant","title":"Taken","path":"/proj/docs/guide"}`)
	ctx := withACL(open, aclSet(t, denyRule("write", "document:tenant:/proj/docs/**")))
	for _, op := range []string{"copy_documents", "move_documents"} {
		out := copyExec(t, d, ctx, `{"op":"`+op+`","scope":"user","to_scope":"tenant","document_ids":["`+s.guideDoc+`"]}`)
		if !refusedAs(out, s.guideDoc, copyForbidden) {
			t.Errorf("%s: a target name the caller may not write is refused as forbidden, before it is probed: %v", op, out)
		}
	}
}

// refusedAs reports whether the plan refused docID with code.
func refusedAs(out map[string]any, docID, code string) bool {
	refused, _ := out["refused"].([]any)
	for _, e := range refused {
		m, _ := e.(map[string]any)
		if m["document_id"] == docID && m["code"] == code {
			return true
		}
	}
	return false
}

// TestMemoryDocRowDelete_AsksTheDocumentACL: a fact's body row is a chunk of a
// document, so removing it by key through Memory asks that document's delete;
// and a row of a document the caller may not read is answered as a missing key,
// as Memory get answers it, and left in place.
func TestMemoryDocRowDelete_AsksTheDocumentACL(t *testing.T) {
	d, base, st := documentFixture(t)
	open := grantedTenantCtx(base)
	m := &Memory{Store: st, SqlMem: d.SqlMem, DocumentFilters: d}
	fact := func(path, key, body string) string {
		doc := copyExec(t, d, open, `{"op":"create_document","scope":"user","title":"`+key+`","path":"`+path+`"}`)
		f := copyExec(t, d, open, `{"op":"upsert_chunk","scope":"user","document_id":"`+asStr(doc["document_id"])+`","natural_key":"memory/fact/`+key+`","title":"`+key+`","body":"`+body+`","type":"fact","subject":"`+key+`"}`)
		return asStr(f["id"])
	}
	hr := fact("/hr/people", "ada", "Ada is on leave.")
	secret := fact("/secret/people", "eve", "Eve is on leave.")
	notes := fact("/notes/x", "bob", "Bob likes tea.")
	del := func(ctx context.Context, chunk string) tools.Result {
		t.Helper()
		r, err := m.Execute(ctx, json.RawMessage(`{"op":"delete","scope":"user","key":"`+chunkBodyKey(chunk)+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	bodyIs := func(chunk, want string) {
		t.Helper()
		if got := copyExec(t, d, open, `{"op":"get_chunk","scope":"user","id":"`+chunk+`"}`); got["body"] != want {
			t.Errorf("the fact body of %s should be untouched: %v", chunk, got["body"])
		}
	}

	r := del(withACL(open, aclSet(t, denyRule("delete", "document:user:/hr/**"))), hr)
	if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission {
		t.Errorf("Memory delete of a fact body in a delete-denied document: %v %s, want an access-control refusal", r.IsError, r.Text)
	}
	bodyIs(hr, "Ada is on leave.")

	noRead := withACL(open, aclSet(t, denyRule("read", "document:user:/secret/**")))
	missing := del(noRead, "no-such-chunk")
	r = del(noRead, secret)
	if r.IsError || r.Text != missing.Text {
		t.Errorf("Memory delete of a fact body in an unreadable document: %s, want the missing key's answer %s", r.Text, missing.Text)
	}
	bodyIs(secret, "Eve is on leave.")

	// The same caller removes a fact body of a document it may delete.
	if r := del(withACL(open, aclSet(t, denyRule("delete", "document:user:/hr/**"))), notes); r.IsError || !strings.Contains(r.Text, `"deleted":true`) {
		t.Errorf("deleting a fact body of an allowed document: %s", r.Text)
	}
}

// TestCopyDocuments_AKeyHeldByAnUnreadableDocumentIsNotNamed: a target scope
// that already holds the copy's work key in a document the caller may not read
// answers as a write refusal does, naming neither the key nor the scope, so a
// dry run cannot probe for a work the caller cannot see (resolve_anchor hides
// the same). A holder the caller may read is still reported as key_collision.
func TestCopyDocuments_AKeyHeldByAnUnreadableDocumentIsNotNamed(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := grantedTenantCtx(base)
	work := func(scope, title string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": scope, "markdown": "# " + title + "\n\n## Article 1\n\nText.\n",
			"reimport": true, "work_key": "law:secret/1"})
		out := copyExec(t, d, open, string(b))
		return asStr(out["document_id"])
	}
	mine := work("user", "Mine")
	copyExec(t, d, open, `{"op":"set_path","scope":"user","id":"`+mine+`","path":"/mine/law"}`)
	held := work("tenant", "Held")
	copyExec(t, d, open, `{"op":"set_path","scope":"tenant","id":"`+held+`","path":"/vault/law"}`)
	call := `{"op":"copy_documents","scope":"user","to_scope":"tenant","document_ids":["` + mine + `"],"path_map":{"/documents":"/copies","/mine":"/copies"}}`

	denied := withACL(open, aclSet(t, denyRule("read", "document:tenant:/vault/**")))
	out := copyExec(t, d, denied, call)
	b, _ := json.Marshal(out)
	if !refusedAs(out, mine, copyForbidden) || strings.Contains(string(b), "law:secret/1") || strings.Contains(string(b), "work_key") {
		t.Errorf("a key held by an unreadable document should be refused as a write refusal is, naming nothing: %s", b)
	}
	if out := copyExec(t, d, open, call); !refusedAs(out, mine, copyKeyCollision) {
		t.Errorf("a key held by a readable document is a key_collision: %v", out)
	}
}

// TestMoveDocuments_UnfinishedMovesAreReportedOnlyWhereReadable: a move between
// two scopes finishes every unfinished move between them, whoever started it,
// but reports (to_finish, moved) only those whose recorded names the caller may
// read in the target. The others are still finished.
func TestMoveDocuments_UnfinishedMovesAreReportedOnlyWhereReadable(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := grantedTenantCtx(base)
	s := seedCopyDocs(t, d, open)
	interruptMove(t, d, open, s, "source_removed")
	ctx := withACL(open, aclSet(t, denyRule("read", "document:tenant:/proj/rfcs/**")))
	call := `{"op":"move_documents","scope":"user","to_scope":"tenant","under_path":"/proj"`

	dry := copyExec(t, d, ctx, call+`}`)
	if b, _ := json.Marshal(dry); strings.Contains(string(b), s.planDoc) || !strings.Contains(string(b), s.guideDoc) {
		t.Errorf("to_finish should list the guide and not the plan, whose target name the caller may not read: %s", b)
	}
	out := copyExec(t, d, ctx, call+`,"dry_run":false,"expect_count":0}`)
	if b, _ := json.Marshal(out); strings.Contains(string(b), s.planDoc) || !strings.Contains(string(b), s.guideDoc) {
		t.Errorf("moved should list the guide and not the plan: %s", b)
	}
	if got := copyExec(t, d, open, `{"op":"get_document","scope":"tenant","path":"/proj/rfcs/plan"}`); asStr(got["document_id"]) != s.planDoc {
		t.Errorf("the plan's move should still be finished: %v", got)
	}
}
