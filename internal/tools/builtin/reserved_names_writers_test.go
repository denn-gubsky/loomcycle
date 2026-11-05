package builtin

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Every writer of the Path tree respects the reserved tenant names, not only the
// Path and Document ops that name a path (RFC FY Part C, review of P2).

// tenantMemoryCtx is an agent inside a run holding only the tenant memory grant —
// the least a Memory set at tenant scope needs.
func tenantMemoryCtx() context.Context {
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	ctx = tools.WithRunID(ctx, "r_agent")
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
	})
}

// TestReservedName_MemorySetCannotTakeOne: Memory set with a path registers a name
// (an upsert), so an agent naming a memory entry at a reserved name would re-point
// what the runtime finds there.
func TestReservedName_MemorySetCannotTakeOne(t *testing.T) {
	for _, name := range memrank.ReservedTenantPaths() {
		t.Run(name, func(t *testing.T) {
			e := newReservedEnv(t)
			m := &Memory{Store: e.d.Store, SqlMem: e.d.SqlMem, MaxValueBytes: 65536, DefaultQuotaBytes: 1 << 20}
			before := e.tenantName(t, name)
			pj, _ := json.Marshal(name)
			req := `{"op":"set","scope":"tenant","key":"planted","value":"x","path":` + string(pj) + `}`
			res, err := m.Execute(tenantMemoryCtx(), json.RawMessage(req))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryPermission {
				t.Errorf("an agent's Memory set took the reserved name %s: %+v", name, res)
			}
			parent, base, _ := splitPath(name)
			if row, gerr := e.d.Store.DirentGet(context.Background(), "tnt", "tenant", "", parent, base); gerr == nil && row.Kind == "memory_entry" {
				t.Errorf("%s now names a memory entry", name)
			}
			if got := e.tenantName(t, name); got != before {
				t.Errorf("%s names %q after a refused set, want %q", name, got, before)
			}
			// The operator's own off-run call may.
			opCtx := tools.WithSubstrateOperator(tools.UpdateRunPolicy(e.opCtx, func(rp *tools.RunPolicy) {
				rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
			}))
			if res, _ := m.Execute(opCtx, json.RawMessage(req)); res.IsError {
				t.Errorf("the operator's Memory set at %s was refused: %s", name, res.Text)
			}
		})
	}
}

// TestReservedName_NoRecordWindowCannotUnseatTheOntology is the reviewer's probe: in
// a tenant with no recorded ontology id (every tenant right after the upgrade), an
// agent re-points /memory/ontology at a memory entry so the ontology resolves to
// nothing, then deletes the real one.
func TestReservedName_NoRecordWindowCannotUnseatTheOntology(t *testing.T) {
	e := newReservedEnv(t)
	key := mustTenantKey(t, e.d, e.opCtx)
	if _, err := e.d.docSQL().Exec(context.Background(), key, `DELETE FROM document_roles`, nil, 0); err != nil {
		t.Fatalf("clear the record: %v", err)
	}
	m := &Memory{Store: e.d.Store, SqlMem: e.d.SqlMem, MaxValueBytes: 65536, DefaultQuotaBytes: 1 << 20}
	_, _ = m.Execute(tenantMemoryCtx(), json.RawMessage(`{"op":"set","scope":"tenant","key":"planted","value":"x","path":"/memory/ontology"}`))
	if r := e.doc(t, e.agentCtx, `{"op":"delete_document","scope":"tenant","id":"`+e.ontology+`"}`); !r.IsError {
		t.Errorf("an agent deleted the ontology after re-pointing its name: %s", r.Text)
	}
	read, err := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
	if err != nil || read.DocumentID != e.ontology {
		t.Errorf("the ontology reads as %q (err %v), want %q", read.DocumentID, err, e.ontology)
	}
}

// TestDirentWrites_GoThroughTheReservedNameChokepoint is the census: every write of
// the dirent plane in this package goes through dirent_write.go, where the reserved
// tenant names are enforced. A new writer calling the store directly would reopen the
// hole a Memory set was found to have.
func TestDirentWrites_GoThroughTheReservedNameChokepoint(t *testing.T) {
	writes := map[string]bool{"DirentCreate": true, "DirentDelete": true, "DirentDeleteUnder": true, "DirentMove": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "dirent_write.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && writes[sel.Sel.Name] {
				offenders = append(offenders, fset.Position(call.Pos()).String()+" "+sel.Sel.Name)
			}
			return true
		})
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("dirent writes outside dirent_write.go (use createDirent / deleteDirent / deleteDirentsUnder / moveDirent):\n%s",
			strings.Join(offenders, "\n"))
	}
}

// TestOntology_BootPassRecordsAnUnrecordedTenant closes the window the lazy migration
// leaves open: right after the upgrade no tenant has a record until something
// resolves its ontology.
func TestOntology_BootPassRecordsAnUnrecordedTenant(t *testing.T) {
	e := newReservedEnv(t)
	key := mustTenantKey(t, e.d, e.opCtx)
	if _, err := e.d.docSQL().Exec(context.Background(), key, `DELETE FROM document_roles`, nil, 0); err != nil {
		t.Fatalf("clear the record: %v", err)
	}
	// A boot context: no run, no identity, no operator marker.
	n, err := e.d.RecordTenantOntologies(context.Background())
	if err != nil || n != 1 {
		t.Errorf("boot pass = %d, %v; want 1 tenant with an ontology", n, err)
	}
	if got, err := e.d.recordedRole(context.Background(), key, roleOntology); err != nil || got != e.ontology {
		t.Errorf("recorded ontology after the boot pass = %q (err %v), want %q", got, err, e.ontology)
	}
}

// TestReservedName_ConsolidatorEnsuresTheEntitiesDocument: the memory bundle's
// consolidator creates /memory/entities from inside its run with exactly this call
// (op, scope, title, path). That is the runtime's own document, so the call is an
// ensure of it: allowed, idempotent, and answering with the document either way.
func TestReservedName_ConsolidatorEnsuresTheEntitiesDocument(t *testing.T) {
	e := newReservedEnv(t)
	call := `{"op":"create_document","scope":"tenant","title":"Entities","path":"/memory/entities"}`
	out, r := docExec(t, e.d, e.agentCtx, call)
	if r.IsError {
		t.Fatalf("the consolidator's create of /memory/entities was refused: %s", r.Text)
	}
	id, _ := out["document_id"].(string)
	if id == "" || e.tenantName(t, "/memory/entities") != id {
		t.Fatalf("/memory/entities names %q, the call answered %v", e.tenantName(t, "/memory/entities"), out)
	}
	again, r := docExec(t, e.d, e.agentCtx, call)
	if r.IsError || again["document_id"] != id {
		t.Errorf("a second ensure answered %v (%s), want the same document %s", again, r.Text, id)
	}
	// Without a title, as the runtime supplies it anyway.
	if _, r := docExec(t, e.d, e.agentCtx, `{"op":"create_document","scope":"tenant","path":"/memory/entities"}`); r.IsError {
		t.Errorf("an ensure without a title was refused: %s", r.Text)
	}
}

// TestReservedName_AnAgentCannotCreateTheEntitiesDocumentWithItsOwnContent: the ensure
// carries nothing of the caller's — a body, status, type, tags, fields or another
// title would be the agent authoring the document every placed fact lands in.
func TestReservedName_AnAgentCannotCreateTheEntitiesDocumentWithItsOwnContent(t *testing.T) {
	for _, extra := range []string{
		`"body":"planted"`, `"status":"confirmed"`, `"type":"ontology"`, `"tags":["x"]`,
		`"fields":{"a":"b"}`, `"title":"Not Entities"`,
	} {
		t.Run(extra, func(t *testing.T) {
			e := newReservedEnv(t)
			_, r := docExec(t, e.d, e.agentCtx, `{"op":"create_document","scope":"tenant","path":"/memory/entities",`+extra+`}`)
			if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission {
				t.Errorf("an agent created /memory/entities with %s: %+v", extra, r)
			}
			if got := e.tenantName(t, "/memory/entities"); got != "" {
				t.Errorf("/memory/entities names %q after a refused create", got)
			}
		})
	}
}

// TestRemember_FilesIntoOneEntitiesDocument: the entity document's get-or-create
// looked only BELOW /memory/entities, so it never found the document it had made and
// each remember created another and moved the name onto it.
func TestRemember_FilesIntoOneEntitiesDocument(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	for _, fact := range []string{"Ada wrote the first program.", "Grace found the first bug."} {
		if _, r := docExec(t, d, ctx, `{"op":"remember","scope":"user","text":"`+fact+`"}`); r.IsError {
			t.Fatalf("remember: %s", r.Text)
		}
	}
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.query(ctx, key, `SELECT count(*) FROM documents WHERE title = ?`, entitiesDocumentTitle)
	if err != nil {
		t.Fatal(err)
	}
	if n := asInt(res.Rows[0][0]); n != 1 {
		t.Errorf("two remembers made %d entity documents, want 1", n)
	}
}

// TestReservedName_CaseFoldedSubtreeOpsLeaveItIntact: Path names are case-sensitive,
// so /MEMORY is not an ancestor of the reserved names and the guard lets it through.
// The store must agree: on SQLite a LIKE prefix folded the case and rm/mv of /MEMORY
// reached /memory/ontology.
func TestReservedName_CaseFoldedSubtreeOpsLeaveItIntact(t *testing.T) {
	e := newReservedEnv(t)
	if r := e.path(t, e.agentCtx, `{"op":"mkdir","scope":"tenant","path":"/MEMORY"}`); r.IsError {
		t.Fatalf("mkdir /MEMORY: %s", r.Text)
	}
	_ = e.path(t, e.agentCtx, `{"op":"rm","scope":"tenant","path":"/MEMORY","recursive":true}`)
	if got := e.tenantName(t, memrank.OntologyPath); got != e.ontology {
		t.Errorf("rm -r /MEMORY left /memory/ontology naming %q, want %q", got, e.ontology)
	}
	if r := e.path(t, e.agentCtx, `{"op":"mkdir","scope":"tenant","path":"/MEMORY"}`); r.IsError {
		t.Fatalf("mkdir /MEMORY again: %s", r.Text)
	}
	_ = e.path(t, e.agentCtx, `{"op":"mv","scope":"tenant","path":"/MEMORY","to":"/elsewhere"}`)
	if got := e.tenantName(t, memrank.OntologyPath); got != e.ontology {
		t.Errorf("mv /MEMORY left /memory/ontology naming %q, want %q", got, e.ontology)
	}
}
