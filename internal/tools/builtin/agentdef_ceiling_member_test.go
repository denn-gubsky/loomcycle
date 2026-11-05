package builtin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// TestMemberCeiling_EveryCeilingCheckIsClassified holds the member
// classification (memberCeilingClass) to the checks checkCapabilityCeiling
// runs, read from the source: a ceiling check added without deciding what a
// member may grant through it fails here, and a row whose check is gone does
// too. "tools" (memberToolsCeiling) and the TeamDef decision check are not in
// that list and are kept by name.
func TestMemberCeiling_EveryCeilingCheckIsClassified(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "agentdef_ceiling.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]bool{"tools": true, "decisionAuthorityIssues": true}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "checkCapabilityCeiling" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			}
			if strings.HasPrefix(name, "ceil") {
				checks[name] = true
			}
			return true
		})
	}
	if len(checks) < 10 {
		t.Fatalf("found %d ceiling checks (%v); the scan is not reading checkCapabilityCeiling", len(checks), checks)
	}
	var missing, stale []string
	for c := range checks {
		if memberCeilingClass[c] == "" {
			missing = append(missing, c)
		}
	}
	for c := range memberCeilingClass {
		if !checks[c] {
			stale = append(stale, c)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("ceiling checks with no member classification: %v — decide what a member may grant through each (memberCeilingClass and the table beside it)", missing)
	}
	if len(stale) > 0 {
		t.Errorf("member classifications for checks that no longer run: %v", stale)
	}
}
