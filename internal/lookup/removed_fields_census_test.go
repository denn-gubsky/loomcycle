package lookup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// notAgentReaders are the functions that decode a stored agent definition
// without resolving it for a run, and why each need not set RemovedFields.
var notAgentReaders = map[string]string{
	"internal/api/http/connector_impl.go:ListAgents": "lists names and tools for a descriptor; no run is built from it",
}

// RFC GW P15: every site that decodes a stored agent definition into
// config.AgentDef or SubstrateAgentDef, for a run to be built from it, also
// reads the raw JSON for removed fields (config.RemovedFieldsInJSON). The
// field left every Go type, so a reader that skips the probe would run the
// definition without the capability_inert notice. The census reads the
// sources: a new reader joins it without editing this test, and must either
// probe or be listed above with a reason.
func TestStoredJSONReaders_EverySiteSetsRemovedFields(t *testing.T) {
	repo := filepath.Join("..", "..")
	found := 0
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(repo, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !strings.Contains(string(src), "AgentDef") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(repo, path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if !decodesAgentDef(fn) {
					continue
				}
				found++
				key := filepath.ToSlash(rel) + ":" + fn.Name.Name
				if _, ok := notAgentReaders[key]; ok {
					continue
				}
				if !mentions(fn, "RemovedFieldsInJSON") {
					t.Errorf("%s decodes a stored agent definition and does not set RemovedFields from config.RemovedFieldsInJSON", key)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if found < 3 {
		t.Fatalf("found %d readers; the census is not reading the sources (lookup's two tiers and the list are known)", found)
	}
}

// decodesAgentDef reports whether fn declares a variable of type
// config.AgentDef or SubstrateAgentDef and passes its address to
// json.Unmarshal.
func decodesAgentDef(fn *ast.FuncDecl) bool {
	vars := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if isAgentDefType(spec.Type) {
			for _, name := range spec.Names {
				vars[name.Name] = true
			}
		}
		return true
	})
	hit := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Unmarshal" {
			return true
		}
		if un, ok := call.Args[1].(*ast.UnaryExpr); ok && un.Op == token.AND {
			if id, ok := un.X.(*ast.Ident); ok && vars[id.Name] {
				hit = true
			}
		}
		return true
	})
	return hit
}

func isAgentDefType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "SubstrateAgentDef"
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		return ok && x.Name == "config" && t.Sel.Name == "AgentDef"
	}
	return false
}

func mentions(fn *ast.FuncDecl, name string) bool {
	hit := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			hit = true
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			hit = true
		}
		return true
	})
	return hit
}
