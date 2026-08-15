package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// verdictFixture returns a fact WITH a span (so it can be judged) and one without.
func verdictFixture(t *testing.T) (*Document, context.Context, string, string) {
	t.Helper()
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	doc, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	mk := func(nk, title, quote string) string {
		body := `{"op":"upsert_chunk","scope":"user","document_id":"` + doc + `","parent_id":"` + root +
			`","natural_key":"` + nk + `","title":"` + title + `","body":"x"`
		if quote != "" {
			body += `,"source_quote":"` + quote + `"`
		}
		res, rr := docExec(t, d, ctx, body+`}`)
		if rr.IsError {
			t.Fatalf("upsert %s: %s", nk, rr.Text)
		}
		id, _ := res["id"].(string)
		return id
	}
	withSpan := mk("f:spanned", "The user lives in Cluj-Napoca.", "I live in Cluj-Napoca.")
	noSpan := mk("f:bare", "Something with no source.", "")
	return d, ctx, withSpan, noSpan
}

// TestVerdict_UnjudgedFactsStayVisible is the correctness of the whole feature.
//
// Nothing in the memory pipeline supplies a confidence — the extractor's contract has no
// such field — so EVERY existing fact has NULL. A floor that treated NULL as low would
// withhold the entire store the moment it shipped, and the loss would be invisible.
func TestVerdict_UnjudgedFactsStayVisible(t *testing.T) {
	d, ctx, _, _ := verdictFixture(t)
	facts, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if r.IsError {
		t.Fatalf("list_facts: %s", r.Text)
	}
	if n, _ := facts["count"].(float64); n != 2 {
		t.Fatalf("unjudged facts must all be visible, got %v of 2", facts["count"])
	}
	// And they must not claim a verdict they never had.
	for _, f := range facts["facts"].([]any) {
		e, _ := f.(map[string]any)["entity"].(map[string]any)
		if _, judged := e["judged_at"]; judged {
			t.Errorf("an unjudged fact reports judged_at: %v", e)
		}
		if _, w := e["withheld"]; w {
			t.Errorf("an unjudged fact reports a withheld flag: %v", e)
		}
	}
}

// TestVerdict_UnsupportedIsWithheldNotDeleted (RFC CC §5).
func TestVerdict_UnsupportedIsWithheldNotDeleted(t *testing.T) {
	d, ctx, withSpan, _ := verdictFixture(t)

	out, r := docExec(t, d, ctx, `{"op":"judge_fact","scope":"user","id":"`+withSpan+
		`","verdict":"unsupported","reason":"the span names no city"}`)
	if r.IsError {
		t.Fatalf("judge_fact: %s", r.Text)
	}
	if w, _ := out["withheld"].(bool); !w {
		t.Errorf("an unsupported verdict must report withheld: %v", out)
	}

	// Withheld from the fact surface...
	facts, _ := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if n, _ := facts["count"].(float64); n != 1 {
		t.Errorf("the refuted fact is still listed: count %v, want 1", facts["count"])
	}
	// ...and NOT deleted: readable on request, with its ground stated.
	facts, _ = docExec(t, d, ctx, `{"op":"list_facts","scope":"user","include_refuted":true}`)
	if n, _ := facts["count"].(float64); n != 2 {
		t.Fatalf("include_refuted did not surface it: count %v, want 2", facts["count"])
	}
	var sawReason bool
	for _, f := range facts["facts"].([]any) {
		e, _ := f.(map[string]any)["entity"].(map[string]any)
		if r, _ := e["judge_reason"].(string); strings.Contains(r, "no city") {
			sawReason = true
			if w, _ := e["withheld"].(bool); !w {
				t.Errorf("the refuted fact does not report withheld: %v", e)
			}
		}
	}
	if !sawReason {
		t.Error("the reason was not surfaced — a withheld fact with no stated ground reads as a bug")
	}
	// The chunk itself survives, which is what makes a wrong refusal recoverable.
	if got, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+withSpan+`"}`); got["id"] != withSpan {
		t.Error("the refuted fact's chunk was destroyed")
	}
}

// TestVerdict_SupportedAndUnclearStayVisible.
//
// `unclear` sits ABOVE the floor deliberately: a judge that is unsure is not evidence
// against a claim, and withholding on a maybe is the invisible loss this design gates on.
func TestVerdict_SupportedAndUnclearStayVisible(t *testing.T) {
	d, ctx, withSpan, _ := verdictFixture(t)
	for _, verdict := range []string{"supported", "unclear"} {
		if _, r := docExec(t, d, ctx, `{"op":"judge_fact","scope":"user","id":"`+withSpan+
			`","verdict":"`+verdict+`","reason":"because"}`); r.IsError {
			t.Fatalf("judge %s: %s", verdict, r.Text)
		}
		facts, _ := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
		if n, _ := facts["count"].(float64); n != 2 {
			t.Errorf("verdict %q withheld a fact it should not: count %v", verdict, facts["count"])
		}
	}
}

// TestVerdict_RefusesWhatItCannotCheck.
//
// A verdict reached with no evidence is the failure this RFC exists to stop, and it would
// be indistinguishable from one that was checked. Also pins that the scale belongs to the
// server: a caller cannot write a number.
func TestVerdict_RefusesWhatItCannotCheck(t *testing.T) {
	d, ctx, withSpan, noSpan := verdictFixture(t)
	cases := []struct{ name, req, want string }{
		{"no span to check against",
			`{"op":"judge_fact","scope":"user","id":"` + noSpan + `","verdict":"supported","reason":"r"}`,
			"no source span"},
		{"a number instead of a verdict",
			`{"op":"judge_fact","scope":"user","id":"` + withSpan + `","verdict":"0.9","reason":"r"}`,
			"verdict must be"},
		{"no reason",
			`{"op":"judge_fact","scope":"user","id":"` + withSpan + `","verdict":"unsupported"}`,
			"reason is required"},
		{"not a fact at all",
			`{"op":"judge_fact","scope":"user","id":"nope","verdict":"supported","reason":"r"}`,
			""},
	}
	for _, c := range cases {
		_, r := docExec(t, d, ctx, c.req)
		if !r.IsError {
			t.Errorf("%s: should have been refused", c.name)
			continue
		}
		if c.want != "" && !strings.Contains(r.Text, c.want) {
			t.Errorf("%s: refusal does not explain itself: %s", c.name, r.Text)
		}
	}
}

// TestVerdict_SurvivesAnEditOfTheClaim.
//
// An ordinary edit is not a re-judgement, and it must not erase that one happened: the
// stale verdict plus the changed text is exactly what tells an operator to look again.
func TestVerdict_SurvivesAnEditOfTheClaim(t *testing.T) {
	d, ctx, withSpan, _ := verdictFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"judge_fact","scope":"user","id":"`+withSpan+
		`","verdict":"unsupported","reason":"the span names no city"}`); r.IsError {
		t.Fatalf("judge: %s", r.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"f:spanned",`+
		`"title":"The user resides in Cluj-Napoca."}`); r.IsError {
		t.Fatalf("edit: %s", r.Text)
	}
	facts, _ := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","include_refuted":true}`)
	var found bool
	for _, f := range facts["facts"].([]any) {
		e, _ := f.(map[string]any)["entity"].(map[string]any)
		if r, _ := e["judge_reason"].(string); strings.Contains(r, "no city") {
			found = true
		}
	}
	if !found {
		t.Error("the verdict was erased by an edit to the claim")
	}
}

// TestFactProjections_DoNotDrift.
//
// `list_facts` builds its own SELECT and scans the columns back into a chunkMetaRow so its
// `entity` block is "byte-identical to get_chunk's — one formatter, no drift". The
// FORMATTER is shared; the two column lists are not, and that gap has now silently dropped
// three fields in a row from list_facts while get_chunk carried them: the source span, the
// subject, and the judge's reason. Each was found by a feature test noticing the absence,
// which only works when someone thinks to look.
//
// This compares the two surfaces directly on one fully-populated fact, so the next field
// added to one and not the other fails here instead of hiding.
func TestFactProjections_DoNotDrift(t *testing.T) {
	d, ctx, withSpan, _ := verdictFixture(t)
	// Populate every field a fact can carry, so an omission on either side shows up.
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"f:spanned",`+
		`"type":"person","subject":"the user","class":"derived","valid_at":1700000000000000000}`); r.IsError {
		t.Fatalf("enrich: %s", r.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"judge_fact","scope":"user","id":"`+withSpan+
		`","verdict":"unclear","reason":"partially supported"}`); r.IsError {
		t.Fatalf("judge: %s", r.Text)
	}

	got, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+withSpan+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	fromGet, _ := got["entity"].(map[string]any)
	if len(fromGet) == 0 {
		t.Fatal("get_chunk returned no entity block")
	}

	facts, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","include_refuted":true}`)
	if r.IsError {
		t.Fatalf("list_facts: %s", r.Text)
	}
	var fromList map[string]any
	for _, f := range facts["facts"].([]any) {
		m, _ := f.(map[string]any)
		if m["id"] == withSpan {
			fromList, _ = m["entity"].(map[string]any)
		}
	}
	if fromList == nil {
		t.Fatal("the fact was not in list_facts")
	}
	for k, want := range fromGet {
		if got, ok := fromList[k]; !ok {
			t.Errorf("list_facts drops %q, which get_chunk reports (%v) — the projections have drifted", k, want)
		} else if fmtAny(got) != fmtAny(want) {
			t.Errorf("%q differs: list_facts %v, get_chunk %v", k, got, want)
		}
	}
}

// fmtAny renders a JSON scalar for comparison without caring whether a number came back
// as a float or an int.
func fmtAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestVerdict_RetiredAndRefutedAreSeparateAxes.
//
// include_retired and include_refuted sit next to each other on the same op and the first
// draft of the graph clause folded them together, so asking for history quietly returned
// refuted facts too. They answer different questions — retired means a later fact corrected
// this one, refuted means it was checked and failed — and a caller reading the correction
// history has no reason to want fabrications mixed in, nor any way to say so once the two
// are coupled.
func TestVerdict_RetiredAndRefutedAreSeparateAxes(t *testing.T) {
	d, ctx, withSpan, _ := verdictFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"judge_fact","scope":"user","id":"`+withSpan+
		`","verdict":"unsupported","reason":"the span says nothing about this"}`); r.IsError {
		t.Fatalf("judge: %s", r.Text)
	}

	seen := func(payload string) bool {
		t.Helper()
		out, r := docExec(t, d, ctx, payload)
		if r.IsError {
			t.Fatalf("%s: %s", payload, r.Text)
		}
		for _, f := range out["facts"].([]any) {
			if f.(map[string]any)["id"] == withSpan {
				return true
			}
		}
		return false
	}

	if seen(`{"op":"list_facts","scope":"user","include_retired":true}`) {
		t.Error("include_retired revealed a refuted fact — the axes are coupled")
	}
	if !seen(`{"op":"list_facts","scope":"user","include_refuted":true}`) {
		t.Error("include_refuted did not reveal the refuted fact")
	}

	// Same on the graph walk, which is where the coupling actually was.
	g, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","query":"Cluj","include_retired":true}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	for _, c := range g["chunks"].([]any) {
		if c.(map[string]any)["id"] == withSpan {
			t.Error("graph_recall include_retired revealed a refuted fact")
		}
	}
}

// TestVerdict_MistypedIsReducedNotWithheld.
//
// The fourth verdict exists because "the span does not support this" and "the span supports
// this but it is filed as the wrong kind of thing" have different fixes — drop it versus
// retype it — and a verdict that collapsed them would tell an operator to delete a true
// fact. So a mistyped fact must stay VISIBLE while reading as lower-confidence than a clean
// one: withholding it would be a false refusal in service of a filing error.
func TestVerdict_MistypedIsReducedNotWithheld(t *testing.T) {
	d, ctx, withSpan, _ := verdictFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"judge_fact","scope":"user","id":"`+withSpan+
		`","verdict":"mistyped","reason":"the span supports this, but location is not what the user is"}`)
	if r.IsError {
		t.Fatalf("judge: %s", r.Text)
	}
	if w, _ := out["withheld"].(bool); w {
		t.Error("a mistyped fact was withheld — the claim is true, only its filing is wrong")
	}
	conf, _ := out["confidence"].(float64)
	if conf <= withholdBelowConfidence {
		t.Errorf("mistyped confidence %v is at or below the floor %v, so it will be withheld",
			conf, withholdBelowConfidence)
	}
	if conf >= confidenceSupported {
		t.Errorf("mistyped confidence %v does not sort behind a clean fact (%v)", conf, confidenceSupported)
	}

	facts, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if r.IsError {
		t.Fatalf("list_facts: %s", r.Text)
	}
	var found map[string]any
	for _, f := range facts["facts"].([]any) {
		m := f.(map[string]any)
		if m["id"] == withSpan {
			found, _ = m["entity"].(map[string]any)
		}
	}
	if found == nil {
		t.Fatal("the mistyped fact is gone from list_facts")
	}
	// The reason is what tells an operator to retype rather than delete, so it has to
	// survive to the surface they read.
	if !strings.Contains(found["judge_reason"].(string), "location is not what the user is") {
		t.Errorf("the retype instruction did not reach the surface: %v", found["judge_reason"])
	}
}

// TestVerdict_MistypedStillNeedsASpan pins that widening the vocabulary did not widen the
// evidence rule. Only `unclear` may be recorded without a span; a claim about whether
// something is filed correctly is still a claim about its content.
func TestVerdict_MistypedStillNeedsASpan(t *testing.T) {
	d, ctx, _, noSpan := verdictFixture(t)
	_, r := docExec(t, d, ctx, `{"op":"judge_fact","scope":"user","id":"`+noSpan+
		`","verdict":"mistyped","reason":"r"}`)
	if !r.IsError || !strings.Contains(r.Text, "no source span") {
		t.Errorf("a span-less fact was judged mistyped: err=%v %s", r.IsError, r.Text)
	}
}

// TestVerdict_MigratesAScopeThatPredatesTheColumns.
//
// ensureSchema's CREATE TABLE IF NOT EXISTS leaves an EXISTING table untouched, so a
// store provisioned before these columns would never gain them and every verdict write
// would fail — on exactly the deployments that already have facts worth judging. The
// migration is what makes this shippable, and dropping the columns back off is the only
// honest way to test it.
func TestVerdict_MigratesAScopeThatPredatesTheColumns(t *testing.T) {
	d, ctx, withSpan, _ := verdictFixture(t)
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	for _, col := range []string{"judged_at", "judge_reason"} {
		if derr := d.exec(ctx, key, `ALTER TABLE chunk_memory_meta DROP COLUMN `+col); derr != nil {
			t.Skipf("this tier cannot drop a column, so the pre-migration state is unreachable: %v", derr)
		}
		if d.tableHasColumn(ctx, key, "chunk_memory_meta", col) {
			t.Fatalf("%s survived the drop — the test cannot reach the state it is for", col)
		}
	}

	// Any op re-runs ensureSchema, which must add both back. judge_fact is the one
	// that would fail: it UPDATEs the columns directly.
	out, r := docExec(t, d, ctx, `{"op":"judge_fact","scope":"user","id":"`+withSpan+
		`","verdict":"unsupported","reason":"the span does not say this"}`)
	if r.IsError {
		t.Fatalf("the migration did not run: %s", r.Text)
	}
	if w, _ := out["withheld"].(bool); !w {
		t.Error("the verdict did not take effect after the migration")
	}
	got, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+withSpan+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	entity, _ := got["entity"].(map[string]any)
	if entity["judge_reason"] != "the span does not say this" {
		t.Errorf("the verdict did not persist after the migration: %v", entity)
	}
}

// TestVerdict_PostgresTierMigratesAndWithholds.
//
// The migration probes for each column with a SELECT that FAILS when it is absent, and on
// PostgreSQL a failed statement aborts its surrounding transaction — so a probe that is
// merely "an error we ignore" on sqlite can poison everything after it on Postgres. CI
// cannot catch that on its own: it always provisions Postgres scopes FRESH, where the
// CREATE TABLE already carries the columns and the probe never fails. This is the only
// place the Postgres ALTER path is actually taken.
//
// It also covers the nullable BIGINT read: judged_at comes back through the same scan as
// event_seq, and a tier that returned it differently would surface as a fact that is
// silently never withheld.
func TestVerdict_PostgresTierMigratesAndWithholds(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the verdict postgres-tier test")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	dropAllSqlmemScopes(t, raw)
	mgr, err := sqlmem.NewPostgres(context.Background(), sqlmem.Config{
		PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000,
	})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		dropAllSqlmemScopes(t, raw)
		_ = raw.Close()
	})
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "pg-verdict")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"PG verdicts"}`)
	if r.IsError {
		t.Fatalf("create_document(pg): %s", r.Text)
	}
	docID, root := asStr(out["document_id"]), asStr(out["root_chunk_id"])
	up, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","natural_key":"fact:pg","title":"A claim.",`+
		`"source_quote":"the source said so","body":"x"}`)
	if r.IsError {
		t.Fatalf("upsert(pg): %s", r.Text)
	}
	chunkID := asStr(up["id"])

	// Back to the pre-migration state, which is the state every existing Postgres
	// deployment is in.
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	for _, col := range []string{"judged_at", "judge_reason"} {
		if derr := d.exec(ctx, key, `ALTER TABLE chunk_memory_meta DROP COLUMN `+col); derr != nil {
			t.Fatalf("postgres: dropping %s: %v", col, derr)
		}
	}

	if _, r := docExec(t, d, ctx, `{"op":"judge_fact","scope":"user","id":"`+chunkID+
		`","verdict":"unsupported","reason":"nothing in the span says this"}`); r.IsError {
		t.Fatalf("postgres: the migration did not run: %s", r.Text)
	}
	// Withheld by default, readable with include_refuted — the same two answers the
	// sqlite tier gives, which is what "parity" has to mean.
	facts, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if r.IsError {
		t.Fatalf("postgres list_facts: %s", r.Text)
	}
	if n, _ := facts["count"].(float64); n != 0 {
		t.Errorf("postgres: a refuted fact was still listed (count %v)", facts["count"])
	}
	facts, r = docExec(t, d, ctx, `{"op":"list_facts","scope":"user","include_refuted":true}`)
	if r.IsError {
		t.Fatalf("postgres list_facts(include_refuted): %s", r.Text)
	}
	if n, _ := facts["count"].(float64); n != 1 {
		t.Fatalf("postgres: include_refuted did not return the withheld fact (count %v)", facts["count"])
	}
	entity, _ := facts["facts"].([]any)[0].(map[string]any)["entity"].(map[string]any)
	if entity["judge_reason"] != "nothing in the span says this" {
		t.Errorf("postgres: the reason did not survive the round trip: %v", entity)
	}
	// An UNJUDGED fact must stay visible on this tier too — the NULL fail-open is a SQL
	// predicate, and NULL handling is exactly where tiers differ.
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","natural_key":"fact:pg2","title":"Another claim.","body":"y"}`); r.IsError {
		t.Fatalf("upsert(pg) #2: %s", r.Text)
	}
	facts, _ = docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if n, _ := facts["count"].(float64); n != 1 {
		t.Errorf("postgres: an unjudged fact must stay visible, got count %v", facts["count"])
	}
}
