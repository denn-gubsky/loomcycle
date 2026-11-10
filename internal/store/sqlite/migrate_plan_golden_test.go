package sqlite

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestMigrationPlan_ExecutesTheGoldenStatementSequence pins the statements
// migrate executes from its fixed lists, pass by pass and in order, to a golden
// copy taken from the single migrate function before its lists moved into
// sections. Every boot replays these lists against an existing database, so a
// statement that moved, changed or went missing changes what an upgrade does.
// The golden file goes once the move has been reviewed (large-function-split
// RFC, P1), so it does not become a second copy of the schema to maintain.
func TestMigrationPlan_ExecutesTheGoldenStatementSequence(t *testing.T) {
	want, err := os.ReadFile("testdata/migrate_statements.golden")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	got := renderMigrationPlan(migrationPlan())
	if got == string(want) {
		return
	}
	g, w := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			t.Fatalf("statement sequence differs from the golden at line %d (%s):\n got: %q\nwant: %q",
				i+1, nearestHeader(w, i), g[i], w[i])
		}
	}
	t.Fatalf("statement sequence differs from the golden in length: got %d lines, want %d", len(g), len(w))
}

// renderMigrationPlan writes each statement under a "=== <pass> <index>" header.
func renderMigrationPlan(p schemaPlan) string {
	var b strings.Builder
	for _, pass := range []struct {
		name  string
		stmts []string
	}{{"tables", p.tables}, {"columns", p.columns}, {"indexes", p.indexes}, {"fixups", p.fixups}} {
		for i, q := range pass.stmts {
			fmt.Fprintf(&b, "=== %s %d\n%s\n", pass.name, i, q)
		}
	}
	return b.String()
}

// nearestHeader names the statement a golden line belongs to.
func nearestHeader(lines []string, i int) string {
	for ; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "=== ") {
			return strings.TrimPrefix(lines[i], "=== ")
		}
	}
	return "before the first statement"
}
