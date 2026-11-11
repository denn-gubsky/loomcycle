package otel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// attrLiteral is an attribute written with a literal key anywhere in the tree.
var attrLiteral = regexp.MustCompile(`attribute\.(?:String|Int|Int64|Bool|Float64|StringSlice|IntSlice|Int64Slice|Float64Slice|BoolSlice)\("([^"]+)"`)

// genAIKeyUse is a gen_ai key (or a key-value built on one) of the pinned
// semconv release; a trailing .Value is a value read from it, not a key used.
var genAIKeyUse = regexp.MustCompile(`genai\.(GenAI\w+)(\.Value)?`)

// TestTenantAttrPolicy_ClassifiesEveryEmittedKey holds every attribute key the
// source writes to a decision in tenantAttrPolicy: a span constant (Attr*), a
// literal key anywhere under cmd/ and internal/, and every gen_ai key of the
// pinned release. A new attribute fails here until someone decides whether a
// tenant's own export may carry it; until then the router drops it.
func TestTenantAttrPolicy_ClassifiesEveryEmittedKey(t *testing.T) {
	fset := token.NewFileSet()
	pkgFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	table, err := os.ReadFile("tenant_span.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range pkgFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Attr") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					key, _ := strconv.Unquote(lit.Value)
					if _, classified := tenantAttrPolicy[key]; !classified {
						t.Errorf("%s: %s = %q is not classified in tenantAttrPolicy (tenant_span.go)", f, name.Name, key)
					}
				}
			}
		}
		if f == "tenant_span.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range genAIKeyUse.FindAllStringSubmatch(string(src), -1) {
			if m[2] != "" {
				continue
			}
			key := m[1]
			if !strings.HasSuffix(key, "Key") {
				// A predefined key-value (GenAIOperationNameChat): its key.
				for _, fam := range []string{"GenAIOperationName", "GenAIProviderName", "GenAIOutputType", "GenAITokenType"} {
					if strings.HasPrefix(key, fam) {
						key = fam + "Key"
					}
				}
			}
			if !strings.Contains(string(table), "genai."+key+")") {
				t.Errorf("%s: genai.%s is used but genai.%s is not classified in tenantAttrPolicy", f, m[1], key)
			}
		}
	}

	root := filepath.Join("..", "..")
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
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
			for _, m := range attrLiteral.FindAllStringSubmatch(string(src), -1) {
				if _, classified := tenantAttrPolicy[m[1]]; !classified {
					t.Errorf("%s: attribute %q is not classified in tenantAttrPolicy (internal/otel/tenant_span.go)", path, m[1])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
