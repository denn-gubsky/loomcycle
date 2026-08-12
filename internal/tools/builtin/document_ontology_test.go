package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ontologyDocFixture imports a nested ontology document and returns its path.
//
// import_md turns heading depth into chunk depth, which is exactly the authoring path
// an operator uses, so the tree under test is built the way a real one is rather than
// assembled row by row.
func ontologyDocFixture(t *testing.T, d *Document, ctx context.Context, md string) string {
	t.Helper()
	mj, _ := json.Marshal(md)
	out, r := docExec(t, d, ctx, `{"op":"import_md","scope":"user","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	id, _ := out["document_id"].(string)
	if id == "" {
		t.Fatalf("import_md returned no document_id: %v", out)
	}
	path := "/test/ontology-" + id
	pj, _ := json.Marshal(path)
	_, r = docExec(t, d, ctx,
		`{"op":"set_path","scope":"user","id":"`+id+`","path":`+string(pj)+`}`)
	if r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	return path
}

// termsByName indexes a term slice for assertion.
func termsByName(terms []ontologyTermForTest) map[string]ontologyTermForTest {
	m := map[string]ontologyTermForTest{}
	for _, tm := range terms {
		m[tm.Name] = tm
	}
	return m
}

// ontologyTermForTest mirrors the fields under assertion, keeping the test readable
// without importing the memory package's full term.
type ontologyTermForTest struct {
	Name   string
	Parent string
	Fields []string
}

func readTerms(t *testing.T, d *Document, ctx context.Context, path string) []ontologyTermForTest {
	t.Helper()
	got, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatalf("OntologyTermsFromTree: %v", err)
	}
	out := make([]ontologyTermForTest, 0, len(got.Terms))
	for _, g := range got.Terms {
		out = append(out, ontologyTermForTest{Name: g.Name, Parent: g.Parent, Fields: g.Fields})
	}
	return out
}

// TestOntologyTree_NestedChunkIsASubclass is the whole point of reading chunks.
//
// This FAILS on the pre-change reader, and not by a near miss: `ParseOntologyMarkdown`
// matched only "## ", so `incident` was recognised as neither a term nor a title and
// vanished with no error and nothing in the panel.
func TestOntologyTree_NestedChunkIsASubclass(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, `# Tenant Ontology

## event
- `+"`occurred_at`"+` — when it happened

### incident
- `+"`severity`"+` — how bad

## project
- `+"`status`"+`
`)

	byName := termsByName(readTerms(t, d, ctx, path))

	inc, ok := byName["incident"]
	if !ok {
		t.Fatalf("the nested type was dropped — have %v", byName)
	}
	if inc.Parent != "event" {
		t.Errorf("incident.Parent = %q, want %q", inc.Parent, "event")
	}
	if got := strings.Join(inc.Fields, ","); got != "severity" {
		t.Errorf("incident fields = %q, want severity", got)
	}
	// A root stays a root, and the DOCUMENT TITLE is not an entity: including it
	// would invent a type nobody declared and make everything its subclass.
	if ev := byName["event"]; ev.Parent != "" {
		t.Errorf("event.Parent = %q, want root", ev.Parent)
	}
	if _, bad := byName["Tenant Ontology"]; bad {
		t.Error("the document's root chunk was read as an entity")
	}
	if len(byName) != 3 {
		t.Errorf("want exactly event/incident/project, got %v", byName)
	}
}

// TestOntologyTree_HeadingInsideABodyIsAComment is the distinction that justifies
// reading the data model instead of the Markdown.
//
// Under a heading rule an operator documenting their type accidentally declares one.
// Under the chunk rule a heading inside a body is prose, and only a genuine child
// CHUNK is a subclass — a difference that is invisible in the exported Markdown and
// unambiguous in the tree.
func TestOntologyTree_HeadingInsideABodyIsAComment(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	// One chunk, whose body happens to contain a heading-looking line. Built with
	// create_chunk rather than import_md precisely because import_md would promote
	// that line to a chunk — the point is a body that carries one.
	out, r := docExec(t, d, ctx,
		`{"op":"create_document","scope":"user","title":"Tenant Ontology","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	body := "- `status`\n\n### Notes on naming\nPrefer lowercase.\n"
	bj, _ := json.Marshal(body)
	_, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"project","body":`+string(bj)+`}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	path := "/test/ontology-comment"
	_, r = docExec(t, d, ctx,
		`{"op":"set_path","scope":"user","id":"`+docID+`","path":"`+path+`"}`)
	if r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}

	byName := termsByName(readTerms(t, d, ctx, path))
	if _, bad := byName["Notes on naming"]; bad {
		t.Error("a heading inside a body was read as an entity — that is documentation")
	}
	p, ok := byName["project"]
	if !ok {
		t.Fatalf("project missing — have %v", byName)
	}
	if got := strings.Join(p.Fields, ","); got != "status" {
		t.Errorf("project fields = %q, want status", got)
	}
}

// TestOntologyTree_DepthBeyondTheCapIsFlattenedNotDropped.
//
// The cap bounds prompt growth, but a cap that DELETES an entity would reintroduce
// the exact silent loss this reader was written to end — one level further down,
// where it would be harder to notice.
func TestOntologyTree_DepthBeyondTheCapIsFlattenedNotDropped(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, `# Tenant Ontology

## l1

### l2

#### l3

##### l4

###### l5
`)

	byName := termsByName(readTerms(t, d, ctx, path))
	for _, want := range []string{"l1", "l2", "l3", "l4", "l5"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("%s was dropped by the depth cap — have %v", want, byName)
		}
	}
	// The chain is bounded at four levels: l4 is the deepest allowed, so l5 lands
	// BESIDE it rather than below it. The assertion is on the parent because that is
	// what the cap actually changes — a cap that only stalled a counter would leave
	// the reported tree unbounded while claiming otherwise.
	if got := byName["l4"].Parent; got != "l3" {
		t.Errorf("l4.Parent = %q, want l3", got)
	}
	if got := byName["l5"].Parent; got != "l3" {
		t.Errorf("l5.Parent = %q, want l3 (flattened to the cap, a sibling of l4)", got)
	}
}

// TestOntologyTree_AbsentDocumentIsNotAnError: no tenant layer, not a failure. The
// caller renders the base seed, and a 500 here would take down every run whose prompt
// mentions the ontology.
func TestOntologyTree_AbsentDocumentIsNotAnError(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	read, err := d.OntologyTermsFromTree(ctx, "user", "/test/nope")
	if err != nil {
		t.Fatalf("absent document should read as empty, got %v", err)
	}
	if len(read.Terms) != 0 {
		t.Errorf("want no terms, got %v", read.Terms)
	}
}

// ontologyRetrievalFixture builds a tenant ontology with `event` → `incident` →
// `outage`, plus user-scope chunks of each type, and returns the tool + a context
// holding the tenant grants the SETUP needs.
//
// The grants are for the setup only. The expansion path deliberately does not require
// them, and TestSubtypeExpansion_WorksWithoutTenantGrants covers that.
func ontologyRetrievalFixture(t *testing.T, confirm bool) (*Document, context.Context) {
	t.Helper()
	d, ctx, _ := documentFixture(t)
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	ctx = tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})
	// SETUP ACTS AS THE OPERATOR. Building the fixture means writing the ontology
	// document and flipping its confirm status, which RFC CA's guard reserves for the
	// operator surfaces — an agent context is refused, correctly. That refusal is what
	// TestOntologyGuard_* asserts; here it would only be testing the fixture.
	ctx = tools.WithSubstrateOperator(ctx)

	md := "# Tenant Ontology\n\n## event\n- `occurred_at`\n\n### incident\n- `severity`\n\n#### outage\n- `minutes_down`\n\n## person\n- `name`\n"
	mj, _ := json.Marshal(md)
	out, r := docExec(t, d, ctx, `{"op":"import_md","scope":"tenant","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	pj, _ := json.Marshal(memrank.OntologyPath)
	if _, r = docExec(t, d, ctx,
		`{"op":"set_path","scope":"tenant","id":"`+docID+`","path":`+string(pj)+`}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	status := "draft"
	if confirm {
		status = memrank.OntologyConfirmedStatus
	}
	root, _ := out["root_chunk_id"].(string)
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+root+`"}`)
	rev := int(g["revision"].(float64))
	if _, r = docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"update_chunk","scope":"tenant","id":"%s","status":"%s","revision":%d}`,
		root, status, rev)); r.IsError {
		t.Fatalf("update_chunk status: %s", r.Text)
	}

	// The DATA lives in the user scope — the point is that a user-scope query is
	// widened by a tenant-scope taxonomy.
	dout, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Log","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	logID, _ := dout["document_id"].(string)
	logRoot, _ := dout["root_chunk_id"].(string)
	for _, c := range []struct{ title, typ string }{
		{"the outage last friday", "outage"},
		{"the deploy incident", "incident"},
		{"the all-hands", "event"},
		{"ada", "person"},
	} {
		if _, r = docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_chunk","scope":"user","document_id":"%s","parent_id":"%s","title":"%s","type":"%s","body":"x"}`,
			logID, logRoot, c.title, c.typ)); r.IsError {
			t.Fatalf("create_chunk %s: %s", c.typ, r.Text)
		}
	}
	return d, ctx
}

func queryChunkTypes(t *testing.T, d *Document, ctx context.Context, typ string) (map[string]bool, []string) {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","type":"`+typ+`"}`)
	if r.IsError {
		t.Fatalf("query_chunks: %s", r.Text)
	}
	got := map[string]bool{}
	rows, _ := out["chunks"].([]any)
	for _, row := range rows {
		m, _ := row.(map[string]any)
		if s, _ := m["type"].(string); s != "" {
			got[s] = true
		}
	}
	// Tolerate the key being ABSENT rather than asserting on it: when the expansion
	// is not working, absent is exactly what it will be, and a panic here would bury
	// the real failure under a type-assertion trace.
	var expanded []string
	if raw, ok := out["type_expanded_to"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				expanded = append(expanded, s)
			}
		}
	}
	return got, expanded
}

// TestSubtypeExpansion_QueryChunksMatchesSubclasses is the payoff.
//
// Without it the hierarchy is decorative — an operator gains a tidy document and no
// new answers, because a search for `event` still misses every `incident` they
// classified. FAILS before this change: only the exact type matched.
func TestSubtypeExpansion_QueryChunksMatchesSubclasses(t *testing.T) {
	d, ctx := ontologyRetrievalFixture(t, true)

	got, expanded := queryChunkTypes(t, d, ctx, "event")
	for _, want := range []string{"event", "incident", "outage"} {
		if !got[want] {
			t.Errorf("type=event did not match %q — have %v", want, got)
		}
	}
	// TRANSITIVE but not indiscriminate: a sibling root must not be dragged in.
	if got["person"] {
		t.Error("type=event matched an unrelated root type")
	}
	if len(expanded) != 3 {
		t.Errorf("type_expanded_to = %v, want the three-type chain", expanded)
	}
	if len(expanded) > 0 && expanded[0] != "event" {
		t.Errorf("the requested type must stay first, got %v", expanded)
	}

	// Asking for the LEAF stays narrow. Expansion goes down the tree, never up:
	// `outage` must not start matching every `event`.
	leaf, _ := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","type":"outage"}`)
	rows, _ := leaf["chunks"].([]any)
	if len(rows) != 1 {
		t.Errorf("type=outage should match only itself, got %d rows", len(rows))
	}
	if _, reported := leaf["type_expanded_to"]; reported {
		t.Error("a leaf type must not report an expansion")
	}
}

// TestSubtypeExpansion_DraftOntologyDoesNotWidenRetrieval.
//
// A draft ontology is inert on every other surface by design. Retrieval must not be
// the one place an unconfirmed edit quietly changes answers — an operator drafting a
// taxonomy would otherwise see their queries shift before they ever confirmed it.
func TestSubtypeExpansion_DraftOntologyDoesNotWidenRetrieval(t *testing.T) {
	d, ctx := ontologyRetrievalFixture(t, false)

	out, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","type":"event"}`)
	if r.IsError {
		t.Fatalf("query_chunks: %s", r.Text)
	}
	rows, _ := out["chunks"].([]any)
	if len(rows) != 1 {
		t.Errorf("a draft ontology widened retrieval: got %d rows, want only the exact type", len(rows))
	}
	if _, reported := out["type_expanded_to"]; reported {
		t.Error("a draft ontology reported an expansion")
	}
}

// TestSubtypeExpansion_ListFactsMatchesSubclasses — the second call site, asserted
// separately because one wired site tells you nothing about the other.
func TestSubtypeExpansion_ListFactsMatchesSubclasses(t *testing.T) {
	d, ctx := ontologyRetrievalFixture(t, true)
	// list_facts reads the entity tier, so the rows need entity metadata — upsert
	// with a natural_key is what puts them there.
	fout, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	fdoc, _ := fout["document_id"].(string)
	froot, _ := fout["root_chunk_id"].(string)
	for _, f := range []struct{ key, typ string }{
		{"incident:deploy", "incident"},
		{"event:all-hands", "event"},
		{"person:ada", "person"},
	} {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"upsert_chunk","scope":"user","document_id":"%s","parent_id":"%s",`+
				`"natural_key":"%s","title":"%s","type":"%s","body":"x"}`,
			fdoc, froot, f.key, f.key, f.typ)); r.IsError {
			t.Fatalf("upsert_chunk %s: %s", f.key, r.Text)
		}
	}

	out, r2 := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","type":"event"}`)
	if r2.IsError {
		t.Fatalf("list_facts: %s", r2.Text)
	}
	types := map[string]bool{}
	facts, _ := out["facts"].([]any)
	for _, f := range facts {
		m, _ := f.(map[string]any)
		if s, _ := m["type"].(string); s != "" {
			types[s] = true
		}
	}
	if !types["event"] || !types["incident"] {
		t.Errorf("list_facts type=event missed a subclass — have %v", types)
	}
	if types["person"] {
		t.Error("list_facts type=event matched an unrelated root")
	}
	if _, reported := out["type_expanded_to"]; !reported {
		t.Error("list_facts did not report the expansion")
	}
}

// TestSubtypeExpansion_WorksWithoutTenantGrants.
//
// Expansion reads the tenant ontology through a key it builds itself, skipping the
// scope=tenant grant check. That is deliberate: requiring the grant would mean subtype
// expansion worked only for agents holding tenant-WRITE authority, so the operators
// most careful about scoping would be the ones whose taxonomy silently did nothing.
//
// Safe because the tenant comes from the run's server-stamped identity, the ontology is
// operator config that already reaches the prompt, and nothing tenant-scoped is
// returned — only which of the agent's own rows match.
func TestSubtypeExpansion_WorksWithoutTenantGrants(t *testing.T) {
	d, setupCtx := ontologyRetrievalFixture(t, true)

	// A context with NO tenant grants at all, same identity.
	bare := tools.WithAgentName(context.Background(), "doc-agent")
	bare = tools.WithRunIdentity(bare, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	_ = setupCtx

	// The tenant scope itself must still be refused — the carve-out is for the
	// internal ontology read, not for the agent.
	if _, r := docExec(t, d, bare, `{"op":"query_chunks","scope":"tenant"}`); !r.IsError {
		t.Error("an ungranted agent reached the tenant scope")
	}
	got, expanded := queryChunkTypes(t, d, bare, "event")
	if !got["incident"] || !got["outage"] {
		t.Errorf("expansion required tenant grants — have %v", got)
	}
	if len(expanded) != 3 {
		t.Errorf("type_expanded_to = %v", expanded)
	}
}

// TestSubtypeExpansion_NoOntologyDocumentIsTheCommonCase.
//
// Most deployments have no tenant ontology at all. A type-filtered query must behave
// exactly as it always did: same rows, no error, no expansion reported — and, because
// the expansion reads the TENANT scope, it must not provision that scope's schema as a
// side effect of a read. The subdirectory assertion is the part that would otherwise go
// unnoticed: creating a schema here also creates a per-scope LOGIN role on the Postgres
// tier, which needs CREATEROLE and can fail.
func TestSubtypeExpansion_NoOntologyDocumentIsTheCommonCase(t *testing.T) {
	root := t.TempDir()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: root})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	d := &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Log","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	rootChunk, _ := out["root_chunk_id"].(string)
	if _, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+rootChunk+`","title":"a note","type":"note","body":"x"}`); r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}

	got, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","type":"note"}`)
	if r.IsError {
		t.Fatalf("a type filter must still work with no ontology: %s", r.Text)
	}
	rows, _ := got["chunks"].([]any)
	if len(rows) != 1 {
		t.Errorf("want the one matching chunk, got %d", len(rows))
	}
	if _, reported := got["type_expanded_to"]; reported {
		t.Error("an expansion was reported with no ontology present")
	}
	// The read must not have PROVISIONED the tenant scope. sqlite lays scopes out as
	// <root>/<tenant>/<scope>/<id>.db, so a `tenant` directory appearing here is a
	// read that built a store — the sqlite-tier symptom of the Postgres-tier failure
	// (a per-scope LOGIN role, which needs CREATEROLE and can be refused).
	var provisioned []string
	_ = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err == nil && e.IsDir() && e.Name() == "tenant" {
			provisioned = append(provisioned, path)
		}
		return nil
	})
	if len(provisioned) > 0 {
		t.Errorf("the ontology read provisioned the tenant scope: %v", provisioned)
	}
}

// TestOntologyTree_ReportsThatItFlattenedTheDepth.
//
// The flattening itself was already correct — nothing is dropped — but it was SILENT,
// so an operator whose level-5 type quietly became a level-4 sibling had no way to know
// their document and their ontology disagreed. That is the same class of failure this
// reader was written to end, so the cap has to announce itself.
func TestOntologyTree_ReportsThatItFlattenedTheDepth(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	shallow := ontologyDocFixture(t, d, ctx, "# Tenant Ontology\n\n## a\n\n### b\n\n#### c\n\n##### d\n")
	if read, err := d.OntologyTermsFromTree(ctx, "user", shallow); err != nil {
		t.Fatalf("read: %v", err)
	} else if read.DepthCapped {
		t.Errorf("a document AT the cap must not report flattening (%d terms)", len(read.Terms))
	}

	deep := ontologyDocFixture(t, d, ctx, "# Tenant Ontology\n\n## a\n\n### b\n\n#### c\n\n##### d\n\n###### e\n")
	read, err := d.OntologyTermsFromTree(ctx, "user", deep)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !read.DepthCapped {
		t.Error("a document nesting past the cap must report that it was flattened")
	}
	// And still nothing lost.
	if len(read.Terms) != 5 {
		t.Errorf("want all five types, got %d", len(read.Terms))
	}
}

// TestOntologyTree_ProposedIsNotInForceButIsReported (RFC CA §2.1).
//
// A proposal has to be BOTH invisible to the runtime and visible to the operator. Half of
// that is easy to get and useless: a suggestion nothing reports is a suggestion nobody
// acts on, and a suggestion that steers extraction is not a suggestion.
func TestOntologyTree_ProposedIsNotInForceButIsReported(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx,
		`{"op":"create_document","scope":"user","title":"Tenant Ontology","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	mk := func(title, status, body string) string {
		bj, _ := json.Marshal(body)
		res, rr := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_chunk","scope":"user","document_id":"%s","parent_id":"%s","title":"%s","status":"%s","body":%s}`,
			docID, root, title, status, string(bj)))
		if rr.IsError {
			t.Fatalf("create_chunk %s: %s", title, rr.Text)
		}
		id, _ := res["id"].(string)
		return id
	}
	mk("event", "", "- `occurred_at`\n")
	propID := mk("outage", "proposed", "- `minutes_down`\n\nSeen 12 times as an untyped fact.\n")
	mk("wontfix", "rejected", "")
	// An UNKNOWN status must stay in force — the fail-open rule.
	mk("project", "wip", "- `status`\n")

	path := "/test/ontology-proposals"
	if _, r = docExec(t, d, ctx,
		`{"op":"set_path","scope":"user","id":"`+docID+`","path":"`+path+`"}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	read, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	inForce := map[string]bool{}
	for _, tm := range read.Terms {
		inForce[tm.Name] = true
	}
	if inForce["outage"] || inForce["wontfix"] {
		t.Errorf("an inert entity reached the in-force set: %v", inForce)
	}
	if !inForce["event"] {
		t.Errorf("a live entity went missing: %v", inForce)
	}
	// THE FAIL-OPEN RULE. Only the two reserved words are inert; a status this build has
	// never heard of must not be able to switch a type off.
	if !inForce["project"] {
		t.Error(`a chunk with status "wip" was dropped — only "proposed"/"rejected" are inert`)
	}

	byName := map[string]memrank.OntologyProposal{}
	for _, p := range read.Proposals {
		byName[p.Name] = p
	}
	if len(byName) != 2 {
		t.Fatalf("want outage + wontfix reported, got %v", byName)
	}
	if got := byName["outage"]; got.ChunkID != propID || got.Status != "proposed" {
		t.Errorf("outage proposal = %+v", got)
	}
	if len(byName["outage"].Fields) != 1 || byName["outage"].Fields[0] != "minutes_down" {
		t.Errorf("a proposal must carry its own fields: %v", byName["outage"].Fields)
	}
	// The evidence has to survive to the operator, or they are judging a bare name.
	if !strings.Contains(byName["outage"].Body, "12 times") {
		t.Errorf("the proposal lost its evidence body: %q", byName["outage"].Body)
	}
	if byName["wontfix"].Status != "rejected" {
		t.Errorf("a rejection must be reported as such (it is a tombstone): %+v", byName["wontfix"])
	}
}

// TestOntologyTree_RejectingAParentDoesNotSwitchOffLiveChildren.
//
// Children of an inert chunk re-parent to the nearest IN-FORCE ancestor rather than
// disappearing with it. Turning a live type off as a side effect of rejecting something
// above it is exactly the silent loss this reader was written to end.
func TestOntologyTree_RejectingAParentDoesNotSwitchOffLiveChildren(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx,
		`{"op":"create_document","scope":"user","title":"Tenant Ontology","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	res, _ := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","scope":"user","document_id":"%s","parent_id":"%s","title":"event","status":"rejected","body":""}`,
		docID, root))
	evID, _ := res["id"].(string)
	if _, r = docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","scope":"user","document_id":"%s","parent_id":"%s","title":"incident","body":"- `+"`severity`"+`\n"}`,
		docID, evID)); r.IsError {
		t.Fatalf("create_chunk child: %s", r.Text)
	}
	path := "/test/ontology-reject-parent"
	if _, r = docExec(t, d, ctx,
		`{"op":"set_path","scope":"user","id":"`+docID+`","path":"`+path+`"}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}

	read, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var inc *memrank.OntologyTerm
	for i := range read.Terms {
		if read.Terms[i].Name == "incident" {
			inc = &read.Terms[i]
		}
	}
	if inc == nil {
		t.Fatalf("the live child vanished with its rejected parent: %+v", read.Terms)
	}
	if inc.Parent != "" {
		t.Errorf("incident.Parent = %q — it should attach to the nearest IN-FORCE ancestor (the root)", inc.Parent)
	}
}

// ontologyAgentFixture returns an operator ctx (for setup) and an AGENT ctx (for the
// calls under test) over the same tenant ontology, with tenant grants on both.
//
// The grants are on the agent ctx DELIBERATELY: the point of the guard is that full
// tenant Document authority is still not authority over the ontology's type system.
// Testing it with an under-granted agent would prove nothing.
func ontologyAgentFixture(t *testing.T) (*Document, context.Context, context.Context) {
	t.Helper()
	d, base, _ := documentFixture(t)
	base = tools.WithMemoryPolicy(base, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	base = tools.WithSqlMemPolicy(base, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})
	opCtx := tools.WithSubstrateOperator(base)

	md := "# Tenant Ontology\n\n## event\n- `occurred_at`\n"
	mj, _ := json.Marshal(md)
	out, r := docExec(t, d, opCtx, `{"op":"import_md","scope":"tenant","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	pj, _ := json.Marshal(memrank.OntologyPath)
	if _, r = docExec(t, d, opCtx,
		`{"op":"set_path","scope":"tenant","id":"`+docID+`","path":`+string(pj)+`}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	return d, opCtx, base
}

// TestOntologyGuard_AnAgentMayProposeButNotDecide is the phase-2 claim.
//
// Phase 1's review gate was unenforced underneath: an agent holding tenant Document
// authority could create a live type outright, or clear its own proposal's status and
// call that reviewed. A gate the reviewed thing can open is ceremony.
func TestOntologyGuard_AnAgentMayProposeButNotDecide(t *testing.T) {
	d, opCtx, agentCtx := ontologyAgentFixture(t)

	read, err := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	docID, rootID := read.DocumentID, read.RootChunkID
	eventID := d.chunkIDForEntity(opCtx, mustTenantKey(t, d, opCtx), docID, "event")
	if eventID == "" {
		t.Fatal("no `event` chunk in the fixture")
	}

	// PROPOSE: allowed, and inert.
	out, r := docExec(t, d, agentCtx,
		`{"op":"propose_entity","name":"outage","parent":"event","body":"- `+"`minutes_down`"+`\n\nSeen 9 times.\n"}`)
	if r.IsError {
		t.Fatalf("propose_entity should be allowed to an agent: %s", r.Text)
	}
	proposedID, _ := out["chunk_id"].(string)
	if proposedID == "" {
		t.Fatalf("propose_entity returned no chunk id: %v", out)
	}
	after, _ := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
	for _, term := range after.Terms {
		if term.Name == "outage" {
			t.Fatal("a proposal went into force — it must be inert until an operator accepts")
		}
	}
	if len(after.Proposals) != 1 || after.Proposals[0].Name != "outage" {
		t.Fatalf("the proposal was not recorded: %+v", after.Proposals)
	}
	// It hangs off the named parent, resolved from a name the agent could actually know.
	if after.Proposals[0].Parent != "event" {
		t.Errorf("proposal parent = %q, want event", after.Proposals[0].Parent)
	}

	// DECIDE: refused, in every spelling.
	refusals := []struct {
		name string
		req  string
	}{
		{"clear its own proposal's status", fmt.Sprintf(
			`{"op":"update_chunk","scope":"tenant","id":"%s","status":"","revision":1}`, proposedID)},
		{"confirm the whole document", fmt.Sprintf(
			`{"op":"update_chunk","scope":"tenant","id":"%s","status":"confirmed","revision":1}`, rootID)},
		{"delete an entity", fmt.Sprintf(`{"op":"delete_chunk","scope":"tenant","id":"%s"}`, proposedID)},
		{"create a LIVE entity", fmt.Sprintf(
			`{"op":"create_chunk","scope":"tenant","document_id":"%s","parent_id":"%s","title":"decided","body":""}`,
			docID, rootID)},
		{"move an entity", fmt.Sprintf(
			`{"op":"move_chunk","scope":"tenant","id":"%s","new_parent_id":"%s"}`, proposedID, rootID)},
		// The four routes the first version of this guard missed. Each reaches the same
		// effect by a different op, which is why the refused set was audited from the op
		// list instead of from the obvious mutations.
		{"import live types over the document", fmt.Sprintf(
			`{"op":"import_md","scope":"tenant","document_id":"%s","markdown":"## decided\n"}`, docID)},
		{"import a canvas over the document", fmt.Sprintf(
			`{"op":"import_canvas","scope":"tenant","document_id":"%s","canvas":{"nodes":[],"edges":[]}}`, docID)},
		// A VALID supersede: the proposal replaces the live `event`, retiring it. The
		// first version of this case omitted `id` and failed input validation instead of
		// the guard — an assertion that passes because the call was malformed proves
		// nothing about the gate.
		{"supersede a live entity", fmt.Sprintf(
			`{"op":"supersede_chunk","scope":"tenant","document_id":"%s","id":"%s","supersedes_id":"%s","natural_key":"k"}`,
			docID, proposedID, eventID)},
		{"delete the whole ontology", fmt.Sprintf(
			`{"op":"delete_document","scope":"tenant","id":"%s"}`, docID)},
		{"re-home the ontology path", fmt.Sprintf(
			`{"op":"set_path","scope":"tenant","id":"%s","path":"/elsewhere/onto"}`, docID)},
	}
	for _, c := range refusals {
		if _, rr := docExec(t, d, agentCtx, c.req); !rr.IsError {
			t.Errorf("an agent was allowed to %s", c.name)
		}
	}
	// Nothing leaked through: still one proposal, still no `decided` type.
	final, _ := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
	for _, term := range final.Terms {
		if term.Name == "decided" || term.Name == "outage" {
			t.Errorf("a refused write took effect anyway: %q is in force", term.Name)
		}
	}

	// THE OPERATOR is unaffected — same calls, allowed. Without this the guard could be
	// "refuse everyone", which passes every assertion above and breaks the product.
	rev := 1
	if g, gr := docExec(t, d, opCtx, `{"op":"get_chunk","scope":"tenant","id":"`+proposedID+`"}`); !gr.IsError {
		rev = int(g["revision"].(float64))
	}
	if _, rr := docExec(t, d, opCtx, fmt.Sprintf(
		`{"op":"update_chunk","scope":"tenant","id":"%s","status":"","revision":%d}`, proposedID, rev)); rr.IsError {
		t.Fatalf("the operator must still be able to accept: %s", rr.Text)
	}
	accepted, _ := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
	var found bool
	for _, term := range accepted.Terms {
		if term.Name == "outage" {
			found = true
			if term.Parent != "event" {
				t.Errorf("accepted outage.Parent = %q, want event — accepting must not move it", term.Parent)
			}
		}
	}
	if !found {
		t.Error("the operator's accept did not put the entity in force")
	}
}

// TestOntologyGuard_LeavesOtherDocumentsAlone.
//
// The guard is scoped to ONE document. If it caught every tenant-scope document, an agent
// with tenant Document grants would lose the ability to write ordinary shared documents —
// a much larger change than RFC CA describes, arriving as a side effect.
func TestOntologyGuard_LeavesOtherDocumentsAlone(t *testing.T) {
	d, _, agentCtx := ontologyAgentFixture(t)

	out, r := docExec(t, d, agentCtx,
		`{"op":"create_document","scope":"tenant","title":"Team notes","body":""}`)
	if r.IsError {
		t.Fatalf("an agent must still be able to create a tenant document: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	c, r := docExec(t, d, agentCtx, `{"op":"create_chunk","scope":"tenant","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"a note","body":"text"}`)
	if r.IsError {
		t.Fatalf("an agent must still be able to write a live chunk elsewhere: %s", r.Text)
	}
	id, _ := c["id"].(string)
	g, _ := docExec(t, d, agentCtx, `{"op":"get_chunk","scope":"tenant","id":"`+id+`"}`)
	rev := int(g["revision"].(float64))
	if _, r = docExec(t, d, agentCtx, fmt.Sprintf(
		`{"op":"update_chunk","scope":"tenant","id":"%s","status":"final","revision":%d}`, id, rev)); r.IsError {
		t.Errorf("an agent must still be able to set a status elsewhere: %s", r.Text)
	}
}

// TestProposeEntity_RefusesWhatAnOperatorHasAlreadySeen.
//
// A curator that re-files the same suggestion every run turns the review surface into
// noise, which is the whole reason a rejection is kept rather than deleted.
func TestProposeEntity_RefusesWhatAnOperatorHasAlreadySeen(t *testing.T) {
	d, opCtx, agentCtx := ontologyAgentFixture(t)

	// Already IN FORCE → refused, and pointed at the useful move instead.
	_, r := docExec(t, d, agentCtx, `{"op":"propose_entity","name":"event"}`)
	if !r.IsError || !strings.Contains(r.Text, "already in force") {
		t.Errorf("proposing a live type should be refused: %s", r.Text)
	}
	// Already PROPOSED → refused.
	if _, rr := docExec(t, d, agentCtx, `{"op":"propose_entity","name":"outage","parent":"event"}`); rr.IsError {
		t.Fatalf("first proposal: %s", rr.Text)
	}
	_, r = docExec(t, d, agentCtx, `{"op":"propose_entity","name":"outage","parent":"event"}`)
	if !r.IsError || !strings.Contains(r.Text, "proposed") {
		t.Errorf("a duplicate proposal should be refused: %s", r.Text)
	}
	// REJECTED → still refused, which is what the tombstone is for.
	read, _ := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
	var pid string
	for _, p := range read.Proposals {
		if p.Name == "outage" {
			pid = p.ChunkID
		}
	}
	g, _ := docExec(t, d, opCtx, `{"op":"get_chunk","scope":"tenant","id":"`+pid+`"}`)
	rev := int(g["revision"].(float64))
	if _, rr := docExec(t, d, opCtx, fmt.Sprintf(
		`{"op":"update_chunk","scope":"tenant","id":"%s","status":"rejected","revision":%d}`, pid, rev)); rr.IsError {
		t.Fatalf("operator reject: %s", rr.Text)
	}
	_, r = docExec(t, d, agentCtx, `{"op":"propose_entity","name":"outage","parent":"event"}`)
	if !r.IsError || !strings.Contains(r.Text, "rejected") {
		t.Errorf("a rejected type must not be re-proposable, and the refusal should say so: %s", r.Text)
	}

	// An unknown parent is refused rather than silently filed at the top level, where an
	// operator would see a root type they never asked for.
	_, r = docExec(t, d, agentCtx, `{"op":"propose_entity","name":"thing","parent":"nonesuch"}`)
	if !r.IsError || !strings.Contains(r.Text, "no entity named") {
		t.Errorf("an unknown parent should be refused: %s", r.Text)
	}
	// And the evidence body is bounded.
	big, _ := json.Marshal(strings.Repeat("x", proposalBodyMax+1))
	_, r = docExec(t, d, agentCtx, `{"op":"propose_entity","name":"huge","body":`+string(big)+`}`)
	if !r.IsError || !strings.Contains(r.Text, "limit") {
		t.Errorf("an oversized proposal body should be refused: %s", r.Text)
	}
}

// mustTenantKey exposes the tenant scope key for a test that needs to address chunks
// directly.
func mustTenantKey(t *testing.T, d *Document, ctx context.Context) sqlmem.ScopeKey {
	t.Helper()
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		t.Fatalf("ontologyTenantKey: %v", err)
	}
	return key
}

// TestProposeEntity_NeedsNoTenantGrant is the claim a live run disproved.
//
// Every guard test above grants tenant scopes, deliberately — the guard is only
// interesting against an agent that HAS full authority. That masked the thing the design
// rests on: a curator is supposed to file suggestions WITHOUT tenant write authority, and
// the first implementation routed the write back through the grant-checked dispatch, so
// the op failed for the exact agent it was built for. The unit tests all passed.
func TestProposeEntity_NeedsNoTenantGrant(t *testing.T) {
	d, opCtx, _ := ontologyAgentFixture(t)

	// An agent with NO memory/sqlmem grants at all — what a curator should be.
	bare := tools.WithAgentName(context.Background(), "curator")
	bare = tools.WithRunIdentity(bare, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})

	// The tenant SCOPE itself stays refused, so this is a carve-out for the op and not a
	// hole in the scope gate.
	if _, r := docExec(t, d, bare, `{"op":"query_chunks","scope":"tenant"}`); !r.IsError {
		t.Error("an ungranted agent reached the tenant scope directly")
	}
	out, r := docExec(t, d, bare,
		`{"op":"propose_entity","name":"outage","parent":"event","body":"- `+"`minutes_down`"+`\n\nSeen 3 times.\n"}`)
	if r.IsError {
		t.Fatalf("propose_entity must work without tenant grants: %s", r.Text)
	}
	if out["chunk_id"] == nil {
		t.Fatalf("no chunk id in the result: %v", out)
	}
	read, err := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(read.Proposals) != 1 || read.Proposals[0].Name != "outage" {
		t.Fatalf("the proposal did not land: %+v", read.Proposals)
	}
	if read.Proposals[0].Parent != "event" {
		t.Errorf("proposal parent = %q, want event", read.Proposals[0].Parent)
	}
	// Still inert, still the operator's call.
	for _, term := range read.Terms {
		if term.Name == "outage" {
			t.Error("an ungranted agent's proposal went into force")
		}
	}
}
