package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// scopedAnchorOntologyMD declares an NBU-resolution class whose anchor grammar keys
// parts by scope, an Act class whose grammar mixes a scope line with a legacy
// pattern: + resolve: anchor= pair, and two classes naming the anchors-only grammar as
// a citation grammar (first, and after a usable one).
//
// The scope pattern ends in (?:\s|$), not \b: RE2's \b is an ASCII word boundary and
// never matches after a Cyrillic letter.
const scopedAnchorOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### NbuResolution\n@anchor_grammar nbu-act\n\n" +
	"##### grammar:nbu-act\n" +
	"scope: ^Постанова$ => resolution\n" +
	"scope: ^Додаток(?:\\s+(?P<n>\\d+))?(?:\\s|$) => annex-{n} | annex\n" +
	"anchor: ^(?P<n>\\d+)\\.\\s.*…$ => point-{n}\n" +
	"anchor: ^(?P<n>\\d+)\\.\\s => chapter-{n}\n\n" +
	"#### Act\n@anchor_grammar act-mixed\n\n" +
	"##### grammar:act-mixed\n" +
	"scope: ^Annex (?P<n>\\d+) => annex-{n}\n" +
	"pattern: ^Article (?P<n>\\d+)\n" +
	"resolve: anchor=art{n}\n\n" +
	"#### Circular\n@citation_grammar nbu-act\n\n" +
	"#### Order\n@citation_grammar act-mixed, nbu-act\n"

const (
	nbuResolution = "Постанова"
	nbuResPoint1  = "1. Затвердити Положення про організацію системи управління ризиками…"
	nbuResAnnex   = "Додаток до постанови Правління Національного банку України"
	nbuResAnnexP1 = "1. Перелік банків, на які поширюється дія постанови…"
	nbuRegulation = "Положення про організацію системи управління ризиками в банках України"
	nbuChapter1   = "1. Основні положення та терміни"
	nbuPoint1     = "1. Це Положення розроблено відповідно до вимог Закону України “Про…"
	nbuChapter2   = "2. Система управління ризиками"
	nbuPoint57    = "57. Банк зобов’язаний забезпечити функціонування системи управління…"
	nbuAnnex2     = "Додаток 2 до Положення про організацію системи управління ризиками"
	nbuAnnex2P3   = "3. Показники, які банк розраховує щокварталу…"
)

// TestAnchorFor_ScopedGrammarKeysEachScopeApart: the resolution's points, the
// regulation's chapters and points, and each annex's points all number from 1; a
// scoped grammar keys them apart by the scope headings above the leaf.
func TestAnchorFor_ScopedGrammarKeysEachScopeApart(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, scopedAnchorOntologyMD, memrank.OntologyConfirmedStatus)
	for _, tc := range []struct {
		path []string
		want string
	}{
		{[]string{nbuResolution}, "resolution"},
		{[]string{nbuResolution, nbuResPoint1}, "resolution/point-1"},
		{[]string{nbuResolution, nbuResAnnex}, "resolution/annex"},
		{[]string{nbuResolution, nbuResAnnex, nbuResAnnexP1}, "resolution/annex/point-1"},
		// The approved act is not a scope: its parts are plain, and its own heading
		// falls back to the heading path.
		{[]string{nbuRegulation}, ""},
		{[]string{nbuRegulation, nbuChapter1}, "chapter-1"},
		{[]string{nbuRegulation, nbuChapter1, nbuPoint1}, "point-1"},
		{[]string{nbuRegulation, nbuChapter2}, "chapter-2"},
		{[]string{nbuRegulation, nbuChapter2, nbuPoint57}, "point-57"},
		{[]string{nbuRegulation, nbuAnnex2}, "annex-2"},
		{[]string{nbuRegulation, nbuAnnex2, nbuAnnex2P3}, "annex-2/point-3"},
		// A sub-part with no anchor of its own takes its point's (the keyer adds ~2).
		{[]string{nbuRegulation, nbuChapter1, nbuPoint1, "Примітка"}, "point-1"},
		{[]string{nbuRegulation, nbuAnnex2, nbuAnnex2P3, "Таблиця"}, "annex-2/point-3"},
		// Inside a scope, a heading no rule keys falls back to the heading path.
		{[]string{nbuRegulation, nbuAnnex2, "Таблиця"}, ""},
		// The leaf is never sought above the innermost scope: keyed by the chapter it
		// would read as a part of chapter 3, outside the annex it is in.
		{[]string{nbuRegulation, "3. Кредитний ризик", "Додаток 1 до розділу", "Таблиця"}, ""},
		// A legacy pattern: + resolve: anchor= pair is a leaf rule beside scope lines.
		{[]string{"Annex 1", "Article 2"}, "annex-1/art2"},
		{[]string{"Article 7"}, "art7"},
	} {
		grammar := "nbu-act"
		if strings.HasPrefix(tc.path[0], "A") {
			grammar = "act-mixed"
		}
		if got := d.anchorFor(ctx, grammar, tc.path); got != tc.want {
			t.Errorf("anchorFor(%s, %q) = %q, want %q", grammar, tc.path, got, tc.want)
		}
	}
}

// TestAnchorKeyer_ScopedSubPartDeduplicatesUnderItsPoint: a sub-part sharing its
// point's anchor is told apart by ~2, as heading-path anchors are.
func TestAnchorKeyer_ScopedSubPartDeduplicatesUnderItsPoint(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, scopedAnchorOntologyMD, memrank.OntologyConfirmedStatus)
	k := &anchorKeyer{d: d, ctx: ctx, grammar: "nbu-act", taken: map[string]bool{}}
	got := []string{
		k.next([]string{nbuResolution, nbuResPoint1}),
		k.next([]string{nbuRegulation, nbuChapter1, nbuPoint1}),
		k.next([]string{nbuRegulation, nbuChapter1, nbuPoint1, "Примітка"}),
	}
	want := []string{"resolution/point-1", "point-1", "point-1~2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("anchors = %q, want %q", got, want)
	}
}

// nbuActMD is an NBU resolution approving a regulation that has an annex: the shape
// the importer produces, with every scope's points numbering from 1.
const nbuActMD = "# Постанова Правління НБУ № 64\n\n" +
	"## " + nbuResolution + "\n\nПравління Національного банку України постановляє:\n\n" +
	"### " + nbuResPoint1 + "\n\nЗатвердити Положення.\n\n" +
	"### " + nbuResAnnex + "\n\nПерелік.\n\n" +
	"#### " + nbuResAnnexP1 + "\n\nБанки.\n\n" +
	"## " + nbuRegulation + "\n\nТекст.\n\n" +
	"### " + nbuChapter1 + "\n\nГлава.\n\n" +
	"#### " + nbuPoint1 + "\n\nПункт перший.\n\n" +
	"### " + nbuChapter2 + "\n\nГлава друга.\n\n" +
	"### " + nbuAnnex2 + "\n\nДодаток.\n\n" +
	"#### " + nbuAnnex2P3 + "\n\nПоказники.\n"

// TestResolveAnchor_ScopedGrammarFindsEachScopesPoint: imported as a work of a class
// whose anchor grammar is scoped, the act's colliding point numbers resolve to the
// right parts by their scoped anchors.
func TestResolveAnchor_ScopedGrammarFindsEachScopesPoint(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, scopedAnchorOntologyMD, memrank.OntologyConfirmedStatus)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Постанова Правління НБУ № 64","type":"NbuResolution"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID := out["document_id"].(string)
	const wk = "nbu:64"
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": nbuActMD,
		"reimport": true, "document_id": docID, "work_key": wk})
	if _, r = docExec(t, d, ctx, string(b)); r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	for anchor, title := range map[string]string{
		"point-1":                  nbuPoint1,
		"resolution/point-1":       nbuResPoint1,
		"resolution/annex/point-1": nbuResAnnexP1,
		"chapter-1":                nbuChapter1,
		"chapter-2":                nbuChapter2,
		"annex-2":                  nbuAnnex2,
		"annex-2/point-3":          nbuAnnex2P3,
	} {
		id := resolvePart(t, d, ctx, wk, anchor, 0)
		if id == "" {
			t.Errorf("resolve_anchor %s: not found", anchor)
			continue
		}
		c, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
		if r.IsError || c["title"] != title {
			t.Errorf("resolve_anchor %s = chunk titled %v, want %q", anchor, c["title"], title)
		}
	}
}

// TestExtractCitations_AnchorsOnlyGrammarIsReportedNotSilent: a grammar with anchor:
// lines and no pattern: line is usable for anchors, but named as a citation grammar it
// has nothing to find citations with — reported as the grammar's issue, never applied
// as a grammar that quietly finds nothing. Checked first in the list and after a usable
// grammar, the two paths a named citation grammar takes.
func TestExtractCitations_AnchorsOnlyGrammarIsReportedNotSilent(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, scopedAnchorOntologyMD, memrank.OntologyConfirmedStatus)
	const want = "grammar nbu-act declares no pattern: line"
	circ, _ := typedDocWithBody(t, d, ctx, "user", "Circular", "Article 3 applies.")
	if got := extractVia(t, d, ctx, map[string]any{"scope": "user", "document_id": circ}); got.Grammar != nil || !strings.Contains(got.GrammarIssue, want) {
		t.Errorf("Circular = %#v", got)
	}
	order, _ := typedDocWithBody(t, d, ctx, "user", "Order", "Article 3 applies.")
	if got := extractVia(t, d, ctx, map[string]any{"scope": "user", "document_id": order}); got.Grammar == nil || !strings.Contains(got.GrammarIssue, want) {
		t.Errorf("Order = %#v", got)
	}
}
