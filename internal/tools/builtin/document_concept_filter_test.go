package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// conceptOntologyMD declares two schemes. "reporting" is the name of two concepts in
// different places, so a bare name can be ambiguous.
const conceptOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Provision\n\n" +
	"## Relations\n@section relations\n\n### filed_under\n@domain Provision\n@range concept\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n\n#### Risk Management\n\n##### outsourcing\n\n#### reporting\n\n" +
	"### file-plan\n\n#### compliance\n\n##### reporting\n\n" +
	"### rank\n\n#### Law\n"

// conceptFixture is a corpus over the concept ontology: the operator context that
// wrote it, a plain reader, and a reader whose access-control set denies the
// ontology document.
type conceptFixture struct {
	c                *corpus
	operator         context.Context
	reader, unreader context.Context
	concepts         map[string]string // mirror tag → concept chunk id
}

func newConceptFixture(t *testing.T, d *Document, base context.Context) *conceptFixture {
	t.Helper()
	vs := newVectorStore(d.Store)
	d.Store = &ftsStore{vs}
	d.Embedder = newFakeEmbedder("fake", "m1", "risk", "note")
	op := installTenantOntology(t, d, base, conceptOntologyMD, memrank.OntologyConfirmedStatus)
	read, ok := d.tenantOntologyRead(op)
	if !ok || !read.Confirmed {
		t.Fatalf("ontology not confirmed: %#v", read)
	}
	reader := func(eff *authz.Effective) context.Context {
		return tools.WithRunPolicy(base, tools.RunPolicy{
			Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user"}},
			SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user"}},
			ACL:    eff,
		})
	}
	return &conceptFixture{
		c:        &corpus{t: t, d: d, vs: vs, ctx: op, docs: map[string]string{}, byID: map[string][]string{}},
		operator: op,
		reader:   reader(nil),
		unreader: reader(aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
			Resources: []string{"document:tenant:" + memrank.OntologyPath}})),
		concepts: func() map[string]string {
			m := map[string]string{}
			for tag, c := range schemeConcepts(read) {
				m[tag] = c.ChunkID
			}
			return m
		}(),
	}
}

// doc / chunk write in the named scope.
func (f *conceptFixture) doc(scope, path string) string {
	f.c.t.Helper()
	return f.c.exec(map[string]any{"op": "create_document", "scope": scope, "title": "Doc", "path": path})["document_id"].(string)
}

func (f *conceptFixture) chunk(scope, docID string, tags ...string) string {
	f.c.t.Helper()
	v := map[string]any{"op": "create_chunk", "scope": scope, "document_id": docID, "title": "Part", "body": "risk note"}
	if len(tags) > 0 {
		v["tags"] = tags
	}
	return f.c.exec(v)["id"].(string)
}

func (f *conceptFixture) link(op, from, to, kind string) string {
	f.c.t.Helper()
	out := f.c.exec(map[string]any{"op": op, "scope": "tenant", "from_id": from, "to_id": to, "kind": kind})
	id, _ := out["id"].(string)
	return id
}

// search runs op=search (or related) as ctx and returns the hit ids and the raw text.
func (f *conceptFixture) search(ctx context.Context, v map[string]any) ([]string, string) {
	f.c.t.Helper()
	if _, ok := v["op"]; !ok {
		v["op"] = "search"
	}
	if _, ok := v["query"]; !ok && v["op"] == "search" {
		v["query"] = "risk note"
	}
	v["limit"] = 50
	b, _ := json.Marshal(v)
	r, err := f.c.d.Execute(ctx, b)
	if err != nil {
		f.c.t.Fatalf("%v: %v", v, err)
	}
	if r.IsError {
		return nil, r.Text
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(r.Text), &out)
	key := "chunks"
	if v["op"] == "related" {
		key = "related"
	}
	var ids []string
	for _, h := range anyList(out[key]) {
		ids = append(ids, h.(map[string]any)["chunk_id"].(string))
	}
	for _, h := range anyList(out["linked"]) {
		ids = append(ids, h.(map[string]any)["chunk_id"].(string))
	}
	return ids, r.Text
}

func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}

func echoOf(t *testing.T, text string) map[string]any {
	t.Helper()
	var out struct {
		Filter struct {
			Applied map[string]any `json:"applied"`
		} `json:"filter"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("not a search answer: %s", text)
	}
	return out.Filter.Applied
}

// TestDocumentConceptFilter_BothTiers runs the concept filters on both SQL Memory
// tiers: the statement join is portable `?` SQL the postgres tier rebinds.
func TestDocumentConceptFilter_BothTiers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertConceptFilters(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertConceptFilters(t, d, ctx)
	})
}

func assertConceptFilters(t *testing.T, d *Document, base context.Context) {
	f := newConceptFixture(t, d, base)
	rm := f.concepts["topics/risk-management"]
	outsourcing := f.concepts["topics/risk-management/outsourcing"]
	law := f.concepts["rank/law"]
	if rm == "" || outsourcing == "" || law == "" {
		t.Fatalf("concepts = %v", f.concepts)
	}

	// Tenant scope: statements.
	kb := f.doc("tenant", "/kb/a")
	direct := f.chunk("tenant", kb)
	f.link("link_chunks", direct, rm, "subject")
	narrower := f.chunk("tenant", kb)
	f.link("link_chunks", narrower, outsourcing, "subject")
	primary := f.chunk("tenant", kb) // a sub-kind of subject
	f.link("link_chunks", primary, rm, "primary_topic")
	tagOnly := f.chunk("tenant", kb, "topics/risk-management")
	other := f.chunk("tenant", kb)
	f.link("link_chunks", other, law, "subject")
	proposed := f.chunk("tenant", kb)
	f.link("propose_statement", proposed, rm, "subject")
	refuted := f.chunk("tenant", kb)
	edge := f.link("link_chunks", refuted, rm, "subject")
	if _, r := docExec(t, d, f.operator, fmt.Sprintf(`{"op":"judge_fact","scope":"tenant","id":%q,"verdict":"unsupported","reason":"not about it"}`, edge)); r.IsError {
		t.Fatalf("judge_fact: %s", r.Text)
	}
	elsewhere := f.chunk("tenant", f.doc("tenant", "/elsewhere/b"))
	f.link("link_chunks", elsewhere, rm, "subject")
	// A relation from a match to a non-match, for expand to follow.
	f.link("link_chunks", direct, other, "cites")

	all := []string{direct, narrower, primary, elsewhere}
	for _, tc := range []struct {
		name string
		in   map[string]any
		want []string
	}{
		{"by name, with the narrower closure", map[string]any{"concept": "Risk Management"}, all},
		{"by tag", map[string]any{"concept": "topics/risk-management"}, all},
		{"by chunk id", map[string]any{"concept": outsourcing}, []string{narrower}},
		{"by narrower tag", map[string]any{"concept": "topics/risk-management/outsourcing"}, []string{narrower}},
		{"by prefix", map[string]any{"concept_prefix": "topics"}, all},
		{"by prefix, case and spacing folded", map[string]any{"concept_prefix": "Topics/Risk Management"}, all},
		{"with under_path", map[string]any{"concept": "Risk Management", "under_path": "/kb"}, []string{direct, narrower, primary}},
	} {
		v := map[string]any{"scope": "tenant"}
		for k, x := range tc.in {
			v[k] = x
		}
		ids, text := f.search(f.reader, v)
		assertHits(t, "tenant "+tc.name+": "+text, ids, tc.want...)
		if got := echoOf(t, text)["concept_matched_by"]; got != "statement" {
			t.Errorf("tenant %s: concept_matched_by = %v, want statement", tc.name, got)
		}
	}
	// Named, though the exact hit lists above already leave them out.
	ids, text := f.search(f.reader, map[string]any{"scope": "tenant", "concept": "Risk Management"})
	for what, id := range map[string]string{"a tag alone": tagOnly, "a proposed statement": proposed,
		"a refuted statement": refuted, "a statement to another concept": other} {
		if setOf(ids)[id] {
			t.Errorf("tenant scope: %s matched: %s", what, text)
		}
	}

	// related (by meaning and by relation) and expand obey the filter.
	ids, text = f.search(f.reader, map[string]any{"op": "related", "scope": "tenant", "id": other, "concept": "Risk Management"})
	if len(ids) == 0 {
		t.Errorf("related returned nothing: %s", text)
	}
	for _, id := range ids {
		if !setOf(all)[id] {
			t.Errorf("related returned %s, outside the concept: %s", id, text)
		}
	}
	ids, text = f.search(f.reader, map[string]any{"op": "related", "scope": "tenant", "related_to": direct, "under_path": "/kb"})
	if !setOf(ids)[other] {
		t.Fatalf("precondition: related_to without the concept filter returns the cited chunk: %s", text)
	}
	ids, text = f.search(f.reader, map[string]any{"op": "related", "scope": "tenant", "related_to": direct, "concept": "Risk Management"})
	if setOf(ids)[other] {
		t.Errorf("related_to returned a neighbour outside the concept: %s", text)
	}
	ids, text = f.search(f.reader, map[string]any{"scope": "tenant", "under_path": "/kb", "expand": map[string]any{"kinds": []string{"cites"}}})
	if !setOf(ids)[other] {
		t.Fatalf("precondition: expand without the concept filter adds the cited chunk: %s", text)
	}
	ids, text = f.search(f.reader, map[string]any{"scope": "tenant", "concept": "Risk Management", "expand": map[string]any{"kinds": []string{"cites"}}})
	if setOf(ids)[other] {
		t.Errorf("expand added a chunk outside the concept: %s", text)
	}

	// User scope: no statement can reach a concept, so the concept's tag does.
	ud := f.doc("user", "/notes/u")
	uTag := f.chunk("user", ud, "topics/risk-management")
	uNarrow := f.chunk("user", ud, "topics/risk-management/outsourcing")
	uUnder := f.chunk("user", ud, "topics/risk-management/undeclared")
	f.chunk("user", ud, "topics/reporting")
	f.chunk("user", ud, "topics/zzz")
	f.chunk("user", ud)
	uOther := f.chunk("user", f.doc("user", "/other/u"), "topics/risk-management")
	for _, tc := range []struct {
		name string
		in   map[string]any
		want []string
	}{
		{"by name", map[string]any{"concept": "Risk Management"}, []string{uTag, uNarrow, uUnder, uOther}},
		{"by narrower chunk id", map[string]any{"concept": outsourcing}, []string{uNarrow}},
		{"by prefix", map[string]any{"concept_prefix": "topics/risk-management"}, []string{uTag, uNarrow, uUnder, uOther}},
		{"with under_path", map[string]any{"concept": "Risk Management", "under_path": "/notes"}, []string{uTag, uNarrow, uUnder}},
	} {
		v := map[string]any{"scope": "user"}
		for k, x := range tc.in {
			v[k] = x
		}
		ids, text := f.search(f.reader, v)
		assertHits(t, "user "+tc.name+": "+text, ids, tc.want...)
		if got := echoOf(t, text)["concept_matched_by"]; got != "tag" {
			t.Errorf("user %s: concept_matched_by = %v, want tag", tc.name, got)
		}
	}
	// An undeclared prefix matches nothing, even where a chunk carries that tag.
	if ids, text := f.search(f.reader, map[string]any{"scope": "user", "concept_prefix": "topics/zzz"}); len(ids) != 0 {
		t.Errorf("an undeclared concept prefix matched: %s", text)
	}

	// An ambiguous name is refused for a caller who can read the ontology, with the tags.
	if _, text := f.search(f.reader, map[string]any{"scope": "tenant", "concept": "reporting"}); !strings.Contains(text, "file-plan/compliance/reporting") || !strings.Contains(text, "topics/reporting") {
		t.Errorf("an ambiguous concept name = %s, want a refusal naming both tags", text)
	}

	// A caller who may not read the ontology gets exactly what an unknown concept gets:
	// no hits, no refusal, the same echo.
	for _, scope := range []string{"tenant", "user"} {
		_, unknown := f.search(f.unreader, map[string]any{"scope": scope, "concept": "Zzz Unknown"})
		for _, in := range []string{"Risk Management", "reporting", rm} {
			ids, text := f.search(f.unreader, map[string]any{"scope": scope, "concept": in})
			if len(ids) != 0 || text != strings.ReplaceAll(unknown, "Zzz Unknown", in) {
				t.Errorf("%s scope, unreadable ontology, concept %q:\n got  %s\n want %s", scope, in, text, strings.ReplaceAll(unknown, "Zzz Unknown", in))
			}
		}
		if _, readable := f.search(f.reader, map[string]any{"scope": scope, "concept": "Zzz Unknown"}); readable != unknown {
			t.Errorf("%s scope: an unknown concept answers differently by caller:\n reader   %s\n unreader %s", scope, readable, unknown)
		}
	}
}
