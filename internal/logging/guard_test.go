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
// non-test file in one of them may not import the stdlib "log" package at
// all: not to call log.Printf (a line with no level and no attributes), and
// not to hand log.Printf to a printf-style logger seam either, which is how
// a line used to slip past a check that looked only at calls. Paths are
// relative to the repo root; subdirectories are separate packages and must
// be listed on their own.
var convertedPackages = []string{
	"internal/api/a2a",
	"internal/api/grpc",
	"internal/api/http",
	"internal/api/mcp",
	"internal/api/webhook",
	"internal/awaited",
	"internal/changesub",
	"internal/channelhooks",
	"internal/channels",
	"internal/concurrency",
	"internal/coord",
	"internal/decision",
	"internal/heartbeat",
	"internal/help",
	"internal/hooks",
	"internal/lookup",
	"internal/loop",
	"internal/memory",
	"internal/memory/backends/fallback",
	"internal/memory/backends/inprocess",
	"internal/metrics",
	"internal/pause",
	"internal/providerbuild",
	"internal/providers",
	"internal/providers/anthropic",
	"internal/providers/anthropic_oauth_dev",
	"internal/providers/codejs",
	"internal/providers/deepseek",
	"internal/providers/ollama",
	"internal/providers/ratelimit",
	"internal/recipes",
	"internal/resolve",
	"internal/retention",
	"internal/runner",
	"internal/runstate",
	"internal/scheduler",
	"internal/snapshot",
	"internal/sqlmem",
	"internal/store",
	"internal/store/cdc",
	"internal/store/postgres",
	"internal/store/sqlite",
	"internal/store/sqlshared",
	"internal/store/storetest",
	"internal/teamrun",
	"internal/tools/a2a",
	"internal/tools/mcp",
	"internal/tools/mcp/http",
	"internal/tools/mcp/stdio",
	"internal/usage",
	"internal/webui",
	"internal/wiring",
}

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

func TestConvertedPackages_DoNotImportStdlibLog(t *testing.T) {
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
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("%s/%s: %v", pkg, name, err)
			}
			scanned++
			if pos := stdlibLogImport(fset, f); pos != "" {
				t.Errorf("%s: imports the stdlib log package in a slog-converted package; log through slog (Info/Warn/Error, with key/value attrs) and pass a *slog.Logger to seams", pos)
			}
		}
		if scanned == 0 {
			t.Errorf("%s: no Go source scanned (moved? update convertedPackages)", pkg)
		}
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
