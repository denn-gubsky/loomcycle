package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// TestRunConfigCensus_EveryLoopRunningPathPersistsIt is a WRITER CENSUS, not a
// behaviour test.
//
// The run's configuration is merged independently in each run-creation path and
// written to the runs row through that path's own store.RunIdentity literal.
// Nothing forces a FIFTH path to do the same: it would compile, it would run,
// and its runs would resume on the agent definition — the exact bug this change
// exists to fix, reintroduced silently for one surface.
//
// The rule is deliberately scoped by what the function DOES rather than by a
// list of names: a function that builds both a store.RunIdentity and a
// loop.RunOptions is starting a run that can pause, so its identity must carry
// the record. A run row that is pure bookkeeping — a team walk's own row, a
// replay's seed run — never enters a loop and has no configuration to restore,
// and is exempt without needing to be named here.
func TestRunConfigCensus_EveryLoopRunningPathPersistsIt(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	type site struct {
		fn       string
		pos      string
		hasField bool
	}
	var runStarters []site

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}
				var identities []*ast.CompositeLit
				buildsRunOptions := false
				ast.Inspect(fn.Body, func(m ast.Node) bool {
					lit, ok := m.(*ast.CompositeLit)
					if !ok {
						return true
					}
					switch selectorName(lit.Type) {
					case "store.RunIdentity":
						identities = append(identities, lit)
					case "loop.RunOptions":
						buildsRunOptions = true
					}
					return true
				})
				if !buildsRunOptions {
					return true
				}
				for _, lit := range identities {
					runStarters = append(runStarters, site{
						fn:       fn.Name.Name,
						pos:      fset.Position(lit.Pos()).String(),
						hasField: hasKey(lit, "RunConfig"),
					})
				}
				return true
			})
		}
	}

	// Non-vacuity: this guard is worthless if it matches nothing, which is how
	// it would rot the first time a path is renamed or restructured.
	if len(runStarters) < 4 {
		t.Fatalf("census found only %d run-starting identity literals; it has stopped "+
			"matching the code it is supposed to guard", len(runStarters))
	}

	var missing []string
	for _, s := range runStarters {
		if !s.hasField {
			missing = append(missing, s.fn+" at "+s.pos)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these run-starting paths build a loop.RunOptions but persist no "+
			"RunConfig on the run row, so their runs will resume on the agent "+
			"definition instead of their own settings:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// selectorName renders a composite literal's type as "pkg.Name", or "" for
// anything else (a map, a slice, an unqualified local type).
func selectorName(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}

// hasKey reports whether a keyed composite literal sets the named field.
func hasKey(lit *ast.CompositeLit, field string) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == field {
			return true
		}
	}
	return false
}
