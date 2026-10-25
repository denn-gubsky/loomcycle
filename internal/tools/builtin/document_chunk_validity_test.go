package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Validity on ordinary chunks: a chunk's own [valid_at, invalid_at) interval,
// stored on the chunks row and independent of the fact tier's sidecar.

// Instants for the edition tests, in unix nanoseconds.
const (
	ns2023 = int64(1672531200) * 1e9 // 2023-01-01
	ns2024 = int64(1704067200) * 1e9 // 2024-01-01
	ns2025 = int64(1748736000) * 1e9 // 2025-06-01
	ns2026 = int64(1767225600) * 1e9 // 2026-01-01
)

// chunkInt64 reads an integer key from a decoded JSON response as int64,
// reporting whether it was present. JSON numbers decode to float64, which cannot
// hold a nanosecond instant exactly, so the response is re-decoded with UseNumber.
func chunkInt64(t *testing.T, text, key string) (int64, bool) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	v, ok := m[key]
	if !ok || v == nil { // a keyed chunk names an unset end as null
		return 0, false
	}
	n, err := v.(json.Number).Int64()
	if err != nil {
		t.Fatalf("%s = %v: %v", key, v, err)
	}
	return n, true
}

func vExec(t *testing.T, d *Document, ctx context.Context, body string) string {
	t.Helper()
	res, err := d.Execute(ctx, json.RawMessage(body))
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %s", body, err, res.Text)
	}
	return res.Text
}

func newValidityDoc(t *testing.T, d *Document, ctx context.Context, path string) string {
	t.Helper()
	return resultField(toolResult(vExec(t, d, ctx, `{"op":"create_document","title":"Norms","path":"`+path+`"}`)), "document_id")
}

func TestDocumentChunkValidity_CreateUpdateAndClear(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newValidityDoc(t, d, ctx, "/qg/norms/ed1")
	created := vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","document_id":%q,"title":"Clause 1","body":"text","valid_at":%d,"invalid_at":%d}`, doc, ns2024, ns2026))
	id := resultField(toolResult(created), "id")
	if v, ok := chunkInt64(t, created, "valid_at"); !ok || v != ns2024 {
		t.Fatalf("create_chunk valid_at = %d (present %v), want %d: %s", v, ok, ns2024, created)
	}
	if v, ok := chunkInt64(t, created, "invalid_at"); !ok || v != ns2026 {
		t.Fatalf("create_chunk invalid_at = %d (present %v), want %d", v, ok, ns2026)
	}
	key := sidecarScope(t, d, ctx)
	revisions := func() int {
		res, err := d.query(ctx, key, `SELECT count(*) FROM chunk_revisions WHERE chunk_id = ?`, id)
		if err != nil {
			t.Fatalf("count revisions: %v", err)
		}
		return asInt(res.Rows[0][0])
	}
	logBefore := revisions()

	// Update ONE end; the other is kept.
	updated := vExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":1,"invalid_at":%d}`, id, ns2025))
	if v, _ := chunkInt64(t, updated, "valid_at"); v != ns2024 {
		t.Errorf("an update naming only invalid_at changed valid_at to %d", v)
	}
	if v, _ := chunkInt64(t, updated, "invalid_at"); v != ns2025 {
		t.Errorf("invalid_at after update = %d, want %d", v, ns2025)
	}
	if rev, _ := chunkInt64(t, updated, "revision"); rev != 2 {
		t.Errorf("revision after a validity update = %d, want 2", rev)
	}

	// An unrelated update leaves both alone.
	kept := vExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":2,"status":"in_force"}`, id))
	if v, _ := chunkInt64(t, kept, "valid_at"); v != ns2024 {
		t.Errorf("a status-only update changed valid_at to %d", v)
	}

	// JSON null clears.
	cleared := vExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":3,"valid_at":null,"invalid_at":null}`, id))
	if _, ok := chunkInt64(t, cleared, "valid_at"); ok {
		t.Errorf("valid_at still reported after an explicit null: %s", cleared)
	}
	if _, ok := chunkInt64(t, cleared, "invalid_at"); ok {
		t.Errorf("invalid_at still reported after an explicit null: %s", cleared)
	}
	if rev, _ := chunkInt64(t, cleared, "revision"); rev != 4 {
		t.Errorf("revision after clearing = %d, want 4", rev)
	}
	// The body never changed, so the body-change log did not grow.
	if after := revisions(); after != logBefore {
		t.Errorf("chunk_revisions grew from %d to %d on metadata-only updates", logBefore, after)
	}
}

// An instant of 0 is the unix epoch, a real value, not "unset".
func TestDocumentChunkValidity_ZeroIsAnInstant(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newValidityDoc(t, d, ctx, "/e")
	out := vExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","document_id":%q,"title":"Epoch","valid_at":0}`, doc))
	if v, ok := chunkInt64(t, out, "valid_at"); !ok || v != 0 {
		t.Errorf("valid_at:0 read back as %d (present %v): %s", v, ok, out)
	}
}

func TestDocumentChunkValidity_RefusesAnEmptyInterval(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newValidityDoc(t, d, ctx, "/e")
	res, _ := d.Execute(ctx, json.RawMessage(fmt.Sprintf(
		`{"op":"create_chunk","document_id":%q,"title":"Never","valid_at":%d,"invalid_at":%d}`, doc, ns2026, ns2024)))
	if !res.IsError {
		t.Fatalf("create_chunk with invalid_at before valid_at succeeded: %s", res.Text)
	}
	id := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","document_id":%q,"title":"Ok","valid_at":%d}`, doc, ns2025))), "id")
	// The stored valid_at counts: an update that sets only invalid_at before it is refused,
	// and refused before the revision moves.
	res, _ = d.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":1,"invalid_at":%d}`, id, ns2024)))
	if !res.IsError {
		t.Fatalf("update_chunk closing the interval before its start succeeded: %s", res.Text)
	}
	got := vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, id))
	if rev, _ := chunkInt64(t, got, "revision"); rev != 1 {
		t.Errorf("a refused update moved the revision to %d", rev)
	}
}

// An undated chunk's responses carry no validity keys at all.
func TestDocumentChunkValidity_UndatedChunkReportsNothing(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newValidityDoc(t, d, ctx, "/e")
	out := vExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","document_id":%q,"title":"Plain","body":"x"}`, doc))
	q := vExec(t, d, ctx, fmt.Sprintf(`{"op":"query_chunks","document_id":%q}`, doc))
	for _, s := range []string{out, q} {
		if strings.Contains(s, "valid_at") || strings.Contains(s, "invalid_at") {
			t.Errorf("an undated chunk reported a validity key: %s", s)
		}
	}
}

func TestDocumentChunkValidity_DoesNotCreateAFact(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newValidityDoc(t, d, ctx, "/qg/norms/ed1")
	created := vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","document_id":%q,"title":"Clause","body":"text","valid_at":%d,"invalid_at":%d}`, doc, ns2024, ns2026))
	// The premise: the chunk IS dated, so its absence from the fact tier says something.
	if _, ok := chunkInt64(t, created, "valid_at"); !ok {
		t.Fatalf("the chunk was not dated: %s", created)
	}
	key := sidecarScope(t, d, ctx)
	res, err := d.query(ctx, key, `SELECT count(*) FROM chunk_memory_meta`)
	if err != nil {
		t.Fatal(err)
	}
	if n := asInt(res.Rows[0][0]); n != 0 {
		t.Errorf("a dated ordinary chunk created %d sidecar rows", n)
	}
	facts := vExec(t, d, ctx, `{"op":"list_facts"}`)
	var lf struct {
		Facts []any `json:"facts"`
	}
	_ = json.Unmarshal([]byte(facts), &lf)
	if len(lf.Facts) != 0 {
		t.Errorf("list_facts lists a dated ordinary chunk: %s", facts)
	}
	stats := vExec(t, d, ctx, `{"op":"verification_stats"}`)
	if n, _ := chunkInt64(t, stats, "facts"); n != 0 {
		t.Errorf("verification_stats counts a dated ordinary chunk: %s", stats)
	}
}

// upsert_chunk's valid_at/invalid_at are the FACT's interval (the sidecar), as
// before; they must not also land on the chunk row.
func TestDocumentChunkValidity_UpsertKeepsItsDatesOnTheSidecar(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newValidityDoc(t, d, ctx, "/facts")
	id := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","document_id":%q,"title":"F","natural_key":"fact:f","body":"b","valid_at":%d,"invalid_at":%d}`,
		doc, ns2024, ns2026))), "id")
	got := vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, id))
	if _, ok := chunkInt64(t, got, "valid_at"); ok {
		t.Errorf("upsert_chunk's fact interval leaked onto the chunk row: %s", got)
	}
	if !strings.Contains(got, `"entity"`) || !strings.Contains(got, fmt.Sprint(ns2024)) {
		t.Errorf("the fact interval is missing from the sidecar block: %s", got)
	}
}

// editionCorpus is three ordinary-chunk editions of one clause: 2023–2024
// (repealed), 2024–2026 and 2026–, plus an undated note, all matching the query
// equally.
func editionCorpus(t *testing.T, c *corpus) (old, mid, cur, undated string) {
	t.Helper()
	doc := c.doc("/qg/norms/clause7")
	old = c.chunk(doc, "norm clause", map[string]any{"status": "repealed", "valid_at": ns2023, "invalid_at": ns2024})
	mid = c.chunk(doc, "norm clause", map[string]any{"status": "in_force", "valid_at": ns2024, "invalid_at": ns2026})
	cur = c.chunk(doc, "norm clause", map[string]any{"status": "in_force", "valid_at": ns2026})
	undated = c.chunk(doc, "norm clause", nil)
	return
}

func TestDocumentSearch_AsOfPicksTheEditionValidThen(t *testing.T) {
	c := newCorpus(t, "norm", "clause")
	_, mid, _, undated := editionCorpus(t, c)
	ids, echo := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "as_of": ns2025})
	assertHits(t, "as_of=2025", ids, mid, undated)
	if echoInt(echo, "matched_chunks") != 3 { // mid, undated and the bodiless root
		t.Errorf("echo = %v, want matched_chunks 3", echo)
	}
	// The hit reports the edition's own interval.
	out := c.exec(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "as_of": ns2025})
	for _, h := range out["chunks"].([]any) {
		hm := h.(map[string]any)
		if hm["chunk_id"] == mid && (hm["valid_at"] == nil || hm["invalid_at"] == nil) {
			t.Errorf("the dated hit does not report its interval: %v", hm)
		}
		if hm["chunk_id"] == undated && (hm["valid_at"] != nil || hm["invalid_at"] != nil) {
			t.Errorf("the undated hit reports an interval: %v", hm)
		}
	}
	// Half-open: at exactly the boundary the later edition holds, the earlier does not.
	ids, _ = c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "as_of": ns2026})
	if got := setOf(ids); got[mid] {
		t.Errorf("as_of at the boundary returned the edition that ended there: %v", ids)
	}
}

func TestDocumentRelated_AsOfCoversTheChunkInterval(t *testing.T) {
	c := newCorpus(t, "norm", "clause")
	old, mid, cur, undated := editionCorpus(t, c)
	ids, _ := c.call(map[string]any{"op": "related", "id": undated, "limit": 50, "as_of": ns2025})
	got := setOf(ids)
	if !got[mid] || got[old] || got[cur] {
		t.Errorf("related as_of=2025 = %v, want the 2024–2026 edition only", ids)
	}
}

func TestDocumentSearch_AsOfRequiresBothIntervalsOnAFactChunk(t *testing.T) {
	c := newCorpus(t, "norm", "clause")
	doc := c.doc("/qg/facts")
	// Sidecar interval 2024–2026 on both facts.
	fact := func(key string) string {
		return c.exec(map[string]any{"op": "upsert_chunk", "document_id": doc, "title": key, "natural_key": key,
			"body": "norm clause", "valid_at": ns2024, "invalid_at": ns2026})["id"].(string)
	}
	agree := fact("fact:agree")
	disagree := fact("fact:disagree")
	// The chunk's own interval: one agrees with 2025, the other ended before it.
	c.exec(map[string]any{"op": "update_chunk", "id": agree, "revision": 1, "valid_at": ns2023})
	c.exec(map[string]any{"op": "update_chunk", "id": disagree, "revision": 1, "invalid_at": ns2024 + 1})

	ids, _ := c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "as_of": ns2025})
	assertHits(t, "as_of=2025 over two intervals", ids, agree)
	// Before the sidecar starts, the chunk interval alone is not enough.
	ids, _ = c.call(map[string]any{"op": "search", "query": "norm clause", "limit": 50, "as_of": ns2023 + 1})
	if len(ids) != 0 {
		t.Errorf("as_of before the sidecar interval returned %v", ids)
	}
}

func TestQueryChunks_AsOf(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newValidityDoc(t, d, ctx, "/qg/norms/clause7")
	mk := func(title string, extra string) string {
		return resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_chunk","document_id":%q,"title":%q%s}`, doc, title, extra))), "id")
	}
	mk("old", fmt.Sprintf(`,"valid_at":%d,"invalid_at":%d`, ns2023, ns2024))
	mid := mk("mid", fmt.Sprintf(`,"valid_at":%d,"invalid_at":%d`, ns2024, ns2026))
	mk("cur", fmt.Sprintf(`,"valid_at":%d`, ns2026))
	// A fact chunk whose sidecar ended before 2025 is excluded by its sidecar.
	fact := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","document_id":%q,"title":"fact","natural_key":"fact:x","valid_at":%d,"invalid_at":%d}`,
		doc, ns2023, ns2024))), "id")

	out := vExec(t, d, ctx, fmt.Sprintf(`{"op":"query_chunks","under_path":"/qg/norms","as_of":%d}`, ns2025))
	var q struct {
		Chunks []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal([]byte(out), &q)
	got := map[string]bool{}
	for _, ch := range q.Chunks {
		got[ch.ID] = true
	}
	// The root (undated) and mid; never old, cur or the ended fact.
	if !got[mid] || len(got) != 2 {
		t.Errorf("query_chunks as_of=2025 = %s, want the root and %s only", out, mid)
	}
	if got[fact] {
		t.Errorf("query_chunks as_of returned a fact whose sidecar interval ended")
	}
	if !strings.Contains(out, fmt.Sprint(ns2024)) {
		t.Errorf("query_chunks rows do not report the interval: %s", out)
	}
	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"query_chunks","as_of":-1}`))
	if !res.IsError {
		t.Errorf("query_chunks accepted a negative as_of")
	}
}

// ---- schema migration ------------------------------------------------------

func TestDocumentSchema_MigrateChunkValidityIsIdempotentAndCrashSafe(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertChunkValidityMigration(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertChunkValidityMigration(t, d, ctx)
	})
}

func assertChunkValidityMigration(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	doc := newValidityDoc(t, d, ctx, "/m")
	id := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","document_id":%q,"title":"Before"}`, doc))), "id")
	key := sidecarScope(t, d, ctx)
	rewind := func(cols ...string) {
		t.Helper()
		// sqlite cannot drop an indexed column; the index goes first.
		if _, err := d.SqlMem.Exec(ctx, key, `DROP INDEX IF EXISTS chunks_validity`, nil, 0); err != nil {
			t.Fatalf("drop index: %v", err)
		}
		for _, col := range cols {
			if _, err := d.SqlMem.Exec(ctx, key, `ALTER TABLE chunks DROP COLUMN `+col, nil, 0); err != nil {
				t.Fatalf("rewind %s: %v", col, err)
			}
		}
	}
	migrate := func(what string) {
		t.Helper()
		d.SqlMem.ForgetEnsured(key)
		if err := d.ensureSchema(ctx, key); err != nil {
			t.Fatalf("%s: ensureSchema: %v", what, err)
		}
		for _, col := range []string{"valid_at", "invalid_at"} {
			if !d.tableHasColumn(ctx, key, "chunks", col) {
				t.Fatalf("%s: chunks.%s absent after ensureSchema", what, col)
			}
		}
	}
	// A scope provisioned before the columns existed.
	rewind("valid_at", "invalid_at")
	if d.tableHasColumn(ctx, key, "chunks", "valid_at") {
		t.Fatal("rewind did not take")
	}
	migrate("old scope")
	// A crash between the two ALTERs: only one column present.
	rewind("invalid_at")
	migrate("half-migrated scope")
	// Idempotent: a second run on a migrated scope is a no-op, not an error.
	migrate("migrated scope")
	// The pre-existing chunk reads back undated, and the columns are writable.
	got := vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, id))
	if _, ok := chunkInt64(t, got, "valid_at"); ok {
		t.Errorf("a migrated pre-existing chunk is dated: %s", got)
	}
	upd := vExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","id":%q,"revision":1,"valid_at":%d}`, id, ns2024))
	if v, _ := chunkInt64(t, upd, "valid_at"); v != ns2024 {
		t.Errorf("valid_at after migration + update = %d", v)
	}
	if !indexExists(t, d, ctx, key, "chunks_validity") {
		t.Errorf("the validity index is missing after migration")
	}
}

// indexExists asks the tier's catalog whether an index is present.
func indexExists(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey, name string) bool {
	t.Helper()
	stmt := `SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?`
	if d.SqlMem.Tier() == "postgres" {
		stmt = `SELECT 1 FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ?`
	}
	res, err := d.query(ctx, key, stmt, name)
	if err != nil {
		t.Fatalf("index probe: %v", err)
	}
	return len(res.Rows) > 0
}

// ---- both tiers ------------------------------------------------------------

// TestDocumentChunkValidity_CoreBothTiers is the store-contract drift guard: the
// same writes read back identically on sqlite and Postgres, extremes included,
// and the as_of predicate selects the same rows on both.
func TestDocumentChunkValidity_CoreBothTiers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertChunkValidityCore(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertChunkValidityCore(t, d, ctx)
	})
}

func assertChunkValidityCore(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	doc := newValidityDoc(t, d, ctx, "/qg/norms/ed1")
	const maxNS = int64(9223372036854775807)
	cases := []struct {
		title              string
		validAt, invalidAt *int64
	}{
		{"epoch", ptr64(0), nil},
		{"negative", ptr64(-ns2023), ptr64(ns2023)},
		{"bounded", ptr64(ns2024), ptr64(ns2026)},
		{"open-start", nil, ptr64(ns2024)},
		{"far", ptr64(ns2026), ptr64(maxNS)},
		{"undated", nil, nil},
	}
	ids := map[string]string{}
	for _, c := range cases {
		v := map[string]any{"op": "create_chunk", "document_id": doc, "title": c.title}
		if c.validAt != nil {
			v["valid_at"] = *c.validAt
		}
		if c.invalidAt != nil {
			v["invalid_at"] = *c.invalidAt
		}
		b, _ := json.Marshal(v)
		ids[c.title] = resultField(toolResult(vExec(t, d, ctx, string(b))), "id")
	}
	for _, c := range cases {
		got := vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, ids[c.title]))
		for _, f := range []struct {
			key  string
			want *int64
		}{{"valid_at", c.validAt}, {"invalid_at", c.invalidAt}} {
			v, ok := chunkInt64(t, got, f.key)
			switch {
			case f.want == nil && ok:
				t.Errorf("%s: %s = %d, want absent", c.title, f.key, v)
			case f.want != nil && (!ok || v != *f.want):
				t.Errorf("%s: %s = %d (present %v), want %d", c.title, f.key, v, ok, *f.want)
			}
		}
	}
	out := vExec(t, d, ctx, fmt.Sprintf(`{"op":"query_chunks","document_id":%q,"as_of":%d}`, doc, ns2025))
	var q struct {
		Chunks []struct {
			Title string `json:"title"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal([]byte(out), &q)
	var titles []string
	for _, ch := range q.Chunks {
		titles = append(titles, ch.Title)
	}
	// Sorted: the tiers order a NULL parent_id differently, which is not what
	// this test is about.
	sort.Strings(titles)
	got := strings.Join(titles, ",")
	if got != "Norms,bounded,epoch,undated" {
		t.Errorf("query_chunks as_of=2025 = %s", got)
	}
}

func ptr64(v int64) *int64 { return &v }

func toolResult(text string) tools.Result { return tools.Result{Text: text} }
