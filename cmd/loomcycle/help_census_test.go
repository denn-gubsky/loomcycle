package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cli"
)

// `operator-token` (and five other subcommands) were dispatched for many
// releases without a line in `loomcycle --help`: the help text is a hand-kept
// list in another package, so nothing tied it to the dispatch switch. This
// reads the switch from source, so a new case with no help line fails here.
func TestTopLevelHelp_ListsEveryDispatchedSubcommand(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "boot_cli.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || !isOSArgs1(sw.Tag) {
			return true
		}
		for _, stmt := range sw.Body.List {
			for _, e := range stmt.(*ast.CaseClause).List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("a subcommand case is not a string literal; this census cannot read it")
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				names = append(names, name)
			}
		}
		return false
	})
	// A floor, so a refactor that moves the switch cannot leave this passing
	// over nothing.
	if len(names) < 30 {
		t.Fatalf("found %d subcommand cases in boot_cli.go, want the whole dispatch switch", len(names))
	}

	var help bytes.Buffer
	cli.PrintHelp(&help, 0)
	// The ways of asking for the help itself are not listed in it.
	skip := map[string]bool{"help": true, "-h": true, "--help": true, "-v": true}
	for _, name := range names {
		if skip[name] {
			continue
		}
		// A help line names its command first, after the indent ("  pause ...")
		// or after the binary's name ("  loomcycle version | ...").
		re := regexp.MustCompile(`(?m)^\s+(loomcycle )?` + regexp.QuoteMeta(name) + `( |$)`)
		if !re.Match(help.Bytes()) {
			t.Errorf("subcommand %q is dispatched in boot_cli.go but `loomcycle --help` has no line for it", name)
		}
	}
}

// isOSArgs1 reports whether e is the expression os.Args[1].
func isOSArgs1(e ast.Expr) bool {
	idx, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	sel, ok := idx.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Args" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	lit, ok2 := idx.Index.(*ast.BasicLit)
	return ok && ok2 && pkg.Name == "os" && lit.Value == "1"
}
