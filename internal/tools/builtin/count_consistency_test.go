package builtin

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestOkJSONCount_CountsTheCollectionItReturns is a STATIC check across every
// okJSONCount call site.
//
// It exists because the dangerous failure here is not a missing count but a
// WRONG one — `okJSONCount(payload, len(somethingElse))` compiles, renders, and
// lies. A count that disagrees with the rendered output is worse than no count,
// because a caller acts on it.
//
// For a call whose payload is a map literal, the length expression must refer
// to a value that actually appears in that literal. A call whose payload is a
// prebuilt variable cannot be checked this way and is reported, so the
// unchecked set stays visible rather than silently growing.
func TestOkJSONCount_CountsTheCollectionItReturns(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	checked, unchecked := 0, 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "okJSONCount" || len(call.Args) != 2 {
					return true
				}
				pos := fset.Position(call.Pos())
				where := name[strings.LastIndex(name, "/")+1:] + ":" + itoa(pos.Line)

				lenVar := lenArgIdent(call.Args[1])
				lit, isLit := payloadLiteral(call.Args[0])
				switch {
				case !isLit:
					unchecked++
				case lenVar == "":
					// A literal count (len of nothing, or a constant) is fine
					// only when the payload is visibly empty.
					if !literalHasEmptyCollection(lit) {
						t.Errorf("%s: constant count on a non-empty payload — "+
							"verify it matches what is rendered", where)
					}
					checked++
				case !literalUsesIdent(lit, lenVar):
					t.Errorf("%s: counts len(%s) but %s is not a value in the payload — "+
						"this is the shape that renders a count disagreeing with the output",
						where, lenVar, lenVar)
				default:
					checked++
				}
				return true
			})
		}
	}

	if checked == 0 {
		t.Fatal("checked zero call sites — the scan is broken, so this guard proves nothing")
	}
	t.Logf("verified %d okJSONCount sites against their payload; %d use a prebuilt payload and are not statically checkable", checked, unchecked)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// lenArgIdent returns X from len(X), or "" when the arg is not that shape.
func lenArgIdent(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "len" || len(call.Args) != 1 {
		return ""
	}
	if id, ok := call.Args[0].(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func payloadLiteral(e ast.Expr) (*ast.CompositeLit, bool) {
	lit, ok := e.(*ast.CompositeLit)
	return lit, ok
}

func literalUsesIdent(lit *ast.CompositeLit, name string) bool {
	found := false
	ast.Inspect(lit, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// literalHasEmptyCollection reports whether the payload visibly contains an
// empty slice literal — the only case where a constant count is self-evidently
// right.
func literalHasEmptyCollection(lit *ast.CompositeLit) bool {
	empty := false
	ast.Inspect(lit, func(n ast.Node) bool {
		if cl, ok := n.(*ast.CompositeLit); ok && cl != lit && len(cl.Elts) == 0 {
			empty = true
		}
		return !empty
	})
	return empty
}

// The helper's own contract: zero is recorded, and a failure never acquires one.
func TestOkJSONCount_ZeroIsRecordedAndErrorsAreNot(t *testing.T) {
	res, err := okJSONCount(map[string]any{"entries": []string{}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count == nil {
		t.Fatal("Count nil for an empty collection — that is the case this exists for")
	}
	if *res.Count != 0 {
		t.Errorf("Count = %d, want 0", *res.Count)
	}

	// And the rendered body still parses as the payload it was given.
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Text), &body); err != nil {
		t.Fatalf("payload no longer valid JSON: %v", err)
	}
	if _, ok := body["entries"]; !ok {
		t.Errorf("payload lost its collection: %s", res.Text)
	}
}

func TestOkJSONCount_EncodeFailureCarriesNoCount(t *testing.T) {
	// A channel cannot be marshalled; okJSON turns that into an error result.
	res, err := okJSONCount(map[string]any{"bad": make(chan int)}, 3)
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result from an unmarshalable payload")
	}
	if res.Count != nil {
		t.Errorf("failure carries Count=%d — it describes a result set that does not exist", *res.Count)
	}
	var _ tools.Result = res
}
