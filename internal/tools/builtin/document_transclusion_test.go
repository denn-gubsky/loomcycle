package builtin

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// RFC BS Phase 2b — transclusion (`![[target]]` embeds) in export_md.
//
// A chunk body's `![[target]]` embed is expanded inline to the referenced
// chunk's (recursively expanded) body — but ONLY in clean export mode
// (include_metadata=false). In metadata/round-trip mode the body stays verbatim
// so an export→import cannot bake the expanded copy in and lose the embed; that
// invariant is the load-bearing one, pinned by
// TestDocumentTransclusion_MetadataModeKeepsEmbedLiteral. The resolution paths
// (path→root, `/path#Section` anchor, title, exact chunk id) all touch SQL
// Memory, so the same assertTransclusionCore body runs on BOTH tiers: the
// always-on sqlite tier via TestDocumentTransclusion_Core, and the postgres tier
// via TestDocument_PostgresTransclusion. That second name is deliberate — CI's
// "SQL Memory postgres tier" step runs builtin tests through an explicit -run
// allowlist, and only a TestDocument_Postgres* name is inside it, so a
// Transclusion-prefixed name would skip the postgres tier in CI. The cycle/depth
// guards are tier-independent Go logic and run sqlite-only.

// setChunkBody rewrites a chunk's body (reading its current revision first). Used
// to give a target chunk a distinctive body to assert against post-expansion.
func setChunkBody(t *testing.T, d *Document, ctx context.Context, id, body string) {
	t.Helper()
	g, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk %s: %s", id, r.Text)
	}
	rev := int(g["revision"].(float64))
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+id+`","revision":`+strconv.Itoa(rev)+`,"body":"`+body+`"}`); r.IsError {
		t.Fatalf("update_chunk %s: %s", id, r.Text)
	}
}

// exportMarkdown runs export_md and returns the rendered markdown; meta selects
// include_metadata (true = round-trip mode, false = clean mode).
func exportMarkdown(t *testing.T, d *Document, ctx context.Context, docID string, meta bool) string {
	t.Helper()
	flag := "false"
	if meta {
		flag = "true"
	}
	out, r := docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`","include_metadata":`+flag+`}`)
	if r.IsError {
		t.Fatalf("export_md: %s", r.Text)
	}
	return out["markdown"].(string)
}

// --- the tier-portable core (runs on sqlite AND postgres) ---

// assertTransclusionCore exercises the four resolution paths that touch SQL
// Memory — a path embed (→ document root body), a `/path#Section` anchor embed
// (→ the titled chunk), a title embed, and an exact chunk-id embed — plus the two
// "left literal" cases (an unresolved embed and a bare `[[link]]`, which is NOT a
// transclusion). Called once per SQL Memory tier.

// TestDocumentTransclusion_Core runs the core on the always-on sqlite tier.
func TestDocumentTransclusion_Core(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	assertTransclusionCore(t, d, ctx)
}

// TestDocument_PostgresTransclusion runs the SAME core on the postgres SQL Memory
// tier. Named with the TestDocument_Postgres* prefix so CI's postgres-tier -run
// allowlist actually gates it (see the file header); skips without the aux DSN.
func TestDocument_PostgresTransclusion(t *testing.T) {
	d, ctx := pgDocumentOrSkip(t)
	assertTransclusionCore(t, d, ctx)
}

func assertTransclusionCore(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()

	// Target document, path-named, with a distinctive ROOT body and two children.
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Alpha","path":"/tr/alpha"}`)
	if r.IsError {
		t.Fatalf("create target: %s", r.Text)
	}
	tgtDoc, tgtRoot := out["document_id"].(string), out["root_chunk_id"].(string)
	setChunkBody(t, d, ctx, tgtRoot, "ALPHA-ROOT-BODY")

	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+tgtDoc+`","parent_id":"`+tgtRoot+`","title":"Section","body":"SECTION-BODY"}`)
	if r.IsError {
		t.Fatalf("create Section: %s", r.Text)
	}
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+tgtDoc+`","parent_id":"`+tgtRoot+`","title":"Widget","body":"WIDGET-BODY"}`)
	if r.IsError {
		t.Fatalf("create Widget: %s", r.Text)
	}
	widgetID := out["id"].(string)

	// Source document whose chunks carry the embeds.
	out, r = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Src","path":"/tr/src"}`)
	if r.IsError {
		t.Fatalf("create source: %s", r.Text)
	}
	srcDoc, srcRoot := out["document_id"].(string), out["root_chunk_id"].(string)

	chunks := []struct{ title, body string }{
		{"c-path", "see ![[/tr/alpha]] end"},           // path → target root body
		{"c-anchor", "sec ![[/tr/alpha#Section]] end"}, // /path#Section → the titled chunk
		{"c-title", "ttl ![[Alpha]] end"},              // title → target root body
		{"c-id", "wid ![[" + widgetID + "]] end"},      // exact chunk id → that chunk
		{"c-nope", "x ![[/no/such]] y"},                // unresolved → stays literal
		{"c-bare", "b [[/tr/alpha]] b"},                // a bare link is NOT an embed
	}
	for _, c := range chunks {
		if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"`+c.title+`","body":"`+c.body+`"}`); r.IsError {
			t.Fatalf("create %s: %s", c.title, r.Text)
		}
	}

	md := exportMarkdown(t, d, ctx, srcDoc, false) // clean mode → embeds expand

	// Resolved embeds are replaced by the target bodies.
	for _, want := range []string{"ALPHA-ROOT-BODY", "SECTION-BODY", "WIDGET-BODY"} {
		if !strings.Contains(md, want) {
			t.Errorf("clean export missing expanded body %q\n---\n%s", want, md)
		}
	}
	// Every embed FORM of the target is gone (path / anchor / title all expanded).
	for _, gone := range []string{"![[/tr/alpha]]", "![[/tr/alpha#Section]]", "![[Alpha]]"} {
		if strings.Contains(md, gone) {
			t.Errorf("clean export left an embed unexpanded: %q\n---\n%s", gone, md)
		}
	}
	// An unresolved embed stays literal.
	if !strings.Contains(md, "![[/no/such]]") {
		t.Errorf("unresolved embed did not stay literal\n---\n%s", md)
	}
	// A bare `[[link]]` is NOT transcluded — its literal survives (only the c-bare
	// chunk carries `[[/tr/alpha]]`, so its presence proves the bare form was left
	// alone rather than expanded to ALPHA-ROOT-BODY).
	if !strings.Contains(md, "[[/tr/alpha]]") {
		t.Errorf("bare [[link]] was expanded or dropped by transclusion\n---\n%s", md)
	}
}

// --- the load-bearing round-trip invariant (sqlite; the includeMeta gate is Go) ---

// TestDocumentTransclusion_MetadataModeKeepsEmbedLiteral pins THE invariant: an
// embed is expanded only in clean mode. In metadata/round-trip mode the body is
// emitted verbatim (the `![[…]]` literal preserved) so
// export(include_metadata=true)→import_md round-trips the EMBED, not a baked-in
// copy. Fail-before: expanding regardless of includeMeta (dropping the
// `!includeMeta` guard in exportMD) makes the metadata export contain the target
// body and lose the literal.
func TestDocumentTransclusion_MetadataModeKeepsEmbedLiteral(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Beta","path":"/rt/beta"}`)
	betaRoot := out["root_chunk_id"].(string)
	setChunkBody(t, d, ctx, betaRoot, "BETA-BODY")

	out, _ = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"RTSrc","path":"/rt/src"}`)
	srcDoc, srcRoot := out["document_id"].(string), out["root_chunk_id"].(string)
	if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c","body":"pre ![[/rt/beta]] post"}`); r.IsError {
		t.Fatalf("create c: %s", r.Text)
	}

	// Metadata mode: the embed MUST stay literal and NOT be expanded.
	meta := exportMarkdown(t, d, ctx, srcDoc, true)
	if !strings.Contains(meta, "![[/rt/beta]]") {
		t.Errorf("metadata export dropped the literal embed (round-trip would lose it)\n---\n%s", meta)
	}
	if strings.Contains(meta, "BETA-BODY") {
		t.Errorf("metadata export expanded the embed (round-trip would bake it in)\n---\n%s", meta)
	}

	// Clean mode over the SAME fixture DOES expand — proving includeMeta is the
	// only thing that differs.
	clean := exportMarkdown(t, d, ctx, srcDoc, false)
	if !strings.Contains(clean, "BETA-BODY") {
		t.Errorf("clean export did not expand the embed\n---\n%s", clean)
	}
	if strings.Contains(clean, "![[/rt/beta]]") {
		t.Errorf("clean export left the embed literal\n---\n%s", clean)
	}
}

// --- the guards (sqlite; tier-independent recursion logic) ---

// TestDocumentTransclusion_CycleTerminates pins the cycle guard: A embeds B and B
// embeds A back. Expansion must terminate (no hang / stack blow-up) with the
// back-reference left literal at the point the cycle would close. Fail-before:
// removing the inChain guard recurses until the goroutine stack overflows.
func TestDocumentTransclusion_CycleTerminates(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	outA, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"CycA","path":"/cy/a"}`)
	aDoc, aRoot := outA["document_id"].(string), outA["root_chunk_id"].(string)
	outB, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"CycB","path":"/cy/b"}`)
	bRoot := outB["root_chunk_id"].(string)

	setChunkBody(t, d, ctx, aRoot, "AA ![[/cy/b]] AA")
	setChunkBody(t, d, ctx, bRoot, "BB ![[/cy/a]] BB")

	md := exportMarkdown(t, d, ctx, aDoc, false)
	// A expanded once into B (so "BB" is present), but B's back-embed of A is the
	// cycle and is left literal.
	if !strings.Contains(md, "AA BB") {
		t.Errorf("cycle export did not expand one level into B\n---\n%s", md)
	}
	if !strings.Contains(md, "![[/cy/a]]") {
		t.Errorf("cycle back-reference was not left literal\n---\n%s", md)
	}
}

// TestDocumentTransclusion_DepthCapStops pins the maxEmbedDepth cap on a long
// ACYCLIC chain d0→d1→…→d5: expansion inlines up to the cap and leaves the next
// embed literal (so the deepest body is never read). Fail-before: removing the
// depth cap expands the whole chain and the deepest body ("L5") appears.
func TestDocumentTransclusion_DepthCapStops(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	roots := make([]string, 6)
	docs := make([]string, 6)
	for i := 0; i < 6; i++ {
		s := strconv.Itoa(i)
		out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D`+s+`","path":"/dp/d`+s+`"}`)
		if r.IsError {
			t.Fatalf("create d%d: %s", i, r.Text)
		}
		docs[i], roots[i] = out["document_id"].(string), out["root_chunk_id"].(string)
	}
	// d0..d4 each embed the next; d5 is a terminal body.
	for i := 0; i < 5; i++ {
		setChunkBody(t, d, ctx, roots[i], "L"+strconv.Itoa(i)+" ![[/dp/d"+strconv.Itoa(i+1)+"]]")
	}
	setChunkBody(t, d, ctx, roots[5], "L5 END")

	md := exportMarkdown(t, d, ctx, docs[0], false)
	// The cap is 4: L0..L4 inline, but the embed of d5 (reached at depth 4) is left
	// literal, so L5's body is never pulled in.
	if !strings.Contains(md, "L4") {
		t.Errorf("depth export stopped too early (L4 missing)\n---\n%s", md)
	}
	if !strings.Contains(md, "![[/dp/d5]]") {
		t.Errorf("depth cap did not leave the over-depth embed literal\n---\n%s", md)
	}
	if strings.Contains(md, "L5") {
		t.Errorf("depth cap did not stop — the deepest body was expanded\n---\n%s", md)
	}
}
