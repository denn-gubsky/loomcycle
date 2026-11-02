package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FT P1 — move_documents: a set of documents moved into another scope,
// every id kept, the source removed only once the target is complete.

// moveSet runs the dry run, then the move with the dry run's count.
func moveSet(t *testing.T, d *Document, ctx context.Context, args string) map[string]any {
	t.Helper()
	dry := copyExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant",`+args+`}`)
	n := int(dry["counts"].(map[string]any)["documents"].(float64))
	b, _ := json.Marshal(n)
	return copyExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant",`+args+`,"dry_run":false,"expect_count":`+string(b)+`}`)
}

// movedDocsLanded checks the seed's two documents are in the tenant scope under
// their ids and names, with the text they had and the relation between them,
// and gone from the user scope.
func movedDocsLanded(t *testing.T, d *Document, ctx context.Context, s copySeed, text map[string]string) {
	t.Helper()
	for _, c := range []struct{ id, path string }{{s.planDoc, "/proj/rfcs/plan"}, {s.guideDoc, "/proj/docs/guide"}} {
		got := copyExec(t, d, ctx, `{"op":"get_document","scope":"tenant","path":"`+c.path+`"}`)
		if asStr(got["document_id"]) != c.id {
			t.Errorf("%s in the tenant scope names %v, want the same id %s", c.path, got["document_id"], c.id)
		}
		if got := cleanExport(t, d, ctx, "tenant", c.id); got != text[c.id] {
			t.Errorf("the moved document's text differs:\n%s\n---\n%s", text[c.id], got)
		}
		if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+c.id+`"}`); !r.IsError {
			t.Errorf("document %s should be gone from the user scope", c.id)
		}
		if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","path":"`+c.path+`"}`); !r.IsError {
			t.Errorf("the user scope should no longer resolve %s", c.path)
		}
	}
	edges := copyExec(t, d, ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+s.planDoc+`","direction":"out"}`)
	list, _ := edges["edges"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["to_document_id"] != s.guideDoc {
		t.Errorf("the relation between the two documents should move with them: %v", edges)
	}
	tkey, _, _ := d.resolveScope(ctx, "tenant")
	res, err := d.query(ctx, tkey, `SELECT count(*) FROM document_moves`)
	if err != nil || asInt(res.Rows[0][0]) != 0 {
		t.Errorf("no move should be left unfinished: %v %v", res, err)
	}
	listed := copyExec(t, d, ctx, `{"op":"query_documents","scope":"tenant","under_path":"/proj"}`)
	if docs, _ := listed["documents"].([]any); len(docs) != 2 {
		t.Errorf("the tenant scope should list both moved documents: %v", listed)
	}
}

func exportsOf(t *testing.T, d *Document, ctx context.Context, s copySeed) map[string]string {
	t.Helper()
	return map[string]string{
		s.planDoc:  cleanExport(t, d, ctx, "user", s.planDoc),
		s.guideDoc: cleanExport(t, d, ctx, "user", s.guideDoc),
	}
}

func TestMoveDocuments_MovesTheSetKeepingEveryID(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	sink := &recordingSink{}
	d.Audit = sink
	s := seedCopyDocs(t, d, ctx)
	text := exportsOf(t, d, ctx, s)
	edgeID := asStr(copyExec(t, d, ctx, `{"op":"get_edges","scope":"user","document_id":"`+s.planDoc+`","direction":"out"}`)["edges"].([]any)[0].(map[string]any)["id"])

	out := moveSet(t, d, ctx, `"under_path":"/proj"`)
	if refused, _ := out["refused"].([]any); len(refused) != 0 {
		t.Fatalf("nothing should be refused: %v", out)
	}
	if moved, _ := out["moved"].([]any); len(moved) != 2 {
		t.Fatalf("both documents should be reported moved: %v", out)
	}
	for _, e := range out["documents"].([]any) {
		if _, ok := e.(map[string]any)["new_id"]; ok {
			t.Errorf("a move mints no ids: %v", e)
		}
	}
	movedDocsLanded(t, d, ctx, s, text)

	// The statement keeps its id; history and tags come along; no copied_from.
	edges := copyExec(t, d, ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+s.planDoc+`","direction":"out"}`)
	if got := asStr(edges["edges"].([]any)[0].(map[string]any)["id"]); got != edgeID {
		t.Errorf("the statement should keep its id %s, got %s", edgeID, got)
	}
	hist := copyExec(t, d, ctx, `{"op":"history","scope":"tenant","id":"`+s.planChunk+`"}`)
	if revs, _ := hist["revisions"].([]any); len(revs) < 2 {
		t.Errorf("the body history should move, under the same chunk id: %v", hist)
	}
	tags := copyExec(t, d, ctx, `{"op":"list_tags","scope":"tenant","document_id":"`+s.planDoc+`"}`)
	if b, _ := json.Marshal(tags); !strings.Contains(string(b), "area/plans") {
		t.Errorf("the document's tags should move: %v", tags)
	}
	pc := copyExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+s.planDoc+`"}`)
	root := copyExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+asStr(pc["root_chunk_id"])+`"}`)
	if f, _ := root["fields"].(map[string]any); f != nil && f["copied_from"] != nil {
		t.Errorf("a move is not a copy, and records no copied_from: %v", f)
	}

	if len(sink.evs) != 1 || sink.evs[0].Action != "document_move" || len(sink.evs[0].MoveDocumentIDs) != 2 ||
		sink.evs[0].MoveFromScope != "user" || sink.evs[0].MoveToScope != "tenant" {
		t.Errorf("the move should be audited once, naming both documents: %+v", sink.evs)
	}
}

func TestMoveDocuments_DryRunIsTheDefaultAndWritesNothing(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)
	out := copyExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant","under_path":"/proj"}`)
	if out["dry_run"] != true || out["counts"].(map[string]any)["documents"] != float64(2) {
		t.Fatalf("a dry run should count the set: %v", out)
	}
	if got := copyExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+s.planDoc+`"}`); asStr(got["document_id"]) != s.planDoc {
		t.Errorf("a dry run must leave the source: %v", got)
	}
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+s.planDoc+`"}`); !r.IsError {
		t.Error("a dry run must write nothing to the target")
	}
}

func TestMoveDocuments_ARelationLeavingTheSetRefusesUnlessDropped(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)
	set := `"document_ids":["` + s.planDoc + `"]`

	dry := copyExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant",`+set+`}`)
	if dry["blocked_by"] != "dangling_relations" || dry["dangling_count"] != float64(1) {
		t.Fatalf("the dry run should list the relation to the guide and say it blocks the move: %v", dry)
	}
	_, r := docExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant",`+set+`,"dry_run":false,"expect_count":1}`)
	if !r.IsError || !strings.Contains(r.Text, "relations") {
		t.Fatalf("a move cutting a relation must be refused, got %s", r.Text)
	}
	if got := copyExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+s.planDoc+`"}`); asStr(got["document_id"]) != s.planDoc {
		t.Fatalf("a refused move leaves the source: %v", got)
	}

	out := copyExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant",`+set+`,"dangling":"drop","dry_run":false,"expect_count":1}`)
	if out["dropped"] != float64(1) {
		t.Fatalf("dangling:drop should report the one relation dropped: %v", out)
	}
	if got := copyExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+s.planDoc+`"}`); asStr(got["document_id"]) != s.planDoc {
		t.Errorf("the plan should be in the tenant scope: %v", got)
	}
	src := copyExec(t, d, ctx, `{"op":"get_edges","scope":"user","document_id":"`+s.guideDoc+`"}`)
	if list, _ := src["edges"].([]any); len(list) != 0 {
		t.Errorf("the dropped relation should be gone from the guide: %v", src)
	}
	if got := copyExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+s.guideDoc+`"}`); asStr(got["document_id"]) != s.guideDoc {
		t.Errorf("the guide, outside the set, stays: %v", got)
	}
}

func TestMoveDocuments_RefusesWhatItCannotDo(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)
	set := `"under_path":"/proj"`
	for name, c := range map[string]struct{ body, want string }{
		"no expect_count": {`"dry_run":false`, "expect_count is required"},
		"stale count":     {`"dry_run":false,"expect_count":3`, "not the 3 expected"},
		"same scope":      {`"dry_run":false,"expect_count":2,"to_scope":"user"`, "already in scope"},
		"bad dangling":    {`"dangling":"keep"`, "dangling must be"},
		"no target scope": {`"to_scope":""`, "to_scope"},
	} {
		body := `{"op":"move_documents","scope":"user","to_scope":"tenant",` + set + `,` + c.body + `}`
		if _, r := docExec(t, d, ctx, body); !r.IsError || !strings.Contains(r.Text, c.want) {
			t.Errorf("%s: want a refusal containing %q, got %s", name, c.want, r.Text)
		}
	}
	if got := copyExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+s.planDoc+`"}`); asStr(got["document_id"]) != s.planDoc {
		t.Errorf("no refused call may move anything: %v", got)
	}

	// An id the target scope already holds.
	tkey, _, _ := d.resolveScope(ctx, "tenant")
	if err := d.ensureSchema(ctx, tkey); err != nil {
		t.Fatal(err)
	}
	if err := d.exec(ctx, tkey, `INSERT INTO documents (id, title, root_chunk_id, created_at, updated_at) VALUES (?, 'other', '', 0, 0)`, s.guideDoc); err != nil {
		t.Fatal(err)
	}
	out := copyExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant",`+set+`}`)
	refused, _ := out["refused"].([]any)
	if len(refused) != 1 || refused[0].(map[string]any)["code"] != moveIDCollision || refused[0].(map[string]any)["document_id"] != s.guideDoc {
		t.Errorf("the guide's id is taken in the target, so it is refused: %v", out)
	}
}

func TestMoveDocuments_ALeaseOnTheSourceRefuses(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	seedCopyDocs(t, d, ctx)
	ukey, _, _ := d.resolveScope(ctx, "user")
	if res, err := d.acquireDocumentLease(ctx, ukey, "linker", "run-elsewhere", time.Now(), time.Minute); err != nil || !res.Acquired {
		t.Fatalf("lease: %+v %v", res, err)
	}
	_, r := docExec(t, d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant","under_path":"/proj"}`)
	if !r.IsError || !strings.Contains(r.Text, "linker") {
		t.Fatalf("a move from a scope held by a pass must be refused, naming the lease: %s", r.Text)
	}
}

// interruptMove leaves the move of the seed's set stopped after the given step,
// as a crash there would.
func interruptMove(t *testing.T, d *Document, ctx context.Context, s copySeed, step string) {
	t.Helper()
	src, srcMem, _ := d.resolveScope(ctx, "user")
	dst, dstMem, _ := d.resolveScope(ctx, "tenant")
	if err := d.ensureSchema(ctx, dst); err != nil {
		t.Fatal(err)
	}
	ids := []string{s.planDoc, s.guideDoc}
	if step == "rows_of_one" {
		ids = ids[:1]
	}
	p := &copyPlan{move: true, src: src, dst: dst, srcMem: srcMem, dstMem: dstMem, chunkDoc: map[string]string{},
		counts: map[string]int{}, chunkMap: map[string]string{}, failedDocs: map[string]string{}}
	if err := d.planCopy(ctx, p, "move_documents", ids, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.planStatements(ctx, p); err != nil {
		t.Fatal(err)
	}
	d.writeCopy(ctx, p)
	if step == "rows_of_one" || step == "target_written" {
		return
	}
	remove := p.docs
	if step == "one_source_removed" {
		remove = remove[:1]
	}
	for _, doc := range remove {
		if _, err := d.removeDocument(ctx, src, srcMem, doc.id); err != nil {
			t.Fatal(err)
		}
	}
	if step != "named" {
		return
	}
	for _, doc := range p.docs {
		for _, n := range doc.targets {
			if _, err := d.registerDocDirent(ctx, dst, doc.id, n); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestMoveDocuments_TheSameCallResumesAfterEveryStep(t *testing.T) {
	for _, step := range []string{"rows_of_one", "target_written", "one_source_removed", "source_removed", "named"} {
		t.Run(step, func(t *testing.T) {
			d, ctx, _ := documentFixture(t)
			ctx = grantedTenantCtx(ctx)
			s := seedCopyDocs(t, d, ctx)
			text := exportsOf(t, d, ctx, s)
			interruptMove(t, d, ctx, s, step)

			out := moveSet(t, d, ctx, `"under_path":"/proj"`)
			if refused, _ := out["refused"].([]any); len(refused) != 0 {
				t.Fatalf("a resumed move refuses nothing: %v", out)
			}
			if u, _ := out["unfinished"].([]any); len(u) != 0 {
				t.Fatalf("the resumed move should finish everything: %v", out)
			}
			if nf, _ := out["name_failures"].([]any); len(nf) != 0 {
				t.Errorf("a name the interrupted call already gave is not a failure: %v", nf)
			}
			movedDocsLanded(t, d, ctx, s, text)
		})
	}
}

func TestMoveDocuments_AnUnfinishedMoveIsNotListedInTheTarget(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)
	interruptMove(t, d, ctx, s, "target_written")
	listed := copyExec(t, d, ctx, `{"op":"query_documents","scope":"tenant"}`)
	if docs, _ := listed["documents"].([]any); len(docs) != 0 {
		t.Errorf("a document mid-move is listed in its source only: %v", listed)
	}
	snap := copyExec(t, d, ctx, `{"op":"graph_snapshot","scope":"tenant"}`)
	if b, _ := json.Marshal(snap); strings.Contains(string(b), s.planDoc) {
		t.Errorf("a document mid-move is not in the target's snapshot: %s", b)
	}
	listed = copyExec(t, d, ctx, `{"op":"query_documents","scope":"user"}`)
	if docs, _ := listed["documents"].([]any); len(docs) != 2 {
		t.Errorf("the source still lists both: %v", listed)
	}
}

func TestMoveDocuments_OnPostgres(t *testing.T) {
	d, ctx := renamePostgresFixture(t)
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user", "tenant"}}
	})
	tkey, _, err := d.resolveScope(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.docSQL().DropScope(context.Background(), tkey); err != nil {
		t.Fatalf("DropScope (stale tenant): %v", err)
	}
	t.Cleanup(func() { _, _ = d.docSQL().DropScope(context.Background(), tkey) })
	s := seedCopyDocs(t, d, ctx)
	text := exportsOf(t, d, ctx, s)
	interruptMove(t, d, ctx, s, "one_source_removed")
	moveSet(t, d, ctx, `"under_path":"/proj"`)
	movedDocsLanded(t, d, ctx, s, text)
}

func TestMoveDocuments_ATargetShortOfItsSourceKeepsTheSource(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)
	interruptMove(t, d, ctx, s, "target_written")
	// The plan's part goes missing from the target between the calls.
	tkey, _, _ := d.resolveScope(ctx, "tenant")
	if err := d.exec(ctx, tkey, `DELETE FROM chunks WHERE id = ?`, s.planChunk); err != nil {
		t.Fatal(err)
	}

	out := moveSet(t, d, ctx, `"under_path":"/proj"`)
	unfinished, _ := out["unfinished"].([]any)
	if len(unfinished) != 1 || unfinished[0].(map[string]any)["document_id"] != s.planDoc ||
		unfinished[0].(map[string]any)["code"] != moveVerifyFailed {
		t.Fatalf("the plan's target is short a chunk, so it is not moved: %v", out)
	}
	if got := copyExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+s.planDoc+`"}`); asStr(got["document_id"]) != s.planDoc {
		t.Errorf("a document whose target is short keeps its source: %v", got)
	}
	if got := cleanExport(t, d, ctx, "user", s.planDoc); !strings.Contains(got, "the second draft of the plan") {
		t.Errorf("the kept source still holds the part's text: %s", got)
	}
}
