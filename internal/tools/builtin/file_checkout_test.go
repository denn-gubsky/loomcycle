package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// ---- helpers ----

// coDoc makes a document of three sections, named at p.
func coDoc(t *testing.T, d *Document, ctx context.Context, p, title string) string {
	t.Helper()
	md := fmt.Sprintf("# %s\n\nIntro of %s.\n\n## Alpha\n\nAlpha text of %s.\n\n## Beta\n\nBeta text of %s.\n\n## Gamma\n\nGamma text of %s.\n", title, title, title, title, title)
	in, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md, "path": p})
	out, r := docExec(t, d, ctx, string(in))
	if r.IsError {
		t.Fatalf("import_md %s: %s", p, r.Text)
	}
	return asStr(out["document_id"])
}

// coChunkIDs maps each in-force section title of a document to its chunk id.
func coChunkIDs(t *testing.T, d *Document, ctx context.Context, docID string) map[string]string {
	t.Helper()
	meta := true
	din, raw := checkoutDocCall(docInput{Op: "export_md", Scope: "user", DocumentID: docID, IncludeMetadata: &meta, checkout: &checkoutExport{}})
	res, err := d.executeInput(ctx, din, raw)
	if err != nil || res.IsError {
		t.Fatalf("export %s: %v %s", docID, err, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	ids := map[string]string{}
	re := regexp.MustCompile(`(?m)^#+ (.+)\n<!-- loom: (.*) -->$`)
	for _, m := range re.FindAllStringSubmatch(asStr(out["markdown"]), -1) {
		var meta struct{ ID string }
		_ = json.Unmarshal([]byte(m[2]), &meta)
		ids[m[1]] = meta.ID
	}
	return ids
}

// coDocCount is the documents of the user scope.
func coDocCount(t *testing.T, d *Document, ctx context.Context) int {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"query_documents","scope":"user","limit":1000}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	return len(jsonArray(out["documents"]))
}

// coSetup is a run with a workspace "w" and ten documents under /kb.
func coSetup(t *testing.T, l lifecycle.Limits) (*fcFixture, *Document, []string) {
	t.Helper()
	f, d := ioFixture(t, l)
	var ids []string
	for i := range 10 {
		ids = append(ids, coDoc(t, d, f.ctx, fmt.Sprintf("/kb/sub%d/doc%d", i%2, i), fmt.Sprintf("Doc %d", i)))
	}
	return f, d, ids
}

func (f *fcFixture) checkoutKB(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	out, res := f.exec(t, ctx, map[string]any{"op": "checkout", "under_path": "/kb", "to": "workspace:w:docs/"})
	if res.IsError {
		t.Fatalf("checkout: %s", res.Text)
	}
	return out
}

// coFiles is a commit answer's files keyed by file.
func coFiles(out map[string]any) map[string]map[string]any {
	m := map[string]map[string]any{}
	for _, raw := range jsonArray(out["files"]) {
		it := raw.(map[string]any)
		m[asStr(it["file"])] = it
	}
	return m
}

// coEdit replaces old with new in a checked-out file, as a script would.
func (f *fcFixture) coEdit(t *testing.T, rel, old, new string) {
	t.Helper()
	body, ok := f.wsGet(t, "w", "docs/"+rel)
	if !ok || !strings.Contains(body, old) {
		t.Fatalf("docs/%s has no %q: %q", rel, old, body)
	}
	f.wsPutOver(t, "docs/"+rel, strings.Replace(body, old, new, 1))
}

func (f *fcFixture) wsPutOver(t *testing.T, p, content string) {
	t.Helper()
	if _, err := f.ws.tool.Manager.PutFile(context.Background(), f.owner(), "w", workspace.PutArgs{Path: p, Size: int64(len(content)), Overwrite: true}, strings.NewReader(content)); err != nil {
		t.Fatalf("put %s: %v", p, err)
	}
}

// ---- the RFC §11 check ----

// A checkout of ten documents; a script changes three and adds one. The dry
// run reports exactly that and writes nothing; the commit makes three new
// editions whose unchanged parts keep their ids, and one new document at
// the path its file maps to.
func TestFileCheckoutCommit_ThreeChangedOneNewCommitsEditionsKeepingUnchangedIDs(t *testing.T) {
	f, d, ids := coSetup(t, lifecycle.Limits{})
	out := f.checkoutKB(t, f.ctx)
	if asInt(out["files"]) != 10 || len(jsonArray(out["left_out"])) != 0 {
		t.Fatalf("checkout = %v", out)
	}
	man, ok := f.wsGet(t, "w", "docs/.loom/checkout.json")
	if !ok || !strings.Contains(man, ids[0]) {
		t.Fatalf("manifest = %q", man)
	}
	before := map[string]map[string]string{}
	for _, i := range []int{1, 2, 3} {
		before[ids[i]] = coChunkIDs(t, d, f.ctx, ids[i])
	}
	f.coEdit(t, "sub1/doc1.md", "Beta text of Doc 1.", "Beta text of Doc 1, revised.")
	f.coEdit(t, "sub0/doc2.md", "Gamma text of Doc 2.", "Gamma text of Doc 2, revised.")
	f.coEdit(t, "sub1/doc3.md", "Alpha text of Doc 3.", "Alpha text of Doc 3, revised.\nAnd a second line.")
	f.wsPut(t, "w", "docs/sub0/fresh.md", "# Fresh\n\n## One\n\nNew text.\n")

	count := coDocCount(t, d, f.ctx)
	dry, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/"})
	if res.IsError {
		t.Fatalf("dry run: %s", res.Text)
	}
	counts, _ := dry["counts"].(map[string]any)
	if asInt(counts["changed"]) != 3 || asInt(counts["new"]) != 1 || asInt(counts["unchanged"]) != 7 || len(counts) != 3 || dry["dry_run"] != true {
		t.Fatalf("dry run counts = %v: %v", counts, dry["files"])
	}
	files := coFiles(dry)
	if it := files["sub1/doc3.md"]; asInt(it["lines_added"]) != 2 || asInt(it["lines_removed"]) != 1 {
		t.Errorf("doc3 line counts = %v", it)
	}
	if it := files["sub0/fresh.md"]; it["status"] != "new" || it["path"] != "/kb/sub0/fresh" {
		t.Errorf("fresh = %v", it)
	}
	if coDocCount(t, d, f.ctx) != count {
		t.Fatal("the dry run wrote a document")
	}
	for i, want := range before {
		if got := coChunkIDs(t, d, f.ctx, i); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("the dry run changed %s: %v, was %v", i, got, want)
		}
	}

	done, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false})
	if res.IsError {
		t.Fatalf("commit: %s", res.Text)
	}
	counts, _ = done["counts"].(map[string]any)
	if asInt(counts["committed"]) != 4 || asInt(counts["unchanged"]) != 7 {
		t.Fatalf("commit counts = %v: %v", counts, done["files"])
	}
	changedTitle := map[string]string{ids[1]: "Beta", ids[2]: "Gamma", ids[3]: "Alpha"}
	for id, was := range before {
		now := coChunkIDs(t, d, f.ctx, id)
		for title, cid := range was {
			if title == changedTitle[id] {
				if now[title] == cid {
					t.Errorf("%s: the changed part %s kept its id", id, title)
				}
				continue
			}
			if now[title] != cid {
				t.Errorf("%s: the unchanged part %s has id %s, was %s", id, title, now[title], cid)
			}
		}
	}
	if coDocCount(t, d, f.ctx) != count+1 {
		t.Fatal("the commit did not make one new document")
	}
	got, r := docExec(t, d, f.ctx, `{"op":"get_document","scope":"user","path":"/kb/sub0/fresh"}`)
	if r.IsError || got["title"] != "Fresh" {
		t.Fatalf("the new document at /kb/sub0/fresh: %v %s", got, r.Text)
	}

	// The manifest now says what was committed: a second dry run finds
	// nothing to do.
	again, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if counts, _ := again["counts"].(map[string]any); asInt(counts["unchanged"]) != 11 || len(counts) != 1 {
		t.Errorf("second dry run = %v", again["files"])
	}
}

// A document changed in the store after the checkout is refused as a
// conflict, and listed; the other changed file goes through.
func TestFileCommit_ADocumentChangedInTheStoreIsAConflictAndTheRestGoThrough(t *testing.T) {
	f, d, ids := coSetup(t, lifecycle.Limits{})
	f.checkoutKB(t, f.ctx)
	f.coEdit(t, "sub0/doc0.md", "Alpha text of Doc 0.", "Alpha from the script.")
	f.coEdit(t, "sub1/doc1.md", "Alpha text of Doc 1.", "Alpha from the script.")
	// Someone edits doc 0 in the store meanwhile.
	alpha := coChunkIDs(t, d, f.ctx, ids[0])["Alpha"]
	g, _ := docExec(t, d, f.ctx, `{"op":"get_chunk","scope":"user","id":"`+alpha+`"}`)
	up, _ := json.Marshal(map[string]any{"op": "update_chunk", "scope": "user", "id": alpha, "body": "Edited in the store.", "revision": g["revision"]})
	if _, r := docExec(t, d, f.ctx, string(up)); r.IsError {
		t.Fatal(r.Text)
	}
	out, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false})
	if res.IsError {
		t.Fatal(res.Text)
	}
	files := coFiles(out)
	if it := files["sub0/doc0.md"]; it["status"] != "conflict" || !strings.Contains(asStr(it["reason"]), "changed in the store") {
		t.Errorf("doc0 = %v", it)
	}
	if it := files["sub1/doc1.md"]; it["status"] != "committed" {
		t.Errorf("doc1 = %v", it)
	}
	_, body, _ := importedChunk(t, d, f.ctx, ids[0], "Alpha")
	if body != "Edited in the store." {
		t.Errorf("the conflicting file was committed over the store's edit: %q", body)
	}
}

// A deleted file deletes nothing: it is reported missing.
func TestFileCommit_ADeletedFileDeletesNothing(t *testing.T) {
	f, d, ids := coSetup(t, lifecycle.Limits{})
	f.checkoutKB(t, f.ctx)
	if _, res := f.exec(t, f.ctx, map[string]any{"op": "rm", "path": "workspace:w:docs/sub0/doc4.md"}); res.IsError {
		t.Fatal(res.Text)
	}
	count := coDocCount(t, d, f.ctx)
	out, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if it := coFiles(out)["sub0/doc4.md"]; it["status"] != "missing" || it["document_id"] != ids[4] {
		t.Errorf("doc4 = %v", it)
	}
	if coDocCount(t, d, f.ctx) != count {
		t.Error("a commit of a deleted file changed the documents")
	}
	if _, r := docExec(t, d, f.ctx, `{"op":"get_document","scope":"user","id":"`+ids[4]+`"}`); r.IsError {
		t.Errorf("doc4 is gone: %s", r.Text)
	}
}

// ---- grants and hooks ----

// Without the Document tool, or without the workspace grant write, a
// checkout is refused before anything is read or written; a commit without
// the grant read likewise.
func TestFileCheckout_WithoutTheGrantsIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	f, _, _ := coSetup(t, lifecycle.Limits{})
	noDoc := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) {
		p.Tools = slices.DeleteFunc(slices.Clone(p.Tools), func(s string) bool { return s == "Document" })
	})
	readOnly := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace.Write = false })
	for name, ctx := range map[string]context.Context{"no Document": noDoc, "workspace read only": readOnly} {
		_, res := f.exec(t, ctx, map[string]any{"op": "checkout", "under_path": "/kb", "to": "workspace:w:docs/"})
		if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryPermission {
			t.Errorf("%s: checkout = %+v", name, res)
		}
		if f.wsHas(t, "w", "docs") {
			t.Fatalf("%s: a refused checkout wrote docs/", name)
		}
	}
	f.checkoutKB(t, f.ctx)
	noRead := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace.Read = false })
	if _, res := f.exec(t, noRead, map[string]any{"op": "commit", "from": "workspace:w:docs/"}); !res.IsError || !strings.Contains(res.Text, "does not allow read") {
		t.Errorf("commit without read = %+v", res)
	}
	noWrite := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace.Write = false })
	if _, res := f.exec(t, noWrite, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false}); !res.IsError || !strings.Contains(res.Text, "does not allow write") {
		t.Errorf("a real commit without write = %+v", res)
	}
}

// A Workspace hook that refuses one file, or a Document hook that refuses
// one document, refuses the whole checkout with nothing written; on a
// commit, a Document hook refusing one import refuses every import.
func TestFileCheckoutCommit_AHookRefusingOneRefusesAllBeforeAnythingIsWritten(t *testing.T) {
	f, d, ids := coSetup(t, lifecycle.Limits{})
	refuse := func(tool, field, value string) context.Context {
		return tools.WithHookPreCheck(f.ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
			var in map[string]any
			_ = json.Unmarshal(input, &in)
			if name == tool && asStr(in[field]) == value {
				return tools.Result{Text: "no", IsError: true}, false
			}
			return tools.Result{}, true
		})
	}
	for name, ctx := range map[string]context.Context{
		"Workspace": refuse("Workspace", "path", "docs/sub1/doc7.md"),
		"Document":  refuse("Document", "document_id", ids[7]),
	} {
		_, res := f.exec(t, ctx, map[string]any{"op": "checkout", "under_path": "/kb", "to": "workspace:w:docs/"})
		if !res.IsError || !strings.Contains(res.Text, "refused by a "+name+" hook") {
			t.Errorf("%s hook: checkout = %+v", name, res)
		}
		if f.wsHas(t, "w", "docs") {
			t.Fatalf("%s hook: a refused checkout wrote docs/", name)
		}
	}
	f.checkoutKB(t, f.ctx)
	f.coEdit(t, "sub0/doc0.md", "Alpha text of Doc 0.", "Alpha changed.")
	f.coEdit(t, "sub1/doc1.md", "Alpha text of Doc 1.", "Alpha changed.")
	before := coChunkIDs(t, d, f.ctx, ids[0])
	ctx := refuse("Document", "document_id", ids[1])
	if _, res := f.exec(t, ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false}); !res.IsError || !strings.Contains(res.Text, "refused by a Document hook") {
		t.Fatalf("commit = %+v", res)
	}
	if got := coChunkIDs(t, d, f.ctx, ids[0]); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Error("a refused commit imported doc0")
	}
	ctx = refuse("Workspace", "path", "docs/sub1/doc1.md")
	if _, res := f.exec(t, ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/"}); !res.IsError || !strings.Contains(res.Text, "refused by a Workspace hook") {
		t.Errorf("commit with a Workspace hook = %+v", res)
	}
}

// ---- bounds ----

// Over max_documents and over max_bytes a checkout writes what fits and
// lists the rest, naming the bound.
func TestFileCheckout_OverABoundWritesWhatFitsAndListsTheRest(t *testing.T) {
	f, _, _ := coSetup(t, lifecycle.Limits{CheckoutMaxDocuments: 4})
	out := f.checkoutKB(t, f.ctx)
	left := jsonArray(out["left_out"])
	if asInt(out["files"]) != 4 || len(left) != 6 || !strings.Contains(fmt.Sprint(left), "workspace.checkout.max_documents") {
		t.Fatalf("checkout = %v", out)
	}

	f2, _, _ := coSetup(t, lifecycle.Limits{CheckoutMaxBytes: 1500})
	out = f2.checkoutKB(t, f2.ctx)
	left = jsonArray(out["left_out"])
	if n := asInt(out["files"]); n == 0 || n == 10 || n+len(left) != 10 || asInt(out["bytes"]) > 1500 || !strings.Contains(fmt.Sprint(left), "workspace.checkout.max_bytes") {
		t.Fatalf("checkout = %v", out)
	}
}

// ---- what the store keeps, and what a script cannot choose ----

// A part a reimport closed is not in the checked-out file, and a commit of
// the file unchanged elsewhere does not bring it back.
func TestFileCheckout_AClosedPartIsLeftOutAndNotReopened(t *testing.T) {
	f, d, ids := coSetup(t, lifecycle.Limits{})
	// A new edition of doc 0 without Gamma closes it.
	ed, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "document_id": ids[0], "reimport": true,
		"markdown": "# Doc 0\n\nIntro of Doc 0.\n\n## Alpha\n\nAlpha text of Doc 0.\n\n## Beta\n\nBeta text of Doc 0.\n"})
	if _, r := docExec(t, d, f.ctx, string(ed)); r.IsError {
		t.Fatal(r.Text)
	}
	f.checkoutKB(t, f.ctx)
	body, _ := f.wsGet(t, "w", "docs/sub0/doc0.md")
	if strings.Contains(body, "Gamma") {
		t.Fatalf("the closed part was checked out: %q", body)
	}
	f.coEdit(t, "sub0/doc0.md", "Beta text of Doc 0.", "Beta, revised.")
	if _, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false}); res.IsError {
		t.Fatal(res.Text)
	}
	if _, ok := coChunkIDs(t, d, f.ctx, ids[0])["Gamma"]; ok {
		t.Error("the commit reopened the closed part")
	}
}

// The loom comments are the script's text after a checkout: an id pointed
// at another document's part and an added relation change nothing.
func TestFileCommit_EditedLoomCommentsChooseNoIDsAndNoRelations(t *testing.T) {
	f, d, ids := coSetup(t, lifecycle.Limits{})
	f.checkoutKB(t, f.ctx)
	other := coChunkIDs(t, d, f.ctx, ids[5])["Alpha"]
	mine := coChunkIDs(t, d, f.ctx, ids[0])
	body, _ := f.wsGet(t, "w", "docs/sub0/doc0.md")
	body = strings.Replace(body, `{"id":"`+mine["Beta"]+`"`, `{"id":"`+other+`"`, 1)
	body = strings.Replace(body, "Beta text of Doc 0.", "Beta, revised.", 1)
	body += "<!-- loom-edges:\n" + mine["Alpha"] + " -> " + other + " cites\n-->\n"
	f.wsPutOver(t, "docs/sub0/doc0.md", body)
	if _, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false}); res.IsError {
		t.Fatal(res.Text)
	}
	if got := coChunkIDs(t, d, f.ctx, ids[5])["Alpha"]; got != other {
		t.Errorf("doc 5's part changed id: %s", got)
	}
	_, b, _ := importedChunk(t, d, f.ctx, ids[5], "Alpha")
	if b != "Alpha text of Doc 5." {
		t.Errorf("doc 5's part was written: %q", b)
	}
	edges, r := docExec(t, d, f.ctx, `{"op":"get_edges","scope":"user","document_id":"`+ids[0]+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if strings.Contains(fmt.Sprint(edges), "cites") {
		t.Errorf("a relation the script wrote was made: %v", edges)
	}
}

// A manifest entry rewritten to name a document that is not named at its
// file's path is a conflict: the manifest does not choose the target.
func TestFileCommit_AManifestPointedAtAnotherDocumentIsAConflict(t *testing.T) {
	f, d, ids := coSetup(t, lifecycle.Limits{})
	victim := coDoc(t, d, f.ctx, "/elsewhere/victim", "Victim")
	f.checkoutKB(t, f.ctx)
	// The entry is rewritten whole: the victim's id and its current
	// revision, so only the path check can stop it.
	_, victimRev, _, res, ok := f.file.checkoutExportOf(f.ctx, "test", "user", victim)
	if !ok {
		t.Fatal(res.Text)
	}
	var m checkoutManifest
	raw, _ := f.wsGet(t, "w", "docs/.loom/checkout.json")
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	for i := range m.Files {
		if m.Files[i].DocumentID == ids[0] {
			m.Files[i].DocumentID, m.Files[i].Revision = victim, victimRev
		}
	}
	mb, _ := json.Marshal(m)
	f.wsPutOver(t, "docs/.loom/checkout.json", string(mb))
	f.coEdit(t, "sub0/doc0.md", "Alpha text of Doc 0.", "Overwritten.")
	out, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if it := coFiles(out)["sub0/doc0.md"]; it["status"] != "conflict" {
		t.Errorf("doc0 = %v", it)
	}
	if _, b, _ := importedChunk(t, d, f.ctx, victim, "Alpha"); b != "Alpha text of Victim." {
		t.Errorf("the victim was written: %q", b)
	}
}

// A new file whose path already names a document the checkout did not take
// is a conflict, never a rename of that document's name.
func TestFileCommit_ANewFileOnATakenNameIsAConflict(t *testing.T) {
	f, d, _ := coSetup(t, lifecycle.Limits{})
	if _, res := f.exec(t, f.ctx, map[string]any{"op": "checkout", "under_path": "/kb/sub0", "to": "workspace:w:docs/"}); res.IsError {
		t.Fatal(res.Text)
	}
	held := coDoc(t, d, f.ctx, "/kb/sub0/later", "Later")
	f.wsPut(t, "w", "docs/later.md", "# Mine\n\nText.\n")
	out, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if it := coFiles(out)["later.md"]; it["status"] != "conflict" {
		t.Errorf("later.md = %v", it)
	}
	got, r := docExec(t, d, f.ctx, `{"op":"get_document","scope":"user","path":"/kb/sub0/later"}`)
	if r.IsError || got["document_id"] != held {
		t.Errorf("the name /kb/sub0/later now names %v", got)
	}
}

// Each new edition's provenance names the workspace, the run and the
// checkout, and it is tagged untrusted, even for a document written in the
// store (no import made it).
func TestFileCommit_AnEditionsProvenanceNamesTheWorkspaceRunAndCheckout(t *testing.T) {
	f, d, _ := coSetup(t, lifecycle.Limits{})
	if _, r := docExec(t, d, f.ctx, `{"op":"create_document","scope":"user","title":"Native","path":"/kb/native"}`); r.IsError {
		t.Fatal(r.Text)
	}
	co := f.checkoutKB(t, f.ctx)
	body, _ := f.wsGet(t, "w", "docs/native.md")
	f.wsPutOver(t, "docs/native.md", body+"\n## Added\n\nBy the script.\n")
	if _, res := f.exec(t, f.ctx, map[string]any{"op": "commit", "from": "workspace:w:docs/", "dry_run": false}); res.IsError {
		t.Fatal(res.Text)
	}
	got, r := docExec(t, d, f.ctx, `{"op":"get_document","scope":"user","path":"/kb/native"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	row, err := f.ws.tool.Manager.Get(context.Background(), f.owner(), "w")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := got["provenance"].(map[string]any)
	for k, want := range map[string]any{"captured_from": "workspace:w", "workspace_id": row.ID, "run_id": f.runID, "checkout": co["checkout_id"]} {
		if p[k] != want {
			t.Errorf("provenance %s = %v, want %v (%v)", k, p[k], want, p)
		}
	}
	if tags := jsonArray(got["tags"]); !slices.Contains(tags, any("untrusted")) || !slices.Contains(tags, any("source/workspace")) {
		t.Errorf("tags = %v", got["tags"])
	}
}

// stripLoomMetadata drops the metadata lines and the relations block, and
// leaves fenced code exactly as written.
func TestStripLoomMetadata_DropsMetadataOutsideFencesOnly(t *testing.T) {
	in := "# T\n<!-- loom: {\"id\":\"a\"} -->\n\nText.\n\n```\n<!-- loom: {\"id\":\"b\"} -->\n```\n<!-- loom-edges:\na -> b cites\n-->\nTail.\n"
	want := "# T\n\nText.\n\n```\n<!-- loom: {\"id\":\"b\"} -->\n```\nTail.\n"
	if got := stripLoomMetadata(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
