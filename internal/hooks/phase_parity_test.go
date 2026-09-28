package hooks

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// phasesDeclared reads the Phase constants from this package's source, so the
// surfaces below are checked against what the runtime actually answers rather
// than against a list kept here that would drift with them.
func phasesDeclared(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Phase" || len(vs.Values) != 1 {
				continue
			}
			if lit, ok := vs.Values[0].(*ast.BasicLit); ok {
				v, _ := strconv.Unquote(lit.Value)
				out = append(out, v)
			}
		}
	}
	if len(out) < 10 {
		t.Fatalf("read %d phases from types.go; the parse is not seeing them", len(out))
	}
	return out
}

// block returns the source from the first line containing start up to the
// first occurrence of end after it.
func block(t *testing.T, path, start, end string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("%s not readable from here: %v", path, err)
	}
	src := string(b)
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("%s: %q not found — did it move or get renamed?", path, start)
	}
	j := strings.Index(src[i:], end)
	if j < 0 {
		t.Fatalf("%s: no %q after %q", path, end, start)
	}
	return src[i : i+j]
}

// Every event the runtime's hooks answer can be named on every surface that
// names one: the TS adapter's two unions and the HookDef tool's enum. An event
// missing from one of them is one a caller there cannot use, or cannot type.
func TestHookEvents_GoAndTSAgree(t *testing.T) {
	surfaces := map[string]string{
		"TS HookPhase":      block(t, "../../adapters/ts/src/types.ts", "export type HookPhase =", ";\n"),
		"TS HookEvent":      block(t, "../../adapters/ts/src/types.ts", "export type HookEvent =", ";\n"),
		"the HookDef tool":  block(t, "../tools/builtin/hookdef.go", `"event":`, "]"),
		"def-fields' hints": block(t, "../../packages/def-fields/src/lib/hooks.ts", "export const HOOK_EVENT_HINTS", "};"),
	}
	for _, phase := range phasesDeclared(t) {
		for name, src := range surfaces {
			want := strconv.Quote(phase)
			if name == "def-fields' hints" {
				want = phase + ":"
			}
			if !strings.Contains(src, want) {
				t.Errorf("%s does not name %s", name, phase)
			}
		}
	}
}
