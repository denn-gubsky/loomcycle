package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// typedOntologyMD declares a document-class tree with a title-identity class and a
// chunk class with a field identity — the two identity shapes RFC FO allows.
const typedOntologyMD = "# Tenant Ontology\n\n## Classes\n@section classes\n\n" +
	"### Document\n- `title` text @required\n\n" +
	"#### Policy\n@identity\n- `owner` text @required\n- `issued` date\n\n" +
	"### Provision\n- `anchor` anchor @identity\n- `kind` enum(definition, obligation)\n"

// installTenantOntology writes md as the tenant ontology document and sets its status.
// The returned context acts as the OPERATOR (writing the ontology is reserved to
// operator surfaces) and holds tenant grants; data is written in the user scope.
func installTenantOntology(t *testing.T, d *Document, ctx context.Context, md, status string) context.Context {
	t.Helper()
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user"}}
	})
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user"}}
	})
	ctx = tools.WithSubstrateOperator(ctx)
	mj, _ := json.Marshal(md)
	out, r := docExec(t, d, ctx, `{"op":"import_md","scope":"tenant","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	pj, _ := json.Marshal(memrank.OntologyPath)
	if _, r = docExec(t, d, ctx, `{"op":"set_path","scope":"tenant","id":"`+docID+`","path":`+string(pj)+`}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	root, _ := out["root_chunk_id"].(string)
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+root+`"}`)
	if _, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":"%s","status":"%s","revision":%d}`,
		root, status, int(g["revision"].(float64)))); r.IsError {
		t.Fatalf("update_chunk status: %s", r.Text)
	}
	return ctx
}

func newUserDoc(t *testing.T, d *Document, ctx context.Context) (docID, rootID string) {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Rulebook"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	return out["document_id"].(string), out["root_chunk_id"].(string)
}

func warningsOf(out map[string]any) string {
	ws, _ := out["warnings"].([]any)
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i], _ = w.(string)
	}
	return strings.Join(parts, "\n")
}

// TestClassRules_FieldWarningsComeBackAndNothingIsRefused (RFC FO §3.2).
//
// Warnings, never a refusal, for unknown fields and wrong values — free-text fields are
// valid today. And an untyped write's response carries no `warnings` key at all.
func TestClassRules_FieldWarningsComeBackAndNothingIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = installTenantOntology(t, d, ctx, typedOntologyMD, memrank.OntologyConfirmedStatus)
	docID, root := newUserDoc(t, d, ctx)

	out, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","scope":"user","document_id":"%s","parent_id":"%s","title":"Art. 5","type":"Provision","body":"x","fields":{"anchor":"art-5","kind":"rule","extra":1}}`,
		docID, root))
	if r.IsError {
		t.Fatalf("a field warning must not refuse the write: %s", r.Text)
	}
	w := warningsOf(out)
	if !strings.Contains(w, `"rule" is not one of`) || !strings.Contains(w, "extra is not declared") {
		t.Errorf("warnings = %q", w)
	}
	if out["identity_key"] != "provision:art-5" {
		t.Errorf("identity_key = %v", out["identity_key"])
	}

	plain, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","scope":"user","document_id":"%s","title":"note","body":"x","fields":{"anything":1}}`, docID))
	if r.IsError {
		t.Fatalf("untyped create: %s", r.Text)
	}
	if _, has := plain["warnings"]; has {
		t.Errorf("an untyped write grew a warnings key: %v", plain)
	}
	if _, has := plain["identity_key"]; has {
		t.Errorf("an untyped write grew an identity key: %v", plain)
	}
}

// TestClassRules_IdentityCollisionIsRefusedBeforeAnythingIsWritten.
//
// The one refusal: a second chunk claiming an identity another holds. Normalisation
// makes "ART-5 " the same identity as "art-5". The refused write leaves no chunk behind,
// and moving the first chunk off the identity frees it.
func TestClassRules_IdentityCollisionIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = installTenantOntology(t, d, ctx, typedOntologyMD, memrank.OntologyConfirmedStatus)
	docID, root := newUserDoc(t, d, ctx)
	mk := func(anchor string) (map[string]any, tools.Result) {
		return docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_chunk","scope":"user","document_id":"%s","parent_id":"%s","title":"p","type":"Provision","body":"x","fields":{"anchor":%q}}`,
			docID, root, anchor))
	}
	first, r := mk("art-5")
	if r.IsError {
		t.Fatalf("first: %s", r.Text)
	}
	before := chunkCount(t, d, ctx, docID)
	if _, r = mk("ART-5 "); !r.IsError || !strings.Contains(r.Text, "identity_collision") {
		t.Fatalf("the duplicate identity was not refused: %s", r.Text)
	}
	if got := chunkCount(t, d, ctx, docID); got != before {
		t.Errorf("a refused write left a chunk behind: %d → %d", before, got)
	}

	// Re-key the first chunk; its old identity is then free.
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+first["id"].(string)+`"}`)
	upd, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":"%s","revision":%d,"fields":{"anchor":"art-6"}}`,
		first["id"], int(g["revision"].(float64))))
	if r.IsError {
		t.Fatalf("update: %s", r.Text)
	}
	if upd["identity_key"] != "provision:art-6" {
		t.Errorf("update identity_key = %v", upd["identity_key"])
	}
	if _, r = mk("art-5"); r.IsError {
		t.Errorf("the freed identity was still refused: %s", r.Text)
	}

	// An update that drops the type clears the key, so the identity is free again.
	g, _ = docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+first["id"].(string)+`"}`)
	if _, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":"%s","revision":%d,"type":""}`,
		first["id"], int(g["revision"].(float64)))); r.IsError {
		t.Fatalf("untype: %s", r.Text)
	}
	if _, r = mk("art-6"); r.IsError {
		t.Errorf("an untyped chunk kept blocking its old identity: %s", r.Text)
	}
}

// TestClassRules_DocumentClassesGovernDocumentTypes.
//
// A document whose type names a document class is checked against it (title identity,
// required fields); one naming a chunk class or nothing declared is warned about.
func TestClassRules_DocumentClassesGovernDocumentTypes(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = installTenantOntology(t, d, ctx, typedOntologyMD, memrank.OntologyConfirmedStatus)

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Outsourcing  Policy","type":"Policy"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	if !strings.Contains(warningsOf(out), "owner is required") {
		t.Errorf("warnings = %q", warningsOf(out))
	}
	if out["identity_key"] != "policy:outsourcing policy" {
		t.Errorf("identity_key = %v", out["identity_key"])
	}
	if _, r = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"outsourcing policy","type":"Policy"}`); !r.IsError {
		t.Error("a second document with the same title identity was accepted")
	}
	chunkClass, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"x","type":"Provision"}`)
	if !strings.Contains(warningsOf(chunkClass), "chunk class, not a document class") {
		t.Errorf("chunk-class document warnings = %q", warningsOf(chunkClass))
	}
	undeclared, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"y","type":"Memo"}`)
	if !strings.Contains(warningsOf(undeclared), "names no class") {
		t.Errorf("undeclared-type document warnings = %q", warningsOf(undeclared))
	}
}

// TestClassRules_UpsertDerivesItsNaturalKeyFromIdentity.
//
// upsert_chunk with no natural_key, on a class that can derive one, keys on the
// identity — and a second upsert of the same thing updates the same chunk instead of
// colliding with it.
func TestClassRules_UpsertDerivesItsNaturalKeyFromIdentity(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = installTenantOntology(t, d, ctx, typedOntologyMD, memrank.OntologyConfirmedStatus)
	docID, _ := newUserDoc(t, d, ctx)
	up := func() (map[string]any, tools.Result) {
		return docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"upsert_chunk","scope":"user","document_id":"%s","title":"Art. 9","type":"Provision","body":"x","fields":{"anchor":"art-9"}}`, docID))
	}
	first, r := up()
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	if first["natural_key"] != "provision:art-9" || first["created"] != true {
		t.Errorf("first upsert = %v", first)
	}
	second, r := up()
	if r.IsError {
		t.Fatalf("second upsert collided with the chunk it should update: %s", r.Text)
	}
	if second["id"] != first["id"] || second["created"] != false {
		t.Errorf("second upsert = %v, want an update of %v", second, first["id"])
	}
}

// TestClassRules_FlatAndDraftOntologiesChangeNoResponse.
//
// The compatibility guarantee on the write path. A confirmed SECTION-LESS ontology gets
// no field warnings (its write responses stay byte-identical), and a DRAFT sectioned one
// governs nothing — no warnings, no identity.
func TestClassRules_FlatAndDraftOntologiesChangeNoResponse(t *testing.T) {
	for name, tc := range map[string]struct{ md, status string }{
		"flat confirmed":  {"# Tenant Ontology\n\n## incident\n- `summary` text @required\n", memrank.OntologyConfirmedStatus},
		"draft sectioned": {typedOntologyMD, memrank.OntologyDraftStatus},
	} {
		t.Run(name, func(t *testing.T) {
			d, ctx, _ := documentFixture(t)
			ctx = installTenantOntology(t, d, ctx, tc.md, tc.status)
			docID, _ := newUserDoc(t, d, ctx)
			for _, typ := range []string{"incident", "Provision"} {
				out, r := docExec(t, d, ctx, fmt.Sprintf(
					`{"op":"create_chunk","scope":"user","document_id":"%s","title":"t","type":"%s","body":"x","fields":{"anchor":"a1","zzz":1}}`, docID, typ))
				if r.IsError {
					t.Fatalf("create %s: %s", typ, r.Text)
				}
				if _, has := out["warnings"]; has {
					t.Errorf("%s: warnings appeared: %v", typ, out["warnings"])
				}
				if _, has := out["identity_key"]; has {
					t.Errorf("%s: identity key appeared: %v", typ, out["identity_key"])
				}
			}
		})
	}
}

// TestOntologyVersion_IsStampedOnEveryFactWrite (RFC FO §3.6).
//
// Both meta writers — the fact write and the supersede seed — stamp the ontology root's
// revision; with no ontology document the column stays NULL, never a fake version 0.
func TestOntologyVersion_IsStampedOnEveryFactWrite(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, _ := newUserDoc(t, d, ctx)
	key := sidecarScope(t, d, ctx)

	bare, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":"%s","title":"a","natural_key":"k:a","body":"x"}`, docID))
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	if v := metaOntologyVersion(t, d, ctx, key, bare["id"].(string)); v != nil {
		t.Errorf("no ontology document: ontology_version = %v, want NULL", v)
	}

	ctx = installTenantOntology(t, d, ctx, typedOntologyMD, memrank.OntologyConfirmedStatus)
	want := d.ontologyVersion(ctx)
	if want < 2 {
		t.Fatalf("ontology root revision = %d; the confirm flip should have bumped it", want)
	}
	stamped, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":"%s","title":"b","natural_key":"k:b","body":"x"}`, docID))
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	if v := metaOntologyVersion(t, d, ctx, key, stamped["id"].(string)); fmt.Sprint(v) != fmt.Sprint(want) {
		t.Errorf("fact write ontology_version = %v, want %d", v, want)
	}

	// The supersede seed: a plain chunk with no sidecar, retired → its seeded row is stamped.
	plain, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":"%s","title":"old","body":"x"}`, docID))
	repl, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":"%s","title":"new","body":"y"}`, docID))
	if _, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"supersede_chunk","scope":"user","id":"%s","supersedes_id":"%s"}`,
		repl["id"], plain["id"])); r.IsError {
		t.Fatalf("supersede: %s", r.Text)
	}
	if v := metaOntologyVersion(t, d, ctx, key, plain["id"].(string)); fmt.Sprint(v) != fmt.Sprint(want) {
		t.Errorf("supersede seed ontology_version = %v, want %d", v, want)
	}
}

func metaOntologyVersion(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey, chunkID string) any {
	t.Helper()
	res, err := d.query(ctx, key, `SELECT ontology_version FROM chunk_memory_meta WHERE chunk_id = ?`, chunkID)
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("read ontology_version of %s: %v (rows %d)", chunkID, err, len(res.Rows))
	}
	v := res.Rows[0][0]
	if n, ok := asInt64(v); ok {
		return n
	}
	return v
}

func chunkCount(t *testing.T, d *Document, ctx context.Context, docID string) int {
	t.Helper()
	key := sidecarScope(t, d, ctx)
	res, err := d.query(ctx, key, `SELECT count(*) FROM chunks WHERE document_id = ?`, docID)
	if err != nil {
		t.Fatal(err)
	}
	return asInt(res.Rows[0][0])
}

// TestListTypes_ReportsTheReservedStructuralVocabulary.
func TestListTypes_ReportsTheReservedStructuralVocabulary(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"list_types","scope":"user"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	got, _ := out["reserved"].([]any)
	if len(got) != len(memrank.ReservedStructuralTypes()) {
		t.Errorf("reserved = %v", out["reserved"])
	}
}

// TestOntologyIdentity_CollisionRefusedOnPostgres runs the identity rules on the
// postgres tier: the ALTER + unique index must exist there, NULL identity keys must not
// collide, and a duplicate identity is refused.
func TestOntologyIdentity_CollisionRefusedOnPostgres(t *testing.T) {
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
	ctx := tools.WithAgentName(context.Background(), "pg-identity")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
	ctx = installTenantOntology(t, d, ctx, typedOntologyMD, memrank.OntologyConfirmedStatus)
	docID, root := newUserDoc(t, d, ctx)
	// Untyped chunks: NULL identity keys, which must not collide under the unique index.
	for i := 0; i < 2; i++ {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":"%s","title":"n%d","body":"x"}`, docID, i)); r.IsError {
			t.Fatalf("untyped create %d: %s", i, r.Text)
		}
	}
	mk := fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":"%s","parent_id":"%s","title":"p","type":"Provision","body":"x","fields":{"anchor":"art-1"}}`, docID, root)
	if out, r := docExec(t, d, ctx, mk); r.IsError || out["identity_key"] != "provision:art-1" {
		t.Fatalf("first identity: %v %s", out, r.Text)
	}
	if _, r := docExec(t, d, ctx, mk); !r.IsError || !strings.Contains(r.Text, "identity_collision") {
		t.Errorf("duplicate identity not refused on postgres: %s", r.Text)
	}
	key := sidecarScope(t, d, ctx)
	if _, err := d.query(ctx, key, `SELECT ontology_version FROM chunk_memory_meta WHERE 1=0`); err != nil {
		t.Errorf("ontology_version column missing on postgres: %v", err)
	}
}
