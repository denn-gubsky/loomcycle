package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// kb/ontologist (RFC GQ P6.5), its shipped body in report mode, against the real
// Document tool: describe_ontology, ontology_signals and the proposal listing, under
// the grants, scopes and tool modes the bundle ships.

// ontOntologyMD declares a document class, two entity classes and one relation kind.
const ontOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n\n#### Guide\nA how-to guide.\n\n" +
	"### Person\nA human.\n\n### Team\nA group of people.\n\n" +
	"## Relations\n@section relations\n\n### member_of\n@domain Person\n@range Team\n"

// Marks no report may carry: every title and body of the fixture holds one.
const (
	ontTitleMark = "ONT-TITLE-MARK"
	ontBodyMark  = "ONT-BODY-MARK"
)

type ontFixture struct {
	t     *testing.T
	d     *builtin.Document
	op    context.Context
	edges int64
}

// newOntFixture is an existing state: a confirmed ontology and a corpus written under
// it before the pass, holding one type and one kind a pass would propose and their
// minimal-pair negatives.
func newOntFixture(t *testing.T) *ontFixture {
	t.Helper()
	d, author, _, op := annRealDocumentOp(t)
	withRealOntology(t, d, author, ontOntologyMD)
	f := &ontFixture{t: t, d: d, op: op}

	g1, g2, g3 := f.doc("Guide"), f.doc("Guide"), f.doc("Guide")
	// Proposed: publication on 4 chunks in 2 documents.
	f.parts(g1, "publication", 2)
	f.parts(g2, "publication", 2)
	// Below the floor: memo on 3 chunks of ONE document.
	f.parts(g3, "memo", 3)
	// Rejected earlier: draftnote, otherwise as publication.
	f.parts(g1, "draftnote", 2)
	f.parts(g2, "draftnote", 2)
	f.proposeThenRule("classes", "draftnote", "rejected")
	// An open proposal already: pending, otherwise as publication.
	f.parts(g1, "pending", 2)
	f.parts(g2, "pending", 2)
	f.proposeThenRule("classes", "pending", "")
	// A structural type is the primitive's, never a candidate.
	f.parts(g3, "section", 5)

	people := f.parts(g1, "Person", 2)
	teams := f.parts(g2, "Team", 2)
	contacts := f.parts(g3, "contact", 1)
	// Proposed, with a domain and a range: supervises, 4 proposed statements, all
	// Person to Team.
	for i := 0; i < 4; i++ {
		f.edge(people[i%2], teams[i%2], "supervises", "proposed")
	}
	// Below the floor: approves, 4 statements all rejected.
	for i := 0; i < 4; i++ {
		f.edge(people[i%2], teams[i%2], "approves", "rejected")
	}
	// Kept, no dominant pair: advises, 6 statements split 3 and 3 over two pairs.
	for i := 0; i < 3; i++ {
		f.edge(people[0], teams[i%2], "advises", "proposed")
		f.edge(teams[i%2], people[1], "advises", "proposed")
	}
	// Kept, ends not declared: knows, 5 statements Person to contact.
	for i := 0; i < 5; i++ {
		f.edge(people[i%2], contacts[0], "knows", "asserted")
	}
	return f
}

func (f *ontFixture) doc(typ string) string {
	f.t.Helper()
	return realDoc(f.t, f.d, f.op, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"type":%q}`,
		ontTitleMark+" "+typ, typ))["document_id"].(string)
}

func (f *ontFixture) parts(docID, typ string, n int) []string {
	f.t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, realDoc(f.t, f.d, f.op, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":%q,"type":%q,"body":%q}`,
			docID, ontTitleMark+" part", typ, ontBodyMark))["id"].(string))
	}
	return ids
}

// edge stores a statement row as a bulk import or a consolidator left it: an authored
// statement of an undeclared kind is refused, so the row is written as it is stored.
func (f *ontFixture) edge(from, to, kind, status string) {
	f.t.Helper()
	f.edges++
	key := sqlmem.ScopeKey{Tenant: "tnt", Scope: "tenant", ScopeID: "tnt"}
	sql := f.d.SqlMem.DocPlane()
	if _, err := sql.Exec(context.Background(), key, sql.Rebind(
		`INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, status, method, valid_at) VALUES (?, ?, ?, ?, ?, ?, 'manual', ?)`),
		[]any{fmt.Sprintf("edge-%s-%d", kind, f.edges), from, to, kind, time.Now().UnixNano(), status, f.edges}, 0); err != nil {
		f.t.Fatalf("insert %s statement: %v", kind, err)
	}
}

// proposeThenRule files an add proposal as the operator and, when status is set,
// rules it.
func (f *ontFixture) proposeThenRule(section, name, status string) {
	f.t.Helper()
	id := realDoc(f.t, f.d, f.op, fmt.Sprintf(`{"op":"propose_entity","section":%q,"name":%q}`, section, name))["chunk_id"].(string)
	if status == "" {
		return
	}
	c := realDoc(f.t, f.d, f.op, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, id))
	realDoc(f.t, f.d, f.op, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":%q,"revision":%v}`, id, status, c["revision"]))
}

// openProposals counts the ontology document's open proposals.
func (f *ontFixture) openProposals() int {
	f.t.Helper()
	o := realDoc(f.t, f.d, f.op, `{"op":"describe_ontology"}`)
	cs := realDoc(f.t, f.d, f.op, fmt.Sprintf(`{"op":"query_chunks","scope":"tenant","document_id":%q,"status":"proposed","limit":1000}`,
		o["document_id"]))["chunks"].([]any)
	return len(cs)
}

// ontPolicy is the run policy the bundle's kb/ontologist definition gives a run.
func ontPolicy(a config.AgentDef) tools.RunPolicy {
	return tools.RunPolicy{
		Tools:            heldTools(a),
		StatementMethods: a.StatementMethods,
		ToolModes:        a.ToolModes,
		Memory:           tools.MemoryPolicyValue{AllowedScopes: a.MemoryScopes},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: a.SqlScopes},
	}
}

func ontRunCtx(p tools.RunPolicy) context.Context {
	base := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	return tools.WithRunID(tools.WithAgentName(tools.WithRunPolicy(base, p), "kb/ontologist"), "run-ontologist")
}

// ontCounting wraps the Document tool and counts its calls.
type ontCounting struct {
	tools.Tool
	n atomic.Int64
}

// CallIsRead keeps the wrapped tool modal, so the dispatcher still classifies its calls.
func (c *ontCounting) CallIsRead(input json.RawMessage) (string, bool, error) {
	return c.Tool.(tools.ModalTool).CallIsRead(input)
}

func (c *ontCounting) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	c.n.Add(1)
	return c.Tool.Execute(ctx, raw)
}

// ontNoModel is an Agent tool that must never be called: report mode spends no model.
type ontNoModel struct{ calls atomic.Int64 }

func (a *ontNoModel) Name() string                 { return "Agent" }
func (a *ontNoModel) Description() string          { return "agent (test double)" }
func (a *ontNoModel) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (a *ontNoModel) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	a.calls.Add(1)
	return tools.Result{IsError: true, Text: "no model call is expected"}, nil
}

type ontRun struct {
	report    map[string]any
	text      string
	docCalls  int64
	modelCall int64
}

// runOntologist runs the shipped kb/ontologist body as a run holding p, with the
// shipped tool modes enforced by the dispatcher.
func runOntologist(t *testing.T, f *ontFixture, p tools.RunPolicy, input string) ontRun {
	t.Helper()
	return runOntologistCode(t, f, knowledgeBaseConfig(t).Agents["kb/ontologist"].Code, p, input)
}

func runOntologistCode(t *testing.T, f *ontFixture, code string, p tools.RunPolicy, input string) ontRun {
	t.Helper()
	doc := &ontCounting{Tool: f.d}
	model := &ontNoModel{}
	set := []tools.Tool{doc, model}
	disp := tools.NewDispatcher(set)
	disp.SetToolModes(p.ToolModes)
	res, err := loop.Run(ontRunCtx(p), loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  "kb/ontologist",
		CodeBody:   code,
		Tools:      set,
		Dispatcher: disp,
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
	return ontRun{report: rep, text: res.FinalText, docCalls: doc.n.Load(), modelCall: model.calls.Load()}
}

func ontShippedPolicy(t *testing.T) tools.RunPolicy {
	t.Helper()
	return ontPolicy(knowledgeBaseConfig(t).Agents["kb/ontologist"])
}

// ontCandidate finds a candidate of the report by name.
func ontCandidate(rep map[string]any, name string) map[string]any {
	cs, _ := rep["candidates"].([]any)
	for _, c := range cs {
		if m, _ := c.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

func ontNames(rep map[string]any) []string {
	var out []string
	cs, _ := rep["candidates"].([]any)
	for _, c := range cs {
		m, _ := c.(map[string]any)
		out = append(out, fmt.Sprint(m["name"]))
	}
	return out
}

// TestKBOntologist_ReportCountsAndCallsNoModel: the report ranks what a propose pass
// would file (the type on 4 chunks of 2 documents, the kinds with 3 or more live
// statements) and drops the minimal pairs that differ in one respect (one document,
// rejected earlier, already proposed, every statement rejected, a structural type; the
// contact type on one row is below the floor too),
// computes a relation's domain and range from its ends, calls no model, writes nothing
// and carries no title or body.
func TestKBOntologist_ReportCountsAndCallsNoModel(t *testing.T) {
	f := newOntFixture(t)
	before := f.openProposals()
	r := runOntologist(t, f, ontShippedPolicy(t), `{"mode":"report"}`)
	rep := r.report
	if rep["status"] != "complete" || rep["model_calls"] != float64(0) || r.modelCall != 0 {
		t.Fatalf("report = %v (model calls %d); want complete with no model call", rep, r.modelCall)
	}
	// By count: advises 6, knows 5, then publication and supervises at 4 (classes
	// before relations).
	if got := strings.Join(ontNames(rep), ","); got != "advises,knows,publication,supervises" {
		t.Errorf("candidates = %s, want advises,knows,publication,supervises", got)
	}
	pub := ontCandidate(rep, "publication")
	if pub == nil || pub["section"] != "classes" || pub["signal"] != "undeclared_type" || pub["rows"] != float64(4) || pub["documents"] != float64(2) {
		t.Errorf("publication = %v, want a class on 4 rows in 2 documents", pub)
	}
	sup := ontCandidate(rep, "supervises")
	if d, _ := sup["directives"].(map[string]any); sup == nil || sup["live"] != float64(4) || d["domain"] != "Person" || d["range"] != "Team" {
		t.Errorf("supervises = %v, want 4 live statements with domain Person and range Team", sup)
	}
	for name, note := range map[string]string{"advises": "no_dominant_pair", "knows": "ends_not_declared"} {
		c := ontCandidate(rep, name)
		if d, _ := c["directives"].(map[string]any); c == nil || len(d) != 0 || c["directives_note"] != note {
			t.Errorf("%s = %v, want no directives (%s)", name, c, note)
		}
	}
	dropped, _ := rep["dropped"].(map[string]any)
	want := map[string]float64{"proposed": 1, "rejected": 1, "below_floor": 3, "bad_name": 0, "cap": 0}
	for k, v := range want {
		if dropped[k] != v {
			t.Errorf("dropped = %v, want %v", dropped, want)
			break
		}
	}
	if rep["considered"] != float64(9) || rep["next_pass"] != "propose" {
		t.Errorf("considered %v next_pass %v, want 9 and propose", rep["considered"], rep["next_pass"])
	}
	if strings.Contains(r.text, ontTitleMark) || strings.Contains(r.text, ontBodyMark) {
		t.Error("the report carries a title or a body")
	}
	if after := f.openProposals(); after != before {
		t.Errorf("open proposals %d -> %d: report mode filed something", before, after)
	}
}

// TestKBOntologist_ManyRowsOfOneDocumentPassTheFloor: a type on 20 rows of one
// document is a candidate (the posts of one launch plan); the same type on 19 rows
// of one document stays below the floor.
func TestKBOntologist_ManyRowsOfOneDocumentPassTheFloor(t *testing.T) {
	f := newOntFixture(t)
	f.parts(f.doc("Guide"), "ledger", 20)
	f.parts(f.doc("Guide"), "roster", 19)
	rep := runOntologist(t, f, ontShippedPolicy(t), `{"mode":"report"}`).report
	led := ontCandidate(rep, "ledger")
	if led == nil || led["rows"] != float64(20) || led["documents"] != float64(1) {
		t.Errorf("ledger = %v, want a candidate on 20 rows of 1 document", led)
	}
	if ros := ontCandidate(rep, "roster"); ros != nil {
		t.Errorf("roster = %v, want it below the floor (19 rows of one document)", ros)
	}
	// memo (3 rows of one document) stays below the floor too.
	if memo := ontCandidate(rep, "memo"); memo != nil {
		t.Errorf("memo = %v, want it below the floor", memo)
	}
	if dropped, _ := rep["dropped"].(map[string]any); dropped["below_floor"] != float64(4) {
		t.Errorf("dropped = %v, want below_floor 4 (memo, contact, approves, roster)", dropped)
	}
}

// TestKBOntologist_EachSectionHasItsOwnCap: eight more relation kinds of 5 statements
// fill the relations' five places, and the class on 4 rows (fewer than any of them)
// is still a candidate. Under one shared cap of ten it lost its place to them.
func TestKBOntologist_EachSectionHasItsOwnCap(t *testing.T) {
	f := newOntFixture(t)
	people := f.parts(f.doc("Guide"), "Person", 2)
	teams := f.parts(f.doc("Guide"), "Team", 2)
	for k := 0; k < 8; k++ {
		for i := 0; i < 5; i++ {
			f.edge(people[i%2], teams[i%2], fmt.Sprintf("relkind%d", k), "proposed")
		}
	}
	rep := runOntologist(t, f, ontShippedPolicy(t), `{"mode":"report"}`).report
	if ontCandidate(rep, "publication") == nil {
		t.Errorf("candidates = %v, want publication kept beside the relation kinds", ontNames(rep))
	}
	rels := 0
	cs, _ := rep["candidates"].([]any)
	for _, c := range cs {
		if m, _ := c.(map[string]any); m["section"] == "relations" {
			rels++
		}
	}
	if rels != 5 {
		t.Errorf("relation candidates = %d, want 5 (%v)", rels, ontNames(rep))
	}
	if ontCandidate(rep, "advises") == nil {
		t.Errorf("candidates = %v, want advises (the largest kind) kept", ontNames(rep))
	}
	// 11 relation kinds pass the floor (advises, knows, supervises, relkind0–7): 6 cut.
	if dropped, _ := rep["dropped"].(map[string]any); dropped["cap"] != float64(6) {
		t.Errorf("dropped = %v, want cap 6", dropped)
	}
}

// TestKBOntologist_ReportSaysABacklogStopsThePass: 30 open proposals filed by
// kb/ontologist make a propose pass stop; the same 30 filed by another agent do not.
func TestKBOntologist_ReportSaysABacklogStopsThePass(t *testing.T) {
	for _, tc := range []struct {
		agent string
		open  float64
		next  string
	}{{"kb/ontologist", 30, "backlog"}, {"kb/someone-else", 0, "propose"}} {
		t.Run(tc.agent, func(t *testing.T) {
			f := newOntFixture(t)
			for i := 0; i < 30; i++ {
				// Ten per run: the runtime's own per-run cap.
				ctx := tools.WithRunID(tools.WithAgentName(ontRunCtx(ontShippedPolicy(t)), tc.agent), fmt.Sprintf("run-%d", i/10))
				// Filed as that agent's run would file them, outside report mode's read mode.
				ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.ToolModes = nil })
				realDoc(t, f.d, ctx, fmt.Sprintf(`{"op":"propose_entity","scope":"tenant","name":"Kind%02d","signal":"undeclared_type"}`, i))
			}
			rep := runOntologist(t, f, ontShippedPolicy(t), `{}`).report
			b, _ := rep["backlog"].(map[string]any)
			if rep["status"] != "complete" || b["open"] != tc.open || b["check"] != "complete" || rep["next_pass"] != tc.next {
				t.Errorf("report = %v, want backlog open %v and next_pass %s", rep, tc.open, tc.next)
			}
		})
	}
}

// TestKBOntologist_UngrantedRunReportsUnreadable: without the ontology grant the
// runtime refuses the signals, and the pass says so rather than reporting nothing.
func TestKBOntologist_UngrantedRunReportsUnreadable(t *testing.T) {
	f := newOntFixture(t)
	p := ontShippedPolicy(t)
	p.StatementMethods = nil
	rep := runOntologist(t, f, p, `{"mode":"report"}`).report
	if rep["status"] != "unreadable" || !strings.Contains(fmt.Sprint(rep["failure"]), "permission") || rep["candidates"] != nil {
		t.Errorf("report = %v, want unreadable with the permission refusal and no candidates", rep)
	}
}

// TestKBOntologist_RefusesBadInput: a bad input is refused before any call, and the
// modes that follow the live measurement say they are not in this build rather than
// being read as a bad value.
func TestKBOntologist_RefusesBadInput(t *testing.T) {
	f := newOntFixture(t)
	for in, reason := range map[string]string{
		``:                     "empty input",
		`not json`:             "not JSON",
		`[]`:                   "JSON object",
		`{"mode":"propose"}`:   "mode propose is not in this build",
		`{"mode":"bootstrap"}`: "mode bootstrap is not in this build",
		`{"mode":"build"}`:     "mode must be report",
		`{"scope":"user"}`:     "scope must be tenant",
		`{"colour":"red"}`:     "unknown field",
	} {
		r := runOntologist(t, f, ontShippedPolicy(t), in)
		if r.report["status"] != "refused" || r.docCalls != 0 || r.modelCall != 0 || !strings.Contains(fmt.Sprint(r.report["reason"]), reason) {
			t.Errorf("%q = %v (%d tool calls), want refused before any call: %s", in, r.report, r.docCalls, reason)
		}
	}
}

// TestKnowledgeBaseBundle_OntologistAuthority pins what kb/ontologist may do in this
// build: read the ontology, its signals and its proposals through Document held read
// only, under the ontology grant and the tenant scope, with no model and no write op
// named in its body. kb/ontology-writer can do nothing at all.
func TestKnowledgeBaseBundle_OntologistAuthority(t *testing.T) {
	cfg := knowledgeBaseConfig(t)
	a, ok := cfg.Agents["kb/ontologist"]
	if !ok {
		t.Fatal("kb/ontologist not registered")
	}
	if a.Provider != "code-js" || a.Model != "" || a.Tier != "" {
		t.Errorf("kb/ontologist provider/model/tier = %q/%q/%q, want code-js with no model", a.Provider, a.Model, a.Tier)
	}
	if got := strings.Join(heldTools(a), ","); got != "Document" {
		t.Errorf("kb/ontologist tools = %v, want exactly [Document]", a.Tools)
	}
	if len(a.ToolModes) != 1 || a.ToolModes["Document"] != "read" {
		t.Errorf("kb/ontologist tool_modes = %v, want Document read only", a.ToolModes)
	}
	if strings.Join(a.StatementMethods, ",") != "ontology" {
		t.Errorf("kb/ontologist statement_methods = %v, want exactly [ontology]", a.StatementMethods)
	}
	if strings.Join(a.MemoryScopes, ",") != "tenant" || strings.Join(a.SqlScopes, ",") != "tenant" {
		t.Errorf("kb/ontologist scopes memory=%v sql=%v, want exactly [tenant] for both", a.MemoryScopes, a.SqlScopes)
	}
	if !a.Internal || !a.DisableContext || !containsString(a.Skills, "-*") || strings.TrimSpace(a.SystemPrompt) != "" {
		t.Errorf("kb/ontologist must be internal, context- and skill-less, with no system prompt: %+v", a)
	}
	if _, err := codejs.Validate(a.Code); err != nil {
		t.Fatalf("the shipped kb/ontologist body does not parse: %v", err)
	}
	allowed := map[string]bool{"describe_ontology": true, "ontology_signals": true, "query_chunks": true, "get_chunk": true}
	ops := regexp.MustCompile(`\bop: "([a-z_]+)"`).FindAllStringSubmatch(a.Code, -1)
	if len(ops) == 0 {
		t.Fatal("found no Document op in the kb/ontologist body")
	}
	for _, m := range ops {
		if !allowed[m[1]] {
			t.Errorf("kb/ontologist calls Document op %s, outside its four reads", m[1])
		}
	}
	for _, reach := range []string{"Agent(", "Channel", "Memory(", "Path("} {
		if strings.Contains(a.Code, reach) {
			t.Errorf("kb/ontologist's body reaches %s, which it does not hold", reach)
		}
	}
	w, ok := cfg.Agents["kb/ontology-writer"]
	if !ok {
		t.Fatal("kb/ontology-writer not registered")
	}
	if len(heldTools(w)) != 0 || !containsString(w.Skills, "-*") || !w.DisableContext || !w.Internal || w.Tier != "middle" || w.Model != "" || w.Provider != "" {
		t.Errorf("kb/ontology-writer must be tool-less, skill-less, context-less, internal, tier middle, unpinned: %+v", w)
	}
	if len(w.MemoryScopes) > 0 || len(w.SqlScopes) > 0 || len(w.StatementMethods) > 0 || len(w.ToolModes) > 0 {
		t.Errorf("kb/ontology-writer holds grants: memory=%v sql=%v methods=%v modes=%v", w.MemoryScopes, w.SqlScopes, w.StatementMethods, w.ToolModes)
	}
	for _, kind := range []string{"REQUEST: class", "REQUEST: relation"} {
		if !strings.Contains(w.SystemPrompt, kind) {
			t.Errorf("the writer's prompt does not describe %s", kind)
		}
	}
	if strings.Contains(w.SystemPrompt, "RFC") || strings.Contains(w.SystemPrompt, "{{") {
		t.Error("the writer's prompt names a design document or a placeholder")
	}
}
