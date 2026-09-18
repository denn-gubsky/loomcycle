package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A capability-scope default applied at ONE construction site is a default that
// silently does not exist at the others. `tools.MemoryPolicyValue{…}` is built
// at a dozen places across three transports, and before this every one of them
// passed `agentDef.MemoryScopes` raw — so "empty means the caller's own data"
// would have been true on the path someone tested and false everywhere else.
//
// This is the writer-census shape: the invariant is centralised, so the guard
// has to be too. A new construction site that reads a definition and skips the
// resolver fails HERE rather than quietly reverting that path to default-deny.
//
// CALLER KINDS, NOT CALL SITES. Only sites that read an AGENT DEFINITION are in
// scope. The admin and meta-tool sessions (`{"agent","user","tenant","global"}`)
// and the ontology/tenant-root maintenance paths (`{"tenant"}`) pass a fixed
// grant that belongs to the SESSION, not to an agent — there is no definition
// there to default from, and routing them through the resolver would be
// meaningless at best. They are exempt by construction: the check fires only
// when the value is a selector ending in `.MemoryScopes`.
func TestScopeDefaultCensus_EveryDefinitionReadUsesTheResolver(t *testing.T) {
	type finding struct{ file, detail string }
	var missing []finding

	roots := []string{".", "../grpc", "../mcp"}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(root, name)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel == nil || (sel.Sel.Name != "MemoryPolicyValue" && sel.Sel.Name != "SqlMemPolicyValue") {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "AllowedScopes" {
						continue
					}
					// A definition read looks like `<x>.MemoryScopes`. Anything
					// else (a literal grant) is a session policy, not agent policy.
					vs, ok := kv.Value.(*ast.SelectorExpr)
					if !ok || vs.Sel == nil || (vs.Sel.Name != "MemoryScopes" && vs.Sel.Name != "SqlScopes") {
						continue
					}
					missing = append(missing, finding{path, exprText(src, vs)})
				}
				return true
			})
		}
	}

	sort.Slice(missing, func(i, j int) bool { return missing[i].file < missing[j].file })
	if len(missing) > 0 {
		var b strings.Builder
		for _, m := range missing {
			b.WriteString("\n  " + m.file + ": AllowedScopes: " + m.detail)
		}
		t.Errorf("these sites read an agent definition's scope list RAW, bypassing "+
			"the matching Effective…Scopes resolver:%s\n\nA scope default applied at some construction "+
			"sites and not others is worse than no default: the same agent behaves "+
			"differently depending on which transport started the run. Wrap the value in "+
			"tools.EffectiveMemoryScopes / EffectiveSqlScopes(ctx, …).", b.String())
	}
}

func exprText(src []byte, e *ast.SelectorExpr) string {
	if x, ok := e.X.(*ast.Ident); ok {
		return x.Name + "." + e.Sel.Name
	}
	return e.Sel.Name
}

// Non-vacuity: the census walks real files and must actually find the
// construction sites it claims to police. A refactor that renames the type or
// moves the sites would otherwise leave this passing over nothing at all.
func TestScopeDefaultCensus_TheCensusSeesTheSites(t *testing.T) {
	seen := 0
	for _, root := range []string{".", "../grpc", "../mcp"} {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(root, e.Name()))
			if err != nil {
				continue
			}
			seen += strings.Count(string(src), "MemoryPolicyValue{") + strings.Count(string(src), "SqlMemPolicyValue{")
		}
	}
	if seen < 20 {
		t.Fatalf("the census found only %d MemoryPolicyValue construction sites; it is meant "+
			"to cover every one of them (12 memory + 13 sql at the time of writing) and has stopped doing so", seen)
	}
}
