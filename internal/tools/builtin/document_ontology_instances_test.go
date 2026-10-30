package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const instancesOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### OrganizationalUnit\n\n### Post\n\n" +
	"## Relations\n@section relations\n\n" +
	"## Instances\n@section instances\n\n" +
	"### Acme Bank\n@a organization\n@nesting sub_organization_of\n@alt Acme\n\n" +
	"#### Compliance Department\n@a OrganizationalUnit\n\n" +
	"### Head of Compliance\n@a Post\n\n" +
	"### Compliance Officer\n@a Post\n@reports_to Head of Compliance\n\n" +
	"### Ghost\nNo class here.\n"

// recordingWriter captures channel writes, standing in for the store-backed publisher.
type recordingWriter struct {
	mu   sync.Mutex
	msgs []channels.WriteRequest
}

func (w *recordingWriter) Write(_ context.Context, req channels.WriteRequest) (channels.WriteResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, req)
	return channels.WriteResult{}, nil
}

func (w *recordingWriter) adopted() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, m := range w.msgs {
		var ev map[string]any
		if m.Channel == "memory/subjects" && json.Unmarshal(m.Payload, &ev) == nil && ev["op"] == "adopted" {
			out = append(out, fmt.Sprint(ev["subject_natural_key"]))
		}
	}
	return out
}

func stateOf(rep InstanceReport, name string) InstanceState {
	for _, s := range rep.Instances {
		if s.Name == name {
			return s
		}
	}
	return InstanceState{}
}

func ontologyEdgeKinds(t *testing.T, d *Document, ctx context.Context) map[string]int {
	t.Helper()
	res, err := d.query(ctx, mustTenantKey(t, d, ctx), `SELECT kind, count(*) FROM chunk_edges WHERE method = ? GROUP BY kind`, edgeMethodOntology)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, row := range res.Rows {
		out[asStr(row[0])] = asInt(row[1])
	}
	return out
}

// TestAdoptInstances_CreatesSubjectsUnderTheConsolidatorsKeys (RFC FO §3.5, 10f.3).
//
// Adoption writes each instance as the subject node the consolidator writes, under the
// key the consolidator computes for the same name — so a fact the consolidator later
// files about "the Acme Bank" lands on the adopted subject, not beside it. Statements
// among instances are written with method `ontology`; the adopted event is published.
func TestAdoptInstances_CreatesSubjectsUnderTheConsolidatorsKeys(t *testing.T) {
	d, base, _ := documentFixture(t)
	w := &recordingWriter{}
	d.Writer = w
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)

	dry, err := d.AdoptInstances(ctx, true)
	if err != nil || stateOf(dry, "Acme Bank").State != "pending" || stateOf(dry, "Ghost").State != "error" {
		t.Fatalf("dry run = %#v, %v", dry, err)
	}
	if len(w.adopted()) != 0 {
		t.Fatal("a dry run published events")
	}

	rep, err := d.AdoptInstances(ctx, false)
	if err != nil || rep.Adopted != 4 {
		t.Fatalf("adopt = %#v, %v", rep, err)
	}
	acme := stateOf(rep, "Acme Bank")
	if acme.Key != "organization:acme-bank" || acme.Key != memrank.ConsolidatorEntityKey("organization", "Acme Bank") {
		t.Errorf("Acme Bank key = %q", acme.Key)
	}
	if got := w.adopted(); len(got) != 4 {
		t.Errorf("adopted events = %v, want one per created subject", got)
	}

	// The consolidator, naming the same subject its own way, lands on the same node.
	key := mustTenantKey(t, d, ctx)
	root, _ := d.chunkIDByNaturalKey(ctx, key, acme.Key)
	if root == "" {
		t.Fatal("no subject under the adopted key")
	}
	if again := memrank.ConsolidatorEntityKey("organization", "  the ACME bank "); again != acme.Key {
		t.Fatalf("the consolidator would key it %q", again)
	}

	// The key is recorded on the instance, so a rename keeps it.
	read, _ := d.tenantOntologyRead(ctx)
	for _, inst := range read.Instances {
		if inst.Name == "Acme Bank" && inst.RegistryKey != acme.Key {
			t.Errorf("registry_key not recorded: %#v", inst)
		}
	}

	kinds := ontologyEdgeKinds(t, d, ctx)
	if kinds["sub_organization_of"] != 1 || kinds["reports_to"] != 1 || kinds["instance_of"] != 3 {
		t.Errorf("ontology statements = %v (want nesting 1, reports_to 1, instance_of for the 3 tenant-class instances)", kinds)
	}

	// Idempotent: nothing new on a second confirm.
	second, _ := d.AdoptInstances(ctx, false)
	if second.Adopted != 0 || stateOf(second, "Acme Bank").State != "unchanged" {
		t.Errorf("second adopt = %#v", second)
	}
	if again := ontologyEdgeKinds(t, d, ctx); fmt.Sprint(again) != fmt.Sprint(kinds) {
		t.Errorf("statements duplicated: %v → %v", kinds, again)
	}
	if len(w.adopted()) != 4 {
		t.Errorf("a second confirm re-published adoption: %v", w.adopted())
	}
}

// TestAdoptInstances_ARenameKeepsTheKeyAndARemovalErasesNothing (RFC FO §8 P1c gate).
func TestAdoptInstances_ARenameKeepsTheKeyAndARemovalErasesNothing(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	first, err := d.AdoptInstances(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	key := mustTenantKey(t, d, ctx)
	acmeKey := stateOf(first, "Acme Bank").Key

	read, _ := d.tenantOntologyRead(ctx)
	var acmeChunk, headChunk string
	for _, inst := range read.Instances {
		switch inst.Name {
		case "Acme Bank":
			acmeChunk = inst.ChunkID
		case "Head of Compliance":
			headChunk = inst.ChunkID
		}
	}
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+acmeChunk+`"}`)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%d,"title":"Acme Bank PLC"}`,
		acmeChunk, int(g["revision"].(float64)))); r.IsError {
		t.Fatal(r.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"tenant","id":"`+headChunk+`"}`); r.IsError {
		t.Fatal(r.Text)
	}

	rep, err := d.AdoptInstances(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	renamed := stateOf(rep, "Acme Bank PLC")
	if renamed.State != "renamed" || renamed.Key != acmeKey {
		t.Errorf("rename = %#v, want renamed under %q", renamed, acmeKey)
	}
	root, _ := d.chunkIDByNaturalKey(ctx, key, acmeKey)
	g, _ = docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+root+`"}`)
	if g["title"] != "Acme Bank PLC" || !strings.Contains(fmt.Sprint(g["fields"]), "Acme Bank") {
		t.Errorf("subject after rename: title %v, fields %v", g["title"], g["fields"])
	}

	headKey := stateOf(first, "Head of Compliance").Key
	if id, _ := d.chunkIDByNaturalKey(ctx, key, headKey); id == "" {
		t.Error("removing an instance erased its subject")
	}
	if len(rep.Orphaned) != 1 || !strings.Contains(rep.Orphaned[0], "Head of Compliance") {
		t.Errorf("orphaned = %v", rep.Orphaned)
	}
}

// TestAdoptInstances_LinksASubjectTheConsolidatorAlreadyLearned.
func TestAdoptInstances_LinksASubjectTheConsolidatorAlreadyLearned(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	nk := memrank.ConsolidatorEntityKey("Post", "Head of Compliance")
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":"head of compliance","path":"/facts/head-compliance","type":"Post","subject":"head of compliance","natural_key":%q}`, nk)); r.IsError {
		t.Fatalf("seed the consolidator's subject: %s", r.Text)
	}
	rep, err := d.AdoptInstances(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(rep, "Head of Compliance"); st.State != "linked" || st.Key != nk {
		t.Errorf("an existing subject should be linked, not duplicated: %#v", st)
	}
	res, _ := d.query(ctx, mustTenantKey(t, d, ctx), `SELECT count(*) FROM chunk_memory_meta WHERE natural_key = ?`, nk)
	if asInt(res.Rows[0][0]) != 1 {
		t.Error("a second subject was minted for an existing key")
	}
}

// TestProposeEntity_Instances.
func TestProposeEntity_Instances(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	if _, r := docExec(t, d, ctx, `{"op":"propose_entity","scope":"user","section":"instances","name":"National Bank","parent":"organization","body":"issuer of 23 documents"}`); r.IsError {
		t.Fatalf("propose instance: %s", refusalText(r))
	}
	read, _ := d.tenantOntologyRead(ctx)
	found := false
	for _, p := range read.Proposals {
		found = found || (p.Name == "National Bank" && p.Section == memrank.SectionInstances && strings.Contains(p.Body, "@a organization"))
	}
	if !found {
		t.Errorf("instance proposal not filed with its class: %#v", read.Proposals)
	}
	for name, body := range map[string]string{
		"existing":  `{"op":"propose_entity","scope":"user","section":"instances","name":"Acme Bank","parent":"organization"}`,
		"no class":  `{"op":"propose_entity","scope":"user","section":"instances","name":"X Corp"}`,
		"bad class": `{"op":"propose_entity","scope":"user","section":"instances","name":"X Corp","parent":"spaceship"}`,
	} {
		if _, r := docExec(t, d, ctx, body); !r.IsError {
			t.Errorf("%s: should be refused", name)
		}
	}
}

// TestAdoptInstances_AForgedRegistryKeyIsNotTrusted (security review).
//
// The key on an instance chunk is trusted only when it is the key derived from the
// instance's own class and name, or when the subject it names records that it was
// adopted from THIS instance chunk. Otherwise a chunk carrying another subject's key —
// say a proposal accepted in place — would link to that subject and rename it.
func TestAdoptInstances_AForgedRegistryKeyIsNotTrusted(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	victimKey := memrank.ConsolidatorEntityKey("organization", "Victim Corp")
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":"Victim Corp","path":"/facts/victim-corp","type":"organization","subject":"Victim Corp","natural_key":%q}`, victimKey)); r.IsError {
		t.Fatal(r.Text)
	}
	read, _ := d.tenantOntologyRead(ctx)
	sec := read.SectionChunks[memrank.SectionInstances]
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":"Mallory Ltd","body":"@a organization\n","fields":{"registry_key":%q}}`,
		read.DocumentID, sec, victimKey)); r.IsError {
		t.Fatal(r.Text)
	}
	rep, err := d.AdoptInstances(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(rep, "Mallory Ltd"); st.Key == victimKey {
		t.Errorf("a forged registry_key was trusted: %#v", st)
	}
	root, _ := d.chunkIDByNaturalKey(ctx, mustTenantKey(t, d, ctx), victimKey)
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+root+`"}`)
	if g["title"] != "Victim Corp" {
		t.Errorf("the victim subject was renamed to %v", g["title"])
	}
}

// TestProposeEntity_AnInstancesEvidenceCannotDeclare (security review).
//
// The body of a proposal is the agent's evidence. Directive lines in it would become
// live declarations the moment an operator accepted the proposal in place — a second @a
// overriding the class, a relation to another instance — so they are neutralised.
func TestProposeEntity_AnInstancesEvidenceCannotDeclare(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)
	body, _ := json.Marshal("seen in 3 documents\n@a Post\n@reports_to Head of Compliance\n- `@nesting` manages\n")
	if _, r := docExec(t, d, ctx, `{"op":"propose_entity","scope":"user","section":"instances","name":"Shadow Org","parent":"organization","body":`+string(body)+`}`); r.IsError {
		t.Fatal(refusalText(r))
	}
	read, _ := d.tenantOntologyRead(ctx)
	for _, p := range read.Proposals {
		if p.Name != "Shadow Org" {
			continue
		}
		parsed := memrank.ParseInstanceBody(p.Name, p.Body)
		if parsed.Class != "organization" || len(parsed.Relations) != 0 || parsed.Nesting != "" {
			t.Errorf("evidence declared something: %#v", parsed)
		}
		return
	}
	t.Fatal("proposal not filed")
}

// TestAdoptInstances_RelationKindsMustBeWritableAndDeclared (security review).
//
// A reserved kind is the runtime's alone and is refused here, not left to whatever the
// statement layer happens to check; with a Relations section declared, an undeclared
// kind is reported rather than written.
func TestAdoptInstances_RelationKindsMustBeWritableAndDeclared(t *testing.T) {
	md := strings.Replace(instancesOntologyMD, "### Compliance Officer\n@a Post\n@reports_to Head of Compliance\n",
		"### Compliance Officer\n@a Post\n@supersedes Head of Compliance\n@made_up_kind Head of Compliance\n", 1)
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus)
	rep, err := d.AdoptInstances(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	kinds := ontologyEdgeKinds(t, d, ctx)
	if kinds["supersedes"] != 0 || kinds["made_up_kind"] != 0 {
		t.Errorf("a reserved or undeclared kind was written: %v", kinds)
	}
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "supersedes") || !strings.Contains(joined, "made_up_kind") {
		t.Errorf("both kinds should be reported: %s", joined)
	}
}

// TestOntologyInstances_AdoptionOnPostgres runs adoption on the postgres tier: subject
// creation under the consolidator's key, the recorded key, the statements, and a second
// confirm that changes nothing.
func TestOntologyInstances_AdoptionOnPostgres(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres tier")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	dropAllSqlmemScopes(t, raw)
	mgr, err := sqlmem.NewPostgres(context.Background(), sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		dropAllSqlmemScopes(t, raw)
		_ = raw.Close()
	})
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	base := tools.WithAgentName(context.Background(), "pg-instances")
	base = tools.WithRunIdentity(base, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
	ctx := installTenantOntology(t, d, base, instancesOntologyMD, memrank.OntologyConfirmedStatus)

	rep, err := d.AdoptInstances(ctx, false)
	if err != nil || rep.Adopted != 4 {
		t.Fatalf("adopt on postgres = %#v, %v", rep, err)
	}
	kinds := ontologyEdgeKinds(t, d, ctx)
	if kinds["sub_organization_of"] != 1 || kinds["instance_of"] != 3 {
		t.Errorf("statements on postgres = %v", kinds)
	}
	again, _ := d.AdoptInstances(ctx, false)
	if again.Adopted != 0 || stateOf(again, "Acme Bank").State != "unchanged" {
		t.Errorf("second adopt on postgres = %#v", again)
	}
}
