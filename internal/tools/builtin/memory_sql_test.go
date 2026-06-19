package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// sqlMemoryFixture builds a Memory tool with a wired SQL Memory manager and
// returns a base ctx with a tenant + agent name + user id but NO sql_scopes
// (so the default-deny path is the starting point — tests opt in).
func sqlMemoryFixture(t *testing.T) (*Memory, *sqlmem.Manager, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	mgr, err := sqlmem.New(sqlmem.Config{
		Root:               t.TempDir(),
		StatementTimeoutMS: 30000,
		MaxRows:            10000,
	})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	tool := &Memory{Store: s, SqlMem: mgr}
	ctx := tools.WithAgentName(context.Background(), "qa-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID:   "alice",
		TenantID: "tenantA",
		AgentID:  "a_test",
	})
	cleanup := func() {
		_ = mgr.Close()
		_ = s.Close()
	}
	return tool, mgr, ctx, cleanup
}

// withSqlScopes attaches an sql_scopes ACL to ctx.
func withSqlScopes(ctx context.Context, scopes ...string) context.Context {
	return tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{AllowedScopes: scopes})
}

// TestMemorySQL_DefaultDenyWithoutScopes is the default-deny regression: a
// Memory tool with SqlMem wired but NO sql_scopes refuses sql_exec.
func TestMemorySQL_DefaultDenyWithoutScopes(t *testing.T) {
	tool, _, ctx, cleanup := sqlMemoryFixture(t)
	defer cleanup()
	// No WithSqlMemPolicy → empty AllowedScopes → default-deny.
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"sql_exec","scope":"agent","statement":"CREATE TABLE t (x INT)"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("sql_exec without sql_scopes should be an error; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "no sql_scopes") {
		t.Errorf("error should explain the default-deny: %s", res.Text)
	}
}

// TestMemorySQL_ExplicitTransactionThroughTool drives the Phase-3a ops through
// the tool's Execute: begin→insert→rollback discards, begin→insert→commit
// persists (so sql_exec correctly routes onto the open txn), and a double begin
// errors.
func TestMemorySQL_ExplicitTransactionThroughTool(t *testing.T) {
	tool, _, ctx, cleanup := sqlMemoryFixture(t)
	defer cleanup()
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", TenantID: "tenantA", AgentID: "a_test", RootRunID: "run-1"})
	ctx = withSqlScopes(ctx, "agent")

	exec := func(payload string) {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(payload))
		if err != nil {
			t.Fatalf("execute %s: %v", payload, err)
		}
		if res.IsError {
			t.Fatalf("op errored: %s -> %s", payload, res.Text)
		}
	}
	count := func() int {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"agent","statement":"SELECT count(*) FROM t"}`))
		if err != nil || res.IsError {
			t.Fatalf("count query: err=%v %s", err, res.Text)
		}
		var out struct {
			Rows [][]any `json:"rows"`
		}
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("decode count: %v (%s)", err, res.Text)
		}
		n, _ := out.Rows[0][0].(float64)
		return int(n)
	}

	exec(`{"op":"sql_exec","scope":"agent","statement":"CREATE TABLE t (x INT)"}`)

	// begin → insert → rollback ⇒ 0 rows
	exec(`{"op":"sql_begin","scope":"agent"}`)
	exec(`{"op":"sql_exec","scope":"agent","statement":"INSERT INTO t VALUES (1)"}`)
	exec(`{"op":"sql_rollback","scope":"agent"}`)
	if c := count(); c != 0 {
		t.Fatalf("after rollback count=%d, want 0", c)
	}

	// begin → insert → commit ⇒ 1 row (sql_exec routed onto the open txn)
	exec(`{"op":"sql_begin","scope":"agent"}`)
	exec(`{"op":"sql_exec","scope":"agent","statement":"INSERT INTO t VALUES (2)"}`)
	exec(`{"op":"sql_commit","scope":"agent"}`)
	if c := count(); c != 1 {
		t.Fatalf("after commit count=%d, want 1", c)
	}

	// double begin errors
	exec(`{"op":"sql_begin","scope":"agent"}`)
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_begin","scope":"agent"}`)); !res.IsError {
		t.Fatal("double sql_begin should error")
	}
	exec(`{"op":"sql_rollback","scope":"agent"}`)
}

// TestMemorySQL_NotEnabledServer refuses when the manager is nil even if the
// agent is granted scopes.
func TestMemorySQL_NotEnabledServer(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()
	tool := &Memory{Store: s} // SqlMem nil
	ctx := withSqlScopes(tools.WithAgentName(context.Background(), "qa-agent"), "agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t"})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"agent","statement":"SELECT 1"}`))
	if !res.IsError || !strings.Contains(res.Text, "not enabled") {
		t.Fatalf("nil SqlMem should refuse with not-enabled; got is_error=%v %s", res.IsError, res.Text)
	}
}

// TestMemorySQL_RoundTripThroughTool exercises create + insert + select end
// to end through the Memory tool with scope=agent.
func TestMemorySQL_RoundTripThroughTool(t *testing.T) {
	tool, _, ctx, cleanup := sqlMemoryFixture(t)
	defer cleanup()
	ctx = withSqlScopes(ctx, "agent")

	exec := func(stmt string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"op": "sql_exec", "scope": "agent", "statement": stmt})
		res, err := tool.Execute(ctx, body)
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("sql_exec %q is_error: %s", stmt, res.Text)
		}
	}
	exec("CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)")
	exec("INSERT INTO notes (body) VALUES ('hello')")

	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"agent","statement":"SELECT body FROM notes"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("sql_query is_error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "hello") {
		t.Errorf("query result missing inserted row: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"truncated":false`) {
		t.Errorf("expected truncated:false: %s", res.Text)
	}
}

// TestMemorySQL_TenantIsolation asserts a table written in scope=agent under
// tenantA is invisible under tenantB (different scope file).
func TestMemorySQL_TenantIsolation(t *testing.T) {
	tool, _, ctxA, cleanup := sqlMemoryFixture(t)
	defer cleanup()
	ctxA = withSqlScopes(ctxA, "agent")

	// tenantA writes.
	if res, _ := tool.Execute(ctxA, json.RawMessage(`{"op":"sql_exec","scope":"agent","statement":"CREATE TABLE secret (s TEXT)"}`)); res.IsError {
		t.Fatalf("tenantA create: %s", res.Text)
	}
	if res, _ := tool.Execute(ctxA, json.RawMessage(`{"op":"sql_exec","scope":"agent","statement":"INSERT INTO secret VALUES ('A')"}`)); res.IsError {
		t.Fatalf("tenantA insert: %s", res.Text)
	}

	// Same agent name, DIFFERENT tenant — its own empty file; the table does
	// not exist there.
	ctxB := tools.WithAgentName(context.Background(), "qa-agent")
	ctxB = tools.WithRunIdentity(ctxB, tools.RunIdentityValue{UserID: "bob", TenantID: "tenantB"})
	ctxB = withSqlScopes(ctxB, "agent")
	res, _ := tool.Execute(ctxB, json.RawMessage(`{"op":"sql_query","scope":"agent","statement":"SELECT s FROM secret"}`))
	if !res.IsError {
		t.Fatalf("tenantB saw tenantA's table; want an error. got %s", res.Text)
	}
}

// TestMemorySQL_ScopeIsolationAgentVsUser asserts the agent and user scopes
// are distinct databases within one tenant.
func TestMemorySQL_ScopeIsolationAgentVsUser(t *testing.T) {
	tool, _, ctx, cleanup := sqlMemoryFixture(t)
	defer cleanup()
	ctx = withSqlScopes(ctx, "agent", "user")

	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_exec","scope":"agent","statement":"CREATE TABLE a (x INT)"}`)); res.IsError {
		t.Fatalf("agent create: %s", res.Text)
	}
	// The user scope has its own DB — the agent-scope table is not visible.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"user","statement":"SELECT x FROM a"}`))
	if !res.IsError {
		t.Fatalf("user scope saw the agent-scope table; want an error. got %s", res.Text)
	}
}

// TestMemorySQL_RunScopeRequiresActiveRun asserts scope=run refuses when no
// run id is on the context, and succeeds when one is present.
func TestMemorySQL_RunScopeRequiresActiveRun(t *testing.T) {
	tool, _, ctx, cleanup := sqlMemoryFixture(t)
	defer cleanup()
	ctx = withSqlScopes(ctx, "run")

	// No RunID on ctx → refuse.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_exec","scope":"run","statement":"CREATE TABLE t (x INT)"}`))
	if !res.IsError || !strings.Contains(res.Text, "active run") {
		t.Fatalf("scope=run without a run id should refuse; got is_error=%v %s", res.IsError, res.Text)
	}

	// With a RunID → succeed.
	ctxRun := tools.WithRunID(ctx, "run-xyz")
	if res, _ := tool.Execute(ctxRun, json.RawMessage(`{"op":"sql_exec","scope":"run","statement":"CREATE TABLE t (x INT)"}`)); res.IsError {
		t.Fatalf("scope=run with a run id should succeed; got %s", res.Text)
	}
}

// TestMemorySQL_RunScopeEphemeralLifecycle asserts a run-scope write is
// readable within the run, then gone after DropRunScope (manager-level drop,
// mirroring what the HTTP finish-path calls).
func TestMemorySQL_RunScopeEphemeralLifecycle(t *testing.T) {
	tool, mgr, ctx, cleanup := sqlMemoryFixture(t)
	defer cleanup()
	ctx = withSqlScopes(ctx, "run")
	// The run scope keys off RootRunID (the tree root) — set it so the drop
	// targets the same id the server's run-completion path would.
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID: "alice", TenantID: "tenantA", RootRunID: "run-ephemeral-1",
	})
	ctx = withSqlScopes(ctx, "run")

	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_exec","scope":"run","statement":"CREATE TABLE scratch (x INT)"}`)); res.IsError {
		t.Fatalf("run create: %s", res.Text)
	}
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_exec","scope":"run","statement":"INSERT INTO scratch VALUES (1)"}`)); res.IsError {
		t.Fatalf("run insert: %s", res.Text)
	}
	// Readable within the run.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"run","statement":"SELECT x FROM scratch"}`))
	if res.IsError || !strings.Contains(res.Text, `"rows":[[1]]`) {
		t.Fatalf("run read-after-write failed: is_error=%v %s", res.IsError, res.Text)
	}

	// Drop the run scope (what purgeEphemeral... calls at run completion).
	removed, err := mgr.DropRunScope("run-ephemeral-1")
	if err != nil {
		t.Fatalf("DropRunScope: %v", err)
	}
	if !removed {
		t.Fatal("DropRunScope removed=false, want true")
	}
	// The table is gone — a fresh file is opened on the next access.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"run","statement":"SELECT x FROM scratch"}`))
	if !res.IsError {
		t.Fatalf("scratch table should be gone after drop; got %s", res.Text)
	}
}

// TestMemorySQL_QueryRefusesWrite asserts sql_query refuses a write attempt
// (the read-only floor is enforced before the driver).
func TestMemorySQL_QueryRefusesWrite(t *testing.T) {
	tool, _, ctx, cleanup := sqlMemoryFixture(t)
	defer cleanup()
	ctx = withSqlScopes(ctx, "agent")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"agent","statement":"DELETE FROM x"}`))
	if !res.IsError {
		t.Fatalf("sql_query of a DELETE should refuse; got %s", res.Text)
	}
}

// TestMemorySQL_ScopeNotInACLRefused asserts a scope outside the agent's ACL
// is refused even when the subsystem is enabled.
func TestMemorySQL_ScopeNotInACLRefused(t *testing.T) {
	tool, _, ctx, cleanup := sqlMemoryFixture(t)
	defer cleanup()
	ctx = withSqlScopes(ctx, "agent") // user/run NOT granted

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"user","statement":"SELECT 1"}`))
	if !res.IsError || !strings.Contains(res.Text, "not in this agent's sql_scopes") {
		t.Fatalf("scope=user outside the ACL should refuse; got is_error=%v %s", res.IsError, res.Text)
	}
}
