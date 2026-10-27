package builtin

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// memoryReadOps are the ops that change nothing in a scope. With
// memoryWriteOps they must cover the schema's op enum exactly.
var memoryReadOps = []string{"get", "list", "search", "recall", "placement", "cursor_get", "cursor_scan", "pending_drain",
	"sql_query", "sql_commit", "sql_rollback"}

// A new op must be classified, or a read-only scope would let it through
// unexamined: every op the schema offers is a write or a listed read, never
// both.
func TestMemoryOps_EveryOpIsClassified(t *testing.T) {
	var schema struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(memoryInputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	ops := schema.Properties.Op.Enum
	if len(ops) < 20 {
		t.Fatalf("read %d ops from the schema; the census is not reading the enum", len(ops))
	}
	for _, op := range ops {
		w, r := memoryWriteOps[op], slices.Contains(memoryReadOps, op)
		if w == r {
			t.Errorf("op %q: write=%v read=%v — classify it exactly once", op, w, r)
		}
	}
	for op := range memoryWriteOps {
		if !slices.Contains(ops, op) {
			t.Errorf("memoryWriteOps names %q, which the schema does not offer", op)
		}
	}
}

// op=permissions tells the model which of its scopes it may only read, so a
// refused write is not a surprise.
func TestContextPermissions_ReportsReadOnlyScopes(t *testing.T) {
	ctx := tools.WithRunPolicy(context.Background(), tools.RunPolicy{
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}, ReadOnlyScopes: []string{"tenant"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}, ReadOnlyScopes: []string{"user"}},
	})
	out, _ := callContext(t, &Context{}, ctx, `{"op":"permissions"}`)
	mem := out["memory"].(map[string]any)
	if got := joinAny(mem["read_only_scopes"]); got != "tenant" {
		t.Errorf("memory.read_only_scopes = %q, want tenant", got)
	}
	if got := joinAny(mem["sql_read_only_scopes"]); got != "user" {
		t.Errorf("memory.sql_read_only_scopes = %q, want user", got)
	}
}

// Every write op on a scope the run holds read-only is refused before it
// reaches the store, naming the scope as read-only; the reads still run; and a
// write to another scope is untouched.
func TestMemory_ReadOnlyScopeRefusesEveryWriteOpAndKeepsReads(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	ctx = tools.WithRunID(ctx, "run_ro")
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user"}, ReadOnlyScopes: []string{"user"}, Consolidation: true}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user"}, ReadOnlyScopes: []string{"user"}}
	})
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"k","value":"v"}`)); res.IsError {
		t.Fatalf("a write to the writable agent scope was refused: %s", res.Text)
	}

	readOnlyRe := regexp.MustCompile(`scope "user" is read-only for this run`)
	for op := range memoryWriteOps {
		req := `{"op":"` + op + `","scope":"user","key":"k","value":"v","statement":"CREATE TABLE t (x INT)"}`
		res, _ := tool.Execute(ctx, json.RawMessage(req))
		if !res.IsError || !readOnlyRe.MatchString(res.Text) {
			t.Errorf("%s on the read-only scope: error=%v %s", op, res.IsError, res.Text)
		}
	}
	if _, err := tool.Store.MemoryGet(ctx, "", "user", "alice", "k"); err == nil {
		t.Error("the read-only scope was written")
	}
	for _, req := range []string{
		`{"op":"get","scope":"user","key":"k"}`,
		`{"op":"list","scope":"user"}`,
	} {
		if res, _ := tool.Execute(ctx, json.RawMessage(req)); res.IsError {
			t.Errorf("a read of the read-only scope was refused: %s → %s", req, res.Text)
		}
	}
	// The refusal comes from the policy alone, and each facet from its own
	// list: clearing the k/v list lets set through, clearing the SQL list
	// alone lets sql_exec past the read-only gate.
	openKV := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Memory.ReadOnlyScopes = nil })
	if res, _ := tool.Execute(openKV, json.RawMessage(`{"op":"set","scope":"user","key":"k","value":"v"}`)); res.IsError {
		t.Errorf("set on a writable user scope: %s", res.Text)
	}
	openSQL := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.SqlMem.ReadOnlyScopes = nil })
	if res, _ := tool.Execute(openSQL, json.RawMessage(`{"op":"sql_exec","scope":"user","statement":"CREATE TABLE t (x INT)"}`)); strings.Contains(res.Text, "read-only") {
		t.Errorf("sql_exec is refused by the k/v read-only list: %s", res.Text)
	}
}
