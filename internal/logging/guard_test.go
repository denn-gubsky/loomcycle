package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// convertedPackages are the packages whose logging moved to log/slog. A
// non-test file in one of them may no longer call log.Printf / Print /
// Println: a new call would silently opt out of levels and structured
// attributes. Paths are relative to the repo root; subdirectories are
// separate packages and must be listed on their own.
var convertedPackages = []string{
	"internal/awaited",
	"internal/channelhooks",
	"internal/channels",
	"internal/concurrency",
	"internal/coord",
	"internal/pause",
	"internal/runner",
	"internal/runstate",
}

// stdlibLogCalls returns the positions of log.Printf/Print/Println calls in f,
// resolving the local name the file gives the "log" import (an aliased or
// absent import is handled; a method on some other value named log is not
// a match unless that name is the import's).
func stdlibLogCalls(fset *token.FileSet, f *ast.File) []string {
	local := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "log" {
			local = "log"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
	}
	if local == "" || local == "_" {
		return nil
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != local {
			return true
		}
		switch sel.Sel.Name {
		case "Printf", "Print", "Println":
			hits = append(hits, fset.Position(call.Pos()).String())
		}
		return true
	})
	return hits
}

func TestConvertedPackages_DoNotCallStdlibLog(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range convertedPackages {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("%s: %v (renamed? update convertedPackages)", pkg, err)
			continue
		}
		fset := token.NewFileSet()
		scanned := 0
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("%s/%s: %v", pkg, name, err)
			}
			scanned++
			for _, pos := range stdlibLogCalls(fset, f) {
				t.Errorf("%s: stdlib log call in a slog-converted package; use slog (Info/Warn/Error, with key/value attrs)", pos)
			}
		}
		if scanned == 0 {
			t.Errorf("%s: no Go source scanned (moved? update convertedPackages)", pkg)
		}
	}
}

// The guard must catch what it guards: a direct call, an aliased import, and
// leave alone a function VALUE (a logf seam defaulting to log.Printf) and a
// method on an unrelated receiver.
func TestStdlibLogCalls_DetectsCallsNotReferences(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      int
	}{
		{"direct", `package p; import "log"; func f() { log.Printf("x"); log.Println(); log.Print() }`, 3},
		{"aliased", `package p; import stdlog "log"; func f() { stdlog.Printf("x") }`, 1},
		{"seam value", `package p; import "log"; var logf = log.Printf; func f() { logf("x") }`, 0},
		{"other receiver", `package p; type l struct{}; func (l) Printf(string) {}; func f() { var log l; log.Printf("x") }`, 0},
		{"fatal is not covered", `package p; import "log"; func f() { log.Fatalf("x") }`, 0},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, tc.name+".go", tc.src, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := len(stdlibLogCalls(fset, f)); got != tc.want {
			t.Errorf("%s: %d hits, want %d", tc.name, got, tc.want)
		}
	}
}
