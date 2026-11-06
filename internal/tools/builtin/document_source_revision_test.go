package builtin

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// RFC GB §5 — a document source that keeps changing: the skybits kind, a
// source revision and update time, a re-import that refreshes the provenance
// (keeping the old one in the root's history), the mark_source marker, and
// find_duplicates' direction by the source's own update time. Every fixture
// here is invented.

const srcRevMD = "# Field notes\n\nThe crew met at dawn.\n\n## Plan\n\nWalk the ridge.\n"

const srcRevMD2 = "# Field notes\n\nThe crew met at dawn.\n\n## Plan\n\nWalk the ridge, then the valley.\n"

const srcRevURL = "https://workspace.example/documents/abc123"

// importSourced imports md as a new user-scope document with the given extra
// import_md fields and returns the document id and root chunk id.
func importSourced(t *testing.T, d *Document, ctx context.Context, md string, extra map[string]any) (string, string) {
	t.Helper()
	in := map[string]any{"op": "import_md", "scope": "user", "markdown": md}
	for k, v := range extra {
		in[k] = v
	}
	call, _ := json.Marshal(in)
	out, res := docExec(t, d, ctx, string(call))
	if res.IsError {
		t.Fatalf("import_md: %s", res.Text)
	}
	return asStr(out["document_id"]), asStr(out["root_chunk_id"])
}

func docProvenanceOf(t *testing.T, d *Document, ctx context.Context, docID string) (map[string]any, []any) {
	t.Helper()
	got, res := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`)
	if res.IsError {
		t.Fatalf("get_document: %s", res.Text)
	}
	p, _ := got["provenance"].(map[string]any)
	tags, _ := got["tags"].([]any)
	return p, tags
}

func docCall(t *testing.T, d *Document, ctx context.Context, in map[string]any) (map[string]any, tools.Result) {
	t.Helper()
	call, _ := json.Marshal(in)
	return docExec(t, d, ctx, string(call))
}

func TestImportSource_SkybitsKindIsAcceptedAndTagged(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, _ := importSourced(t, d, ctx, srcRevMD, map[string]any{"source_url": srcRevURL, "source_kind": "skybits"})
	p, tags := docProvenanceOf(t, d, ctx, docID)
	if p["source_kind"] != "skybits" {
		t.Errorf("source_kind = %v, want skybits", p["source_kind"])
	}
	if !slices.Contains(tags, any("source/skybits")) {
		t.Errorf("tags = %v, want source/skybits", tags)
	}
	for name, schema := range map[string]string{"Document": documentInputSchema, "File": string((&File{}).InputSchema())} {
		if !strings.Contains(schema, `"skybits"`) {
			t.Errorf("the %s schema does not offer the skybits kind", name)
		}
	}
}

func TestImportSource_RevisionAndUpdatedAtAreStoredAndShown(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, _ := importSourced(t, d, ctx, srcRevMD, map[string]any{"source_url": srcRevURL, "source_kind": "skybits",
		"source_revision": "doc:r12", "source_updated_at": "2026-10-01T10:00:00+02:00"})
	p, _ := docProvenanceOf(t, d, ctx, docID)
	if p["source_revision"] != "doc:r12" || p["source_updated_at"] != "2026-10-01T08:00:00Z" {
		t.Errorf("provenance = %v, want the revision and the update time in UTC", p)
	}
	exp, res := docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`"}`)
	if res.IsError {
		t.Fatalf("export_md: %s", res.Text)
	}
	if ep, _ := exp["provenance"].(map[string]any); ep["source_revision"] != "doc:r12" || ep["source_updated_at"] != "2026-10-01T08:00:00Z" {
		t.Errorf("export_md provenance = %v", exp["provenance"])
	}
	// Server-owned: a root edit cannot set them.
	_, root := importSourced(t, d, ctx, srcRevMD, map[string]any{"source_url": srcRevURL + "x"})
	ch, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+root+`"}`)
	upd, _ := json.Marshal(map[string]any{"op": "update_chunk", "scope": "user", "id": root, "revision": ch["revision"],
		"fields": map[string]any{"provenance": map[string]any{"source_revision": "forged", "source_status": "removed"}}})
	if _, r := docExec(t, d, ctx, string(upd)); r.IsError {
		t.Fatalf("update_chunk: %s", r.Text)
	}
	key := sidecarScope(t, d, ctx)
	cb, _ := d.readBody(ctx, store.MemoryScopeUser, key, root)
	if strings.Contains(string(cb.Fields), "forged") || strings.Contains(string(cb.Fields), "removed") {
		t.Errorf("a root edit wrote provenance fields: %s", cb.Fields)
	}
}

func TestImportSource_MalformedRevisionOrUpdatedAtIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	for name, extra := range map[string]map[string]any{
		"date only":     {"source_updated_at": "2026-10-01"},
		"words":         {"source_updated_at": "yesterday"},
		"no zone":       {"source_updated_at": "2026-10-01T10:00:00"},
		"65 runes":      {"source_revision": strings.Repeat("р", 65)},
		"control chars": {"source_revision": "doc:\nr1"},
	} {
		in := map[string]any{"op": "import_md", "scope": "user", "markdown": srcRevMD, "source_url": srcRevURL}
		for k, v := range extra {
			in[k] = v
		}
		if _, r := docCall(t, d, ctx, in); !r.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if out, _ := docExec(t, d, ctx, `{"op":"query_documents","scope":"user"}`); len(out["documents"].([]any)) != 0 {
		t.Fatalf("a refused import left documents: %v", out["documents"])
	}
	if _, r := docCall(t, d, ctx, map[string]any{"op": "import_md", "scope": "user", "markdown": srcRevMD,
		"source_url": srcRevURL, "source_revision": strings.Repeat("р", 64)}); r.IsError {
		t.Errorf("a 64-rune revision was refused: %s", r.Text)
	}
}

// rootHistory lists the root's revisions, newest first.
func rootHistory(t *testing.T, d *Document, ctx context.Context, root string) []map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"history","scope":"user","id":"`+root+`"}`)
	if r.IsError {
		t.Fatalf("history: %s", r.Text)
	}
	var revs []map[string]any
	for _, x := range out["revisions"].([]any) {
		revs = append(revs, x.(map[string]any))
	}
	return revs
}

// checkRefreshed asserts the second edition's provenance and that the first
// one is still readable in the root's history.
func checkRefreshed(t *testing.T, d *Document, ctx context.Context, docID, root string) {
	t.Helper()
	p, tags := docProvenanceOf(t, d, ctx, docID)
	if p["source_revision"] != "doc:r2" || p["source_updated_at"] != "2026-10-02T00:00:00Z" || p["source_kind"] != "skybits" || p["source_url"] != srcRevURL {
		t.Errorf("after the re-import, provenance = %v", p)
	}
	if !slices.Contains(tags, any("source/skybits")) {
		t.Errorf("tags = %v", tags)
	}
	revs := rootHistory(t, d, ctx, root)
	var older map[string]any
	for _, r := range revs {
		if pv, _ := r["provenance"].(map[string]any); pv["source_revision"] == "doc:r1" {
			older = r
		}
	}
	if older == nil {
		t.Fatalf("the earlier provenance is not in the root's history: %v", revs)
	}
	newest := revs[0]
	if pv, _ := newest["provenance"].(map[string]any); pv["source_revision"] != "doc:r2" {
		t.Errorf("the newest revision's provenance = %v", newest["provenance"])
	}
	v, r := docCall(t, d, ctx, map[string]any{"op": "get_version", "scope": "user", "id": root, "revision": older["revision"]})
	if r.IsError {
		t.Fatalf("get_version: %s", r.Text)
	}
	if pv, _ := v["provenance"].(map[string]any); pv["source_revision"] != "doc:r1" {
		t.Errorf("get_version provenance = %v", v["provenance"])
	}
	df, r := docCall(t, d, ctx, map[string]any{"op": "diff", "scope": "user", "id": root,
		"from_revision": older["revision"], "to_revision": newest["revision"]})
	if r.IsError {
		t.Fatalf("diff: %s", r.Text)
	}
	if fp, _ := df["from_provenance"].(map[string]any); fp["source_revision"] != "doc:r1" {
		t.Errorf("diff from_provenance = %v", df["from_provenance"])
	}
	if tp, _ := df["to_provenance"].(map[string]any); tp["source_revision"] != "doc:r2" {
		t.Errorf("diff to_provenance = %v", df["to_provenance"])
	}
}

func TestImportMD_ReimportByDocumentIDRefreshesTheProvenance(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, root := importSourced(t, d, ctx, srcRevMD, map[string]any{"source_url": srcRevURL, "source_kind": "skybits",
			"source_revision": "doc:r1", "source_updated_at": "2026-10-01T00:00:00Z"})
		// The kind is left out: a re-import keeps the stored one.
		if _, r := docCall(t, d, ctx, map[string]any{"op": "import_md", "scope": "user", "markdown": srcRevMD2, "reimport": true,
			"document_id": docID, "source_url": srcRevURL, "source_revision": "doc:r2", "source_updated_at": "2026-10-02T00:00:00Z"}); r.IsError {
			t.Fatalf("reimport: %s", r.Text)
		}
		checkRefreshed(t, d, ctx, docID, root)
	})
}

func TestImportMD_ReimportByWorkKeyRefreshesTheProvenance(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		const wk = "notes:field/1"
		first, r := docCall(t, d, ctx, map[string]any{"op": "import_md", "scope": "user", "markdown": srcRevMD, "reimport": true,
			"work_key": wk, "edition_date": 1_000_000, "source_url": srcRevURL, "source_kind": "skybits",
			"source_revision": "doc:r1", "source_updated_at": "2026-10-01T00:00:00Z"})
		if r.IsError {
			t.Fatalf("first edition: %s", r.Text)
		}
		docID, root := asStr(first["document_id"]), asStr(first["root_chunk_id"])
		if p, _ := docProvenanceOf(t, d, ctx, docID); p["source_revision"] != "doc:r1" {
			t.Fatalf("first edition provenance = %v", p)
		}
		if _, r := docCall(t, d, ctx, map[string]any{"op": "import_md", "scope": "user", "markdown": srcRevMD2, "reimport": true,
			"work_key": wk, "edition_date": 2_000_000, "source_url": srcRevURL,
			"source_revision": "doc:r2", "source_updated_at": "2026-10-02T00:00:00Z"}); r.IsError {
			t.Fatalf("reimport: %s", r.Text)
		}
		checkRefreshed(t, d, ctx, docID, root)
	})
}

func TestImportMD_ReimportWithAnotherSourceIsRefused(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, root := importSourced(t, d, ctx, srcRevMD, map[string]any{"source_url": srcRevURL, "source_kind": "skybits",
			"source_revision": "doc:r1"})
		before := rootHistory(t, d, ctx, root)
		for name, extra := range map[string]map[string]any{
			"another url":          {"source_url": srcRevURL + "-other"},
			"another url and kind": {"source_url": srcRevURL + "-other", "source_kind": "web"},
		} {
			in := map[string]any{"op": "import_md", "scope": "user", "markdown": srcRevMD2, "reimport": true,
				"document_id": docID, "source_revision": "doc:r2"}
			for k, v := range extra {
				in[k] = v
			}
			_, r := docCall(t, d, ctx, in)
			if !r.IsError || !strings.Contains(r.Text, "source_mismatch") {
				t.Errorf("%s: %s, want a source_mismatch refusal", name, r.Text)
			}
		}
		if p, _ := docProvenanceOf(t, d, ctx, docID); p["source_revision"] != "doc:r1" {
			t.Errorf("a refused re-import changed the provenance: %v", p)
		}
		if after := rootHistory(t, d, ctx, root); len(after) != len(before) {
			t.Errorf("a refused re-import wrote a revision: %v -> %v", before, after)
		}
		exp, _ := docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`","include_metadata":false}`)
		if strings.Contains(asStr(exp["markdown"]), "valley") {
			t.Errorf("a refused re-import changed the text: %s", exp["markdown"])
		}
	})
}

// sourcedDoc is an imported document for mark_source, with its chunk bodies.
func sourcedDoc(t *testing.T, d *Document, ctx context.Context) (docID, root string) {
	t.Helper()
	return importSourced(t, d, ctx, srcRevMD, map[string]any{"source_url": srcRevURL, "source_kind": "skybits", "source_revision": "doc:r1"})
}

func chunkBodies(t *testing.T, d *Document, ctx context.Context, docID string) map[string]string {
	t.Helper()
	key := sidecarScope(t, d, ctx)
	out := map[string]string{}
	for _, id := range importChunkIDs(t, d, ctx, docID) {
		cb, err := d.readBody(ctx, store.MemoryScopeUser, key, id)
		if err != nil {
			t.Fatal(err)
		}
		out[id] = cb.Body
	}
	return out
}

func TestMarkSource_SetsTheStatusAndTheRemovedTag(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, root := sourcedDoc(t, d, ctx)
		op := operatorCtx(ctx)
		for _, step := range []struct {
			status  string
			tagged  bool
			changed bool
		}{{"removed", true, true}, {"removed", true, false}, {"trashed", true, true}, {"present", false, true}} {
			out, r := docCall(t, d, op, map[string]any{"op": "mark_source", "scope": "user", "id": docID, "status": step.status})
			if r.IsError {
				t.Fatalf("mark_source %s: %s", step.status, r.Text)
			}
			if out["changed"] != step.changed {
				t.Errorf("mark_source %s: changed = %v, want %v", step.status, out["changed"], step.changed)
			}
			p, tags := docProvenanceOf(t, d, ctx, docID)
			if p["source_status"] != step.status || p["source_revision"] != "doc:r1" {
				t.Errorf("after %s, provenance = %v", step.status, p)
			}
			if slices.Contains(tags, any("source/removed")) != step.tagged {
				t.Errorf("after %s, tags = %v", step.status, tags)
			}
		}
		// Each change is a revision of the root; the earlier status stays in its history.
		statuses := map[string]bool{}
		for _, r := range rootHistory(t, d, ctx, root) {
			if pv, _ := r["provenance"].(map[string]any); pv != nil {
				statuses[asStr(pv["source_status"])] = true
			}
		}
		for _, s := range []string{"", "removed", "trashed", "present"} {
			if !statuses[s] {
				t.Errorf("history lacks the provenance with status %q: %v", s, statuses)
			}
		}
		// A path names the document too.
		if _, r := docCall(t, d, op, map[string]any{"op": "set_path", "scope": "user", "id": docID, "path": "/sources/field-notes"}); r.IsError {
			t.Fatal(r.Text)
		}
		if _, r := docCall(t, d, op, map[string]any{"op": "mark_source", "scope": "user", "path": "/sources/field-notes", "status": "removed"}); r.IsError {
			t.Fatalf("mark_source by path: %s", r.Text)
		}
		if p, _ := docProvenanceOf(t, d, ctx, docID); p["source_status"] != "removed" {
			t.Errorf("by path: provenance = %v", p)
		}
		// A re-import keeps the marker: only mark_source sets it.
		if _, r := docCall(t, d, ctx, map[string]any{"op": "import_md", "scope": "user", "markdown": srcRevMD2, "reimport": true,
			"document_id": docID, "source_url": srcRevURL, "source_revision": "doc:r2"}); r.IsError {
			t.Fatalf("reimport: %s", r.Text)
		}
		if p, _ := docProvenanceOf(t, d, ctx, docID); p["source_status"] != "removed" || p["source_revision"] != "doc:r2" {
			t.Errorf("after a re-import, provenance = %v", p)
		}
	})
}

func TestMarkSource_NeedsTheGrantInARun(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, _ := sourcedDoc(t, d, ctx)
	_, r := docCall(t, d, tools.WithRunID(ctx, "run-model"), map[string]any{"op": "mark_source", "scope": "user", "id": docID, "status": "removed"})
	if !r.IsError || !strings.Contains(r.Text, "permission") {
		t.Errorf("a run without the grant: %s, want a permission refusal", r.Text)
	}
	_, r = docCall(t, d, statementMethodRun(ctx, "judge"), map[string]any{"op": "mark_source", "scope": "user", "id": docID, "status": "removed"})
	if !r.IsError {
		t.Errorf("a run holding only the judge grant was let through")
	}
	if p, _ := docProvenanceOf(t, d, ctx, docID); p["source_status"] != nil {
		t.Errorf("a refused call set the status: %v", p)
	}
	if _, r = docCall(t, d, statementMethodRun(ctx, "rule"), map[string]any{"op": "mark_source", "scope": "user", "id": docID, "status": "removed"}); r.IsError {
		t.Errorf("a run granted statement_methods: %s", r.Text)
	}
	if _, r = docCall(t, d, operatorCtx(ctx), map[string]any{"op": "mark_source", "scope": "user", "id": docID, "status": "present"}); r.IsError {
		t.Errorf("the operator: %s", r.Text)
	}
}

func TestMarkSource_RefusesWhatItCannotMark(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	op := operatorCtx(ctx)
	docID, _ := sourcedDoc(t, d, ctx)
	plain, _ := importSourced(t, d, ctx, srcRevMD, nil)
	for name, in := range map[string]map[string]any{
		"bad status":   {"id": docID, "status": "gone"},
		"no status":    {"id": docID},
		"no document":  {"status": "removed"},
		"not imported": {"id": plain, "status": "removed"},
		"unknown id":   {"id": "no-such-doc", "status": "removed"},
	} {
		in["op"], in["scope"] = "mark_source", "user"
		if _, r := docCall(t, d, op, in); !r.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMarkSource_NeverTouchesBodies(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, _ := sourcedDoc(t, d, ctx)
	before := chunkBodies(t, d, ctx, docID)
	for _, s := range []string{"removed", "trashed", "present"} {
		if _, r := docCall(t, d, operatorCtx(ctx), map[string]any{"op": "mark_source", "scope": "user", "id": docID, "status": s}); r.IsError {
			t.Fatal(r.Text)
		}
	}
	after := chunkBodies(t, d, ctx, docID)
	if len(after) != len(before) {
		t.Fatalf("chunks %v -> %v", before, after)
	}
	for id, b := range before {
		if after[id] != b {
			t.Errorf("chunk %s body %q -> %q", id, b, after[id])
		}
	}
}

func TestMarkSource_IsAWrite(t *testing.T) {
	if DocumentCallReadsOnly(json.RawMessage(`{"op":"mark_source","id":"d","status":"removed"}`)) {
		t.Error("mark_source reads as read-only")
	}
	if !documentWriteOps["mark_source"] {
		t.Error("mark_source is not on documentWriteOps")
	}
	if !ontologyWriteOp("mark_source") {
		t.Error("the ontology guard does not ask mark_source")
	}
	if r, ok := docACLOps["mark_source"]; !ok || r.verb != "write" || r.target != docACLDocument {
		t.Errorf("ACL rule = %+v, want write on the document", r)
	}
}

func TestFindDuplicates_PrefersTheEarlierSourceUpdatedAt(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		older, _ := importSourced(t, d, ctx, srcRevMD, map[string]any{"source_url": srcRevURL + "/a", "source_updated_at": "2026-10-05T00:00:00Z"})
		newer, _ := importSourced(t, d, ctx, srcRevMD, map[string]any{"source_url": srcRevURL + "/b", "source_updated_at": "2026-10-01T00:00:00Z"})
		key := sidecarScope(t, d, ctx)
		// created_at says older is the original; the source's own times say newer.
		if err := d.exec(ctx, key, `UPDATE documents SET created_at = ? WHERE id = ?`, 1, older); err != nil {
			t.Fatal(err)
		}
		if err := d.exec(ctx, key, `UPDATE documents SET created_at = ? WHERE id = ?`, 2, newer); err != nil {
			t.Fatal(err)
		}
		ids, _ := json.Marshal([]string{older, newer})
		out := findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","basis":["sha256"],"document_ids":`+string(ids)+`}`)
		pairs := dupPairs(t, out)
		if len(pairs) != 1 || pairs[0]["original_id"] != newer || pairs[0]["copy_id"] != older {
			t.Errorf("pairs = %v, want %s (updated earlier at its source) as the original", pairs, newer)
		}
	})
}

// File op=import of a new edition by work_key refreshes what the provenance
// says of the captured file and of its source, and refuses another source.
func TestFileImport_ReimportRefreshesTheProvenance(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	capture := func(name, md string) inbox.Result {
		return inbox.Capture(ctx, inbox.Item{Name: name, MediaType: "text/markdown", Origin: "mcp:workspace/get"}, []byte(md))
	}
	const wk = "workspace:abc123"
	k1 := capture("notes-r1.md", srcRevMD)
	first, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": k1.Record.Ref, "reimport": true, "work_key": wk, "edition_date": 1_000_000,
		"source": map[string]any{"url": srcRevURL, "kind": "skybits", "revision": "doc:r1", "updated_at": "2026-10-01T00:00:00Z"}})
	if res.IsError {
		t.Fatal(res.Text)
	}
	docID, root := asStr(first["document_id"]), asStr(first["root_chunk_id"])
	if p, _ := docProvenanceOf(t, d, ctx, docID); p["sha256"] != k1.Record.SHA256 || p["source_revision"] != "doc:r1" {
		t.Fatalf("first edition provenance = %v", p)
	}
	k2 := capture("notes-r2.md", srcRevMD2)
	if _, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": k2.Record.Ref, "reimport": true, "work_key": wk, "edition_date": 2_000_000,
		"source": map[string]any{"url": srcRevURL, "revision": "doc:r2", "updated_at": "2026-10-02T00:00:00Z"}}); res.IsError {
		t.Fatal(res.Text)
	}
	p, _ := docProvenanceOf(t, d, ctx, docID)
	if p["sha256"] != k2.Record.SHA256 || p["file"] != "notes-r2.md" || p["source_revision"] != "doc:r2" || p["source_kind"] != "skybits" {
		t.Errorf("after the re-import, provenance = %v", p)
	}
	var shas []string
	for _, r := range rootHistory(t, d, ctx, root) {
		if pv, _ := r["provenance"].(map[string]any); pv != nil {
			shas = append(shas, asStr(pv["sha256"]))
		}
	}
	if !slices.Contains(shas, k1.Record.SHA256) || !slices.Contains(shas, k2.Record.SHA256) {
		t.Errorf("history provenance sha256s = %v, want both editions'", shas)
	}
	k3 := capture("notes-r3.md", srcRevMD)
	_, res = fileOp(t, f, ctx, map[string]any{"op": "import", "path": k3.Record.Ref, "reimport": true, "work_key": wk, "edition_date": 3_000_000,
		"source": map[string]any{"url": srcRevURL + "-elsewhere"}})
	if !res.IsError || !strings.Contains(res.Text, "source_mismatch") {
		t.Errorf("another source: %s, want source_mismatch", res.Text)
	}
}

// A re-import of the same URL may name its kind anew: documents imported as
// kind other are taken to skybits by their next re-import, the source/<kind>
// tag following.
func TestImportMD_ReimportOfTheSameURLMayChangeTheKind(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, _ := importSourced(t, d, ctx, srcRevMD, map[string]any{"source_url": srcRevURL})
		if p, tags := docProvenanceOf(t, d, ctx, docID); p["source_kind"] != "other" || !slices.Contains(tags, any("source/other")) {
			t.Fatalf("first import: provenance %v tags %v", p, tags)
		}
		if _, r := docCall(t, d, ctx, map[string]any{"op": "import_md", "scope": "user", "markdown": srcRevMD2, "reimport": true,
			"document_id": docID, "source_url": srcRevURL, "source_kind": "skybits", "source_revision": "doc:r2"}); r.IsError {
			t.Fatalf("reimport: %s", r.Text)
		}
		p, tags := docProvenanceOf(t, d, ctx, docID)
		if p["source_kind"] != "skybits" || p["source_revision"] != "doc:r2" {
			t.Errorf("provenance = %v", p)
		}
		if !slices.Contains(tags, any("source/skybits")) || slices.Contains(tags, any("source/other")) {
			t.Errorf("tags = %v, want source/skybits in place of source/other", tags)
		}
		// Left out again, the kind stays skybits.
		if _, r := docCall(t, d, ctx, map[string]any{"op": "import_md", "scope": "user", "markdown": srcRevMD, "reimport": true,
			"document_id": docID, "source_url": srcRevURL, "source_revision": "doc:r3"}); r.IsError {
			t.Fatalf("reimport: %s", r.Text)
		}
		if p, _ := docProvenanceOf(t, d, ctx, docID); p["source_kind"] != "skybits" {
			t.Errorf("a re-import without a kind changed it: %v", p)
		}
	})
}
