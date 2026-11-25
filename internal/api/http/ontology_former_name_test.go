package http

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOntologyAPI_FormerClassNameIsNotAnUndeclaredType (RFC GQ P5.1): the panel lists
// the types in use that no class declares and tells the operator to map an old name
// with @renamed_from. Once a class does, the type leaves the list; a type nobody
// mapped stays on it. The rows are written first and the directive added after.
func TestOntologyAPI_FormerClassNameIsNotAnUndeclaredType(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes\n", kids: []ontologyTestChunk{
			{title: "Document", body: "- `title` text\n", kids: []ontologyTestChunk{{title: "Rule", body: "A rule.\n"}}},
		}},
	})
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)
	exec := func(payload map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(payload)
		res, err := doc.Execute(dctx, b)
		if err != nil || res.IsError {
			t.Fatalf("%v: %v %s", payload["op"], err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	for title, typ := range map[string]string{"Access policy": "Policy", "A notice": "Notice"} {
		exec(map[string]any{"op": "create_document", "scope": "tenant", "title": title, "type": typ})
	}
	undeclared := func() string {
		var names []string
		for _, tc := range s.getOntology(t, mi.Tenant).UndeclaredTypes {
			names = append(names, tc.Type)
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	if got := undeclared(); got != "Notice,Policy" {
		t.Fatalf("undeclared types before the directive = %q, want Notice,Policy", got)
	}

	q := exec(map[string]any{"op": "query_chunks", "scope": "tenant", "sql": "SELECT id, revision FROM chunks WHERE title = 'Rule'"})
	row := q["rows"].([]any)[0].([]any)
	exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": row[0], "revision": row[1],
		"body": "A rule.\n@renamed_from Policy\n"})

	if got := undeclared(); got != "Notice" {
		t.Errorf("undeclared types after Rule was declared renamed from Policy = %q, want Notice", got)
	}
}
