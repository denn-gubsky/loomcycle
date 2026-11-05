package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The tenant ontology is a document identity, not a name (RFC FY Part C): Path's
// tenant scope is gated like Document's, the names the runtime finds tenant-wide
// documents by are reserved to the operator, and the guard follows the ontology's
// recorded id wherever it is named.

// reservedEnv is a tenant with an ontology, a shared tenant document an agent may
// rename freely, and a user-scope document a move could carry into the tenant.
type reservedEnv struct {
	d        *Document
	p        *Path
	opCtx    context.Context // the operator's own off-run call
	agentCtx context.Context // an agent inside a run, holding both tenant grants
	ontology string
	decoy    string // tenant document at /notes/decoy
	userDoc  string // user-scope document at /drafts/ents
}

func newReservedEnv(t *testing.T) reservedEnv {
	t.Helper()
	d, opCtx, base := ontologyAgentFixture(t)
	e := reservedEnv{d: d, p: &Path{Store: d.Store}, opCtx: opCtx, agentCtx: tools.WithRunID(base, "r_agent")}
	read, err := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
	if err != nil || read.DocumentID == "" {
		t.Fatalf("fixture ontology: %v %+v", err, read)
	}
	e.ontology = read.DocumentID
	out, r := docExec(t, d, opCtx, `{"op":"create_document","scope":"tenant","title":"Decoy","path":"/notes/decoy"}`)
	if r.IsError {
		t.Fatalf("decoy: %s", r.Text)
	}
	e.decoy, _ = out["document_id"].(string)
	out, r = docExec(t, d, opCtx, `{"op":"create_document","scope":"user","title":"Ents","path":"/drafts/ents"}`)
	if r.IsError {
		t.Fatalf("user doc: %s", r.Text)
	}
	e.userDoc, _ = out["document_id"].(string)
	// An explicit /memory directory: Path mv moves a row, so an implicit directory
	// cannot be moved at all, and the ancestor case would pass for that reason alone.
	if _, err := d.Store.DirentCreate(context.Background(), store.DirentRow{
		TenantID: "tnt", Scope: "tenant", ParentPath: "/", Name: "memory", Kind: "directory",
	}); err != nil {
		t.Fatalf("seed /memory: %v", err)
	}
	return e
}

func (e reservedEnv) path(t *testing.T, ctx context.Context, req string) tools.Result {
	t.Helper()
	_, r := pathExec(t, e.p, ctx, req)
	return r
}

func (e reservedEnv) doc(t *testing.T, ctx context.Context, req string) tools.Result {
	t.Helper()
	_, r := docExec(t, e.d, ctx, req)
	return r
}

// tenantName returns the document a tenant-scope name points at, or "".
func (e reservedEnv) tenantName(t *testing.T, full string) string {
	t.Helper()
	parent, name, _ := splitPath(full)
	row, err := e.d.Store.DirentGet(context.Background(), "tnt", "tenant", "", parent, name)
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return ""
		}
		t.Fatalf("DirentGet %s: %v", full, err)
	}
	return direntDocumentID(row)
}

// TestOntology_AnAgentCannotUnnameOrReplaceIt: every way to take the ontology's
// name away, or to put something else under a reserved name, is refused inside a
// run and allowed to the operator's own off-run call.
func TestOntology_AnAgentCannotUnnameOrReplaceIt(t *testing.T) {
	md, _ := json.Marshal("# Fake Ontology\n\n## anything\n")
	cases := []struct {
		name string
		call func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result
		// took reports whether the call took effect, read from the store rather than
		// from the answer: a refusal that still changed the tree is not a refusal.
		took func(t *testing.T, e reservedEnv) bool
	}{
		{"Path rm the ontology's name",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				return e.path(t, ctx, `{"op":"rm","scope":"tenant","path":"/memory/ontology"}`)
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, memrank.OntologyPath) == "" }},
		{"Path mv the ontology away (source)",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				return e.path(t, ctx, `{"op":"mv","scope":"tenant","path":"/memory/ontology","to":"/archive/onto"}`)
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, memrank.OntologyPath) == "" }},
		{"Path mv a document onto a reserved name (destination)",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				return e.path(t, ctx, `{"op":"mv","scope":"tenant","path":"/notes/decoy","to":"/memory/entities"}`)
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, "/memory/entities") == e.decoy }},
		{"Path rm -r /memory (an ancestor)",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				return e.path(t, ctx, `{"op":"rm","scope":"tenant","path":"/memory","recursive":true}`)
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, memrank.OntologyPath) == "" }},
		{"Path mv /memory (an ancestor)",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				return e.path(t, ctx, `{"op":"mv","scope":"tenant","path":"/memory","to":"/old-memory"}`)
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, memrank.OntologyPath) == "" }},
		{"create_document at the ontology's name",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				return e.doc(t, ctx, `{"op":"create_document","scope":"tenant","title":"Fake","path":"/memory/ontology"}`)
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, memrank.OntologyPath) != e.ontology }},
		{"import_md at the ontology's name",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				return e.doc(t, ctx, `{"op":"import_md","scope":"tenant","path":"/memory/ontology","markdown":`+string(md)+`}`)
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, memrank.OntologyPath) != e.ontology }},
		{"set_path another document onto the ontology's name",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				return e.doc(t, ctx, `{"op":"set_path","scope":"tenant","id":"`+e.decoy+`","path":"/memory/ontology"}`)
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, memrank.OntologyPath) != e.ontology }},
		{"delete_document addressed by the ontology's name",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				return e.doc(t, ctx, `{"op":"delete_document","scope":"tenant","path":"/memory/ontology"}`)
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, memrank.OntologyPath) == "" }},
		{"move_documents into a reserved name",
			func(t *testing.T, e reservedEnv, ctx context.Context) tools.Result {
				args := `"document_ids":["` + e.userDoc + `"],"path_map":{"/drafts/ents":"/memory/entities"}`
				dry, r := docExec(t, e.d, ctx, `{"op":"move_documents","scope":"user","to_scope":"tenant",`+args+`}`)
				if r.IsError {
					return r
				}
				n := int(dry["counts"].(map[string]any)["documents"].(float64))
				_, r = docExec(t, e.d, ctx, fmt.Sprintf(`{"op":"move_documents","scope":"user","to_scope":"tenant",%s,"dry_run":false,"expect_count":%d}`, args, n))
				if !r.IsError && n == 0 {
					return tools.Result{IsError: true, Text: "refused per document: " + r.Text}
				}
				return r
			},
			func(t *testing.T, e reservedEnv) bool { return e.tenantName(t, "/memory/entities") == e.userDoc }},
	}
	for _, c := range cases {
		t.Run("agent/"+c.name, func(t *testing.T) {
			e := newReservedEnv(t)
			r := c.call(t, e, e.agentCtx)
			if !r.IsError {
				t.Errorf("an agent inside a run was allowed to %s: %s", c.name, r.Text)
			}
			if c.took(t, e) {
				t.Errorf("an agent's %s took effect", c.name)
			}
			read, err := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
			if err != nil || read.DocumentID != e.ontology {
				t.Errorf("after the refused call the ontology resolves to %q (err %v), want %q", read.DocumentID, err, e.ontology)
			}
		})
		t.Run("operator/"+c.name, func(t *testing.T) {
			e := newReservedEnv(t)
			if r := c.call(t, e, e.opCtx); r.IsError {
				t.Errorf("the operator's off-run call was refused: %s", r.Text)
			}
			if !c.took(t, e) {
				t.Errorf("the operator's %s did not take effect", c.name)
			}
		})
	}
}

// TestOntology_GuardFollowsTheRecordedIdNotTheName: once the operator renames the
// ontology it is still the ontology, and a new document the operator puts at the
// old name is an ordinary document.
func TestOntology_GuardFollowsTheRecordedIdNotTheName(t *testing.T) {
	e := newReservedEnv(t)
	read, _ := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)

	if r := e.path(t, e.opCtx, `{"op":"mv","scope":"tenant","path":"/memory/ontology","to":"/archive/onto"}`); r.IsError {
		t.Fatalf("operator rename: %s", r.Text)
	}
	// The renamed document is still guarded.
	live := fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":"%s","parent_id":"%s","title":"decided","body":""}`,
		read.DocumentID, read.RootChunkID)
	if r := e.doc(t, e.agentCtx, live); !r.IsError {
		t.Error("after a rename an agent created a live entity in the ontology")
	}
	if r := e.doc(t, e.agentCtx, `{"op":"delete_document","scope":"tenant","path":"/archive/onto"}`); !r.IsError {
		t.Error("after a rename an agent deleted the ontology by its new name")
	}

	// A new document at the old name is not the ontology.
	out, r := docExec(t, e.d, e.opCtx, `{"op":"create_document","scope":"tenant","title":"Impostor","path":"/memory/ontology"}`)
	if r.IsError {
		t.Fatalf("operator create at the old name: %s", r.Text)
	}
	impostor, _ := out["document_id"].(string)
	impRoot, _ := out["root_chunk_id"].(string)
	if r := e.doc(t, e.agentCtx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":"%s","parent_id":"%s","title":"note","body":"x"}`,
		impostor, impRoot)); r.IsError {
		t.Errorf("the document at the old name was guarded as if it were the ontology: %s", r.Text)
	}
	got, err := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
	if err != nil || got.DocumentID != read.DocumentID {
		t.Errorf("the ontology reads as %q (err %v), want the renamed %q", got.DocumentID, err, read.DocumentID)
	}
	for _, term := range got.Terms {
		if term.Name == "decided" {
			t.Error("a refused write took effect")
		}
	}
}

// TestOntology_ATenantWithNoRecordAdoptsTheDocumentAtTheName is the upgrade path, on
// both tiers: a scope from before the recorded id has its ontology only at the name.
// The first resolve records it, after which an out-of-band rename (one no hook sees)
// leaves it the ontology.
func TestOntology_ATenantWithNoRecordAdoptsTheDocumentAtTheName(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		base := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
			rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
			rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
		})
		opCtx := tools.WithSubstrateOperator(base)
		agentCtx := tools.WithRunID(base, "r_agent")
		md, _ := json.Marshal("# Tenant Ontology\n\n## event\n")
		out, r := docExec(t, d, opCtx, `{"op":"import_md","scope":"tenant","path":"/memory/ontology","markdown":`+string(md)+`}`)
		if r.IsError {
			t.Fatalf("import_md: %s", r.Text)
		}
		ontology, _ := out["document_id"].(string)
		out, r = docExec(t, d, opCtx, `{"op":"create_document","scope":"tenant","title":"Notes","path":"/notes/n"}`)
		if r.IsError {
			t.Fatalf("notes: %s", r.Text)
		}
		notes, _ := out["document_id"].(string)

		// The scope as an older release left it: no table for the record at all.
		key := mustTenantKey(t, d, opCtx)
		if _, err := d.docSQL().Exec(context.Background(), key, `DROP TABLE IF EXISTS document_roles`, nil, 0); err != nil {
			t.Fatalf("drop: %v", err)
		}
		d.docSQL().ForgetEnsured(key)

		// An ordinary agent write resolves the ontology and records it.
		if r := docExec2(t, d, agentCtx, `{"op":"create_chunk","scope":"tenant","document_id":"`+notes+`","title":"n","body":"x"}`); r.IsError {
			t.Fatalf("agent write elsewhere: %s", r.Text)
		}
		if got, err := d.recordedRole(context.Background(), key, roleOntology); err != nil || got != ontology {
			t.Fatalf("recorded ontology = %q (err %v), want %q", got, err, ontology)
		}
		if _, err := d.Store.DirentMove(context.Background(), "tnt", "tenant", "", "/memory/", "ontology", "/archive/", "onto"); err != nil {
			t.Fatalf("out-of-band rename: %v", err)
		}
		if r := docExec2(t, d, agentCtx, `{"op":"delete_document","scope":"tenant","id":"`+ontology+`"}`); !r.IsError {
			t.Error("after an out-of-band rename an agent deleted the ontology")
		}
		read, err := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
		if err != nil || read.DocumentID != ontology {
			t.Errorf("the ontology reads as %q (err %v), want %q", read.DocumentID, err, ontology)
		}
		// Recording again is an upsert, not a duplicate-key failure.
		if err := d.recordRole(context.Background(), key, roleOntology, ontology); err != nil {
			t.Errorf("re-record: %v", err)
		}
	})
}

func docExec2(t *testing.T, d *Document, ctx context.Context, body string) tools.Result {
	t.Helper()
	_, r := docExec(t, d, ctx, body)
	return r
}

// TestOntology_RefusalNamesTheReservedName keeps the refusal actionable without
// naming anything internal.
func TestOntology_RefusalNamesTheReservedName(t *testing.T) {
	e := newReservedEnv(t)
	r := e.path(t, e.agentCtx, `{"op":"rm","scope":"tenant","path":"/memory/ontology"}`)
	if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission || !strings.Contains(r.Text, memrank.OntologyPath) {
		t.Errorf("refusal = %+v, want a permission error naming %s", r, memrank.OntologyPath)
	}
}

// TestPath_OperatorRenameKeepsAnUnrecordedOntology: in a tenant nothing has
// resolved since the upgrade, the ontology is known only by its name. An operator's
// Path rename must record it first, or the rename would leave the tenant with no
// ontology and the renamed document open to agents.
func TestPath_OperatorRenameKeepsAnUnrecordedOntology(t *testing.T) {
	d, opCtx, base := ontologyAgentFixture(t)
	key := mustTenantKey(t, d, opCtx)
	if _, err := d.docSQL().Exec(context.Background(), key, `DELETE FROM document_roles`, nil, 0); err != nil {
		t.Fatalf("clear the record: %v", err)
	}
	ontology, err := d.documentAtName(opCtx, key, memrank.OntologyPath)
	if err != nil || ontology == "" {
		t.Fatalf("fixture ontology at the name: %q %v", ontology, err)
	}
	p := &Path{Store: d.Store, Identities: d}
	if _, r := pathExec(t, p, opCtx, `{"op":"mv","scope":"tenant","path":"/memory/ontology","to":"/archive/onto"}`); r.IsError {
		t.Fatalf("operator rename: %s", r.Text)
	}
	if _, r := docExec(t, d, tools.WithRunID(base, "r_agent"), `{"op":"delete_document","scope":"tenant","id":"`+ontology+`"}`); !r.IsError {
		t.Error("after an operator's rename an agent deleted the ontology")
	}
	read, err := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
	if err != nil || read.DocumentID != ontology {
		t.Errorf("the ontology reads as %q (err %v), want the renamed %q", read.DocumentID, err, ontology)
	}
}
