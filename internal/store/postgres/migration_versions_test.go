package postgres

import (
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// TestMigrations_EachVersionHasExactlyOneUpAndOneDown: golang-migrate refuses
// a source with two files of one version, which fails every Postgres boot.
// Two pull requests can each take the next free number and both merge (0122
// did, twice), so the guard is a test on the files, not a convention.
func TestMigrations_EachVersionHasExactlyOneUpAndOneDown(t *testing.T) {
	entries, err := migrationsFS.ReadDir(migrationsSubdir)
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`^(\d+)_[a-z0-9_]+\.(up|down)\.sql$`)
	type pair struct{ up, down []string }
	byVersion := map[int]*pair{}
	for _, e := range entries {
		m := name.FindStringSubmatch(e.Name())
		if m == nil {
			t.Errorf("%s: not <version>_<name>.(up|down).sql", e.Name())
			continue
		}
		v, _ := strconv.Atoi(m[1])
		p := byVersion[v]
		if p == nil {
			p = &pair{}
			byVersion[v] = p
		}
		if m[2] == "up" {
			p.up = append(p.up, e.Name())
		} else {
			p.down = append(p.down, e.Name())
		}
	}
	versions := make([]int, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	for _, v := range versions {
		if p := byVersion[v]; len(p.up) != 1 || len(p.down) != 1 {
			t.Errorf("version %04d: up %v, down %v — want exactly one of each", v, p.up, p.down)
		}
	}
}
