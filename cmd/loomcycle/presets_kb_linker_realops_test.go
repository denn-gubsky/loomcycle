package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The fixture tests pin the linker to the ops' contracts as written. This one
// runs its maintain pass against the REAL Document tool, so a contract the
// fixture got wrong — a field name, a refusal, the lease gate, the grant —
// fails here: R1 (the method grant), R2 (maintain_statement), R3
// (list_statements filters) and R6 (the lease), end to end on sqlite.

func realDocument(t *testing.T) (*builtin.Document, context.Context) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir(), StatementTimeoutMS: 30000, MaxRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := tools.WithAgentName(context.Background(), "kb/linker")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	ctx = tools.WithRunID(ctx, "run-linker-1")
	ctx = tools.WithRunPolicy(ctx, tools.RunPolicy{
		Tools:            []string{"Document", "Agent", "Channel"},
		StatementMethods: []string{"rule", "extractor"},
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"user"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}},
	})
	return &builtin.Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}, ctx
}

func realDoc(t *testing.T, d *builtin.Document, ctx context.Context, body string) map[string]any {
	t.Helper()
	res, err := d.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out
}

// runRealLinker runs kb/linker's shipped body through the real loop and code-js
// provider with d as its Document tool, and returns its report, which must be
// complete.
func runRealLinker(t *testing.T, d *builtin.Document, ctx context.Context, input string) map[string]any {
	t.Helper()
	f := newKBFixture()
	agent := knowledgeBaseConfig(t).Agents["kb/linker"]
	set := []tools.Tool{d, &kbFakeAgent{f: f}, &kbFakeChannel{f: f}}
	res, err := loop.Run(ctx, loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  "kb/linker",
		CodeBody:   agent.Code,
		Tools:      set,
		Dispatcher: tools.NewDispatcher(set),
		Segments:   []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: input}}}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(res.FinalText), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, res.FinalText)
	}
	t.Logf("report: %s", res.FinalText)
	if rep["status"] != "complete" {
		t.Fatalf("status = %v, failure %v\n%s", rep["status"], rep["failure"], res.FinalText)
	}
	return rep
}

func TestKBLinker_MaintainPassAgainstTheRealDocumentTool(t *testing.T) {
	d, ctx := realDocument(t)
	target := realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Target"}`)["document_id"].(string)
	law := realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Law"}`)["document_id"].(string)
	chunk := func(doc, title, body string) string {
		return realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"body":%q}`, doc, title, body))["id"].(string)
	}
	c1 := chunk(target, "Clause 1", kbBodyClaim)
	x1 := chunk(law, "Article 1", kbBodyLaw)
	x2 := chunk(law, "Article 2", kbBodyRule)
	propose := func(to, quote string) string {
		return realDoc(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:citation","evidence":[{"chunk_id":%q,"quote":%q}]}`,
			c1, to, c1, quote))["id"].(string)
	}
	healthy := propose(x1, "report a material incident")
	stale := propose(x2, "within 24 hours")
	// The second statement's quote leaves the passage.
	got := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, c1))
	rev := got["revision"]
	realDoc(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%v,"body":%q}`,
		c1, rev, "BODYALPHA Every operator shall report a material incident to the regulator without delay once it is detected."))

	rep := runRealLinker(t, d, ctx, fmt.Sprintf(`{"mode":"maintain","document_id":%q,"write":"propose"}`, target))
	lease, _ := rep["lease"].(map[string]any)
	if lease["acquired"] != true || lease["released"] != true {
		t.Errorf("lease = %v, want acquired and released through R6 under the R1 grant", lease)
	}
	if refusals, _ := rep["refusals"].(map[string]any); len(refusals) != 0 {
		t.Errorf("refusals = %v, want none from the real ops", refusals)
	}

	edge := func(id string) map[string]any {
		s, _ := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, id))["statement"].(map[string]any)
		return s
	}
	if h := edge(healthy); h["verified_at"] == nil {
		t.Errorf("healthy statement = %v, want verified", h)
	}
	s := edge(stale)
	ev, _ := s["evidence"].([]any)
	if len(ev) != 1 || ev[0].(map[string]any)["stale"] != true {
		t.Errorf("stale statement evidence = %v, want its one row marked stale", ev)
	}
	if s["verified_at"] != nil {
		t.Errorf("stale statement = %v, want it not verified", s)
	}
}

// withRealOntology writes md as the tenant's confirmed ontology and returns ctx
// with the tenant scope granted to the run. The ontology is the operator's to
// write, and an operator call carries no run; the linker then runs as a plain run.
func withRealOntology(t *testing.T, d *builtin.Document, ctx context.Context, md string) context.Context {
	t.Helper()
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user"}}
	})
	op := tools.WithSubstrateOperator(context.Background())
	op = tools.WithRunIdentity(op, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	op = tools.WithRunPolicy(op, tools.RunPolicy{
		Tools:  []string{"*"},
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user"}},
	})
	mj, _ := json.Marshal(md)
	onto := realDoc(t, d, op, `{"op":"import_md","scope":"tenant","markdown":`+string(mj)+`}`)
	realDoc(t, d, op, fmt.Sprintf(`{"op":"set_path","scope":"tenant","id":%q,"path":"/memory/ontology"}`, onto["document_id"]))
	root := realDoc(t, d, op, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, onto["root_chunk_id"]))
	realDoc(t, d, op, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":"confirmed","revision":%v}`,
		onto["root_chunk_id"], root["revision"]))
	return ctx
}

// kbRealOntologyMD declares a Law class whose first article is its definitions
// passage, a scheme of two concepts, and uses_term. defines is a seed kind;
// uses_term is one a tenant declares.
const kbRealOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n\n#### Law\n@definitions (?i)^Стаття 1\\b\n\n" +
	"## Relations\n@section relations\n\n### uses_term\n@inverse term_used_by\n\n" +
	"## Schemes\n@section schemes\n\n### topics\n\n#### договір\n\n#### оператор\n"

// TestKBLinker_TermsPassAgainstTheRealDocumentTool: the runtime decides which
// passages are definitional from the class's @definitions, and the linker reads
// that from its chunk listing, so this runs L3 at tenant scope against the real
// tool. The article that defines договір gets defines; the term it only uses,
// and both terms in the next article, get uses_term.
func TestKBLinker_TermsPassAgainstTheRealDocumentTool(t *testing.T) {
	d, ctx := realDocument(t)
	ctx = withRealOntology(t, d, ctx, kbRealOntologyMD)

	law := realDoc(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Закон про послуги","type":"Law"}`)
	chunk := func(title, body string) string {
		return realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":%q,"body":%q}`,
			law["document_id"], title, body))["id"].(string)
	}
	art1 := chunk("Стаття 1. Визначення термінів",
		"У цьому Законі терміни вживаються в такому значенні:\n1) договір — угода, яку оператор укладає з клієнтом на надання послуг;")
	art2 := chunk("Стаття 2. Сфера дії",
		"Цей Закон регулює відносини, що виникають, коли оператор укладає договір з клієнтом на надання послуг.")

	rep := runRealLinker(t, d, ctx, fmt.Sprintf(
		`{"document_id":%q,"scope":"tenant","write":"propose","layers":["L3"],"terms_scheme":"topics"}`, law["document_id"]))
	if refusals, _ := rep["refusals"].(map[string]any); len(refusals) != 0 {
		t.Errorf("refusals = %v, want none", refusals)
	}

	concepts := map[string]string{}
	for _, c := range realDoc(t, d, ctx, `{"op":"describe_ontology"}`)["schemes"].([]any)[0].(map[string]any)["concepts"].([]any) {
		concepts[c.(map[string]any)["chunk_id"].(string)] = c.(map[string]any)["name"].(string)
	}
	got := map[string]string{}
	for _, status := range []string{"proposed", "asserted"} {
		list := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"tenant","status":%q,"document_id":%q}`, status, law["document_id"]))
		for _, raw := range list["statements"].([]any) {
			s := raw.(map[string]any)
			from := map[string]string{art1: "art1", art2: "art2"}[s["from_id"].(string)]
			got[from+"→"+concepts[s["to_id"].(string)]] = s["kind"].(string)
		}
	}
	want := map[string]string{
		"art1→договір": "defines", "art1→оператор": "uses_term",
		"art2→договір": "uses_term", "art2→оператор": "uses_term",
	}
	if len(got) != len(want) {
		t.Errorf("statements = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	// A second pass over the same text proposes nothing: the runtime holds every
	// statement and quote already, and the report must say unchanged rather than
	// proposed.
	filed := layerCount(t, rep, "L3", "proposed") + layerCount(t, rep, "L3", "evidence_added")
	again := runRealLinker(t, d, ctx, fmt.Sprintf(
		`{"document_id":%q,"scope":"tenant","write":"propose","layers":["L3"],"terms_scheme":"topics"}`, law["document_id"]))
	for counter, want := range map[string]float64{"proposed": 0, "evidence_added": 0, "unchanged": filed} {
		if n := layerCount(t, again, "L3", counter); n != want {
			t.Errorf("second pass %s = %v, want %v", counter, n, want)
		}
	}
	if filed == 0 {
		t.Error("the first pass filed nothing, so the second pass proves nothing")
	}
}

// kbClassifyOntologyMD: a Law is a NormativeAct whose title starts "Закон
// України"; its number, from the identifier pattern, is its identity.
const kbClassifyOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n- `title` text\n\n" +
	"#### NormativeAct\n@identifier_pattern (?P<number>\\d+-[IVX]+)\n- `number` text @identity\n\n" +
	"##### Law\n@title_pattern ^Закон України\n\n" +
	"## Relations\n@section relations\n\n### uses_term\n@inverse term_used_by\n"

// TestKBLinker_ClassifyPassAgainstTheRealDocumentTool: L0 against the runtime's
// own classify op. A document whose title matches exactly one class by rule is
// classified, and the class is applied to it.
func TestKBLinker_ClassifyPassAgainstTheRealDocumentTool(t *testing.T) {
	d, ctx := realDocument(t)
	ctx = withRealOntology(t, d, ctx, kbClassifyOntologyMD)
	law := realDoc(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Закон України № 2297-VI про захист персональних даних"}`)

	rep := runRealLinker(t, d, ctx, fmt.Sprintf(
		`{"document_id":%q,"scope":"tenant","write":"propose","layers":["L0"]}`, law["document_id"]))
	if refusals, _ := rep["refusals"].(map[string]any); len(refusals) != 0 {
		t.Errorf("refusals = %v, want none", refusals)
	}
	L, _ := rep["layers"].(map[string]any)["L0"].(map[string]any)
	counts, _ := L["counts"].(map[string]any)
	if counts["classified"] != float64(1) || counts["live"] != float64(1) {
		t.Errorf("L0 = %v, want one live classification", L)
	}
	got := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q}`, law["document_id"]))
	if got["type"] != "Law" {
		t.Errorf("document type = %v, want Law applied by the classify layer", got["type"])
	}
}

// kbEditionsOntologyMD: an Order cites acts by name; each act is a document whose
// work key the import gives it.
const kbEditionsOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n- `title` text\n\n" +
	"#### Order\n@citation_grammar act-cite\n\n" +
	"##### grammar:act-cite\n" +
	"pattern: the\\s+(?P<act>[A-Z][a-z]+)\\s+Act\n" +
	"resolve: work_key=act:{act}, kind=cites\n\n" +
	"#### Act\n\n" +
	"## Relations\n@section relations\n\n### cites\n"

// TestKBLinker_NewEditionCitationsAgainstTheRealDocumentTool: an order's
// citations are filed, a new edition is imported over it, and the maintain pass
// settles them, all through the real tools. Point 1 changed but kept its
// citation: the re-import copies the statement and the pass keeps that one
// copy. Point 2 broke its citation over two lines: the old quote is gone, the
// grammar still finds it, and the pass files it on the new part with the new
// quote. Point 3 now names another act: its citation is closed and not moved.
func TestKBLinker_NewEditionCitationsAgainstTheRealDocumentTool(t *testing.T) {
	d, ctx := realDocument(t)
	ctx = withRealOntology(t, d, ctx, kbEditionsOntologyMD)
	for _, act := range []string{"Penal", "Civil", "Fiscal"} {
		md, _ := json.Marshal("# " + act + " Act\n\n## Article 1\n\nThe " + act + " Act applies.\n")
		realDoc(t, d, ctx, fmt.Sprintf(`{"op":"import_md","scope":"tenant","markdown":%s,"work_key":"act:%s"}`, md, act))
	}
	const (
		ed1 = "# Order 5\n\n## Point 1\n\nThis point applies the Penal Act in full.\n\n" +
			"## Point 2\n\nThe operator follows the Civil Act and its rules.\n\n" +
			"## Point 3\n\nDuels fall under the Fiscal Act alone.\n"
		ed2 = "# Order 5\n\n## Point 1\n\nThis point applies the Penal Act in part.\n\n" +
			"## Point 2\n\nThe operator follows the Civil\nAct and its rules.\n\n" +
			"## Point 3\n\nDuels fall under the Penal Act alone.\n"
	)
	md1, _ := json.Marshal(ed1)
	order := realDoc(t, d, ctx, `{"op":"import_md","scope":"tenant","markdown":`+string(md1)+`}`)
	doc := order["document_id"].(string)
	root := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, order["root_chunk_id"]))
	realDoc(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"type":"Order","revision":%v}`, order["root_chunk_id"], root["revision"]))

	cite := runRealLinker(t, d, ctx, fmt.Sprintf(`{"document_id":%q,"scope":"tenant","write":"propose","layers":["L2"]}`, doc))
	if n := layerCount(t, cite, "L2", "proposed"); n != 3 {
		t.Fatalf("first edition: proposed = %v, want 3 citations", n)
	}

	md2, _ := json.Marshal(ed2)
	realDoc(t, d, ctx, fmt.Sprintf(`{"op":"import_md","scope":"tenant","markdown":%s,"document_id":%q,"reimport":true}`, md2, doc))
	rep := runRealLinker(t, d, ctx, fmt.Sprintf(`{"mode":"maintain","document_id":%q,"scope":"tenant","write":"propose"}`, doc))
	if refusals, _ := rep["refusals"].(map[string]any); len(refusals) != 0 {
		t.Errorf("refusals = %v, want none", refusals)
	}

	// What the order cites now, read back from the store: part title → the cited
	// work's title (its key) and the live quotes.
	type cite1 struct{ act, quote string }
	live := map[string]cite1{}
	list := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"tenant","status":"proposed","document_id":%q}`, doc))
	for _, raw := range list["statements"].([]any) {
		s := raw.(map[string]any)
		if s["from_document_id"] != doc {
			continue
		}
		from := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, s["from_id"]))
		if from["invalid_at"] != nil {
			t.Errorf("live statement %v starts at a closed part", s["id"])
		}
		e := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"tenant","id":%q}`, s["id"]))["statement"].(map[string]any)
		var quotes []string
		for _, ev := range e["evidence"].([]any) {
			if r := ev.(map[string]any); r["stale"] != true {
				quotes = append(quotes, r["quote"].(string))
			}
		}
		key := from["title"].(string)
		if _, dup := live[key]; dup {
			t.Errorf("%s has two live citations", key)
		}
		live[key] = cite1{s["to_title"].(string), fmt.Sprint(quotes)}
	}
	want := map[string]cite1{
		"Point 1": {"act:Penal", "[the Penal Act]"},
		"Point 2": {"act:Civil", "[the Civil\nAct]"},
	}
	if len(live) != len(want) {
		t.Errorf("live citations = %v, want %v", live, want)
	}
	for k, v := range want {
		if live[k] != v {
			t.Errorf("%s cites %+v, want %+v", k, live[k], v)
		}
	}
}
