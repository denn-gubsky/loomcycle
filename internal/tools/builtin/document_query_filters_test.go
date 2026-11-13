package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC GL P2 — query_documents' title_contains, created_after and created_before,
// each combined with an existing filter.

func queryTitles(t *testing.T, d *Document, ctx context.Context, body string) []string {
	t.Helper()
	out, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	var titles []string
	for _, e := range out["documents"].([]any) {
		titles = append(titles, e.(map[string]any)["title"].(string))
	}
	sort.Strings(titles)
	return titles
}

// mkDoc creates a document in scope and returns its id and created_at.
func mkDoc(t *testing.T, d *Document, ctx context.Context, scope, title, path, typ string) (string, int64) {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":%q,"title":%q,"path":%q,"type":%q}`, scope, title, path, typ))
	if r.IsError {
		t.Fatalf("create_document %s: %s", title, r.Text)
	}
	id := out["document_id"].(string)
	// Decoded as a json.Number: a unix-nanosecond created_at does not survive a
	// float64, and the bounds below are exact.
	res, err := d.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"op":"query_documents","scope":%q,"limit":1000}`, scope)))
	if err != nil || res.IsError {
		t.Fatalf("read back %s: %v %s", title, err, res.Text)
	}
	dec := json.NewDecoder(strings.NewReader(res.Text))
	dec.UseNumber()
	var got struct {
		Documents []struct {
			DocumentID string      `json:"document_id"`
			CreatedAt  json.Number `json:"created_at"`
		} `json:"documents"`
	}
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	for _, e := range got.Documents {
		if e.DocumentID == id {
			created, err := e.CreatedAt.Int64()
			if err != nil {
				t.Fatal(err)
			}
			return id, created
		}
	}
	t.Fatalf("read back %s: not listed", title)
	return "", 0
}

func TestQueryDocuments_TitleContainsIgnoresCaseAndCombinesWithType(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		mkDoc(t, d, ctx, "user", "Widget Guide", "/a/widget-guide", "Guide")
		mkDoc(t, d, ctx, "user", "widget pricing", "/a/widget-pricing", "Proposal")
		mkDoc(t, d, ctx, "user", "Gadget guide", "/a/gadget-guide", "Guide")
		if got := queryTitles(t, d, ctx, `{"op":"query_documents","title_contains":"WIDGET"}`); strings.Join(got, "|") != "Widget Guide|widget pricing" {
			t.Fatalf("title_contains WIDGET = %v", got)
		}
		if got := queryTitles(t, d, ctx, `{"op":"query_documents","title_contains":"widget","type":"Guide"}`); strings.Join(got, "|") != "Widget Guide" {
			t.Fatalf("title_contains widget + type Guide = %v", got)
		}
	})
}

// % and _ in title_contains match themselves: unescaped, "100%" would match
// every title starting "100" and "a_b" would match "axb".
func TestQueryDocuments_TitleContainsMatchesWildcardCharactersLiterally(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		mkDoc(t, d, ctx, "user", "100% done", "/w/pct", "Note")
		mkDoc(t, d, ctx, "user", "100 percent done", "/w/word", "Note")
		mkDoc(t, d, ctx, "user", "a_b plan", "/w/underscore", "Note")
		mkDoc(t, d, ctx, "user", "axb plan", "/w/x", "Note")
		mkDoc(t, d, ctx, "user", "wow! that", "/w/bang", "Note")
		for _, c := range []struct{ q, want string }{
			{"100%", "100% done"},
			{"a_b", "a_b plan"},
			{"%", "100% done"},
			{"_", "a_b plan"},
			{"!", "wow! that"},
		} {
			body := fmt.Sprintf(`{"op":"query_documents","title_contains":%q,"under_path":"/w"}`, c.q)
			if got := queryTitles(t, d, ctx, body); strings.Join(got, "|") != c.want {
				t.Errorf("title_contains %q = %v, want [%s]", c.q, got, c.want)
			}
		}
	})
}

func TestQueryDocuments_CreatedBoundsAreExclusiveAndCombineWithUnderPath(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, t1 := mkDoc(t, d, ctx, "user", "first", "/t/first", "Note")
		_, t2 := mkDoc(t, d, ctx, "user", "second", "/t/second", "Note")
		_, t3 := mkDoc(t, d, ctx, "user", "third", "/t/third", "Note")
		mkDoc(t, d, ctx, "user", "elsewhere", "/u/elsewhere", "Note")
		if !(t1 < t2 && t2 < t3) {
			t.Fatalf("created_at not increasing: %d %d %d", t1, t2, t3)
		}
		for _, c := range []struct {
			filter, want string
		}{
			{fmt.Sprintf(`"created_after":%d`, t1), "second|third"},
			{fmt.Sprintf(`"created_before":%d`, t3), "first|second"},
			{fmt.Sprintf(`"created_after":%d,"created_before":%d`, t1, t3), "second"},
		} {
			body := `{"op":"query_documents","under_path":"/t",` + c.filter + `}`
			if got := queryTitles(t, d, ctx, body); strings.Join(got, "|") != c.want {
				t.Errorf("%s = %v, want %s", c.filter, got, c.want)
			}
		}
	})
}

// A caller whose access-control set hides some documents takes the paged path;
// the new filters apply there too.
func TestQueryDocuments_NewFiltersApplyUnderAnAccessControlSet(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	mkDoc(t, d, op, "tenant", "Widget guide", "/kb/widget-guide", "Guide")
	mkDoc(t, d, op, "tenant", "Widget secrets", "/kb/secret/widget", "Guide")
	_, tg := mkDoc(t, d, op, "tenant", "Gadget guide", "/kb/gadget-guide", "Guide")
	member := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
		Resources: []string{"document:tenant:/kb/secret/**"}}))
	if got := queryTitles(t, d, member, `{"op":"query_documents","scope":"tenant","title_contains":"widget"}`); strings.Join(got, "|") != "Widget guide" {
		t.Fatalf("member title_contains widget = %v", got)
	}
	if got := queryTitles(t, d, member, fmt.Sprintf(`{"op":"query_documents","scope":"tenant","created_before":%d}`, tg)); strings.Join(got, "|") != "Widget guide" {
		t.Fatalf("member created_before = %v", got)
	}
}

// A title with a non-ASCII capital is found by its exact text on every tier.
// Postgres folds all of Unicode; SQLite's LOWER folds ASCII only, so there a
// non-ASCII letter matches in the case given while the ASCII letters still fold.
func TestQueryDocuments_TitleContainsFindsNonASCIITitles(t *testing.T) {
	type tc struct{ q, want string }
	run := func(t *testing.T, d *Document, ctx context.Context, cases []tc) {
		mkDoc(t, d, ctx, "user", "Отчёт за год", "/n/report", "Note")
		mkDoc(t, d, ctx, "user", "École guide", "/n/ecole", "Note")
		for _, c := range cases {
			body := fmt.Sprintf(`{"op":"query_documents","title_contains":%q,"under_path":"/n"}`, c.q)
			if got := queryTitles(t, d, ctx, body); strings.Join(got, "|") != c.want {
				t.Errorf("title_contains %q = %v, want [%s]", c.q, got, c.want)
			}
		}
	}
	exact := []tc{{"Отчёт за год", "Отчёт за год"}, {"Отчёт", "Отчёт за год"}, {"École guide", "École guide"}, {"École", "École guide"}}
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		run(t, d, ctx, append(exact, tc{"ÉCOLE GUIDE", "École guide"}))
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		run(t, d, ctx, append(exact, tc{"отчёт", "Отчёт за год"}, tc{"ОТЧЁТ", "Отчёт за год"}, tc{"école", "École guide"}, tc{"ÉCOLE", "École guide"}))
	})
}
