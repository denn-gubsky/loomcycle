package builtin

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// gateFixture gives an agent ctx over a tenant with a CONFIRMED ontology that declares
// one type of its own (`research` under `project`), so the gate is exercised against a
// real effective set rather than the bare seed.
func gateFixture(t *testing.T) (*Document, context.Context) {
	t.Helper()
	d, base, _ := documentFixture(t)
	base = tools.WithMemoryPolicy(base, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	base = tools.WithSqlMemPolicy(base, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})
	op := tools.WithSubstrateOperator(base)

	md := "# Tenant Ontology\n\n## project\n- `status`\n\n### research\n- `question`\n"
	mj, _ := json.Marshal(md)
	out, r := docExec(t, d, op, `{"op":"import_md","scope":"tenant","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	id, _ := out["document_id"].(string)
	pj, _ := json.Marshal(memrank.OntologyPath)
	if _, r = docExec(t, d, op, `{"op":"set_path","scope":"tenant","id":"`+id+`","path":`+string(pj)+`}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	root, _ := out["root_chunk_id"].(string)
	g, _ := docExec(t, d, op, `{"op":"get_chunk","scope":"tenant","id":"`+root+`"}`)
	rev := int(g["revision"].(float64))
	if _, r = docExec(t, d, op, `{"op":"update_chunk","scope":"tenant","id":"`+root+
		`","status":"confirmed","revision":`+strconv.Itoa(rev)+`}`); r.IsError {
		t.Fatalf("confirm: %s", r.Text)
	}
	return d, base
}

// TestEntityGate_RefusesATypeTheOntologyDoesNotDeclare.
//
// An invented type becomes an entity node nobody declared and nothing can find. The
// consolidator refuses two such names, but that reaches one writer — every other path
// (an agent's upsert_chunk, an MCP session, a curator) had no check at all.
func TestEntityGate_RefusesATypeTheOntologyDoesNotDeclare(t *testing.T) {
	d, ctx := gateFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	doc, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	mk := func(typ, subject string) tools.Result {
		body := `{"op":"upsert_chunk","scope":"user","document_id":"` + doc + `","parent_id":"` + root +
			`","natural_key":"k:` + typ + `-` + subject + `","title":"t","type":"` + typ + `"`
		if subject != "" {
			body += `,"subject":"` + subject + `"`
		}
		_, res := docExec(t, d, ctx, body+`}`)
		return res
	}

	// An invented type, WITH a subject → refused, and the message names the alternatives.
	r = mk("process", "deploy")
	if !r.IsError {
		t.Fatal("an undeclared entity type was accepted")
	}
	for _, want := range []string{"project", "research", "person", "propose_entity"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("the refusal does not mention %q, so it is not actionable: %s", want, r.Text)
		}
	}
	// A SEED type → allowed.
	if r = mk("person", "ada"); r.IsError {
		t.Errorf("a seed type was refused: %s", r.Text)
	}
	// The tenant's OWN declared subtype → allowed. A gate that only knew the seed would
	// refuse the vocabulary the operator curated, which is worse than no gate.
	if r = mk("research", "memory-survey"); r.IsError {
		t.Errorf("a confirmed tenant type was refused: %s", r.Text)
	}
}

// TestEntityGate_LeavesDocumentWritesAlone is the regression that killed the first
// version of this gate.
//
// `type` holds TWO vocabularies: an entity's type and a document chunk's own structure
// (rfc, section, image). Keying the gate on "a natural_key is present" refused an
// idempotent DOCUMENT write — legitimate, already tested, and on a real deployment it
// would have blocked a documentation store for using its own words. The pair is the
// discriminator: a document carries no subject.
func TestEntityGate_LeavesDocumentWritesAlone(t *testing.T) {
	d, ctx := gateFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Docs","body":""}`)
	doc, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)

	// A document type the ontology has never heard of, with NO subject → allowed.
	for _, typ := range []string{"rfc", "section", "publication", "image"} {
		if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+
			`","parent_id":"`+root+`","natural_key":"doc:`+typ+`","title":"T","type":"`+typ+`"}`); r.IsError {
			t.Errorf("a document write with type %q was refused — the gate is judging the wrong "+
				"population: %s", typ, r.Text)
		}
	}
	// And the consolidator's own STATEMENT node: type "fact", no subject. A deny-list on
	// `fact` would break the pipeline this gate exists to protect.
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+
		`","parent_id":"`+root+`","natural_key":"memory/fact/x","title":"A claim.","type":"fact"}`); r.IsError {
		t.Errorf("the statement node was refused: %s", r.Text)
	}
}

// TestEntityGate_FailsOpenWithNoOntology: a deployment with no ontology document must
// write exactly as it does today. A verification feature that refuses writes when it
// cannot verify is an outage.
func TestEntityGate_FailsOpenWithNoOntology(t *testing.T) {
	d, ctx, _ := documentFixture(t) // no tenant ontology at all
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts","body":""}`)
	doc, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+
		`","parent_id":"`+root+`","natural_key":"k:1","title":"t","type":"whatever","subject":"thing"}`); r.IsError {
		t.Errorf("the gate refused a write with no ontology to check against: %s", r.Text)
	}
}

// TestSubject_IsStoredAndSurfaced — the subject is data now, not something recoverable
// by parsing a natural key. That is what makes `location:user` findable by a query
// instead of by reading keys.
func TestSubject_IsStoredAndSurfaced(t *testing.T) {
	d, ctx := gateFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts","body":""}`)
	doc, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	up, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+
		`","parent_id":"`+root+`","natural_key":"person:ada","title":"Ada","type":"person","subject":"Ada Lovelace"}`)
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	id, _ := up["id"].(string)
	got, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	entity, _ := got["entity"].(map[string]any)
	if entity["subject"] != "Ada Lovelace" {
		t.Errorf("subject = %v, want it stored verbatim", entity["subject"])
	}
	// Preserved when an edit does not restate it.
	if _, r = docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+
		`","parent_id":"`+root+`","natural_key":"person:ada","title":"Ada L."}`); r.IsError {
		t.Fatalf("re-upsert: %s", r.Text)
	}
	got, _ = docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	entity, _ = got["entity"].(map[string]any)
	if entity["subject"] != "Ada Lovelace" {
		t.Errorf("the subject was lost on an edit: %v", entity["subject"])
	}
	// And it is queryable as a column, which is the point of making it data.
	key, _, _ := d.resolveScope(ctx, "user")
	res, err := d.query(ctx, key, `SELECT subject FROM chunk_memory_meta WHERE subject IS NOT NULL`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Rows) != 1 || asStr(res.Rows[0][0]) != "Ada Lovelace" {
		t.Errorf("subject is not queryable: %v", res.Rows)
	}
}
