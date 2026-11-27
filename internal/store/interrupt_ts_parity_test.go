package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestInterruptStatusTSParity_NamesTheStatusesTheServerWrites pins the TS
// adapter's hand-written InterruptStatus union to the InterruptStatus*
// constants, read from this package's source rather than listed here. The
// union named statuses the server never wrote ("answered", "expired") and
// missed three it does (RFC HG §3.4).
func TestInterruptStatusTSParity_NamesTheStatusesTheServerWrites(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "store.go", nil, 0)
	if err != nil {
		t.Fatalf("parse store.go: %v", err)
	}
	var server []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, "InterruptStatus") || i >= len(vs.Values) {
				continue
			}
			if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				v, _ := strconv.Unquote(lit.Value)
				server = append(server, v)
			}
		}
		return true
	})
	if len(server) < 4 {
		t.Fatalf("found %d InterruptStatus constants (%v) — the check has stopped checking", len(server), server)
	}

	src, err := os.ReadFile("../../adapters/ts/src/types.ts")
	if err != nil {
		t.Fatalf("read the TS adapter's types: %v", err)
	}
	m := regexp.MustCompile(`export type InterruptStatus\s*=([^;]+);`).FindSubmatch(src)
	if m == nil {
		t.Fatal("adapters/ts/src/types.ts declares no InterruptStatus")
	}
	var ts []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(m[1], -1) {
		ts = append(ts, string(q[1]))
	}
	slices.Sort(server)
	slices.Sort(ts)
	if !slices.Equal(server, ts) {
		t.Errorf("TS InterruptStatus = %v, the server writes %v", ts, server)
	}
}
