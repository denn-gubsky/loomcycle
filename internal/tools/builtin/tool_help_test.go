package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// builtinToolCensus is one zero value of every tool in this package. It is a
// list, but not one that can go stale: TestBuiltinToolCensus_IsComplete fails
// when a type in this package grows an InputSchema method without an entry here.
func builtinToolCensus() []tools.Tool {
	return []tools.Tool{
		&A2AAgentDef{}, &A2AServerCardDef{}, &AgentDef{}, &AgentTool{}, &Bash{}, &Bashbox{},
		&Channel{}, &Context{}, &CredentialDef{}, &Document{}, &DocumentSourceDef{}, &Edit{},
		&Evaluation{}, &Glob{}, &Grep{}, &HTTP{}, &History{}, &HookDef{}, &Interruption{}, &MCPServerDef{},
		&Memory{}, &MemoryBackendDef{}, &NotebookEdit{}, &OperatorTokenDef{}, &Path{}, &Read{},
		&Recall{}, &ScheduleDef{}, &SkillDef{}, &SkillTool{}, &TeamDef{}, &VolumeDef{},
		&WebFetch{}, &WebSearch{}, &WebhookDef{}, &Write{},
	}
}

func TestBuiltinToolCensus_IsComplete(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Name.Name != "InputSchema" {
					continue
				}
				typ := fn.Recv.List[0].Type
				if star, ok := typ.(*ast.StarExpr); ok {
					typ = star.X
				}
				if id, ok := typ.(*ast.Ident); ok {
					declared[id.Name] = true
				}
			}
		}
	}
	for _, tl := range builtinToolCensus() {
		delete(declared, strings.TrimPrefix(fmt.Sprintf("%T", tl), "*builtin."))
	}
	for typ := range declared {
		t.Errorf("tool type %s is not in builtinToolCensus; add it so its help articles are checked", typ)
	}
}

func censusByName(t *testing.T) map[string]tools.Tool {
	t.Helper()
	m := map[string]tools.Tool{}
	for _, tl := range builtinToolCensus() {
		m[tl.Name()] = tl
	}
	return m
}

func bundledHelp(t *testing.T) *help.Set {
	t.Helper()
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	return set
}

// A tool that has an article has one for every operation its schema offers, and
// no article for an operation it no longer has. The operations come from the
// tool's live InputSchema, the same place the model reads them.
func TestToolHelpArticles_CoverEveryOperation(t *testing.T) {
	set := bundledHelp(t)
	byName := censusByName(t)
	for name, tl := range byName {
		if _, ok := set.ToolArticle(name); !ok {
			continue
		}
		ops := tools.SchemaEnum(tl.InputSchema(), "op")
		for _, op := range ops {
			if !set.Has(name + "/" + op) {
				t.Errorf("%s has an article but no article for op %q (add tools/%s/%s.md)", name, op, name, op)
			}
		}
		offered := map[string]bool{}
		for _, op := range ops {
			offered[op] = true
		}
		for _, a := range set.OpsOf(name) {
			if !offered[a.Op] {
				t.Errorf("%s: article %s documents an op the tool does not offer", name, a.Name)
			}
		}
	}
	// Every article under tools/ belongs to a real tool: a misspelt directory
	// would otherwise be an article no pointer ever reaches.
	for _, topic := range set.All() {
		if topic.Tool != "" && byName[topic.Tool] == nil {
			t.Errorf("article %s is filed under tools/%s, which is not a builtin tool", topic.Name, topic.Tool)
		}
	}
}

// Every call example in the bundled corpus is one the tool would accept: it
// names a real tool and validates against that tool's live input schema. An
// example the tool rejects teaches the mistake it was written to prevent.
func TestToolHelpExamples_ValidateAgainstTheToolSchema(t *testing.T) {
	set := bundledHelp(t)
	byName := censusByName(t)
	n := 0
	for _, topic := range set.All() {
		exs, err := topic.Examples()
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		for _, ex := range exs {
			n++
			tl := byName[ex.Tool]
			if tl == nil {
				t.Errorf("%s line %d: example calls %q, which is not a builtin tool", topic.Name, ex.Line, ex.Tool)
				continue
			}
			var schema map[string]any
			if err := json.Unmarshal(tl.InputSchema(), &schema); err != nil {
				t.Fatalf("%s schema: %v", ex.Tool, err)
			}
			var input any
			if err := json.Unmarshal(ex.Input, &input); err != nil {
				t.Errorf("%s line %d: %v", topic.Name, ex.Line, err)
				continue
			}
			for _, p := range validateSchema(schema, input, "") {
				t.Errorf("%s line %d (%s): %s", topic.Name, ex.Line, ex.Tool, p)
			}
		}
	}
	if n == 0 {
		t.Fatal("no examples found in the bundled corpus; the check is asserting nothing")
	}
}

// Every tool whose input has a fixed set of scope values reports its grants, so
// its description and Context op=self can tell the model which it may use.
// Derived from the schema: a new scoped tool cannot ship without the report.
func TestScopedTools_EveryScopeEnumReportsGrants(t *testing.T) {
	for _, tl := range builtinToolCensus() {
		if len(tools.SchemaEnum(tl.InputSchema(), "scope")) == 0 {
			continue
		}
		if _, ok := tl.(tools.ScopedTool); !ok {
			t.Errorf("%s has a scope enum but does not implement tools.ScopedTool", tl.Name())
		}
	}
}

// The grant report IS the enforcement: a scope the report refuses is refused by
// a real call with the same reason, and a scope it grants gets past the check.
func TestScopeGrants_AgreeWithTheToolsOwnRefusal(t *testing.T) {
	ctx := tools.WithAgentName(context.Background(), "helper")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t1", UserID: "u1"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}})

	m := &Memory{}
	fields := m.ScopeGrants(ctx)
	if len(fields) != 2 || fields[1].Applies == "" {
		t.Fatalf("Memory reports %d scope fields, want the k/v and the sql_* field: %+v", len(fields), fields)
	}
	for _, g := range fields[0].Grants {
		_, _, err := m.resolveScope(ctx, g.Scope)
		if g.Granted != (err == nil) {
			t.Errorf("Memory scope %s: report granted=%v, call err=%v", g.Scope, g.Granted, err)
		}
		if err != nil && g.Reason != err.Error() {
			t.Errorf("Memory scope %s: reason %q, call says %q", g.Scope, g.Reason, err.Error())
		}
	}
	if got := grantedScopes(fields[0]); got != "user" {
		t.Errorf("Memory granted = %s, want user", got)
	}
	if got := grantedScopes(fields[1]); got != "" {
		t.Errorf("Memory sql granted = %s, want none (no sql_scopes)", got)
	}
	// Document's tenant scope needs BOTH grants; with only memory_scopes it is
	// refused, and the reason names the missing one.
	ctx2 := tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}})
	for _, g := range (&Document{}).ScopeGrants(ctx2)[0].Grants {
		if g.Scope == "tenant" && (g.Granted || !strings.Contains(g.Reason, "sql_scopes: [tenant]")) {
			t.Errorf("Document tenant with only memory_scopes: %+v, want refused naming sql_scopes", g)
		}
	}
}

func grantedScopes(f tools.ScopeField) string {
	var s []string
	for _, g := range f.Grants {
		if g.Granted {
			s = append(s, g.Scope)
		}
	}
	return strings.Join(s, ",")
}

// validateSchema checks v against the subset of JSON Schema the builtin tool
// schemas use: type, enum, properties, required, additionalProperties:false,
// items, minimum and maximum. Keywords outside that subset are ignored, which
// errs toward accepting an example rather than inventing a rule.
func validateSchema(s map[string]any, v any, at string) []string {
	var probs []string
	where := at
	if where == "" {
		where = "input"
	}
	if typ, ok := s["type"]; ok && !typeMatches(typ, v) {
		return []string{fmt.Sprintf("%s: want type %v, got %T", where, typ, v)}
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				found = true
			}
		}
		if !found {
			probs = append(probs, fmt.Sprintf("%s: %v is not one of %v", where, v, enum))
		}
	}
	if n, ok := v.(float64); ok {
		if min, ok := s["minimum"].(float64); ok && n < min {
			probs = append(probs, fmt.Sprintf("%s: %v is below the minimum %v", where, n, min))
		}
		if max, ok := s["maximum"].(float64); ok && n > max {
			probs = append(probs, fmt.Sprintf("%s: %v is above the maximum %v", where, n, max))
		}
	}
	if obj, ok := v.(map[string]any); ok {
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, present := obj[r.(string)]; !present {
					probs = append(probs, fmt.Sprintf("%s: missing required %q", where, r))
				}
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, known := props[k].(map[string]any)
			if !known {
				if ap, ok := s["additionalProperties"].(bool); ok && !ap {
					probs = append(probs, fmt.Sprintf("%s: unknown field %q", where, k))
				} else if props != nil {
					// Tools decode into a struct and silently drop what they do
					// not know. An example carrying a field the schema never
					// mentions is teaching an argument that does nothing.
					probs = append(probs, fmt.Sprintf("%s: field %q is not in the schema", where, k))
				}
				continue
			}
			probs = append(probs, validateSchema(sub, obj[k], strings.TrimPrefix(at+"."+k, "."))...)
		}
	}
	if arr, ok := v.([]any); ok {
		if items, ok := s["items"].(map[string]any); ok {
			for i, e := range arr {
				probs = append(probs, validateSchema(items, e, fmt.Sprintf("%s[%d]", where, i))...)
			}
		}
	}
	return probs
}

func typeMatches(typ any, v any) bool {
	switch t := typ.(type) {
	case []any:
		for _, x := range t {
			if typeMatches(x, v) {
				return true
			}
		}
		return false
	case string:
		switch t {
		case "object":
			_, ok := v.(map[string]any)
			return ok
		case "array":
			_, ok := v.([]any)
			return ok
		case "string":
			_, ok := v.(string)
			return ok
		case "boolean":
			_, ok := v.(bool)
			return ok
		case "number":
			_, ok := v.(float64)
			return ok
		case "integer":
			n, ok := v.(float64)
			return ok && n == float64(int64(n))
		case "null":
			return v == nil
		}
	}
	return true
}

// The validator is the part of this file most likely to assert nothing, so each
// rule it applies is shown to fail once.
func TestValidateSchema_EachRuleRejects(t *testing.T) {
	schema := map[string]any{}
	_ = json.Unmarshal([]byte(pathInputSchema), &schema)
	for _, c := range []struct{ in, want string }{
		{`{"path": "/a"}`, `missing required "op"`},
		{`{"op": "list"}`, `is not one of`},
		{`{"op": "ls", "scope": "global"}`, `is not one of`},
		{`{"op": "ls", "limit": 0}`, `below the minimum`},
		{`{"op": "ls", "recursive": "yes"}`, `want type boolean`},
		{`{"op": "ls", "dir": "/a"}`, `not in the schema`},
	} {
		var v any
		_ = json.Unmarshal([]byte(c.in), &v)
		probs := validateSchema(schema, v, "")
		if !strings.Contains(strings.Join(probs, "; "), c.want) {
			t.Errorf("%s: problems %q, want one containing %q", c.in, probs, c.want)
		}
	}
	var ok any
	_ = json.Unmarshal([]byte(`{"op": "ls", "path": "/", "recursive": true, "limit": 10}`), &ok)
	if probs := validateSchema(schema, ok, ""); len(probs) != 0 {
		t.Errorf("valid input rejected: %q", probs)
	}
}

// A builtin tool's schema is one object. Anthropic and Gemini reject a
// top-level oneOf/anyOf/allOf and flatten it, and the flattening keeps only one
// branch's discriminator: Agent's seven ops reached those models as `spawn`
// alone. Schema-reading surfaces (Context op=guide, the help checks) cannot see
// the operations inside a combinator either.
func TestBuiltinSchemas_HaveNoTopLevelCombinator(t *testing.T) {
	for _, tl := range builtinToolCensus() {
		var s map[string]json.RawMessage
		if err := json.Unmarshal(tl.InputSchema(), &s); err != nil {
			t.Fatalf("%s schema: %v", tl.Name(), err)
		}
		for _, k := range []string{"oneOf", "anyOf", "allOf"} {
			if _, ok := s[k]; ok {
				t.Errorf("%s schema has a top-level %s; write it as one object with an op enum", tl.Name(), k)
			}
		}
	}
}

// The digest an agent reads to learn its tools lists every Agent operation.
func TestContextGuide_ListsEveryAgentOperation(t *testing.T) {
	c := &Context{Tools: []tools.Tool{&AgentTool{}}}
	ctx := tools.WithAgentTools(context.Background(), []string{"Agent"})
	res, _ := c.Execute(ctx, json.RawMessage(`{"op":"guide"}`))
	var out struct {
		Tools []struct {
			Name string   `json:"name"`
			Ops  []string `json:"ops"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || len(out.Tools) != 1 {
		t.Fatalf("guide: %s", res.Text)
	}
	if got := strings.Join(out.Tools[0].Ops, ","); got != "spawn,parallel_spawn,open,send,poll,cancel,close" {
		t.Errorf("guide lists Agent ops %q", got)
	}
}
