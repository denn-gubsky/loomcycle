package providers_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The embedder census.
//
// The server's embedder is not the driver: it is the driver inside its
// provider's embedder lane (lanegate.WrapEmbedder), inside the health observer
// (providers.ObserveEmbedder). A wrapper hides whatever the value beneath it
// implements beyond providers.Embedder, so a type assertion on an embedder
// finds only what the OUTERMOST wrapper is — and is false, with no error
// anywhere, for anything a layer below implements.
//
// Embedders have no providers.As, because they have had exactly one optional
// interface: the health report, which the outermost wrapper implements. This
// test holds that count. A new assertion on an embedder value fails here until
// its author has decided where in the wrapper chain the asserted type lives
// (and, if it is not the outermost, given embedders an As like providers').

// embedderAssertions lists every assertion on an embedder value the tree may
// contain: file → the asserted type as written.
var embedderAssertions = map[string]string{
	// Implemented by *providers.ObservedEmbedder, the outermost wrapper.
	"internal/api/http/memory_changes.go": "embedderHealthReporter",
}

// embedderOperand reports whether x reads like an embedder value: a variable or
// field named embedder / Embedder / emb.
func embedderOperand(x ast.Expr) bool {
	name := ""
	switch v := x.(type) {
	case *ast.Ident:
		name = v.Name
	case *ast.SelectorExpr:
		name = v.Sel.Name
	}
	lower := strings.ToLower(name)
	return lower == "emb" || strings.HasSuffix(lower, "embedder")
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	}
	return fmt.Sprintf("%T", e)
}

// embedderAssertionsIn returns the asserted types of every assertion (and type
// switch) on an embedder-named value in f.
func embedderAssertionsIn(f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.TypeAssertExpr:
			if !embedderOperand(x.X) {
				return true
			}
			if x.Type == nil { // the x.(type) of a type switch: its cases are the types
				out = append(out, "type switch")
				return true
			}
			out = append(out, exprString(x.Type))
		}
		return true
	})
	return out
}

func TestEmbedderAssertions_AllClassified(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	found := map[string]string{}
	var violations []string
	files := 0
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if censusSkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			files++
			for _, typ := range embedderAssertionsIn(f) {
				if embedderAssertions[rel] == typ {
					found[rel] = typ
					continue
				}
				violations = append(violations, fmt.Sprintf("%s asserts an embedder to %s. The server's embedder is wrapped twice (its provider's lane, then the health observer): an assertion sees only the outermost wrapper. Decide which layer implements %s, then list the assertion in embedderAssertions.", rel, typ, typ))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 100 {
		t.Fatalf("the census parsed only %d files — it is not walking the module", files)
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
	// The list must not outlive what it lists: a stale entry would let the same
	// assertion come back somewhere else in that file unexamined.
	for rel, typ := range embedderAssertions {
		if found[rel] != typ {
			t.Errorf("embedderAssertions lists %s → %s, which is no longer there", rel, typ)
		}
	}
}

// TestEmbedderAssertions_CensusCatchesEachShape — the walk is only a guard if
// it can fail.
func TestEmbedderAssertions_CensusCatchesEachShape(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"a field", `func f(s *S) { _, _ = s.embedder.(interface{ Health() int }) }`, 1},
		{"an exported field", `func f(d *D) { _, _ = d.Embedder.(*Concrete) }`, 1},
		{"a local", `func f(emb any) { _, _ = emb.(Reporter) }`, 1},
		{"a type switch", `func f(embedder any) { switch embedder.(type) { case Reporter: } }`, 1},
		{"another value", `func f(p any) { _, _ = p.(Reporter) }`, 0},
		{"no assertion", `func f(s *S) { _ = s.embedder }`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\n"+tc.body, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := len(embedderAssertionsIn(f)); got != tc.want {
				t.Fatalf("found %d assertions, want %d", got, tc.want)
			}
		})
	}
}
