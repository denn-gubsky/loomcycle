package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// kb/taxonomist (RFC GM P3.2), its shipped body, against the real Document tool:
// the runtime's list_annotations, documents_summary, describe_ontology and
// propose_entity, with a fake kb/taxonomy-writer.

// taxOntologyMD declares two document classes and one scheme in force.
const taxOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n\n#### Guide\nA how-to guide for one product.\n\n#### Note\nA short note.\n\n" +
	"## Schemes\n@section schemes\n\n### topics\n\n#### Widgets\nThe widgets the company makes.\n"

type taxSpawn struct {
	prompt    string
	untrusted []string
}

// taxFake stands in for kb/taxonomy-writer.
type taxFake struct {
	mu     sync.Mutex
	spawns []taxSpawn
	reply  func(n int, kind string, s taxSpawn) (status, text string)
}

func (f *taxFake) kinds() []string {
	var out []string
	for _, s := range f.spawns {
		out = append(out, taxKind(s.prompt))
	}
	return out
}

func taxKind(prompt string) string {
	first, _, _ := strings.Cut(prompt, "\n")
	return strings.TrimPrefix(first, "REQUEST: ")
}

type taxFakeAgent struct{ f *taxFake }

func (a *taxFakeAgent) Name() string                 { return "Agent" }
func (a *taxFakeAgent) Description() string          { return "agent (test double)" }
func (a *taxFakeAgent) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (a *taxFakeAgent) Execute(_ context.Context, raw json.RawMessage) (tools.Result, error) {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	if in["name"] != "kb/taxonomy-writer" || in["result"] != "object" {
		return tools.Result{IsError: true, Text: fmt.Sprintf("unexpected spawn %s", raw)}, nil
	}
	s := taxSpawn{}
	s.prompt, _ = in["prompt"].(string)
	us, _ := in["untrusted"].([]any)
	for _, u := range us {
		um, _ := u.(map[string]any)
		txt, _ := um["text"].(string)
		s.untrusted = append(s.untrusted, txt)
	}
	a.f.mu.Lock()
	a.f.spawns = append(a.f.spawns, s)
	n := len(a.f.spawns)
	a.f.mu.Unlock()
	status, text := a.f.reply(n, taxKind(s.prompt), s)
	out := map[string]any{"agent_id": "x", "run_id": fmt.Sprintf("w%d", n), "status": status, "stop_reason": "end_turn",
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "model": "fake-model"}}
	if status == "completed" {
		out["final_text"] = text
	} else {
		out["error"] = "the child " + status
	}
	return okResult(out)
}

// taxSchemeReply is a fixed two-level scheme: what a writer returns for every
// generate, update and review request.
func taxSchemeReply(int, string, taxSpawn) (string, string) {
	return "completed", `{"concepts":[` +
		`{"name":"installation","meaning":"Setting a widget up.","excludes":"Repairs (maintenance).","children":[` +
		`{"name":"unpacking","meaning":"Taking it out of the box.","excludes":"","children":[]}]},` +
		`{"name":"maintenance","meaning":"Keeping a widget running.","excludes":"","children":[]}]}`
}

type taxFixture struct {
	d      *builtin.Document
	op     context.Context
	driver context.Context
	guides []string
	notes  []string
}

// newTaxFixture: guides Guide documents and notes Note documents, each with a
// document brief, plus a document with no brief.
func newTaxFixture(t *testing.T, guides, notes int) *taxFixture {
	t.Helper()
	d, author, _, op := annRealDocumentOp(t)
	withRealOntology(t, d, author, taxOntologyMD)
	base := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	// The taxonomist's own grants as the bundle ships them: Document and
	// Agent, the two tenant scopes, and no statement method.
	driver := tools.WithRunID(tools.WithAgentName(tools.WithRunPolicy(base, tools.RunPolicy{
		Tools:  []string{"Document", "Agent"},
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
	}), "kb/taxonomist"), "run-taxonomist")
	f := &taxFixture{d: d, op: op, driver: driver}
	mk := func(class string, i int) string {
		title := fmt.Sprintf("%s %02d", class, i)
		out := realDoc(t, d, op, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":"/kb/%s-%02d","type":%q}`,
			title, strings.ToLower(class), i, class))
		realDoc(t, d, op, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","document_id":%q,"level":"document","manual":true,`+
			`"brief":"BRIEF-%s-%02d about widget care.","abstract":"Abstract of %s."}`, out["document_id"], class, i, title))
		return out["document_id"].(string)
	}
	for i := 0; i < guides; i++ {
		f.guides = append(f.guides, mk("Guide", i))
	}
	for i := 0; i < notes; i++ {
		f.notes = append(f.notes, mk("Note", i))
	}
	realDoc(t, d, op, `{"op":"create_document","scope":"tenant","title":"Unannotated","path":"/kb/unannotated","type":"Guide"}`)
	return f
}

func runTaxonomist(t *testing.T, f *taxFixture, fake *taxFake, input string) map[string]any {
	t.Helper()
	return runTaxonomistWith(t, f, f.d, knowledgeBaseConfig(t).Agents["kb/taxonomist"].Code, fake, input)
}

// runTaxonomistWith runs code, a kb/taxonomist body, with doc as its Document tool.
func runTaxonomistWith(t *testing.T, f *taxFixture, doc tools.Tool, code string, fake *taxFake, input string) map[string]any {
	t.Helper()
	set := []tools.Tool{doc, &taxFakeAgent{f: fake}}
	res, err := loop.Run(f.driver, loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  "kb/taxonomist",
		CodeBody:   code,
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
	return rep
}

// taxSchemesInForce names the schemes describe_ontology reads as in force: a
// proposal is not one, so filing must not grow the list.
func taxSchemesInForce(t *testing.T, f *taxFixture) []string {
	t.Helper()
	var out []string
	for _, s := range realDoc(t, f.d, f.op, `{"op":"describe_ontology"}`)["schemes"].([]any) {
		out = append(out, s.(map[string]any)["name"].(string))
	}
	return out
}

func taxNum(m map[string]any, path ...string) float64 {
	var cur any = m
	for _, p := range path {
		mm, _ := cur.(map[string]any)
		cur = mm[p]
	}
	v, _ := cur.(float64)
	return v
}

func TestKBTaxonomist_ReportCountsAndCallsNoModel(t *testing.T) {
	f := newTaxFixture(t, 30, 15)
	fake := &taxFake{reply: taxSchemeReply}
	rep := runTaxonomist(t, f, fake, `{"mode":"report","batch_size":20,"runs":2}`)
	if rep["status"] != "complete" || len(fake.spawns) != 0 {
		t.Fatalf("report = %v, spawns %d; want complete with no model call", rep, len(fake.spawns))
	}
	if taxNum(rep, "corpus", "used") != 45 || taxNum(rep, "corpus", "skipped", "no_brief") != 0 {
		t.Errorf("corpus = %v, want the 45 briefed documents (the unannotated one has no record)", rep["corpus"])
	}
	// 45 briefs in batches of about 20 is 3 batches; 2 runs x 3 + 1 review = 7.
	if rep["batches"] != float64(3) || taxNum(rep, "calls", "planned_generate") != 7 || taxNum(rep, "calls", "estimated_chars") <= 0 {
		t.Errorf("plan = batches %v calls %v, want 3 batches and 7 planned calls with an estimate", rep["batches"], rep["calls"])
	}
	if strings.Contains(fmt.Sprint(rep), "BRIEF-") {
		t.Error("the report carries brief text")
	}
}

// TestKBTaxonomist_GenerateFilesOneInertWholeSchemeProposal: each run is a
// generate request and an update per further batch, starting at a different
// batch; a review closes the pass; the result is one proposal, not in force.
func TestKBTaxonomist_GenerateFilesOneInertWholeSchemeProposal(t *testing.T) {
	f := newTaxFixture(t, 30, 15)
	fake := &taxFake{reply: taxSchemeReply}
	rep := runTaxonomist(t, f, fake,
		`{"mode":"generate","scheme":"care","domain":"How widgets are cared for.","batch_size":20,"runs":2}`)
	if rep["status"] != "complete" {
		t.Fatalf("generate = %v", rep)
	}
	if got := strings.Join(fake.kinds(), ","); got != "generate,update,update,generate,update,update,review" {
		t.Fatalf("requests = %s", got)
	}
	// The two runs meet the batches in a different order.
	if fake.spawns[0].untrusted[0] == fake.spawns[3].untrusted[0] {
		t.Error("both runs started with the same batch")
	}
	for i, s := range fake.spawns {
		if strings.Contains(s.prompt, "BRIEF-") {
			t.Errorf("request %d carries a brief in its trusted prompt", i+1)
		}
		if i < 6 && !strings.Contains(strings.Join(s.untrusted, "\n"), "BRIEF-") {
			t.Errorf("request %d carries no brief as data", i+1)
		}
	}
	if !strings.Contains(fake.spawns[6].prompt, "at least 1 of the 2 candidates") || len(fake.spawns[6].untrusted) != 2 {
		t.Errorf("review = %q with %d blocks, want both candidates and the recurrence rule", fake.spawns[6].prompt, len(fake.spawns[6].untrusted))
	}
	p, _ := rep["proposal"].(map[string]any)
	if p["filed"] != true || p["concepts"] != float64(3) {
		t.Fatalf("proposal = %v, want filed with 3 concepts", p)
	}
	if got := taxSchemesInForce(t, f); strings.Join(got, ",") != "topics" {
		t.Errorf("schemes in force = %v: a proposal put a scheme in force", got)
	}
	ch := realDoc(t, f.d, f.op, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, p["chunk_id"]))
	if ch["status"] != "proposed" || ch["title"] != "care" {
		t.Errorf("filed chunk = %v, want the proposed scheme care", ch)
	}
	fields, _ := json.Marshal(ch["fields"])
	if !strings.Contains(string(fields), `"origin":"generated"`) {
		t.Errorf("scheme proposal fields = %s, want header origin generated", fields)
	}
	if taxNum(rep, "usage", "input_tokens") != 700 {
		t.Errorf("usage = %v, want the 7 children's tokens summed", rep["usage"])
	}
}

func TestKBTaxonomist_WriteNoneFilesNothing(t *testing.T) {
	f := newTaxFixture(t, 10, 0)
	fake := &taxFake{reply: taxSchemeReply}
	rep := runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"care","domain":"Care.","runs":1,"write":"none"}`)
	if rep["status"] != "complete" || rep["proposal"] != nil {
		t.Fatalf("generate write none = %v, want complete and no proposal", rep)
	}
	sc, _ := rep["scheme"].(map[string]any)
	if sc == nil || sc["outline"] == nil {
		t.Fatalf("scheme = %v, want its outline", sc)
	}
	// With one run the review keeps whatever passes its checks.
	if got := strings.Join(fake.kinds(), ","); got != "generate,review" || !strings.Contains(fake.spawns[1].prompt, "KEEP: every concept") {
		t.Errorf("requests = %s", got)
	}
}

// TestKBTaxonomist_ReplyIsData: a concept outside the shape is dropped (a
// directive-looking name, children deeper than asked, a duplicate name), and a
// reply that is not a concept tree files nothing.
func TestKBTaxonomist_ReplyIsData(t *testing.T) {
	f := newTaxFixture(t, 10, 0)
	fake := &taxFake{reply: func(int, string, taxSpawn) (string, string) {
		return "completed", "Here it is:\n```json\n" + `{"concepts":[` +
			`{"name":"@acl admins","meaning":"x"},` +
			`{"name":"installation","meaning":"Setting up.\n@retention 1d","children":[{"name":"deep","meaning":"too deep"}]},` +
			`{"name":"Installation","meaning":"dup"}]}` + "\n```"
	}}
	rep := runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"care","domain":"Care.","runs":1,"depth":1}`)
	if rep["status"] != "complete" {
		t.Fatalf("generate = %v", rep)
	}
	dropped, _ := rep["dropped"].(map[string]any)
	if dropped["malformed"] == nil || dropped["too_deep"] == nil || dropped["duplicate"] == nil {
		t.Errorf("dropped = %v, want malformed, too_deep and duplicate counted", dropped)
	}
	p, _ := rep["proposal"].(map[string]any)
	if p["filed"] != true || p["concepts"] != float64(1) {
		t.Fatalf("proposal = %v, want one concept filed", p)
	}

	g := newTaxFixture(t, 10, 0)
	bad := &taxFake{reply: func(int, string, taxSpawn) (string, string) { return "completed", `{"concepts":"none"}` }}
	rep = runTaxonomist(t, g, bad, `{"mode":"generate","scheme":"care","domain":"Care.","runs":1}`)
	if rep["status"] != "generate_failed" || rep["proposal"] != nil || len(bad.spawns) != 1 {
		t.Fatalf("bad reply = %v (%d spawns), want generate_failed after the one generate request, nothing filed", rep, len(bad.spawns))
	}
}

func TestKBTaxonomist_DomainsReportsAndFilesNothing(t *testing.T) {
	f := newTaxFixture(t, 8, 4)
	fake := &taxFake{reply: func(int, string, taxSpawn) (string, string) {
		return "completed", `{"taxonomies":[` +
			`{"name":"care","domain":"How widgets are cared for.","projects":["guide","Ghost"],"why":"many guides"},` +
			`{"name":"Topics","domain":"Again.","projects":[],"why":"x"},` +
			`{"name":"","domain":"no name"}]}`
	}}
	rep := runTaxonomist(t, f, fake, `{"mode":"domains"}`)
	if rep["status"] != "complete" || len(fake.spawns) != 1 || fake.kinds()[0] != "domains" {
		t.Fatalf("domains = %v (%v)", rep, fake.kinds())
	}
	if !strings.Contains(fake.spawns[0].prompt, "Guide (a kind of Document)") ||
		!strings.Contains(fake.spawns[0].prompt, "SCHEMES IN FORCE (do not propose these again):\ntopics") {
		t.Errorf("domains request lacks the classes or schemes in force:\n%s", fake.spawns[0].prompt)
	}
	// A taxonomy arranges documents: the entity classes of facts (person,
	// event, ...) are not kinds of document.
	if strings.Contains(fake.spawns[0].prompt, "person") {
		t.Errorf("domains request lists an entity class as a document class:\n%s", fake.spawns[0].prompt)
	}
	ds, _ := rep["domains"].([]any)
	if len(ds) != 2 {
		t.Fatalf("domains = %v, want two kept", ds)
	}
	d0 := ds[0].(map[string]any)
	if fmt.Sprint(d0["projects"]) != "[Guide]" || d0["unknown_classes_dropped"] != float64(1) {
		t.Errorf("domain 0 = %v, want projects [Guide] with the unknown class dropped", d0)
	}
	if ds[1].(map[string]any)["exists"] != true {
		t.Errorf("domain 1 = %v, want exists for a scheme in force", ds[1])
	}
	if got := taxSchemesInForce(t, f); len(got) != 1 {
		t.Errorf("schemes = %v", got)
	}
	if rep["proposal"] != nil {
		t.Error("domains mode filed a proposal")
	}
}

// TestKBTaxonomist_ProjectsLimitTheCorpus: with projects, only documents of
// those classes (and their subclasses) are read.
func TestKBTaxonomist_ProjectsLimitTheCorpus(t *testing.T) {
	f := newTaxFixture(t, 6, 4)
	fake := &taxFake{reply: taxSchemeReply}
	rep := runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"care","domain":"Care.","projects":["Guide"],"runs":1}`)
	if taxNum(rep, "corpus", "used") != 6 || taxNum(rep, "corpus", "skipped", "out_of_projection") != 4 {
		t.Fatalf("corpus = %v, want the 6 guides and 4 notes out of projection", rep["corpus"])
	}
	if strings.Contains(strings.Join(fake.spawns[0].untrusted, ""), "BRIEF-Note") {
		t.Error("a note reached the writer")
	}
	p, _ := rep["proposal"].(map[string]any)
	ch := realDoc(t, f.d, f.op, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, p["chunk_id"]))
	fields, _ := json.Marshal(ch["fields"])
	if !strings.Contains(string(fields), `"projects":["Guide"]`) {
		t.Errorf("header = %s, want projects [Guide]", fields)
	}
	rep = runTaxonomist(t, f, fake, `{"mode":"report","projects":["Ghost"]}`)
	if rep["status"] != "unknown_class" {
		t.Errorf("unknown class = %v, want unknown_class", rep)
	}
}

func TestKBTaxonomist_StopsBeforeSpending(t *testing.T) {
	f := newTaxFixture(t, 45, 0)
	fake := &taxFake{reply: taxSchemeReply}
	// 3 batches x 3 runs + 1 = 10 planned calls, over a budget of 5.
	rep := runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"care","domain":"Care.","batch_size":20,"max_calls":5}`)
	if rep["status"] != "budget_refused" || len(fake.spawns) != 0 {
		t.Errorf("over budget = %v with %d spawns, want budget_refused before any call", rep, len(fake.spawns))
	}
	rep = runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"Topics","domain":"Care."}`)
	if rep["status"] != "scheme_exists" || len(fake.spawns) != 0 {
		t.Errorf("existing scheme = %v, want scheme_exists before any call", rep)
	}
	// The runtime compares names by their tag segment: "Topics/" is "topics".
	rep = runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"Topics/","domain":"Care."}`)
	if rep["status"] != "scheme_exists" || len(fake.spawns) != 0 {
		t.Errorf("existing scheme by segment = %v, want scheme_exists before any call", rep)
	}
	// A whole-scheme proposal already filed, pending or rejected, refuses the
	// name too: an operator has seen it.
	p := realDoc(t, f.d, f.op, `{"op":"propose_entity","scope":"tenant","section":"schemes","name":"Care plan","body":"Care.",`+
		`"header":{"domain":"Care."},"concepts":[{"name":"installation"}]}`)
	for _, status := range []string{"proposed", "rejected"} {
		if status == "rejected" {
			g := realDoc(t, f.d, f.op, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, p["chunk_id"]))
			realDoc(t, f.d, f.op, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":"rejected","revision":%v}`,
				p["chunk_id"], g["revision"]))
		}
		rep = runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"care-plan","domain":"Care."}`)
		if rep["status"] != "scheme_proposed" || rep["proposal_status"] != status || len(fake.spawns) != 0 {
			t.Errorf("%s scheme proposal = %v, want scheme_proposed (%s) before any call", status, rep, status)
		}
	}
	// A concept proposal of the same name under a scheme in force is not a scheme.
	realDoc(t, f.d, f.op, `{"op":"propose_entity","scope":"tenant","section":"schemes","parent":"topics","name":"Upkeep","body":"x"}`)
	rep = runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"Upkeep","domain":"Care.","runs":1,"write":"none"}`)
	if rep["status"] != "complete" {
		t.Errorf("a concept proposal blocked a scheme name: %v", rep)
	}
	fake = &taxFake{reply: taxSchemeReply}
	// A request larger than the writer's window is refused before any call:
	// 3 runs x 500 concepts in the review.
	rep = runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"care","domain":"Care.","batch_size":20,"max_concepts":500}`)
	if rep["status"] != "input_too_large" || len(fake.spawns) != 0 ||
		taxNum(rep, "requests", "largest_chars") <= taxNum(rep, "requests", "limit_chars") {
		t.Errorf("oversized review = %v, want input_too_large before any call", rep)
	}
	down := &taxFake{reply: func(int, string, taxSpawn) (string, string) { return "failed", "" }}
	rep = runTaxonomist(t, f, down, `{"mode":"generate","scheme":"care","domain":"Care.","batch_size":20}`)
	if rep["status"] != "provider_unavailable" || len(down.spawns) != 3 {
		t.Errorf("provider down = %v after %d spawns, want provider_unavailable after 3", rep, len(down.spawns))
	}
}

func TestKBTaxonomist_RefusesBadInput(t *testing.T) {
	f := newTaxFixture(t, 2, 0)
	for _, in := range []string{
		`not json`,
		`{"mode":"build"}`,
		`{"mode":"generate","scheme":"care"}`,
		`{"mode":"generate","domain":"Care."}`,
		`{"mode":"domains","scheme":"care"}`,
		`{"mode":"report","write":"propose"}`,
		`{"mode":"generate","scheme":"care","domain":"Care.","runs":9}`,
		`{"mode":"generate","scheme":"a\nb","domain":"Care."}`,
		`{"scope":"user"}`,
		`{"colour":"red"}`,
	} {
		fake := &taxFake{reply: taxSchemeReply}
		if rep := runTaxonomist(t, f, fake, in); rep["status"] != "refused" || len(fake.spawns) != 0 {
			t.Errorf("%s = %v, want refused", in, rep)
		}
	}
}

// TestKnowledgeBaseBundle_TaxonomistAuthority pins what the taxonomist may do:
// read briefs and the ontology, spawn one tool-less writer, and file through
// propose_entity, with no statement method, the tenant scope on both planes
// and no other write op in its body. The writer can do nothing at all.
func TestKnowledgeBaseBundle_TaxonomistAuthority(t *testing.T) {
	cfg := knowledgeBaseConfig(t)
	a, ok := cfg.Agents["kb/taxonomist"]
	if !ok {
		t.Fatal("kb/taxonomist not registered")
	}
	if a.Provider != "code-js" || a.Model != "" || a.Tier != "" {
		t.Errorf("kb/taxonomist provider/model/tier = %q/%q/%q, want code-js with no model", a.Provider, a.Model, a.Tier)
	}
	got := append([]string(nil), a.Tools...)
	sortStrings(got)
	if strings.Join(got, ",") != "Agent,Document" {
		t.Errorf("kb/taxonomist tools = %v, want exactly [Document Agent]", a.Tools)
	}
	if len(a.StatementMethods) > 0 || len(a.ToolModes) > 0 {
		t.Errorf("kb/taxonomist grants methods=%v modes=%v, want none", a.StatementMethods, a.ToolModes)
	}
	// It ships with exactly the tenant scope on both planes (owner ruling
	// 2026-11-20), unlike kb/annotator, whose grants the operator adds: the
	// briefs are tenant-scoped and its one write is an inert proposal.
	if strings.Join(a.MemoryScopes, ",") != "tenant" || strings.Join(a.SqlScopes, ",") != "tenant" {
		t.Errorf("kb/taxonomist scopes memory=%v sql=%v, want exactly [tenant] for both", a.MemoryScopes, a.SqlScopes)
	}
	if !a.Internal || !a.DisableContext || !containsString(a.Skills, "-*") || strings.TrimSpace(a.SystemPrompt) != "" {
		t.Errorf("kb/taxonomist must be internal, context- and skill-less, with no system prompt: %+v", a)
	}
	if _, err := codejs.Validate(a.Code); err != nil {
		t.Fatalf("the shipped kb/taxonomist body does not parse: %v", err)
	}
	// Five reads (query_chunks and get_chunk find a scheme proposal of the same
	// name before any model call) and the one write, propose_entity.
	allowed := map[string]bool{"describe_ontology": true, "list_annotations": true, "documents_summary": true,
		"query_chunks": true, "get_chunk": true, "propose_entity": true}
	for _, m := range regexp.MustCompile(`\bop: "([a-z_]+)"`).FindAllStringSubmatch(a.Code, -1) {
		if !allowed[m[1]] {
			t.Errorf("kb/taxonomist calls Document op %s, outside its six", m[1])
		}
	}
	if strings.Contains(a.Code, "Channel") || strings.Contains(a.Code, "Memory(") {
		t.Error("kb/taxonomist reaches a tool it does not hold")
	}
	w, ok := cfg.Agents["kb/taxonomy-writer"]
	if !ok {
		t.Fatal("kb/taxonomy-writer not registered")
	}
	if len(w.Tools) != 0 || !containsString(w.Skills, "-*") || !w.DisableContext || !w.Internal || w.Tier != "middle" || w.Model != "" {
		t.Errorf("kb/taxonomy-writer must be tool-less, skill-less, context-less, internal, tier middle: %+v", w)
	}
	if len(w.MemoryScopes) > 0 || len(w.SqlScopes) > 0 || len(w.StatementMethods) > 0 {
		t.Errorf("kb/taxonomy-writer holds grants: memory=%v sql=%v methods=%v", w.MemoryScopes, w.SqlScopes, w.StatementMethods)
	}
	if w.Effort != "low" || w.MaxTokens < 32768 {
		t.Errorf("kb/taxonomy-writer effort/max_tokens = %q/%d, want low and at least 32768 (a thinking trace cut the JSON at 16k)", w.Effort, w.MaxTokens)
	}
	// The taxonomist sizes its requests against the writer's window: the two
	// must not drift apart.
	for _, want := range []string{fmt.Sprintf("window_tokens: %d,", w.MaxContextTokens), fmt.Sprintf("output_tokens: %d,", w.MaxTokens)} {
		if !strings.Contains(a.Code, want) {
			t.Errorf("kb/taxonomist's CONFIG lacks %q, the writer's own setting", want)
		}
	}
	for _, kind := range []string{"REQUEST: domains", "REQUEST: generate", "REQUEST: update", "REQUEST: review"} {
		if !strings.Contains(w.SystemPrompt, kind) {
			t.Errorf("the writer's prompt does not describe %s", kind)
		}
		if !strings.Contains(a.Code, `"`+kind) {
			t.Errorf("the taxonomist never sends %s", kind)
		}
	}
	if strings.Contains(w.SystemPrompt, "RFC") || strings.Contains(w.SystemPrompt, "{{") {
		t.Error("the writer's prompt names a design document or a placeholder")
	}
}

// TestKBTaxonomist_NamesAreCheckedAsTheProposalChecksThem: a name propose_entity
// would refuse (a leading #, no letters or digits for its tag, two names with
// one tag) is dropped from the reply, so the scheme is still filed.
func TestKBTaxonomist_NamesAreCheckedAsTheProposalChecksThem(t *testing.T) {
	f := newTaxFixture(t, 10, 0)
	fake := &taxFake{reply: func(int, string, taxSpawn) (string, string) {
		return "completed", `{"concepts":[` +
			`{"name":"# heading","meaning":"x"},{"name":"/","meaning":"x"},{"name":"-","meaning":"x"},` +
			`{"name":"machine learning","meaning":"x"},{"name":"Machine/Learning","meaning":"x"},` +
			`{"name":"machine-learning","meaning":"x"},{"name":"robotics","meaning":"x"}]}`
	}}
	rep := runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"care","domain":"Care.","runs":1}`)
	p, _ := rep["proposal"].(map[string]any)
	if rep["status"] != "complete" || p["filed"] != true || p["concepts"] != float64(2) {
		t.Fatalf("generate = %v, want the scheme filed with machine learning and robotics", rep)
	}
	dropped, _ := rep["dropped"].(map[string]any)
	// Each request drops the same names: the generate reply and the review's.
	if dropped["malformed"] != float64(6) || dropped["duplicate"] != float64(4) {
		t.Errorf("dropped = %v, want 3 malformed and 2 duplicates per reply", dropped)
	}
	for _, in := range []string{`"#care"`, `"/"`} {
		if rep := runTaxonomist(t, f, fake, `{"mode":"generate","scheme":`+in+`,"domain":"Care."}`); rep["status"] != "refused" {
			t.Errorf("scheme %s = %v, want refused", in, rep)
		}
	}
}

// TestKBTaxonomist_RunsAreClampedToTheBatches: a run beyond the number of
// batches would meet the same briefs in the same order, so it is not made; a
// corpus of one batch gets one generation and the review.
func TestKBTaxonomist_RunsAreClampedToTheBatches(t *testing.T) {
	f := newTaxFixture(t, 10, 0)
	fake := &taxFake{reply: taxSchemeReply}
	rep := runTaxonomist(t, f, fake, `{"mode":"generate","scheme":"care","domain":"Care.","runs":3,"write":"none"}`)
	if got := strings.Join(fake.kinds(), ","); got != "generate,review" {
		t.Fatalf("requests = %s, want one generation and the review", got)
	}
	if taxNum(rep, "runs_used") != 1 || rep["runs_clamped"] != true || taxNum(rep, "calls", "planned_generate") != 2 {
		t.Errorf("report = %v, want runs_used 1, runs_clamped and 2 planned calls", rep)
	}
	g := newTaxFixture(t, 45, 0)
	fake = &taxFake{reply: taxSchemeReply}
	rep = runTaxonomist(t, g, fake, `{"mode":"generate","scheme":"care","domain":"Care.","batch_size":20,"runs":5,"write":"none"}`)
	if taxNum(rep, "runs_used") != 3 || len(fake.spawns) != 10 {
		t.Errorf("5 runs over 3 batches = %v with %d requests, want 3 runs and 10 requests", rep["runs_used"], len(fake.spawns))
	}
}

// taxCountingDoc counts the Document ops a run makes.
type taxCountingDoc struct {
	tools.Tool
	mu  sync.Mutex
	ops map[string]int
}

func (c *taxCountingDoc) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	c.mu.Lock()
	c.ops[fmt.Sprint(in["op"])]++
	c.mu.Unlock()
	return c.Tool.Execute(ctx, raw)
}

// TestKBTaxonomist_PagesSamplesAndEstimates runs the body with small pages, so
// the corpus read pages, stops at its scan cap and is sampled, and checks the
// report's estimate to the character.
func TestKBTaxonomist_PagesSamplesAndEstimates(t *testing.T) {
	f := newTaxFixture(t, 45, 0)
	code := knowledgeBaseConfig(t).Agents["kb/taxonomist"].Code
	for _, r := range [][2]string{{"page: 500,", "page: 10,"}, {"scan_max: 20000,", "scan_max: 40,"}, {"summary_ids: 500,", "summary_ids: 7,"}} {
		if !strings.Contains(code, r[0]) {
			t.Fatalf("the body has no %q to shrink", r[0])
		}
		code = strings.Replace(code, r[0], r[1], 1)
	}
	doc := &taxCountingDoc{Tool: f.d, ops: map[string]int{}}
	rep := runTaxonomistWith(t, f, doc, code, &taxFake{reply: taxSchemeReply}, `{"mode":"report","max_briefs":25,"batch_size":20}`)
	if rep["status"] != "complete" || doc.ops["list_annotations"] != 4 || doc.ops["documents_summary"] != 6 {
		t.Fatalf("report = %v, ops %v; want 4 pages of 10 and 6 summary calls of 7 ids", rep, doc.ops)
	}
	c, _ := rep["corpus"].(map[string]any)
	if c["scan_truncated"] != true || c["documents"] != float64(40) || c["sampled"] != true || c["used"] != float64(25) {
		t.Fatalf("corpus = %v, want 40 scanned (truncated), sampled to 25", c)
	}
	// Every line has the same length in UTF-16 units (JS counts those; the
	// dash is one), so the estimate is exact: 25 briefs in batches of 12 and
	// 13, runs clamped to the 2 batches, every update and the review counted at
	// max_concepts concepts of 400 characters, and 4200 + 1500 characters
	// around each of the 5 requests.
	line := len([]rune("- [Guide] Guide 00 — BRIEF-Guide-00 about widget care."))
	briefs := (12*line + 11) + (13*line + 12)
	scheme := 150 * 400
	want := briefs*2 + scheme*2*(2-1) + scheme*2 + (4200+1500)*5
	if rep["batches"] != float64(2) || taxNum(rep, "calls", "planned_generate") != 5 || taxNum(rep, "calls", "estimated_chars") != float64(want) {
		t.Errorf("plan = batches %v calls %v, want 2 batches, 5 calls, %d estimated characters", rep["batches"], rep["calls"], want)
	}
}
