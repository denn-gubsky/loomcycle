// Package funclen holds a ratchet over the length of the Go functions in
// internal/ and cmd/: no function grows past maxLines unless it is on the
// allowlist, and an allowlisted one never grows. See the large-function-split
// RFC (RFC GH) in the project documents.
package funclen

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

// maxLines is the length (end line - start line + 1 of the FuncDecl) a
// function may have without an allowlist entry.
const maxLines = 300

// roots are read as literals so scripts/ci/affected counts this test as a
// reader of both trees: a change anywhere under them selects it.
var roots = []string{
	filepath.Join("..", "..", "..", "internal"),
	filepath.Join("..", "..", "..", "cmd"),
}

// repoRoot turns a walked path into the repository-relative key.
var repoRoot = filepath.Join("..", "..", "..")

// skipDirs are not walked: they hold no loomcycle Go source (node_modules
// under the Web UI) or only test inputs.
var skipDirs = map[string]bool{"node_modules": true, "testdata": true, "vendor": true}

// allowed is a function this ratchet tolerates over maxLines, at no more than
// lines. kind is logic (to be split by the RFC phase named), table (a list
// that reads top to bottom and stays at its length, decision D3) or fixture
// (test data, also D3).
type allowed struct {
	lines int
	kind  string
	phase string
}

// allowlist only shrinks. A phase that splits a function deletes or lowers
// its entry in the same pull request; the test fails on an entry that is gone
// or no longer over maxLines, so a stale entry cannot keep headroom. Keys are
// "repo/relative/path.go Func" or "repo/relative/path.go Recv.Method".
// Lengths measured on origin/main (cf550c366) on 2026-11-10.
var allowlist = map[string]allowed{
	"internal/api/http/resume.go Server.resumePausedRun":                  {729, "logic", "P2"},
	"internal/store/storetest/contract.go Run":                            {743, "table", "D3"},
	"internal/tools/builtin/teamdef.go TeamDef.execRun":                   {722, "logic", "P5"},
	"internal/api/http/server.go Server.prepareSubRunValues":              {722, "logic", "P5"},
	"internal/api/http/server.go Server.routes":                           {708, "table", "D3"},
	"internal/api/mcp/tools.go baseToolDescriptors":                       {705, "table", "D3"},
	"internal/loop/stateful.go runStateful":                               {631, "logic", "P5"},
	"internal/api/http/auth_principal.go requiredScopeFor":                {465, "table", "D3"},
	"internal/tools/builtin/document_editions.go Document.reimportMD":     {463, "logic", "P5"},
	"internal/tools/builtin/document_ontology.go Document.ontologyForKey": {419, "logic", "P5"},
	"internal/tools/builtin/context.go Context.execSelf":                  {419, "logic", "P5"},
	"internal/tools/builtin/memory.go Memory.execRecall":                  {344, "logic", "P5"},
	"internal/authz/surfaces.go HTTPSurface":                              {341, "table", "D3"},
	"internal/loop/loop.go runLoop":                                       {335, "logic", "P5"},
	"cmd/loomcycle/boot_background.go boot.startStoreSweepers":            {328, "logic", "P5"},
	"internal/memory/eval/extraction_fixtures.go ExtractionFixture":       {311, "fixture", "D3"},
}

// TestFuncLength_CensusNoFunctionOverLimit walks every non-test, non-generated
// Go file under internal/ and cmd/ and fails on a function over maxLines that
// is not allowlisted, an allowlisted one longer than its recorded length, and
// an allowlist entry that no longer exists or is now within maxLines.
//
// "Census" in the name puts it in scripts/ci/census-tests.sh, which runs on
// every pull request.
func TestFuncLength_CensusNoFunctionOverLimit(t *testing.T) {
	measured := map[string]int{}
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("parse %s: %w", p, err)
			}
			// The "Code generated ... DO NOT EDIT." header (go help generate).
			if ast.IsGenerated(f) {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				n := fset.Position(fn.End()).Line - fset.Position(fn.Pos()).Line + 1
				key := rel + " " + funcName(fn)
				// Several init funcs in one file share a key; keep the longest.
				if n > measured[key] {
					measured[key] = n
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(measured) == 0 {
		t.Fatal("no functions measured: the walk found no Go source")
	}

	keys := make([]string, 0, len(measured))
	for k := range measured {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := measured[k]
		entry, listed := allowlist[k]
		switch {
		case !listed && n > maxLines:
			t.Errorf("%s is %d lines (limit %d): split it, or (only for a table) add it to the allowlist in scripts/ci/funclen with a reviewer's approval", k, n, maxLines)
		case listed && n > entry.lines:
			t.Errorf("%s grew to %d lines (allowlisted at %d, %s, %s): bring it back to %d or split it; raising the entry needs a reviewer's approval", k, n, entry.lines, entry.kind, entry.phase, entry.lines)
		case listed && n <= maxLines:
			t.Errorf("%s is now %d lines, within the limit %d: delete its allowlist entry in scripts/ci/funclen", k, n, maxLines)
		}
	}
	for k := range allowlist {
		if _, ok := measured[k]; !ok {
			t.Errorf("allowlist entry %q names no function in internal/ or cmd/: delete the entry in scripts/ci/funclen", k)
		}
	}
}

// funcName is "Func" for a function and "Recv.Method" for a method, with the
// receiver's pointer and type parameters dropped.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	switch x := typ.(type) {
	case *ast.IndexExpr:
		typ = x.X
	case *ast.IndexListExpr:
		typ = x.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
