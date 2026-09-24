package help

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tool article and its operation articles load from tools/, take their name,
// tool and op from where they are stored, and stay out of the topic index.
func TestLoadSet_ToolArticlesLoadFromToolsDir(t *testing.T) {
	set, err := LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	art, ok := set.ToolArticle("Path")
	if !ok {
		t.Fatal("tool article Path not loaded")
	}
	if art.Tool != "Path" || art.Op != "" {
		t.Errorf("Path article: Tool=%q Op=%q, want Path/\"\"", art.Tool, art.Op)
	}
	ls, ok := set.Get("Path/ls")
	if !ok {
		t.Fatal("operation article Path/ls not loaded")
	}
	if ls.Tool != "Path" || ls.Op != "ls" || !ls.IsOpArticle() {
		t.Errorf("Path/ls: Tool=%q Op=%q, want Path/ls", ls.Tool, ls.Op)
	}
	var ops []string
	for _, o := range set.OpsOf("Path") {
		ops = append(ops, o.Op)
	}
	if got, want := strings.Join(ops, ","), "ls,mkdir,mv,resolve,rm,stat"; got != want {
		t.Errorf("OpsOf(Path) = %s, want %s", got, want)
	}
	for _, n := range set.IndexNames() {
		if strings.Contains(n, "/") {
			t.Errorf("index lists operation article %q; operation articles are reached through their tool article", n)
		}
	}
	if !contains(set.IndexNames(), "Path") {
		t.Error("index is missing the tool article Path")
	}
}

// A model writes an operation the way it writes the call: Memory.recall,
// "Memory recall". Each spelling reaches the article.
func TestGet_OperationArticleAcceptsCallSpellings(t *testing.T) {
	set, err := LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	for _, q := range []string{"Path/ls", "path/ls", "Path.ls", "Path ls", "  PATH.LS "} {
		got, ok := set.Get(q)
		if !ok || got.Name != "Path/ls" {
			t.Errorf("Get(%q) = %v, %v; want Path/ls", q, got, ok)
		}
	}
}

// ToolArticle is exact: a feature topic that shares a word with a tool is not
// that tool's article, and case matters because tool names are identifiers.
func TestToolArticle_IsExactAndIgnoresFeatureTopics(t *testing.T) {
	set, err := LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if _, ok := set.ToolArticle("path"); ok {
		t.Error("ToolArticle(\"path\") resolved; tool names are case-sensitive")
	}
	// `skills` is a feature topic aliased as `skill`; it is not the Skill tool's article.
	if _, ok := set.ToolArticle("Skill"); ok {
		t.Error("ToolArticle(\"Skill\") resolved to a feature topic")
	}
	if _, ok := set.ToolArticle("Path/ls"); ok {
		t.Error("ToolArticle returned an operation article")
	}
}

// Every bundled topic passes the authoring rules: no design-doc citations, and
// a call example wherever a model lands to learn a call.
func TestBundledCorpus_PassesLint(t *testing.T) {
	set, err := LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	for _, topic := range set.All() {
		for _, p := range set.Lint(topic) {
			t.Errorf("%s: %s", topic.Name, p)
		}
	}
}

// Lint is only worth trusting if each rule can fail. One broken article per rule.
func TestLint_EachRuleCatchesItsViolation(t *testing.T) {
	set := &Set{topics: map[string]*Topic{}}
	cases := []struct {
		name  string
		topic *Topic
		want  string
	}{
		{"op article without example",
			&Topic{Name: "X/get", Tool: "X", Op: "get", Content: "Gets a thing."},
			"no call example"},
		{"example calls another op",
			&Topic{Name: "X/get", Tool: "X", Op: "get", Content: "## Examples\n\n```json\n{\"op\": \"set\"}\n```\n"},
			`calls op "set"`},
		{"op-less tool article without example",
			&Topic{Name: "X", Tool: "X", Content: "A tool with no operations."},
			"no call example"},
		{"design-doc citation",
			&Topic{Name: "topic", Content: "As designed in RFC AK, documents are chunked."},
			"cites a design document"},
		{"example that is not JSON",
			&Topic{Name: "X/get", Tool: "X", Op: "get", Content: "## Examples\n\n```json\n{op: get}\n```\n"},
			"not a JSON object"},
	}
	for _, c := range cases {
		probs := set.Lint(c.topic)
		if !anyContains(probs, c.want) {
			t.Errorf("%s: Lint = %q, want a problem containing %q", c.name, probs, c.want)
		}
	}
}

// Only calls count as examples: a `json result` fence is output, a fence outside
// an Examples section is prose, and `tool=` names the tool for a topic that is
// not itself a tool article.
func TestExamples_OnlyCallFencesInExamplesSections(t *testing.T) {
	topic := &Topic{Name: "scopes", Content: strings.Join([]string{
		"```json",
		`{"op": "not-an-example"}`,
		"```",
		"## Examples — user scope",
		"```json tool=Memory",
		`{"op": "get", "scope": "user", "key": "k"}`,
		"```",
		"```json result",
		`{"value": 1}`,
		"```",
		"## Afterword",
		"```json tool=Memory",
		`{"op": "also-not"}`,
		"```",
	}, "\n")}
	exs, err := topic.Examples()
	if err != nil {
		t.Fatalf("Examples: %v", err)
	}
	if len(exs) != 1 || exs[0].Tool != "Memory" || !strings.Contains(string(exs[0].Input), `"get"`) {
		t.Fatalf("Examples = %+v, want exactly the Memory get call", exs)
	}
	if exs[0].Line != 5 {
		t.Errorf("Line = %d, want 5", exs[0].Line)
	}
}

// An operator may document an MCP tool: tools/<mcp tool name>.md and its
// operation articles load from LOOMCYCLE_HELP_ROOT, a symlink among them is
// refused, and an article that breaks the authoring rules still loads, with a
// warning in the log.
func TestLoadSet_OperatorToolArticles(t *testing.T) {
	dir := t.TempDir()
	tools := filepath.Join(dir, "tools", "mcp__jobs__search")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "tools", rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mcp__jobs__search.md", "---\nname: mcp__jobs__search\ndescription: x\n---\nSearch jobs.\n")
	write("mcp__jobs__search/by_title.md", "---\nname: mcp__jobs__search/by_title\ndescription: x\n---\nNo example here.\n")
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("---\nname: mcp__jobs__search/leak\ndescription: x\n---\nsecret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(tools, "leak.md")); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if a, ok := set.ToolArticle("mcp__jobs__search"); !ok || a.Source != "filesystem" {
		t.Fatalf("operator tool article not loaded: %v %v", a, ok)
	}
	if _, ok := set.Get("mcp__jobs__search/by_title"); !ok {
		t.Error("operator operation article not loaded")
	}
	if _, ok := set.Get("mcp__jobs__search/leak"); ok {
		t.Error("symlinked operation article was loaded")
	}
	if !strings.Contains(logs.String(), "no call example") {
		t.Errorf("no lint warning logged for the example-less article; log:\n%s", logs.String())
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func anyContains(xs []string, sub string) bool {
	for _, v := range xs {
		if strings.Contains(v, sub) {
			return true
		}
	}
	return false
}
