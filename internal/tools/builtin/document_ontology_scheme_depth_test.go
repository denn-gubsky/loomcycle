package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// Concept schemes may be eight levels deep; classes stay at four (RFC GM P1.2).
//
// One Markdown document holds only three concept levels (root #, section ##, scheme
// ###, concepts ####–######), so the deeper levels here are built the two ways an
// operator can: create_chunk under a concept, and import_md under a parent chunk.

// deepSchemeOntologyMD declares the scheme "topics" with concepts c1 > c2 > c3 — as
// deep as one Markdown document goes — plus a class tree a > b > c > d, at the class cap.
const deepSchemeOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### a\n\n#### b\n\n##### c\n\n###### d\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n@render\n\n#### c1\n\n##### c2\n\n###### c3\n"

// conceptTag is the mirror tag of topics/c1/…/cN.
func conceptTag(n int) string {
	segs := []string{"topics"}
	for i := 1; i <= n; i++ {
		segs = append(segs, fmt.Sprintf("c%d", i))
	}
	return strings.Join(segs, "/")
}

// installDeepScheme installs deepSchemeOntologyMD and extends the c1… chain to `levels`
// concepts below the scheme, each the child of the one before. viaImport adds the
// levels below c3 with ONE import_md under c3; otherwise one create_chunk per level.
func installDeepScheme(t *testing.T, d *Document, base context.Context, levels int, viaImport bool) context.Context {
	t.Helper()
	ctx := installTenantOntology(t, d, base, deepSchemeOntologyMD, memrank.OntologyConfirmedStatus)
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		t.Fatal("ontology unreadable")
	}
	parent := schemeConcepts(read)[conceptTag(3)].ChunkID
	if parent == "" {
		t.Fatalf("c3 not read: %v", schemeConcepts(read))
	}
	if viaImport {
		var md strings.Builder
		for i := 4; i <= levels; i++ {
			md.WriteString(strings.Repeat("#", i-3) + fmt.Sprintf(" c%d\n\n", i))
		}
		mj, _ := json.Marshal(md.String())
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"import_md","scope":"tenant","document_id":%q,"parent_id":%q,"markdown":%s}`,
			read.DocumentID, parent, mj)); r.IsError {
			t.Fatalf("import_md under c3: %s", r.Text)
		}
		return ctx
	}
	for i := 4; i <= levels; i++ {
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":"c%d"}`,
			read.DocumentID, parent, i))
		if r.IsError {
			t.Fatalf("create_chunk c%d: %s", i, r.Text)
		}
		parent = out["id"].(string)
	}
	return ctx
}

func topicsScheme(t *testing.T, read OntologyRead) memrank.OntologyScheme {
	t.Helper()
	for _, s := range read.Schemes {
		if s.Name == "topics" {
			return s
		}
	}
	t.Fatalf("no topics scheme: %#v", read.Schemes)
	return memrank.OntologyScheme{}
}

// TestOntology_SchemeEightLevelsDeepIsRead — a concept eight levels below its scheme
// keeps its written parent and its full eight-segment tag, with nothing reported.
func TestOntology_SchemeEightLevelsDeepIsRead(t *testing.T) {
	for _, viaImport := range []bool{false, true} {
		t.Run(map[bool]string{false: "create_chunk", true: "import_md under a parent"}[viaImport], func(t *testing.T) {
			d, base, _ := documentFixture(t)
			ctx := installDeepScheme(t, d, base, 8, viaImport)
			read, _ := d.tenantOntologyRead(ctx)
			by := schemeConcepts(read)
			for i := 1; i <= 8; i++ {
				c, ok := by[conceptTag(i)]
				if !ok {
					t.Fatalf("c%d not read at its written depth; concepts = %v", i, slices.Sorted(maps.Keys(by)))
				}
				if want := fmt.Sprintf("c%d", i-1); i > 1 && c.Parent != want {
					t.Errorf("c%d.Parent = %q, want %q", i, c.Parent, want)
				}
			}
			if s := topicsScheme(t, read); len(s.Issues) != 0 {
				t.Errorf("an 8-level scheme reported issues: %v", s.Issues)
			}
			if read.DepthCapped {
				t.Error("an 8-level scheme set the document-wide DepthCapped")
			}
			// The prompt listing indents the deepest concept seven levels under the first.
			if r := memrank.RenderSchemes(read.Schemes); !strings.Contains(r, "\n"+strings.Repeat("  ", 7)+"- c8\n") {
				t.Errorf("c8 not rendered at depth 8:\n%s", r)
			}
		})
	}
}

// TestOntology_SchemeNineLevelsDeepIsAnIssue — past the scheme cap a concept is kept,
// flattened onto level 8 beside its written parent, and the scheme says so. The
// document-wide DepthCapped stays false: its note speaks about class types.
func TestOntology_SchemeNineLevelsDeepIsAnIssue(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installDeepScheme(t, d, base, 9, false)
	read, _ := d.tenantOntologyRead(ctx)
	by := schemeConcepts(read)
	if _, ok := by[conceptTag(8)]; !ok {
		t.Fatalf("c8 not read: %v", slices.Sorted(maps.Keys(by)))
	}
	flat := conceptTag(7) + "/c9"
	c9, ok := by[flat]
	if !ok {
		t.Fatalf("c9 not kept at the cap as %s: %v", flat, slices.Sorted(maps.Keys(by)))
	}
	if c9.Parent != "c7" {
		t.Errorf("c9.Parent = %q, want c7 (a sibling of c8)", c9.Parent)
	}
	s := topicsScheme(t, read)
	if !slices.Contains(s.Issues, memrank.SchemeDepthIssue()) {
		t.Errorf("scheme issues = %v, want %q", s.Issues, memrank.SchemeDepthIssue())
	}
	if read.DepthCapped {
		t.Error("a deep scheme alone set the document-wide DepthCapped (the class-types note)")
	}
}

// TestOntology_ClassDepthCapIsUnchanged — a class five levels below the section is
// still flattened onto level 4 and reported, exactly as before schemes got their own cap.
func TestOntology_ClassDepthCapIsUnchanged(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, deepSchemeOntologyMD, memrank.OntologyConfirmedStatus)
	read, _ := d.tenantOntologyRead(ctx)
	if read.DepthCapped {
		t.Fatal("precondition: a class tree at the cap reported flattening")
	}
	key := mustTenantKey(t, d, ctx)
	res, err := d.query(ctx, key, `SELECT id FROM chunks WHERE document_id = ? AND title = 'd'`, read.DocumentID)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("find d: %v %v", res, err)
	}
	dChunk := asStr(res.Rows[0][0])
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":"e"}`,
		read.DocumentID, dChunk)); r.IsError {
		t.Fatal(r.Text)
	}
	read, _ = d.tenantOntologyRead(ctx)
	got := map[string]string{}
	for _, tm := range read.Terms {
		got[tm.Name] = tm.Parent
	}
	if got["e"] != "c" || got["d"] != "c" {
		t.Errorf("level-5 class e: parent %q (want c, flattened beside d); d parent %q", got["e"], got["d"])
	}
	if !read.DepthCapped {
		t.Error("a class past the cap no longer reports DepthCapped")
	}
}

// TestOntology_EightLevelMirrorTagRoundTrips — the mirror writes the eight-segment tag
// on the concept chunk, and a document carrying it is found by `concept` (the tag and
// the name) and by `concept_prefix` at an ancestor.
func TestOntology_EightLevelMirrorTagRoundTrips(t *testing.T) {
	d, base, _ := documentFixture(t)
	assertEightLevelRoundTrip(t, d, base)
}

// TestOntologySchemes_EightLevelsDeepOnPostgres runs the deep-scheme read, mirror and
// concept filters on the postgres tier.
func TestOntologySchemes_EightLevelsDeepOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	assertEightLevelRoundTrip(t, d, base)
}

func assertEightLevelRoundTrip(t *testing.T, d *Document, base context.Context) {
	t.Helper()
	vs := newVectorStore(d.Store)
	d.Store = &ftsStore{vs}
	d.Embedder = newFakeEmbedder("fake", "m1", "risk", "note")
	ctx := installDeepScheme(t, d, base, 8, false)

	if rep, err := d.MirrorSchemeTags(ctx, false); err != nil || rep.Added != 8 {
		t.Fatalf("mirror = %#v, %v", rep, err)
	}
	read, _ := d.tenantOntologyRead(ctx)
	c8 := schemeConcepts(read)[conceptTag(8)].ChunkID
	if tags, _ := d.listChunkTags(ctx, mustTenantKey(t, d, ctx), c8); !slices.Contains(tags, conceptTag(8)) {
		t.Fatalf("c8 tags after mirror = %v, want %s", tags, conceptTag(8))
	}

	f := &conceptFixture{c: &corpus{t: t, d: d, vs: vs, ctx: ctx, docs: map[string]string{}, byID: map[string][]string{}}}
	ud := f.doc("user", "/notes/deep")
	deep := f.chunk("user", ud, conceptTag(8))
	shallow := f.chunk("user", ud, conceptTag(2))
	f.chunk("user", ud) // untagged
	for _, tc := range []struct {
		name string
		in   map[string]any
		want []string
	}{
		{"concept by the 8-level tag", map[string]any{"concept": conceptTag(8)}, []string{deep}},
		{"concept by name", map[string]any{"concept": "c8"}, []string{deep}},
		{"concept_prefix at an ancestor", map[string]any{"concept_prefix": conceptTag(5)}, []string{deep}},
		{"concept_prefix at a shallow ancestor", map[string]any{"concept_prefix": conceptTag(2)}, []string{deep, shallow}},
	} {
		v := map[string]any{"scope": "user"}
		for k, x := range tc.in {
			v[k] = x
		}
		ids, text := f.search(ctx, v)
		assertHits(t, tc.name+": "+text, ids, tc.want...)
	}
}

// TestExportMd_FlattensBeyondSixHeadingLevels documents existing behaviour, not a
// change: export_md renders chunk depth as heading level and Markdown stops at six, so
// a concept four or more levels below its scheme exports as `######` and a re-import
// of the whole document makes it a sibling. Deep concepts live in the chunk tree.
func TestExportMd_FlattensBeyondSixHeadingLevels(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installDeepScheme(t, d, base, 5, false)
	read, _ := d.tenantOntologyRead(ctx)
	out, r := docExec(t, d, ctx, `{"op":"export_md","scope":"tenant","id":"`+read.DocumentID+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	md, _ := out["markdown"].(string)
	for _, want := range []string{"\n###### c3\n", "\n###### c4\n", "\n###### c5\n"} {
		if !strings.Contains(md, want) {
			t.Errorf("export lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "#######") {
		t.Errorf("export wrote a heading deeper than six:\n%s", md)
	}
}
