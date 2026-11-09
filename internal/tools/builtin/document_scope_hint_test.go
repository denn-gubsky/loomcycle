package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A tenant id read without scope reads the user scope and is not found there.
// Agents on a knowledge base took that "no such chunk" to mean the id was wrong
// and began inventing ids; the refusal now says the call passed no scope.
func TestDocument_NotFoundWithoutScopeSaysItReadTheUserScope(t *testing.T) {
	d, _, ctx := tenantDocFixture(t, "tnt")
	chunk := tenantChunk(t, d, ctx, "A quorum is half the members.")

	for op, in := range map[string]string{
		"get_chunk":    `{"op":"get_chunk","id":"` + chunk + `"}`,
		"get_document": `{"op":"get_document","path":"/norms/board"}`,
	} {
		res, err := d.Execute(ctx, json.RawMessage(in))
		if err != nil || !res.IsError {
			t.Fatalf("%s without scope: want a not-found refusal, got %v %s", op, err, res.Text)
		}
		if !strings.Contains(res.Text, `repeat the call with scope "tenant"`) {
			t.Errorf("%s: the refusal does not say the call read the user scope:\n%s", op, res.Text)
		}
	}
	// With the scope it is found, and the hint is never on a call that named one.
	if res, _ := d.Execute(ctx, json.RawMessage(`{"op":"get_chunk","scope":"tenant","id":"`+chunk+`"}`)); res.IsError {
		t.Fatalf("get_chunk with scope tenant: %s", res.Text)
	}
	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"get_chunk","scope":"user","id":"`+chunk+`"}`))
	if !res.IsError || strings.Contains(res.Text, "passed no scope") {
		t.Errorf("an explicit scope=user miss carries the hint:\n%s", res.Text)
	}
}

// An agent that may not read the tenant scope is not sent there.
func TestDocument_NotFoundWithoutScopeHasNoHintWithoutATenantGrant(t *testing.T) {
	d, _, ctx := tenantDocFixture(t, "tnt")
	chunk := tenantChunk(t, d, ctx, "A quorum is half the members.")
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}}
	})
	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"get_chunk","id":"`+chunk+`"}`))
	if !res.IsError || strings.Contains(res.Text, "passed no scope") {
		t.Errorf("want a plain not-found refusal, got:\n%s", res.Text)
	}
}
