package sqlite

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestMigrate_NoIndexInTheTablesPassNamesAnAddedColumn makes an invariant that was
// only a comment into something that fails.
//
// migrate() runs the tables pass before the column adds, so an index in the tables
// pass that names a column only an ALTER adds fails on every database old enough to
// lack the column — three indexes on `memory` did exactly that and broke every sqlite
// upgrade from before RFC CL. It survived review and the whole suite because a FRESH
// database gets those columns from CREATE TABLE, so nothing that starts from an empty
// file can see the problem. TestMigrate_OpensADBWhoseMemoryTablePredates... covers the
// `memory` table concretely; this covers every other table, including ones nobody has
// written a legacy fixture for.
//
// It reads the statement lists migrate executes (migrationPlan), not the source, so it
// holds wherever a section's statements live.
func TestMigrate_NoIndexInTheTablesPassNamesAnAddedColumn(t *testing.T) {
	plan := migrationPlan()

	// Columns that only an upgraded DB gets from an ALTER, per table.
	added := map[string]map[string]bool{}
	alterRe := regexp.MustCompile(`ALTER TABLE (\w+) ADD COLUMN (\w+)`)
	for _, q := range plan.columns {
		for _, m := range alterRe.FindAllStringSubmatch(q, -1) {
			if added[m[1]] == nil {
				added[m[1]] = map[string]bool{}
			}
			added[m[1]][m[2]] = true
		}
	}
	if len(added) == 0 {
		t.Fatal("found no ALTER TABLE ... ADD COLUMN statements — this test is now vacuous")
	}

	idxRe := regexp.MustCompile(`(?s)CREATE (?:UNIQUE )?INDEX IF NOT EXISTS (\w+) ON (\w+)\(([^)]*)\)(.*)`)
	wordRe := regexp.MustCompile(`\w+`)

	var offenders []string
	indexes := 0
	for _, q := range plan.tables {
		m := idxRe.FindStringSubmatch(q)
		if m == nil {
			continue
		}
		indexes++
		name, table, cols, tail := m[1], m[2], m[3], m[4]
		for _, ref := range wordRe.FindAllString(cols+" "+tail, -1) {
			if added[table][ref] {
				offenders = append(offenders,
					name+" on "+table+" references "+ref+", which is added by ALTER")
				break
			}
		}
	}
	if indexes == 0 {
		t.Fatal("found no CREATE INDEX in the tables pass — this test is now vacuous")
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("these indexes are in the tables pass but name a column an upgraded DB "+
			"only gets from the column-add pass, so migrate() fails on any database old enough to "+
			"lack it. Move them into the indexes pass, which runs after the ALTERs:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
