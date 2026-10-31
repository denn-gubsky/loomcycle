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
	"github.com/denn-gubsky/loomcycle/internal/store"
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
	return runRealLinkerWith(t, d, ctx, input, newKBFixture())
}

// runRealLinkerWith is runRealLinker with f standing in for the verdict agent and the
// queue, so a test can script the verdict and count the model calls.
func runRealLinkerWith(t *testing.T, d *builtin.Document, ctx context.Context, input string, f *kbFixture) map[string]any {
	t.Helper()
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

// TestKBLinker_RejudgeAgainstTheRealDocumentTool: check 6 reads when a statement was
// filed or last verified (get_edge) and when its ends' bodies were last written (the
// chunk_revisions log) from the real tool. A statement whose text did not change since
// costs no model call; once its target is edited, the verdict is asked again, and a
// relation it no longer finds is flagged for a person, not expired.
func TestKBLinker_RejudgeAgainstTheRealDocumentTool(t *testing.T) {
	d, ctx := realDocument(t)
	target := realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Target"}`)["document_id"].(string)
	law := realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Law"}`)["document_id"].(string)
	c1 := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Clause 1","body":%q}`, target, kbBodyClaim))["id"].(string)
	x1 := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Article 1","body":%q}`, law, kbBodyLaw))["id"].(string)
	st := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"elaborates","method":"extractor:relate","confidence":0.7,"evidence":[{"chunk_id":%q,"quote":"report a material incident"},{"chunk_id":%q,"quote":"in any case within 72 hours"}]}`,
		c1, x1, c1, x1))["id"].(string)
	in := fmt.Sprintf(`{"mode":"maintain","document_id":%q,"write":"propose","rejudge":true}`, target)
	none := `[{"candidate": 1, "kind": "none"}]`

	// Filed after both ends were written, and verified by this pass: nothing changed
	// since, so no model call.
	g := newKBFixture()
	g.defaultVerdict = none
	rep := runRealLinkerWith(t, d, ctx, in, g)
	if n := len(g.callsOf("Agent", "")); n != 0 {
		t.Errorf("unchanged: verdict calls = %d, want 0", n)
	}
	if n := layerCount(t, rep, "L6", "rejudge_unchanged"); n != 1 {
		t.Errorf("unchanged: rejudge_unchanged = %v, want 1", n)
	}
	if e := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, st))["statement"].(map[string]any); e["verified_at"] == nil {
		t.Fatalf("statement = %v, want verified by the first pass", e)
	}

	// The target's body is rewritten (its quote kept): judged again, and flagged.
	got := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, x1))
	realDoc(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%v,"body":%q}`,
		x1, got["revision"], kbBodyLaw+" Records are kept for five years."))
	h := newKBFixture()
	h.defaultVerdict = none
	rep = runRealLinkerWith(t, d, ctx, in, h)
	if n := len(h.callsOf("Agent", "")); n != 1 {
		t.Errorf("changed: verdict calls = %d, want 1", n)
	}
	if n := layerCount(t, rep, "L6", "rejudged_none"); n != 1 {
		t.Errorf("changed: rejudged_none = %v, want 1", n)
	}
	e := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, st))["statement"].(map[string]any)
	attrs, _ := e["attributes"].(map[string]any)
	if attrs["needs_review"] != true || e["expired_at"] != nil {
		t.Errorf("statement = %v, want flagged for review and still live", e)
	}
}

// TestKBLinker_SweepAgainstTheRealDocumentTool: the sweep reads the real
// list_statements with under_path, order verified and include_retired. A statement
// under the subtree is verified; one outside is not touched; a statement retired
// just now is not yet a purge candidate; and unlink_chunks, the op a purge sends,
// removes a retired statement under the linker's own grants.
func TestKBLinker_SweepAgainstTheRealDocumentTool(t *testing.T) {
	d, ctx := realDocument(t)
	acts := realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Act","path":"/acts/act"}`)["document_id"].(string)
	laws := realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Law","path":"/laws/law"}`)["document_id"].(string)
	chunk := func(doc, body string) string {
		return realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"P","body":%q}`, doc, body))["id"].(string)
	}
	c1, x1, x2 := chunk(acts, kbBodyClaim), chunk(laws, kbBodyLaw), chunk(laws, kbBodyRule)
	propose := func(from, to, quoteChunk, quote string) string {
		return realDoc(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:citation","evidence":[{"chunk_id":%q,"quote":%q}]}`,
			from, to, quoteChunk, quote))["id"].(string)
	}
	inside := propose(c1, x1, c1, "report a material incident")
	outside := propose(x1, x2, x1, "material incidents")
	retired := propose(c1, x2, c1, "to the regulator")
	realDoc(t, d, ctx, fmt.Sprintf(`{"op":"maintain_statement","scope":"user","id":%q,"action":"expire","reason":"test","method":"rule:citation"}`, retired))

	rep := runRealLinker(t, d, ctx, `{"mode":"sweep","path":"/acts","write":"propose"}`)
	edge := func(id string) map[string]any {
		return realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, id))["statement"].(map[string]any)
	}
	if edge(inside)["verified_at"] == nil {
		t.Errorf("the statement under /acts was not verified")
	}
	if edge(outside)["verified_at"] != nil {
		t.Errorf("the statement outside /acts was verified")
	}
	P, _ := layerReport(t, rep, "L6")["purge"].(map[string]any)
	if P["candidates"] != float64(0) {
		t.Errorf("purge = %v, want no candidate (retired just now)", P)
	}
	// The purge's candidate test reads expired_at off the listing: it must be there,
	// as a number, or every retired statement would read as never retired.
	seen := false
	for _, raw := range realDoc(t, d, ctx, `{"op":"list_statements","scope":"user","status":"proposed","under_path":"/acts","include_retired":true}`)["statements"].([]any) {
		if row := raw.(map[string]any); row["id"] == retired {
			_, isNum := row["expired_at"].(float64)
			seen = isNum
		}
	}
	if !seen {
		t.Fatal("the retired statement is not listed with a numeric expired_at under include_retired")
	}
	got, r := d.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"op":"unlink_chunks","scope":"user","id":%q}`, retired)))
	if r != nil || got.IsError {
		t.Fatalf("unlink_chunks on a retired statement: %v %s", r, got.Text)
	}
	list := realDoc(t, d, ctx, `{"op":"list_statements","scope":"user","status":"proposed","under_path":"/acts","include_retired":true}`)
	for _, raw := range list["statements"].([]any) {
		if raw.(map[string]any)["id"] == retired {
			t.Error("the purged statement is still listed")
		}
	}
}

// TestKBLinker_FilingAgainstTheRealDocumentTool: a law is classified live and filed
// under /by-class through the real tools. Its class home's name is already held by
// another document (the real get_document answers it; a free name is "no such
// path"), so the law is filed beside it with its id appended and neither loses a
// name; the law keeps the name it was created with.
func TestKBLinker_FilingAgainstTheRealDocumentTool(t *testing.T) {
	d, ctx := realDocument(t)
	ctx = withRealOntology(t, d, ctx, kbClassifyOntologyMD)
	decoy := realDoc(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Memo","path":"/by-class/Law/2297-VI"}`)["document_id"].(string)
	law := realDoc(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Закон України № 2297-VI","path":"/inbox/a"}`)["document_id"].(string)

	rep := runRealLinker(t, d, ctx, fmt.Sprintf(`{"document_id":%q,"scope":"tenant","write":"propose","layers":["L0"],"file":{"root":"/by-class"}}`, law))

	if fl, _ := layerReport(t, rep, "L0")["filing"].(map[string]any); fl["status"] != "filed" || fl["suffixed"] != true {
		t.Errorf("filing = %v, want filed, suffixed", fl)
	}
	at := func(path string) string {
		got, err := d.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"op":"get_document","scope":"tenant","path":%q}`, path)))
		if err != nil || got.IsError {
			return ""
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(got.Text), &out)
		id, _ := out["document_id"].(string)
		return id
	}
	if got := at("/by-class/Law/2297-VI-" + law[:8]); got != law {
		t.Errorf("the law is not filed beside the held name with its id appended (got %q)", got)
	}
	if at("/by-class/Law/2297-VI") != decoy {
		t.Error("filing took the name another document held")
	}
	if at("/inbox/a") != law {
		t.Error("filing moved the law off the name it was created with")
	}
}

// TestKBLinker_SubjectDrainAgainstTheRealChannelTool: the crossing between the
// runtime's publish and the linker's read. The runtime writes a subject event at
// (tenant, memory/subjects, scope tenant, scope_id ""); the real Channel tool, given
// the channel as the bundle declares it, must read it there, and the drain's ack
// must commit the tenant queue's cursor. The publish below is the runtime's call,
// field for field.
func TestKBLinker_SubjectDrainAgainstTheRealChannelTool(t *testing.T) {
	d, ctx := realDocument(t)
	cfg := knowledgeBaseConfig(t)
	agent := cfg.Agents["kb/linker"]
	defs := map[string]tools.ChannelDef{}
	for name, c := range cfg.Channels {
		defs[name] = tools.ChannelDef{Name: name, Scope: c.Scope, DefaultTTL: c.DefaultTTL, MaxMessages: c.MaxMessages, Semantic: c.Semantic}
	}
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user", "tenant"}}
		rp.Channel = tools.ChannelPolicyValue{Subscribe: agent.Channels.Subscribe, Channels: defs}
	})
	subject := realDoc(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Ada Lovelace"}`)["document_id"].(string)

	payload, _ := json.Marshal(map[string]any{"op": "adopted", "subject_natural_key": "person:ada-lovelace",
		"document_id": subject, "timestamp": 1})
	if _, err := (&channels.StorePublisher{Store: d.Store, Bus: d.Bus}).Write(ctx, channels.WriteRequest{
		Channel: "memory/subjects", TenantID: "tnt", Scope: store.MemoryScopeTenant, ScopeID: "",
		Payload: payload, PublishedBy: "u1", MaxMessages: 256,
	}); err != nil {
		t.Fatal(err)
	}

	set := []tools.Tool{d, &kbFakeAgent{f: newKBFixture()}, &builtin.Channel{Store: d.Store, Bus: d.Bus}}
	res, err := loop.Run(ctx, loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  "kb/linker",
		CodeBody:   agent.Code,
		Tools:      set,
		Dispatcher: tools.NewDispatcher(set),
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text",
			Text: `{"mode":"drain","scope":"tenant","write":"propose"}`}}}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(res.FinalText), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, res.FinalText)
	}
	if rep["status"] != "complete" {
		t.Fatalf("status = %v, failure %v\n%s", rep["status"], rep["failure"], res.FinalText)
	}
	msgs, _ := rep["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want the one subject event read", rep["messages"])
	}
	if m := msgs[0].(map[string]any); m["op"] != "adopted" || m["document_id"] != subject || m["outcome"] != "complete" || m["acked"] != true {
		t.Errorf("message = %v, want the adoption passed and acked", m)
	}
	cur, err := d.Store.ChannelCommittedCursor(ctx, "tnt", "memory/subjects", store.MemoryScopeTenant, "")
	if err != nil || cur == "" {
		t.Errorf("tenant queue cursor = %q, %v — the ack did not commit it", cur, err)
	}
}

// TestKBLinker_NoAgentCanPublishASubjectEvent: memory/subjects is written by the
// runtime only. An agent granted publish on it is still refused by the real
// Channel tool, so it cannot forge a merge that would have the linker re-point a
// subject's statements onto another.
func TestKBLinker_NoAgentCanPublishASubjectEvent(t *testing.T) {
	d, ctx := realDocument(t)
	cfg := knowledgeBaseConfig(t)
	c := cfg.Channels["memory/subjects"]
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Channel = tools.ChannelPolicyValue{Publish: []string{"memory/subjects"}, Channels: map[string]tools.ChannelDef{
			"memory/subjects": {Name: "memory/subjects", Scope: c.Scope, MaxMessages: c.MaxMessages, Semantic: c.Semantic, Publisher: c.Publisher},
		}}
	})
	res, err := (&builtin.Channel{Store: d.Store, Bus: d.Bus}).Execute(ctx, json.RawMessage(
		`{"op":"publish","channel":"memory/subjects","value":{"op":"merged","document_id":"a","surviving_document_id":"b"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("an agent published a subject event: %s", res.Text)
	}
	msgs, _ := d.Store.SnapshotReadChannelMessages(ctx)
	for _, m := range msgs {
		if m.Channel == "memory/subjects" {
			t.Errorf("a message landed: %+v", m)
		}
	}
}

// TestKBLinker_MergeDrainAgainstTheRealMerge: the crossing between the operator's
// merge and the linker's drain. A real merge publishes `merged` on the tenant's
// memory/subjects; the linker, draining that queue through the real Channel tool,
// confirms the merge against the duplicate's own root (fields.merged_into = the
// survivor) and runs its subject pass rather than reporting merge_unconfirmed. A
// merge that announced before recording merged_into, or recorded it under another
// name, fails here and nowhere else.
func TestKBLinker_MergeDrainAgainstTheRealMerge(t *testing.T) {
	d, ctx := realDocument(t)
	cfg := knowledgeBaseConfig(t)
	agent := cfg.Agents["kb/linker"]
	defs := map[string]tools.ChannelDef{}
	for name, c := range cfg.Channels {
		defs[name] = tools.ChannelDef{Name: name, Scope: c.Scope, DefaultTTL: c.DefaultTTL, MaxMessages: c.MaxMessages, Semantic: c.Semantic}
	}
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user", "tenant"}}
		rp.Channel = tools.ChannelPolicyValue{Subscribe: agent.Channels.Subscribe, Channels: defs}
	})
	subject := func(title, slug, key string) map[string]any {
		return realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":"/facts/%s","type":"person","subject":%q,"natural_key":%q}`,
			title, slug, title, key))
	}
	dup := subject("Ada King", "ada-king", "person:ada-king")
	surv := subject("Ada Lovelace", "ada-lovelace", "person:ada-lovelace")
	report := realDoc(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Report","path":"/kb/report"}`)["document_id"].(string)
	cite := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Summary","body":"Ada King wrote the notes."}`, report))["id"].(string)
	realDoc(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites"}`, cite, dup["root_chunk_id"]))

	// The operator plane: no run, the tenant's grants.
	op := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "tnt"})
	op = tools.UpdateRunPolicy(op, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
	})
	res, err := d.MergeSubjects(op, "tenant", dup["document_id"].(string), surv["document_id"].(string))
	if err != nil || res.NotAnnounced != "" {
		t.Fatalf("merge: %+v %v", res, err)
	}

	set := []tools.Tool{d, &kbFakeAgent{f: newKBFixture()}, &builtin.Channel{Store: d.Store, Bus: d.Bus}}
	run, err := loop.Run(ctx, loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  "kb/linker",
		CodeBody:   agent.Code,
		Tools:      set,
		Dispatcher: tools.NewDispatcher(set),
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text",
			Text: `{"mode":"drain","scope":"tenant","write":"propose"}`}}}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(run.FinalText), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, run.FinalText)
	}
	if rep["status"] != "complete" {
		t.Fatalf("status = %v, failure %v\n%s", rep["status"], rep["failure"], run.FinalText)
	}
	msgs, _ := rep["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want the one merge read", rep["messages"])
	}
	if m := msgs[0].(map[string]any); m["op"] != "merged" || m["document_id"] != dup["document_id"] || m["outcome"] != "complete" || m["acked"] != true {
		t.Errorf("message = %v, want the merge confirmed, passed and acked", m)
	}
}
