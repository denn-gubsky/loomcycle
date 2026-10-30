package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FP §13 R5 — extract_citations applies a document class's citation grammar.

// citationsOntologyMD: Law cites with a two-pattern grammar whose resolve line fills a
// work key, an anchor and a relation kind; Memo declares no citation grammar; Order
// names one that does not exist.
const citationsOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### Law\n@citation_grammar act-cite\n\n" +
	"##### grammar:act-cite\n" +
	"pattern: (?P<verb>amends|repeals) Article (?P<art>\\d+) of the (?P<act>[A-Z][a-z]+) Act\n" +
	"pattern: the (?P<act>[A-Z][a-z]+) Act\n" +
	"resolve: work_key=act:{act}, anchor=article-{art}, kind={verb}\n\n" +
	"#### Memo\n\n" +
	"#### Order\n@citation_grammar missing-grammar\n"

const (
	penalEdition1 = "# Penal Act\n\n## Article 2\n\nTheft.\n\n## Article 9\n\nDuels.\n"
	penalEdition2 = "# Penal Act\n\n## Article 2\n\nTheft.\n"
	// The citing text opens in Cyrillic, so a byte offset and a character offset differ.
	citingBody = "Ця постанова amends Article 2 of the Penal Act and mentions the Civil Act."
)

type citationsAnswer struct {
	DocumentID   string            `json:"document_id"`
	Grammar      map[string]string `json:"grammar"`
	GrammarIssue string            `json:"grammar_issue"`
	Hits         []citationHit     `json:"hits"`
	Truncated    bool              `json:"truncated"`
}

func extractVia(t *testing.T, d *Document, ctx context.Context, q map[string]any) citationsAnswer {
	t.Helper()
	q["op"] = "extract_citations"
	b, _ := json.Marshal(q)
	_, res := docExec(t, d, ctx, string(b))
	if res.IsError {
		t.Fatalf("extract_citations: %s", res.Text)
	}
	var out citationsAnswer
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

// typedDocWithBody creates a scope document of class typ with one part holding body.
func typedDocWithBody(t *testing.T, d *Document, ctx context.Context, scope, typ, body string) (docID, chunkID string) {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":%q,"title":"Order 7","type":%q}`, scope, typ))
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, _ = out["document_id"].(string)
	bj, _ := json.Marshal(body)
	c, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":%q,"document_id":%q,"title":"Preamble","body":%s}`, scope, docID, bj))
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	chunkID, _ = c["id"].(string)
	return docID, chunkID
}

// TestExtractCitations_FindsAndResolvesWithoutFilingPlaceholders: every pattern's
// citations in text order, an overlapping shorter match dropped, character offsets,
// groups, the filled work key / anchor / kind, the cited part resolved — and a cited
// work the scope does not hold left unfiled.
func TestExtractCitations_FindsAndResolvesWithoutFilingPlaceholders(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, citationsOntologyMD, memrank.OntologyConfirmedStatus)
	reimport(t, d, ctx, penalEdition1, "act:Penal", 0)
	docID, chunkID := typedDocWithBody(t, d, ctx, "user", "Law", citingBody)

	got := extractVia(t, d, ctx, map[string]any{"scope": "user", "document_id": docID})
	if got.Grammar["name"] != "act-cite" || got.Grammar["class"] != "Law" {
		t.Fatalf("grammar = %v (issue %q)", got.Grammar, got.GrammarIssue)
	}
	if len(got.Hits) != 2 {
		t.Fatalf("want 2 hits (the inner \"the Penal Act\" overlaps the first), got %#v", got.Hits)
	}
	runes := []rune(citingBody)
	for _, h := range got.Hits {
		if h.ChunkID != chunkID || string(runes[h.CharStart:h.CharEnd]) != h.Quote {
			t.Errorf("hit %q: chunk %s, runes[%d:%d] = %q", h.Quote, h.ChunkID, h.CharStart, h.CharEnd, string(runes[h.CharStart:h.CharEnd]))
		}
	}
	first, second := got.Hits[0], got.Hits[1]
	if first.Quote != "amends Article 2 of the Penal Act" || first.CharStart != 13 {
		t.Errorf("first hit = %#v", first)
	}
	if !reflect.DeepEqual(first.Groups, map[string]string{"verb": "amends", "art": "2", "act": "Penal"}) ||
		first.WorkKey != "act:Penal" || first.Anchor != "article-2" || first.Kind != "amends" {
		t.Errorf("first hit fill = %#v", first)
	}
	if want := resolvePart(t, d, ctx, "act:Penal", "article-2", 0); want == "" || first.Resolved.ChunkID != want ||
		!first.Resolved.Exact || first.Resolved.DocumentID == "" || first.Resolved.Placeholder {
		t.Errorf("first hit resolved = %#v, want chunk %s", first.Resolved, want)
	}
	// The second pattern captures no art and no verb: no anchor, no kind.
	if second.Quote != "the Civil Act" || second.WorkKey != "act:Civil" || second.Anchor != "" || second.Kind != "" {
		t.Errorf("second hit = %#v", second)
	}
	if second.Resolved != (citationResolution{}) {
		t.Errorf("an unheld work resolved to %#v", second.Resolved)
	}
	if out, _ := docExec(t, d, ctx, `{"op":"resolve_anchor","scope":"user","work_key":"act:Civil"}`); out["found"] != false {
		t.Errorf("extract_citations filed a placeholder: %v", out)
	}

	// One chunk by id gives the same hits.
	if one := extractVia(t, d, ctx, map[string]any{"scope": "user", "id": chunkID}); !reflect.DeepEqual(one.Hits, got.Hits) {
		t.Errorf("by id = %#v", one.Hits)
	}
	// limit cuts and says so.
	if cut := extractVia(t, d, ctx, map[string]any{"scope": "user", "document_id": docID, "limit": 1}); len(cut.Hits) != 1 || !cut.Truncated {
		t.Errorf("limit 1 = %d hits, truncated %v", len(cut.Hits), cut.Truncated)
	}
}

// TestExtractCitations_ResolvesAsOfTheCitingEdition: without as_of, a citation resolves
// as of the edition of the part that makes it (its valid_at), so an old edition's
// citation of a since-repealed article still reaches it; with as_of, as of that moment.
func TestExtractCitations_ResolvesAsOfTheCitingEdition(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, citationsOntologyMD, memrank.OntologyConfirmedStatus)
	const t1, t2 = int64(1_600_000_000_000_000_000), int64(1_700_000_000_000_000_000)
	reimport(t, d, ctx, penalEdition1, "act:Penal", t1)
	reimport(t, d, ctx, penalEdition2, "act:Penal", t2) // Article 9 closes at t2

	// A first edition's parts carry no date (nothing says when they took force), so the
	// citing part is a second edition's: it is in force from t1, and that is its date.
	reimport(t, d, ctx, "# Order 1\n\n## Preamble\n\nThis order repeals nothing.\n", "order:1", t1-1)
	citing := reimport(t, d, ctx, "# Order 1\n\n## Preamble\n\nThis order repeals Article 9 of the Penal Act.\n", "order:1", t1)
	docID, _ := citing["document_id"].(string)
	rootID, _ := citing["root_chunk_id"].(string)
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+rootID+`"}`)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"type":"Law","revision":%d}`, rootID, int(g["revision"].(float64)))); r.IsError {
		t.Fatalf("type the root: %s", r.Text)
	}

	then := extractVia(t, d, ctx, map[string]any{"scope": "user", "document_id": docID})
	if len(then.Hits) != 1 || !then.Hits[0].Resolved.Exact || then.Hits[0].Kind != "repeals" {
		t.Fatalf("as of the citing edition = %#v", then.Hits)
	}
	now := extractVia(t, d, ctx, map[string]any{"scope": "user", "document_id": docID, "as_of": t2 + 1})
	if len(now.Hits) != 1 || now.Hits[0].Resolved.Exact || now.Hits[0].Resolved.DocumentID == "" {
		t.Errorf("as of after the repeal the work resolves but not the part: %#v", now.Hits)
	}
}

// TestExtractCitations_NoGrammarMeansNoHits: a class without a citation grammar, one
// naming an undeclared grammar, and a draft ontology all answer grammar:null.
func TestExtractCitations_NoGrammarMeansNoHits(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, citationsOntologyMD, memrank.OntologyConfirmedStatus)
	memo, _ := typedDocWithBody(t, d, ctx, "user", "Memo", citingBody)
	if got := extractVia(t, d, ctx, map[string]any{"scope": "user", "document_id": memo}); got.Grammar != nil || len(got.Hits) != 0 || got.GrammarIssue != "" {
		t.Errorf("Memo = %#v", got)
	}
	order, _ := typedDocWithBody(t, d, ctx, "user", "Order", citingBody)
	if got := extractVia(t, d, ctx, map[string]any{"scope": "user", "document_id": order}); got.Grammar != nil || len(got.Hits) != 0 || got.GrammarIssue == "" {
		t.Errorf("Order = %#v", got)
	}

	d2, base2, _ := documentFixture(t)
	ctx2 := installTenantOntology(t, d2, base2, citationsOntologyMD, memrank.OntologyDraftStatus)
	law, _ := typedDocWithBody(t, d2, ctx2, "user", "Law", citingBody)
	if got := extractVia(t, d2, ctx2, map[string]any{"scope": "user", "document_id": law}); got.Grammar != nil || len(got.Hits) != 0 {
		t.Errorf("a draft ontology applied its grammar: %#v", got)
	}

	for name, q := range map[string]string{
		"neither": `{"op":"extract_citations","scope":"user"}`,
		"both":    `{"op":"extract_citations","scope":"user","id":"x","document_id":"y"}`,
	} {
		if _, r := docExec(t, d, ctx, q); !r.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestExtractCitations_AnUnreadableWorkReadsAsUnresolved: a cited work held by a
// document the caller may not read answers exactly as a work the scope does not hold.
// Anything more — an id, or a flag — tells a confined reader that the work exists,
// the oracle resolve_anchor closed for the same caller. The citing document the caller
// may not read is refused outright.
func TestExtractCitations_AnUnreadableWorkReadsAsUnresolved(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, citationsOntologyMD, memrank.OntologyConfirmedStatus)
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "markdown": penalEdition1, "reimport": true, "work_key": "act:Penal"})
	out, r := docExec(t, d, ctx, string(b))
	if r.IsError {
		t.Fatalf("import the cited work: %s", r.Text)
	}
	penal, _ := out["document_id"].(string)
	if _, r := docExec(t, d, ctx, `{"op":"set_path","scope":"tenant","id":"`+penal+`","path":"/kb/secret/penal"}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	citing, _ := typedDocWithBody(t, d, ctx, "tenant", "Law", citingBody)
	if _, r := docExec(t, d, ctx, `{"op":"set_path","scope":"tenant","id":"`+citing+`","path":"/kb/public/order"}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	eff := aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}})
	denied := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.ACL = eff })

	// Raw JSON, not the decoded struct: a field the struct does not know would still
	// reach the caller.
	_, res := docExec(t, d, denied, `{"op":"extract_citations","scope":"tenant","document_id":"`+citing+`"}`)
	if res.IsError {
		t.Fatalf("extract_citations: %s", res.Text)
	}
	var raw struct {
		Hits []struct {
			Quote    string          `json:"quote"`
			Resolved json.RawMessage `json:"resolved"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(res.Text), &raw); err != nil {
		t.Fatal(err)
	}
	// Hit 0 cites the Penal Act (held, unreadable); hit 1 the Civil Act (not held).
	if len(raw.Hits) != 2 || string(raw.Hits[0].Resolved) != string(raw.Hits[1].Resolved) {
		t.Errorf("an unreadable work answered differently from an absent one: %s", res.Text)
	}
	_, res = docExec(t, d, denied, `{"op":"extract_citations","scope":"tenant","document_id":"`+penal+`"}`)
	refusedByACL(t, "extract_citations on an unreadable document", res)
}

// TestMatchCitations_OverlapGoesToTheEarlierStart is the pure matcher: a later-starting
// match inside an earlier one is dropped; same start, the earlier pattern wins.
func TestMatchCitations_OverlapGoesToTheEarlierStart(t *testing.T) {
	g := memrank.OntologyGrammar{Name: "g", Patterns: []string{`(?P<a>ab)`, `(?P<a>abc)`, `(?P<a>bc)d`}, Resolve: "work_key=k:{a}"}
	hits := matchCitations(g, "abcd abcd")
	var keys []string
	for _, h := range hits {
		keys = append(keys, h.WorkKey+"@"+fmt.Sprint(h.CharStart))
	}
	if want := []string{"k:ab@0", "k:ab@5"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("hits = %v, want %v", keys, want)
	}
}

// TestExtractCitations_AClassAppliesEachOfItsGrammars: a class naming two citation
// grammars gets the hits of both, in text order, and a grammar it names that does
// not exist is reported without stopping the other.
func TestExtractCitations_AClassAppliesEachOfItsGrammars(t *testing.T) {
	md := "# Tenant Ontology\n\n## Classes\n@section classes\n\n### Document\n- `title` text\n\n" +
		"#### Law\n@citation_grammar act-cite, code-cite, missing-grammar\n\n" +
		"##### grammar:act-cite\npattern: the (?P<act>[A-Z][a-z]+) Act\nresolve: work_key=act:{act}\n\n" +
		"##### grammar:code-cite\npattern: the (?P<code>[A-Z][a-z]+) Code\nresolve: work_key=code:{code}\n"
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus)
	docID, _ := typedDocWithBody(t, d, ctx, "user", "Law", "Під the Civil Code and the Penal Act.")
	got := extractVia(t, d, ctx, map[string]any{"scope": "user", "document_id": docID})
	if got.Grammar["name"] != "act-cite, code-cite" {
		t.Errorf("grammar = %v", got.Grammar)
	}
	if got.GrammarIssue == "" {
		t.Error("the missing grammar was not reported")
	}
	if len(got.Hits) != 2 || got.Hits[0].WorkKey != "code:Civil" || got.Hits[1].WorkKey != "act:Penal" {
		t.Errorf("hits = %#v", got.Hits)
	}
	desc := describeOntologyVia(t, d, ctx)
	for _, g := range desc.Grammars {
		if !reflect.DeepEqual(g.UsedAs, []string{"citation"}) {
			t.Errorf("grammar %s used_as %v", g.Name, g.UsedAs)
		}
	}
	if len(desc.Grammars) != 2 {
		t.Errorf("described grammars = %#v", desc.Grammars)
	}
}
