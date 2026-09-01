package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// placementFixture builds a Memory tool over a store with SQL Memory, an ontology
// document authored the way an operator authors one, and the grants named by scopes.
func placementFixture(t *testing.T, confirmed bool, scopes ...string) (*Memory, context.Context) {
	t.Helper()
	d, base, _ := documentFixture(t)
	// Authoring the ontology is an OPERATOR act and needs tenant grants on both planes.
	// The agent ctx built below is separate and deliberately narrower — the point of the
	// op is what an agent gets to know, not what an operator can write.
	base = tools.WithMemoryPolicy(base, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	base = tools.WithSqlMemPolicy(base, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})
	dctx := tools.WithSubstrateOperator(base)

	// The ontology lives in the TENANT scope at a fixed path, which is where the reader
	// looks — authoring it anywhere else would test nothing.
	md := strings.Join([]string{
		"# Tenant Ontology",
		"",
		"## service",
		"- `@memory_scope` tenant",
		"- `name`",
		"",
		"## person",
		"- `@memory_scope` user",
		"- `name`",
		"",
		"## location",
		"- `name`",
		"",
	}, "\n")
	mj, _ := json.Marshal(md)
	out, r := docExec(t, d, dctx, `{"op":"import_md","scope":"tenant","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	pj, _ := json.Marshal(memrank.OntologyPath)
	if _, r := docExec(t, d, dctx, `{"op":"set_path","scope":"tenant","id":"`+docID+`","path":`+string(pj)+`}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	if confirmed {
		root, _ := out["root_chunk_id"].(string)
		got, r := docExec(t, d, dctx, `{"op":"get_chunk","scope":"tenant","id":"`+root+`"}`)
		if r.IsError {
			t.Fatalf("get_chunk: %s", r.Text)
		}
		rev := int(got["revision"].(float64))
		if _, r := docExec(t, d, dctx, fmt.Sprintf(
			`{"op":"update_chunk","scope":"tenant","id":"%s","revision":%d,"status":"%s"}`,
			root, rev, memrank.OntologyConfirmedStatus)); r.IsError {
			t.Fatalf("confirm: %s", r.Text)
		}
	}

	if len(scopes) == 0 {
		scopes = []string{"agent", "user", "tenant"}
	}
	ctx := tools.WithAgentName(context.Background(), "consolidator")
	// SAME TENANT as the fixture that authored the ontology ("tnt"). The reader keys the
	// ontology on the run's tenant, so a mismatch here would read an empty plane and
	// every assertion below would pass or fail for the wrong reason.
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u_alice", TenantID: "tnt"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: scopes})
	return &Memory{Store: d.Store, SqlMem: d.SqlMem}, ctx
}

func placements(t *testing.T, m *Memory, ctx context.Context, body string) []map[string]any {
	t.Helper()
	res := memExecJSON(t, m, ctx, body)
	if res.IsError {
		t.Fatalf("placement: %s", res.Text)
	}
	var out struct {
		Placements []map[string]any `json:"placements"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode %q: %v", res.Text, err)
	}
	return out.Placements
}

func TestMemoryPlacement_AnswersFromTheOperatorsDeclaration(t *testing.T) {
	m, ctx := placementFixture(t, true)
	got := placements(t, m, ctx, `{"op":"placement","scope":"user","items":[
		{"type":"service","subject":"checkout-api"},
		{"type":"person","subject":"Maria"},
		{"type":"location","subject":"Cluj-Napoca"}]}`)
	if len(got) != 3 {
		t.Fatalf("want 3 answers, got %d: %v", len(got), got)
	}
	// Declared tenant → moved. Declared user → the caller's own scope, not a move.
	// Undeclared → left alone.
	if got[0]["scope"] != "tenant" || got[0]["moved"] != true {
		t.Errorf("service should move to tenant: %v", got[0])
	}
	if got[1]["scope"] != "user" || got[1]["moved"] != false {
		t.Errorf("person declares user, which is where it was going: %v", got[1])
	}
	if got[2]["scope"] != "user" || got[2]["moved"] != false {
		t.Errorf("an undeclared type must be left alone: %v", got[2])
	}
	for i, row := range got {
		if s, _ := row["reason"].(string); s == "" {
			t.Errorf("row %d carries no reason", i)
		}
	}
}

// The op DECIDES and must never write. A placement call that created a chunk or a row
// would make asking about a fact indistinguishable from storing one.
func TestMemoryPlacement_WritesNothing(t *testing.T) {
	m, ctx := placementFixture(t, true)
	before := memExecJSON(t, m, ctx, `{"op":"list","scope":"user"}`)
	placements(t, m, ctx, `{"op":"placement","scope":"user","items":[{"type":"service","subject":"checkout-api"}]}`)
	after := memExecJSON(t, m, ctx, `{"op":"list","scope":"user"}`)
	if before.Text != after.Text {
		t.Errorf("placement changed the store.\nbefore: %s\nafter:  %s", before.Text, after.Text)
	}
}

// TestMemoryPlacement_DraftOntologyPlacesNothing: the confirm gate governs placement like
// it governs everything else the ontology says. A half-written document must not start
// moving facts between partitions.
func TestMemoryPlacement_DraftOntologyPlacesNothing(t *testing.T) {
	m, ctx := placementFixture(t, false)
	got := placements(t, m, ctx, `{"op":"placement","scope":"user","items":[{"type":"service","subject":"checkout-api"}]}`)
	if got[0]["moved"] != false || got[0]["scope"] != "user" {
		t.Errorf("a draft ontology must place nothing: %v", got[0])
	}
	if s, _ := got[0]["reason"].(string); !strings.Contains(s, "draft") {
		t.Errorf("the reason should name the draft gate, got %q", s)
	}
}

// The grant is the enable switch: declaring a scope changes nothing until the operator
// also grants the writer.
func TestMemoryPlacement_UngrantedTenantPlacesNothing(t *testing.T) {
	m, ctx := placementFixture(t, true, "agent", "user")
	got := placements(t, m, ctx, `{"op":"placement","scope":"user","items":[{"type":"service","subject":"checkout-api"}]}`)
	if got[0]["moved"] != false || got[0]["scope"] != "user" {
		t.Errorf("without the tenant grant nothing may be placed there: %v", got[0])
	}
	if s, _ := got[0]["reason"].(string); !strings.Contains(s, "not granted") {
		t.Errorf("the reason should name the missing grant, got %q", s)
	}
}

func TestMemoryPlacement_RefusesAnEmptyOrOversizedBatch(t *testing.T) {
	m, ctx := placementFixture(t, true)
	if r := memExecJSON(t, m, ctx, `{"op":"placement","scope":"user"}`); !r.IsError {
		t.Error("an empty batch should be refused with guidance, not answered")
	}
	var b strings.Builder
	b.WriteString(`{"op":"placement","scope":"user","items":[`)
	for i := 0; i < placementBatchMax+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"service","subject":"s"}`)
	}
	b.WriteString(`]}`)
	if r := memExecJSON(t, m, ctx, b.String()); !r.IsError {
		t.Errorf("a batch over %d should be refused", placementBatchMax)
	}
}

// The self guard, end to end through the op: a live store types the end-user's own
// entity as `location`, and that is what carries their home city.
func TestMemoryPlacement_NeverPlacesTheRunsOwnUser(t *testing.T) {
	m, ctx := placementFixture(t, true)
	got := placements(t, m, ctx, `{"op":"placement","scope":"user","items":[
		{"type":"service","subject":"user"},
		{"type":"service","subject":"u_alice"}]}`)
	for i, row := range got {
		if row["moved"] == true {
			t.Errorf("row %d placed a fact about the run's own user in %v: %v", i, row["scope"], row)
		}
	}
}
