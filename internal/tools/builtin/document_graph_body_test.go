package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// graph_recall rows and the text behind them. A fact's title IS its claim, but an
// ordinary chunk the walk reaches (a norm clause, a decision section) is a bare
// heading, and a model handed only the heading needs one get_chunk per row — which
// in practice it never makes. So ordinary rows carry an excerpt of their body.

// clauseBody is longer than the default excerpt, with words throughout, so the
// cut has a word boundary to land on.
var clauseBody = strings.Repeat("Records are kept for seven years after the account closes. ", 14)

const (
	shortNoteBody = "A short body."
	factBody      = "Fact body text"
)

// bodyGraphFixture is graphFixture plus the three kinds of row a walk from Ada
// reaches in one hop: a fact (sidecar row, a body of its own), a long ordinary
// clause and a short ordinary note (no sidecar row).
func bodyGraphFixture(t *testing.T) (*Document, contextT, map[string]string) {
	t.Helper()
	d, ctx, docID, ids := graphFixture(t)
	link := func(from, to, kind string) {
		t.Helper()
		if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","document_id":"`+docID+
			`","from_id":"`+from+`","to_id":"`+to+`","kind":"`+kind+`"}`); r.IsError {
			t.Fatalf("link: %s", r.Text)
		}
	}
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+ids["Ada"]+`","title":"Ada wrote the first program","natural_key":"fact:ada-program","body":"`+factBody+`"}`)
	if r.IsError {
		t.Fatalf("upsert fact: %s", r.Text)
	}
	ids["fact"] = asStr(out["id"])
	link(ids["fact"], ids["Ada"], "about")
	for _, c := range []struct{ name, title, body string }{
		{"clause", "Clause 4.2 Retention", clauseBody},
		{"note", "Short note", shortNoteBody},
	} {
		b, _ := json.Marshal(map[string]any{"op": "create_chunk", "scope": "user", "document_id": docID,
			"parent_id": ids["Ada"], "title": c.title, "body": c.body})
		out, r := docExec(t, d, ctx, string(b))
		if r.IsError {
			t.Fatalf("create_chunk %s: %s", c.name, r.Text)
		}
		ids[c.name] = asStr(out["id"])
		link(ids["Ada"], ids[c.name], "notes")
	}
	return d, ctx, ids
}

// graphRows decodes a graph_recall response's rows keyed by title.
func graphRows(t *testing.T, text string) map[string]map[string]any {
	t.Helper()
	var out struct {
		Chunks []map[string]any `json:"chunks"`
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	rows := map[string]map[string]any{}
	for _, c := range out.Chunks {
		rows[asStr(c["title"])] = c
	}
	return rows
}

func recallFromAda(t *testing.T, d *Document, ctx contextT, ids map[string]string, extra string) string {
	t.Helper()
	_, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":["`+ids["Ada"]+`"],"hops":1`+extra+`}`)
	if r.IsError {
		t.Fatalf("graph_recall%s: %s", extra, r.Text)
	}
	return r.Text
}

func TestGraphRecall_OrdinaryChunkCarriesItsBody(t *testing.T) {
	d, ctx, ids := bodyGraphFixture(t)
	rows := graphRows(t, recallFromAda(t, d, ctx, ids, ""))

	note := rows["Short note"]
	if note == nil {
		t.Fatalf("the walk did not reach the note: %v", rows)
	}
	if got := asStr(note["body"]); got != shortNoteBody {
		t.Errorf("short note body = %q, want %q", got, shortNoteBody)
	}
	if _, cut := note["body_truncated"]; cut {
		t.Error("a body shorter than the excerpt must not be reported as truncated")
	}

	clause := rows["Clause 4.2 Retention"]
	if clause == nil {
		t.Fatalf("the walk did not reach the clause: %v", rows)
	}
	body := asStr(clause["body"])
	if n := utf8.RuneCountInString(body); n == 0 || n > 600 {
		t.Fatalf("clause excerpt is %d characters, want 1..600: %q", n, body)
	}
	if !strings.HasPrefix(clauseBody, body) {
		t.Errorf("the excerpt is not the start of the body: %q", body)
	}
	if next := clauseBody[len(body)]; next != ' ' {
		t.Errorf("the excerpt was cut mid-word (next character %q): %q", next, body)
	}
	if cut, _ := clause["body_truncated"].(bool); !cut {
		t.Error("a cut body must say so with body_truncated: true")
	}
}

func TestGraphRecall_FactRowsStayTitleOnlyByDefault(t *testing.T) {
	d, ctx, ids := bodyGraphFixture(t)
	rows := graphRows(t, recallFromAda(t, d, ctx, ids, ""))
	for _, title := range []string{"Ada wrote the first program", "Ada", "Charles"} {
		row := rows[title]
		if row == nil {
			t.Fatalf("the walk did not reach %q: %v", title, rows)
		}
		if _, has := row["body"]; has {
			t.Errorf("fact-tier row %q carries a body by default: %v", title, row)
		}
	}
	// The guard that makes this test mean something: bodies ARE on for this walk.
	if _, has := rows["Short note"]["body"]; !has {
		t.Error("the ordinary note carries no body — the default is not on, so this test asserts nothing")
	}
}

// graphGoldenBodyCharsZero is the walk below as main returned it before bodies
// existed, normalised by normaliseGraphResponse.
const graphGoldenBodyCharsZero = `chars_used=
chunks=
  {"id":"<Ada wrote the first program>","title":"Ada wrote the first program","hop":1,"via_kind":"about","via_id":"<Ada>","retired":false}
  {"id":"<Ada>","title":"Ada","type":"person","hop":0,"retired":false}
  {"id":"<Analytical Engine Co>","title":"Analytical Engine Co","type":"organization","hop":1,"via_kind":"works_at","via_id":"<Ada>","retired":false}
  {"id":"<Charles>","title":"Charles","type":"person","hop":1,"via_kind":"knows","via_id":"<Ada>","retired":false}
  {"id":"<Clause 4.2 Retention>","title":"Clause 4.2 Retention","hop":1,"via_kind":"notes","via_id":"<Ada>","retired":false}
  {"id":"<Short note>","title":"Short note","hop":1,"via_kind":"notes","via_id":"<Ada>","retired":false}
hops=1
seeded_by="ids"
seeds=1
truncated=false
`

var graphTimestamp = regexp.MustCompile(`\b\d{16,19}\b`)

// normaliseGraphResponse rewrites a response so two runs compare byte for byte:
// chunk ids become their titles, timestamps become <ts>, and the rows are sorted
// (their order follows the random ids). Each row keeps its exact bytes otherwise —
// field order, omitted fields and all — which is what "byte-identical" is about.
func normaliseGraphResponse(t *testing.T, text string, ids map[string]string) string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &top); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	titleOf := map[string]string{}
	var rows []json.RawMessage
	_ = json.Unmarshal(top["chunks"], &rows)
	for _, r := range rows {
		var m map[string]any
		_ = json.Unmarshal(r, &m)
		titleOf[asStr(m["id"])] = asStr(m["title"])
	}
	for _, id := range ids {
		if _, ok := titleOf[id]; !ok {
			titleOf[id] = id
		}
	}
	sub := func(s string) string {
		for id, title := range titleOf {
			s = strings.ReplaceAll(s, `"`+id+`"`, `"<`+title+`>"`)
		}
		return graphTimestamp.ReplaceAllString(s, "<ts>")
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	keys = append(keys, "chars_used") // absent on an unbudgeted walk; pinned as absent
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if k == "chunks" {
			lines := make([]string, 0, len(rows))
			for _, r := range rows {
				lines = append(lines, "  "+sub(string(r)))
			}
			sort.Strings(lines)
			b.WriteString("chunks=\n" + strings.Join(lines, "\n") + "\n")
			continue
		}
		b.WriteString(k + "=" + string(top[k]) + "\n")
	}
	return b.String()
}

// TestGraphRecall_BodyCharsZeroIsByteIdentical: body_chars 0 is the way back to
// the structural walk, and it must be exactly the old response — a caller that
// opted out must not pay for, or parse, anything new.
func TestGraphRecall_BodyCharsZeroIsByteIdentical(t *testing.T) {
	d, ctx, ids := bodyGraphFixture(t)
	got := normaliseGraphResponse(t, recallFromAda(t, d, ctx, ids, `,"body_chars":0`), ids)
	if got != graphGoldenBodyCharsZero {
		t.Errorf("body_chars:0 changed the response.\n got:\n%s\nwant:\n%s", got, graphGoldenBodyCharsZero)
	}
}

func TestGraphRecall_BodyCharsAppliesToFacts(t *testing.T) {
	d, ctx, ids := bodyGraphFixture(t)
	rows := graphRows(t, recallFromAda(t, d, ctx, ids, `,"body_chars":12`))
	fact := rows["Ada wrote the first program"]
	if fact == nil {
		t.Fatalf("the walk did not reach the fact: %v", rows)
	}
	// "Fact body text" cut at 12 is "Fact body te"; the cut backs off to the word.
	if got := asStr(fact["body"]); got != "Fact body" {
		t.Errorf("fact body with body_chars 12 = %q, want %q", got, "Fact body")
	}
	if cut, _ := fact["body_truncated"].(bool); !cut {
		t.Error("the fact's cut body must be marked body_truncated")
	}
	for title, row := range rows {
		if n := utf8.RuneCountInString(asStr(row["body"])); n > 12 {
			t.Errorf("%q: body of %d characters over body_chars 12", title, n)
		}
	}
	if got := asStr(rows["Clause 4.2 Retention"]["body"]); got == "" {
		t.Error("an explicit body_chars must still give ordinary rows a body")
	}
}

// TestGraphRecall_BudgetCountsBodies: the budget is on what reaches the reader, and
// a body reaches the reader. A budget that fits every title but not the clause's
// body must leave the clause out rather than overspend.
func TestGraphRecall_BudgetCountsBodies(t *testing.T) {
	d, ctx, ids := bodyGraphFixture(t)
	titles := 0
	for title := range graphRows(t, recallFromAda(t, d, ctx, ids, `,"body_chars":0`)) {
		titles += len(title)
	}
	budget := titles + len(shortNoteBody) + 20 // room for every title and the short note, not the clause
	text := recallFromAda(t, d, ctx, ids, fmt.Sprintf(`,"budget_chars":%d`, budget))
	rows := graphRows(t, text)
	var top map[string]any
	_ = json.Unmarshal([]byte(text), &top)

	spent, bodies := 0, 0
	for title, row := range rows {
		spent += len(title) + len(asStr(row["body"]))
		if asStr(row["body"]) != "" {
			bodies++
		}
	}
	if bodies == 0 {
		t.Fatalf("no row carried a body, so the budget had none to count: %v", rows)
	}
	if used := asInt(top["chars_used"]); used != spent {
		t.Errorf("chars_used = %d, but the rows hold %d characters of title and body", used, spent)
	}
	if spent > budget {
		t.Errorf("rows hold %d characters, over the %d budget", spent, budget)
	}
	if _, in := rows["Clause 4.2 Retention"]; in {
		t.Error("the clause's body does not fit the budget, yet the clause came back")
	}
	if tr, _ := top["truncated"].(bool); !tr {
		t.Error("leaving the clause out must report truncated: true")
	}
}

// TestGraphRecall_AsOfHonoursTheChunksOwnInterval: three editions of one norm,
// each an ordinary chunk with its own validity, linked from the norm's entity.
// as_of inside the middle edition's interval returns that edition alone — the
// same answer search and query_chunks give for the same instant.
func TestGraphRecall_AsOfHonoursTheChunksOwnInterval(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertGraphAsOfChunkInterval(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertGraphAsOfChunkInterval(t, d, ctx)
	})
}

func assertGraphAsOfChunkInterval(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	created := vExec(t, d, ctx, `{"op":"create_document","title":"Retention norm","path":"/qg/norms/retention"}`)
	doc, root := resultField(toolResult(created), "document_id"), resultField(toolResult(created), "root_chunk_id")
	norm := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","document_id":%q,"parent_id":%q,"title":"Retention norm","type":"norm","natural_key":"norm:retention"}`,
		doc, root))), "id")
	editions := []struct {
		title              string
		validAt, invalidAt int64
	}{
		{"Edition 2023", ns2023, ns2024},
		{"Edition 2024", ns2024, ns2026},
		{"Edition 2026", ns2026, 0},
	}
	for _, e := range editions {
		v := map[string]any{"op": "create_chunk", "document_id": doc, "parent_id": root,
			"title": e.title, "body": e.title + " text", "valid_at": e.validAt}
		if e.invalidAt != 0 {
			v["invalid_at"] = e.invalidAt
		}
		b, _ := json.Marshal(v)
		id := resultField(toolResult(vExec(t, d, ctx, string(b))), "id")
		vExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","document_id":%q,"from_id":%q,"to_id":%q,"kind":"has_edition"}`, doc, norm, id))
	}

	rows := graphRows(t, vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"graph_recall","seed_ids":[%q],"hops":1,"as_of":%d}`, norm, ns2025)))
	var got []string
	for title := range rows {
		if strings.HasPrefix(title, "Edition") {
			got = append(got, title)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "Edition 2024" {
		t.Fatalf("as_of mid-2025 returned editions %v, want only Edition 2024", got)
	}
	mid := rows["Edition 2024"]
	if v, ok := mid["valid_at"].(json.Number); !ok || v.String() != fmt.Sprint(ns2024) {
		t.Errorf("Edition 2024 valid_at = %v, want its own %d", mid["valid_at"], ns2024)
	}
	if v, ok := mid["invalid_at"].(json.Number); !ok || v.String() != fmt.Sprint(ns2026) {
		t.Errorf("Edition 2024 invalid_at = %v, want its own %d", mid["invalid_at"], ns2026)
	}

	// include_retired still turns the time filter off entirely.
	all := graphRows(t, vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"graph_recall","seed_ids":[%q],"hops":1,"as_of":%d,"include_retired":true}`, norm, ns2025)))
	for _, e := range editions {
		if _, ok := all[e.title]; !ok {
			t.Errorf("include_retired dropped %q: %v", e.title, all)
		}
	}
}

// TestGraphRecall_FactWithOwnIntervalReportsBoth: a fact keeps its fact dates in
// valid_at/invalid_at, and reports a chunk interval of its own beside them under
// different names — and as_of requires both.
func TestGraphRecall_FactWithOwnIntervalReportsBoth(t *testing.T) {
	const ns2030 = int64(1893456000) * 1e9 // still ahead, so the fact is current by default
	d, ctx, ids := bodyGraphFixture(t)
	fact := ids["fact"]
	// Give the fact a sidecar interval, then an own chunk interval that disagrees.
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+
		resultField(toolResult(vExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+fact+`"}`)), "document_id")+
		`","title":"Ada wrote the first program","natural_key":"fact:ada-program","valid_at":1000,"invalid_at":`+
		fmt.Sprint(ns2030)+`}`); r.IsError {
		t.Fatalf("re-upsert fact: %s", r.Text)
	}
	got := vExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+fact+`"}`)
	rev := resultField(toolResult(got), "revision")
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%s,"valid_at":%d}`, fact, rev, ns2024))

	rows := graphRows(t, recallFromAda(t, d, ctx, ids, ""))
	row := rows["Ada wrote the first program"]
	if row == nil {
		t.Fatalf("the walk did not reach the fact: %v", rows)
	}
	for _, f := range []struct {
		key  string
		want string
	}{
		{"valid_at", "1000"},
		{"invalid_at", fmt.Sprint(ns2030)},
		{"chunk_valid_at", fmt.Sprint(ns2024)},
	} {
		if v, ok := row[f.key].(json.Number); !ok || v.String() != f.want {
			t.Errorf("fact %s = %v, want %s (row %v)", f.key, row[f.key], f.want, row)
		}
	}
	if _, has := row["chunk_invalid_at"]; has {
		t.Errorf("chunk_invalid_at reported for a chunk with no own end: %v", row)
	}

	// The sidecar holds at 1500; the chunk's own interval only starts in 2024. A fact
	// true by its fact dates but outside its own dates is not true then.
	at := graphRows(t, recallFromAda(t, d, ctx, ids, `,"as_of":1500`))
	if _, in := at["Ada wrote the first program"]; in {
		t.Error("as_of 1500 returned a fact whose own chunk interval starts in 2024")
	}
	at = graphRows(t, recallFromAda(t, d, ctx, ids, fmt.Sprintf(`,"as_of":%d`, ns2025)))
	if _, in := at["Ada wrote the first program"]; !in {
		t.Error("as_of mid-2025 lies inside both of the fact's intervals and must return it")
	}
}

func TestGraphRecall_BodyCharsOverCapIsRefused(t *testing.T) {
	d, ctx, ids := bodyGraphFixture(t)
	for _, n := range []int{4001, -1} {
		_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":["%s"],"body_chars":%d}`, ids["Ada"], n))
		if !r.IsError {
			t.Errorf("body_chars %d was accepted: %s", n, r.Text)
			continue
		}
		if !strings.Contains(r.Text, "4000") {
			t.Errorf("body_chars %d refusal does not name the 4000 cap: %s", n, r.Text)
		}
	}
	rows := graphRows(t, recallFromAda(t, d, ctx, ids, `,"body_chars":4000`))
	if got := asStr(rows["Clause 4.2 Retention"]["body"]); got != clauseBody {
		t.Errorf("body_chars 4000 should return the whole %d-character clause, got %d", len(clauseBody), len(got))
	}
}
