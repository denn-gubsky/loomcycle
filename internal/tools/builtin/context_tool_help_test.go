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

// op=doc returns how to call the tool — its article and operation topics — and
// not only the description and schema the model already holds.
func TestContextDoc_ReturnsTheToolsArticle(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	c := &Context{Help: set, Tools: []tools.Tool{&Path{}}}
	ctx := tools.WithAgentTools(context.Background(), []string{"Path", "Context"})
	out, res := callContext(t, c, ctx, `{"op":"doc","name":"Path"}`)
	if res.IsError {
		t.Fatalf("op=doc failed: %s", res.Text)
	}
	if art, _ := out["help"].(string); !strings.Contains(art, "## Operations") {
		t.Errorf("help = %.80q…, want the Path tool article", out["help"])
	}
	if got := joinAny(out["operation_topics"]); !strings.HasPrefix(got, "Path/ls,") {
		t.Errorf("operation_topics = %s", got)
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

// Every operation article yields a correct call for its own tool, so every
// failed call on a documented operation can carry one. Derived from the loaded
// corpus: a new article without a usable example fails here.
func TestContextHelpExample_EveryOperationArticleHasOne(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	c := &Context{Help: set}
	n := 0
	for _, topic := range set.All() {
		if !topic.IsOpArticle() {
			continue
		}
		n++
		ex, ok := c.HelpExample(topic.Name)
		if !ok {
			t.Errorf("%s: no example for the error to carry", topic.Name)
			continue
		}
		var in struct {
			Op string `json:"op"`
		}
		if err := json.Unmarshal([]byte(ex), &in); err != nil || in.Op != topic.Op {
			t.Errorf("%s: example %s does not call op %q", topic.Name, ex, topic.Op)
		}
		if strings.Contains(ex, "\n") {
			t.Errorf("%s: example is not compacted to one line", topic.Name)
		}
	}
	if n == 0 {
		t.Fatal("no operation articles loaded; the check asserts nothing")
	}
}

// The crossing: a real failed call through a real dispatcher, with the real
// Context tool and corpus, carries that operation's example.
func TestDispatcher_FailedPathCallCarriesTheArticlesExample(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	ctxTool := &Context{Help: set}
	want, _ := ctxTool.HelpExample("Path/mv")
	d := tools.NewDispatcher([]tools.Tool{&Path{}, ctxTool})
	res := d.Execute(context.Background(), "Path", json.RawMessage(`{"op":"mv","path":"/a"}`))
	if !res.IsError || !strings.Contains(res.Text, want) || !strings.Contains(res.Text, `"topic":"Path/mv"`) {
		t.Errorf("failed Path mv result = %q, want the Path/mv example %s", res.Text, want)
	}
}
