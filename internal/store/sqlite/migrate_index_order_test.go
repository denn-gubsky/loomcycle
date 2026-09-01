package sqlite

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestMigrate_NoIndexInTheSchemaBlockNamesAnAlterAddedColumn makes an invariant that was
// only a comment into something that fails.
//
// migrate() already says "ALTER must precede the partial indexes that reference the new
// columns" — and three indexes on `memory` violated it anyway, which broke every sqlite
// upgrade from before RFC CL. The reason it survived review and the whole test suite is
// that a FRESH database gets those columns from CREATE TABLE, so nothing that starts from
// an empty file can see the problem. TestMigrate_OpensADBWhoseMemoryTablePredates... covers
// the `memory` table concretely; this covers every other table, including ones nobody has
// written a legacy fixture for.
//
// It reads the source, which is unusual and deliberate: the property is about the ORDER of
// two statement lists, and the cheapest honest way to check it for all tables is to look at
// them. The alternative — a legacy fixture per table — is a lot of code that still misses
// the next table someone adds.
func TestMigrate_NoIndexInTheSchemaBlockNamesAnAlterAddedColumn(t *testing.T) {
	src, err := os.ReadFile("sqlite.go")
	if err != nil {
		t.Fatalf("read sqlite.go: %v", err)
	}
	s := string(src)

	// The schema block is everything before its own execution loop; the ALTERs come after.
	parts := strings.SplitN(s, "for _, q := range stmts", 2)
	if len(parts) != 2 {
		t.Fatal("could not find the `stmts` execution loop — migrate()'s shape changed, update this test")
	}
	schemaBlock := parts[0]

	// Columns that only an upgraded DB gets from an ALTER, per table.
	added := map[string]map[string]bool{}
	for _, m := range regexp.MustCompile(`ALTER TABLE (\w+) ADD COLUMN (\w+)`).FindAllStringSubmatch(s, -1) {
		if added[m[1]] == nil {
			added[m[1]] = map[string]bool{}
		}
		added[m[1]][m[2]] = true
	}
	if len(added) == 0 {
		t.Fatal("found no ALTER TABLE ... ADD COLUMN statements — this test is now vacuous")
	}

	idxRe := regexp.MustCompile(`CREATE (?:UNIQUE )?INDEX IF NOT EXISTS (\w+) ON (\w+)\(([^)]*)\)([^` + "`" + `]*)`)
	wordRe := regexp.MustCompile(`\w+`)

	var offenders []string
	for _, m := range idxRe.FindAllStringSubmatch(schemaBlock, -1) {
		name, table, cols, tail := m[1], m[2], m[3], m[4]
		for _, ref := range wordRe.FindAllString(cols+" "+tail, -1) {
			if added[table][ref] {
				offenders = append(offenders,
					name+" on "+table+" references "+ref+", which is added by ALTER")
				break
			}
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("these indexes are in the CREATE-TABLE block but name a column an upgraded DB "+
			"only gets from the ALTER block, so migrate() fails on any database old enough to "+
			"lack it. Move them into addIndexes, which runs after the ALTERs:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
