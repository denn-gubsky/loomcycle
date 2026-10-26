package builtin

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The memory scope id of a document row is not the SQL Memory scope id. For
// agent and user scope the two agree; for tenant scope SQL Memory carries the
// tenant (it cannot key on an empty component) while the Memory plane leaves the
// scope id empty and lets the tenant_id column carry the identity — which is
// where the Memory tool reads a tenant scope. A document row written or read at
// the SQL Memory id lands where no tenant-scope Memory read ever looks.
//
// This guard finds every call in this package that hands a `<x>.ScopeID`
// selector to a Memory-plane function: a store method named Memory*, or one of
// the Document helpers that pass their scope id straight through to one. Each
// such site must go through docMemScopeID instead, so the tenant mapping lives
// in one function. It is a syntax check, not a type check: a ScopeID field read
// off any struct and handed to the Memory plane is flagged, which is the point —
// the only struct here whose ScopeID is not already a Memory scope id is
// sqlmem.ScopeKey, and a site that has a Memory scope id names it differently.

// memoryPlaneHelpers are this package's functions that take a Memory scope id
// and pass it to the store unchanged.
var memoryPlaneHelpers = map[string]bool{
	"readBody":            true,
	"unitsOf":             true,
	"deleteUnitsOf":       true,
	"embedText":           true,
	"indexCurrent":        true,
	"publishChange":       true,
	"resolveChunkPending": true,
	"Search":              true, // the memory backend's Search(ctx, scope, scopeID, …)
}

// memoryPlaneFields are struct fields that carry a Memory scope id to a later
// store call (the fact supersede path stamps a body row at these coordinates).
var memoryPlaneFields = map[string]bool{
	"bodyScopeID": true,
}

// knownSQLScopeIDOnMemoryPlane holds the sites still allowed to hand the SQL
// Memory scope id to the Memory plane, by file and enclosing function, with the
// number of such calls in each. It was the census (45 functions) when the guard
// landed and is empty now; it may only shrink.
var knownSQLScopeIDOnMemoryPlane = map[string]int{}

func TestDocumentRows_MemoryScopeIDGoesThroughDocMemScopeID(t *testing.T) {
	sites, scanned := scanMemoryPlaneSites(t, ".")
	// Non-vacuous: this package makes well over a hundred Memory-plane calls; a
	// scan that sees a handful has stopped recognising them.
	if scanned < 100 {
		t.Fatalf("scanned only %d Memory-plane calls — the detector no longer recognises them", scanned)
	}
	for site, n := range sites {
		if want := knownSQLScopeIDOnMemoryPlane[site]; n > want {
			t.Errorf("%s hands a <x>.ScopeID to the Memory plane %d time(s) (census allows %d): "+
				"a document row's Memory scope id is docMemScopeID(mscope, key) — for tenant scope it is \"\", "+
				"not the SQL Memory scope id", site, n, want)
		}
	}
	for site, want := range knownSQLScopeIDOnMemoryPlane {
		if got := sites[site]; got < want {
			t.Errorf("census entry %s allows %d but the code has %d — shrink the entry (it may only shrink)", site, want, got)
		}
	}
}

// TestMemoryPlaneDetector_FlagsAScopeKeyScopeID proves the detector on a planted
// bypass, so a green guard cannot mean "the detector matches nothing".
func TestMemoryPlaneDetector_FlagsAScopeKeyScopeID(t *testing.T) {
	src := `package builtin
func (d *Document) planted(key sqlmem.ScopeKey) {
	_, _ = d.Store.MemoryGet(ctx, t, mscope, key.ScopeID, "doc.chunk:x")
	_, _ = d.readBody(ctx, mscope, docMemScopeID(mscope, key), "x")
	_ = factChunkRef{bodyScopeID: key.ScopeID}
	scopeID := key.ScopeID
	_ = d.Store.MemoryEmbedDelete(ctx, t, mscope, scopeID, "doc.chunk:x")
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "planted.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites := map[string]int{}
	scanned := scanFile(f, "planted.go", sites)
	if scanned != 4 {
		t.Errorf("scanned %d Memory-plane sites, want 4", scanned)
	}
	if got := sites["planted.go:(*Document).planted"]; got != 3 {
		t.Errorf("flagged %d bypasses, want 3 (the store call, the struct field, the local copy): %v", got, sites)
	}
}

func scanMemoryPlaneSites(t *testing.T, dir string) (map[string]int, int) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	fset := token.NewFileSet()
	sites := map[string]int{}
	scanned := 0
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		scanned += scanFile(f, filepath.Base(p), sites)
	}
	return sites, scanned
}

// scanFile records, per enclosing function, every Memory-plane call or field
// that receives a `<x>.ScopeID` selector, and returns how many Memory-plane
// calls and fields it examined.
func scanFile(f *ast.File, file string, sites map[string]int) int {
	scanned := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		site := file + ":" + funcName(fn)
		// A local copied from a ScopeID field (scopeID := key.ScopeID) is the
		// same value under another name, so it is flagged where it is used.
		aliases := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
				for i, rhs := range as.Rhs {
					if id, ok := as.Lhs[i].(*ast.Ident); ok && isScopeIDSelector(rhs) {
						aliases[id.Name] = true
					}
				}
			}
			return true
		})
		isBypass := func(e ast.Expr) bool {
			if isScopeIDSelector(e) {
				return true
			}
			id, ok := e.(*ast.Ident)
			return ok && aliases[id.Name]
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if !isMemoryPlaneCallee(x.Fun) {
					return true
				}
				scanned++
				for _, a := range x.Args {
					if isBypass(a) {
						sites[site]++
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && memoryPlaneFields[id.Name] {
					scanned++
					if isBypass(x.Value) {
						sites[site]++
					}
				}
			}
			return true
		})
	}
	return scanned
}

func isMemoryPlaneCallee(fun ast.Expr) bool {
	var name string
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		name = f.Sel.Name
	case *ast.Ident:
		name = f.Name
	default:
		return false
	}
	return strings.HasPrefix(name, "Memory") || memoryPlaneHelpers[name]
}

func isScopeIDSelector(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "ScopeID"
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		return fmt.Sprintf("(*%s).%s", exprName(star.X), fn.Name.Name)
	}
	return fmt.Sprintf("%s.%s", exprName(recv), fn.Name.Name)
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return fmt.Sprintf("%T", e)
}
