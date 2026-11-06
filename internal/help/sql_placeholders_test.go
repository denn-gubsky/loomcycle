package help

import (
	"strings"
	"testing"
)

// The SQL Memory articles are what an agent reads before writing SQL. They once
// said only "use ? placeholders", which is a syntax error on the PostgreSQL tier,
// so every agent that followed them failed there. Each must name both forms, and
// the pgvector example (PostgreSQL-only) must not use `?`.
func TestSQLMemoryHelp_NamesPostgresPlaceholders(t *testing.T) {
	set, err := LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	for _, name := range []string{"Memory/sql_exec", "Memory/sql_query", "sql-memory"} {
		top, ok := set.Get(name)
		if !ok {
			t.Fatalf("help article %s not loaded", name)
		}
		text := top.Description + "\n" + top.Content
		if !strings.Contains(text, "$1") {
			t.Errorf("%s: does not name the PostgreSQL placeholder form $1, $2, …", name)
		}
		if strings.Contains(text, "?::vector") {
			t.Errorf("%s: pgvector example uses ?::vector; the PostgreSQL tier needs $n::vector", name)
		}
	}
}
