package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func callContext(t *testing.T, c *Context, ctx context.Context, in string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := c.Execute(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("result is not JSON: %v\n%s", err, res.Text)
		}
	}
	return out, res
}

// A tool article comes back with its operations, derived from what is loaded;
// the topic index leaves operation articles out.
func TestContextHelp_ToolArticleListsItsOperations(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	c := &Context{Help: set}
	out, _ := callContext(t, c, context.Background(), `{"op":"help","topic":"Path"}`)
	ops, _ := out["operations"].([]any)
	if len(ops) != 6 {
		t.Fatalf("operations = %v, want the 6 Path operation articles", out["operations"])
	}
	first, _ := ops[0].(map[string]any)
	if first["topic"] != "Path/ls" || first["description"] == "" {
		t.Errorf("first operation = %v", first)
	}

	idx, _ := callContext(t, c, context.Background(), `{"op":"help"}`)
	for _, e := range idx["topics"].([]any) {
		if name := e.(map[string]any)["name"].(string); strings.Contains(name, "/") {
			t.Errorf("index lists operation article %q", name)
		}
	}
}

// Asking for an operation that has no article names the ones that do, rather
// than dumping the whole topic index.
func TestContextHelp_UnknownOperationNamesTheToolsOperations(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	_, res := callContext(t, &Context{Help: set}, context.Background(), `{"op":"help","topic":"Path.list"}`)
	if !res.IsError || !strings.Contains(res.Text, "Path documents: Path/ls, Path/mkdir") {
		t.Errorf("result = %q, want an error naming Path's operation articles", res.Text)
	}
	if strings.Contains(res.Text, "vector-memory") {
		t.Errorf("error dumped the topic index: %q", res.Text)
	}
}

// op=self reports each held scoped tool's grants from the tool's own check, and
// only for tools the run holds.
func TestContextSelf_ReportsScopeGrantsOfHeldTools(t *testing.T) {
	c := &Context{Tools: []tools.Tool{&Memory{}, &Path{}, &History{}}}
	ctx := tools.WithAgentName(context.Background(), "helper")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t1", UserID: "u1"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}})
	ctx = tools.WithAgentTools(ctx, []string{"Memory", "Path", "Context"})

	out, _ := callContext(t, c, ctx, `{"op":"self"}`)
	scopes, ok := out["scopes"].(map[string]any)
	if !ok {
		t.Fatalf("op=self has no scopes block: %v", out)
	}
	if _, held := scopes["History"]; held {
		t.Error("scopes reports History, which the run does not hold")
	}
	mem := scopes["Memory"].([]any)
	kv := mem[0].(map[string]any)
	if got := joinAny(kv["granted"]); got != "user,tenant" {
		t.Errorf("Memory granted = %s, want user,tenant", got)
	}
	if reason, _ := kv["refused"].(map[string]any)["agent"].(string); !strings.Contains(reason, "memory_scopes") {
		t.Errorf("Memory agent refusal = %q, want the memory_scopes reason", reason)
	}
	sql := mem[1].(map[string]any)
	if sql["applies"] != "sql_* ops" || joinAny(sql["granted"]) != "" {
		t.Errorf("Memory sql field = %v, want applies sql_* ops with nothing granted", sql)
	}
	if got := joinAny(scopes["Path"].([]any)[0].(map[string]any)["granted"]); got != "agent,user,tenant" {
		t.Errorf("Path granted = %s", got)
	}
}

// op=permissions reports the two grants it used to leave out.
func TestContextPermissions_IncludesSqlAndHistoryScopes(t *testing.T) {
	ctx := tools.WithSqlMemPolicy(context.Background(), tools.SqlMemPolicyValue{AllowedScopes: []string{"run"}})
	ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{Scopes: []string{"user"}})
	out, _ := callContext(t, &Context{}, ctx, `{"op":"permissions"}`)
	if got := joinAny(out["memory"].(map[string]any)["sql_scopes"]); got != "run" {
		t.Errorf("memory.sql_scopes = %s, want run", got)
	}
	if got := joinAny(out["history_scopes"]); got != "user" {
		t.Errorf("history_scopes = %s, want user", got)
	}
}

func joinAny(v any) string {
	arr, _ := v.([]any)
	s := make([]string, 0, len(arr))
	for _, x := range arr {
		s = append(s, x.(string))
	}
	return strings.Join(s, ",")
}
