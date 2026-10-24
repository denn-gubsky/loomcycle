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

// guardedRoots are walked in full: every non-test Go file under them must
// log through slog and may not import the stdlib "log" package at all — not
// to call log.Printf (a line with no level and no attributes), and not to
// hand log.Printf to a printf-style logger seam either. Covering whole trees,
// not a list of converted packages, means a new package is held to it from
// its first commit. bench/, test/ and deploy/builder are standalone tools
// outside these roots and keep the stdlib logger.
var guardedRoots = []string{"cmd", "internal"}

// stdlibLogImport returns the position of f's import of the stdlib "log"
// package, or "" when it has none. Any import counts — a plain one, an alias,
// even a blank one — because no use of the package is left once a package
// logs through slog.
func stdlibLogImport(fset *token.FileSet, f *ast.File) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "log" {
			return fset.Position(imp.Pos()).String()
		}
	}
	return ""
}

func TestGuardedRoots_DoNotImportStdlibLog(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, top := range guardedRoots {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if name == "testdata" || name == "node_modules" || (strings.HasPrefix(name, ".") && path != filepath.Join(root, top)) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			scanned++
			if pos := stdlibLogImport(fset, f); pos != "" {
				t.Errorf("%s: imports the stdlib log package; log through slog (Info/Warn/Error, with key/value attrs) and pass a *slog.Logger to seams", pos)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	// A wrong root would scan nothing and pass; the tree holds thousands.
	if scanned < 500 {
		t.Fatalf("scanned only %d Go files under %v — wrong root?", scanned, guardedRoots)
	}
}

// The guard must catch what it guards: a direct call, an aliased import, a
// function VALUE handed to a printf seam, a log.Fatalf — and leave alone a
// file that only has its own value named log.
func TestStdlibLogImport_CatchesEveryUse(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		{"direct", `package p; import "log"; func f() { log.Printf("x") }`, true},
		{"aliased", `package p; import stdlog "log"; func f() { stdlog.Printf("x") }`, true},
		{"seam value", `package p; import "log"; var logf = log.Printf; func f() { logf("x") }`, true},
		{"fatal", `package p; import "log"; func f() { log.Fatalf("x") }`, true},
		{"slog only", `package p; import "log/slog"; func f() { slog.Info("x") }`, false},
		{"other receiver", `package p; type l struct{}; func (l) Printf(string) {}; func f() { var log l; log.Printf("x") }`, false},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, tc.name+".go", tc.src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := stdlibLogImport(fset, f) != ""; got != tc.want {
			t.Errorf("%s: flagged %v, want %v", tc.name, got, tc.want)
		}
	}
}
