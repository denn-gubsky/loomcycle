package builtin

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// verbatimFixture builds a Document with a working vector search (the same in-memory
// store + fake embedder the `related` tests use) and one document to hang facts on.
func verbatimFixture(t *testing.T) (*Document, context.Context, string, string) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	vs := newVectorStore(s)
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	emb := newFakeEmbedder("fake", "fake-embed-001",
		"github", "username", "denn", "postgres", "role", "createrole", "berlin", "lives")
	d := &Document{Store: vs, SqlMem: mgr, Bus: channels.NewBus(), Embedder: emb}
	ctx := tools.WithAgentName(context.Background(), "verbatim-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1"})

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts","path":"/v/one"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	return d, ctx, out["document_id"].(string), out["root_chunk_id"].(string)
}

// mkFact writes a fact and optionally judges it, returning the chunk id.
func mkFact(t *testing.T, d *Document, ctx context.Context, doc, root, nk, body, quote, verdict string) string {
	t.Helper()
	req := fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"parent_id":%q,`+
		`"natural_key":%q,"title":%q,"body":%q`, doc, root, nk, body, body)
	if quote != "" {
		req += fmt.Sprintf(`,"source_quote":%q`, quote)
	}
	out, r := docExec(t, d, ctx, req+`}`)
	if r.IsError {
		t.Fatalf("upsert %s: %s", nk, r.Text)
	}
	id := out["id"].(string)
	if verdict != "" {
		if _, jr := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"judge_fact","scope":"user","id":%q,"verdict":%q,"reason":"scripted"}`, id, verdict)); jr.IsError {
			t.Fatalf("judge %s: %s", nk, jr.Text)
		}
	}
	return id
}

// pgVerbatimFixture is verbatimFixture on the postgres SQL-Memory tier. No embedder and
// no vector store: the ops that need those are exercised on sqlite, and what this tier is
// here to prove is the COUNTING query.
func pgVerbatimFixture(t *testing.T) (*Document, context.Context, string, string) {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the verification-stats postgres-tier test")
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
	ctx := tools.WithAgentName(context.Background(), "pg-verbatim")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"PG facts"}`)
	if r.IsError {
		t.Fatalf("create_document(pg): %s", r.Text)
	}
	return d, ctx, asStr(out["document_id"]), asStr(out["root_chunk_id"])
}

// TestVerbatim_QuotesAVerifiedFactWithItsCitation is the feature: a lookup question
// answered with the stored claim and the span it was checked against, and no generated
// text anywhere in the path.
func TestVerbatim_QuotesAVerifiedFactWithItsCitation(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	id := mkFact(t, d, ctx, doc, root, "fact:gh",
		"The user's github username is denn.", "my github username is denn", "supported")

	out, r := docExec(t, d, ctx, `{"op":"verbatim_answer","scope":"user","query":"github username"}`)
	if r.IsError {
		t.Fatalf("verbatim_answer: %s", r.Text)
	}
	if ok, _ := out["answered"].(bool); !ok {
		t.Fatalf("a verified fact was not quoted: %v", out)
	}
	if got, _ := out["answer"].(string); got != "The user's github username is denn." {
		t.Errorf("answer = %q, want the stored claim VERBATIM", got)
	}
	// The citation is the point: an answer with no source is just a shorter generation.
	if got, _ := out["source"].(string); got != "my github username is denn" {
		t.Errorf("source = %q, want the span it was verified against", got)
	}
	if got, _ := out["chunk_id"].(string); got != id {
		t.Errorf("chunk_id = %q, want %q", got, id)
	}
}

// TestVerbatim_RefusesAnUnverifiedFact.
//
// The whole reason this is the last phase. An unverified claim quoted verbatim reads as
// authority it has not earned — worse than synthesising the same claim, because a
// generated sentence invites the doubt a quotation suppresses.
func TestVerbatim_RefusesAnUnverifiedFact(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	mkFact(t, d, ctx, doc, root, "fact:gh",
		"The user's github username is denn.", "my github username is denn", "") // never judged

	out, r := docExec(t, d, ctx, `{"op":"verbatim_answer","scope":"user","query":"github username"}`)
	if r.IsError {
		t.Fatalf("verbatim_answer: %s", r.Text)
	}
	if ok, _ := out["answered"].(bool); ok {
		t.Fatalf("an unverified fact was quoted as authority: %v", out)
	}
	// The reason has to be actionable: "nothing here" and "here but unchecked" lead to
	// different next steps (write it down vs run the judge).
	if reason, _ := out["reason"].(string); !strings.Contains(reason, "not been verified") {
		t.Errorf("the refusal does not say the fact is merely unverified: %q", reason)
	}
}

// TestVerbatim_RefusesARefutedFact. A fact the judge threw out must never come back as
// an answer — it is withheld from the fact surfaces precisely so it stops being asserted.
func TestVerbatim_RefusesARefutedFact(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	mkFact(t, d, ctx, doc, root, "fact:gh",
		"The user's github username is denn.", "my github username is denn", "unsupported")

	out, r := docExec(t, d, ctx, `{"op":"verbatim_answer","scope":"user","query":"github username"}`)
	if r.IsError {
		t.Fatalf("verbatim_answer: %s", r.Text)
	}
	if ok, _ := out["answered"].(bool); ok {
		t.Fatalf("a REFUTED fact was quoted: %v", out)
	}
}

// TestVerbatim_RefusesWhenTwoFactsMatchAboutEqually.
//
// Two facts that both answer the question are not an answer; they are a question about
// which one is meant. Quoting either as authority is the confident-wrong failure this op
// is shaped against, and it is the failure a caller cannot detect downstream.
func TestVerbatim_RefusesWhenTwoFactsMatchAboutEqually(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	// Same tokens, so the fake embedder scores them identically.
	mkFact(t, d, ctx, doc, root, "fact:gh1",
		"The user's github username is denn.", "my github username is denn", "supported")
	mkFact(t, d, ctx, doc, root, "fact:gh2",
		"The user's github username is denn.", "it is denn on github", "supported")

	out, r := docExec(t, d, ctx, `{"op":"verbatim_answer","scope":"user","query":"github username"}`)
	if r.IsError {
		t.Fatalf("verbatim_answer: %s", r.Text)
	}
	if ok, _ := out["answered"].(bool); ok {
		t.Fatalf("one of two equally-matching facts was quoted as THE answer: %v", out)
	}
	if reason, _ := out["reason"].(string); !strings.Contains(reason, "about equally well") {
		t.Errorf("the refusal does not name the ambiguity: %q", reason)
	}
	if _, has := out["runner_up"]; !has {
		t.Error("the runner-up is not reported, so a caller cannot see what it collided with")
	}
}

// TestVerbatim_NeverSkipsPastABetterUnverifiedMatch.
//
// The subtle one. If the best-matching fact is unverified and a WORSE match is verified,
// answering with the worse one is answering with something we know is not the closest
// thing we have — verified, and wrong. Silence is correct.
func TestVerbatim_NeverSkipsPastABetterUnverifiedMatch(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	// The unverified fact matches the query on both tokens; the verified one on one.
	mkFact(t, d, ctx, doc, root, "fact:best",
		"The user's github username is denn.", "my github username is denn", "")
	mkFact(t, d, ctx, doc, root, "fact:worse",
		"The user's postgres role needs createrole.", "the role needs createrole", "supported")

	out, r := docExec(t, d, ctx, `{"op":"verbatim_answer","scope":"user","query":"github username"}`)
	if r.IsError {
		t.Fatalf("verbatim_answer: %s", r.Text)
	}
	if ok, _ := out["answered"].(bool); ok {
		t.Fatalf("answered with a worse-matching fact because it happened to be verified: %v", out)
	}
	// It must refuse because the TOP fact is unverified — not because nothing matched.
	// Without this the test would pass on an embedder that scored everything below the
	// floor, which proves nothing about the rule it is named for.
	if reason, _ := out["reason"].(string); !strings.Contains(reason, "not been verified") {
		t.Errorf("refused for the wrong reason (%q); the unverified top match is the point", reason)
	}
	if id, _ := out["chunk_id"].(string); id == "" {
		t.Error("no chunk_id reported, so the caller cannot see which fact blocked the answer")
	}
}

// TestVerbatim_RefusesAWeakMatch. A store with nothing relevant must say so rather than
// quoting the least-bad thing it holds.
func TestVerbatim_RefusesAWeakMatch(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	mkFact(t, d, ctx, doc, root, "fact:pg",
		"The user's postgres role needs createrole.", "the role needs createrole", "supported")

	out, r := docExec(t, d, ctx, `{"op":"verbatim_answer","scope":"user","query":"berlin lives"}`)
	if r.IsError {
		t.Fatalf("verbatim_answer: %s", r.Text)
	}
	if ok, _ := out["answered"].(bool); ok {
		t.Fatalf("an unrelated fact was quoted as the answer: %v", out)
	}
	// It must refuse on the FLOOR, and report the score and the floor with it — that
	// pair is what lets an operator tell a mis-calibrated threshold (the score is high,
	// the floor is higher) from a store that simply holds nothing relevant.
	if reason, _ := out["reason"].(string); !strings.Contains(reason, "not close enough") {
		t.Errorf("refused for the wrong reason: %q", reason)
	}
	if _, has := out["score"]; !has {
		t.Errorf("no score reported on a floor refusal: %v", out)
	}
	if _, has := out["min_score"]; !has {
		t.Errorf("no min_score reported, so the floor cannot be judged: %v", out)
	}
}

// TestVerbatim_DocumentProseNeitherAnswersNorBlocks.
//
// A document chunk is prose, not a claim: nothing asserted it and it has no span to cite,
// so it cannot be an answer. It must also not BLOCK one — a store that holds documents
// alongside facts is every store, and letting prose out-rank a fact would make this
// feature useless exactly where it is useful.
func TestVerbatim_DocumentProseNeitherAnswersNorBlocks(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":"Notes",`+
			`"body":"Notes about the github username convention we use."}`, doc, root)); r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	mkFact(t, d, ctx, doc, root, "fact:gh",
		"The user's github username is denn.", "my github username is denn", "supported")

	out, r := docExec(t, d, ctx, `{"op":"verbatim_answer","scope":"user","query":"github username"}`)
	if r.IsError {
		t.Fatalf("verbatim_answer: %s", r.Text)
	}
	if ok, _ := out["answered"].(bool); !ok {
		t.Fatalf("document prose blocked a verified fact from answering: %v", out)
	}
	if got, _ := out["answer"].(string); !strings.Contains(got, "username is denn") {
		t.Errorf("answered with prose instead of the fact: %q", got)
	}
}

// TestVerificationStats_ReportsTheGateNumber.
//
// The phase this op belongs to is gated on "verified facts are the norm rather than the
// exception", and nothing could report that — so the decision would have been made on
// impression, which is the habit this line exists to replace. The populations are counted
// separately because they mean different things: a fact with no span can never be
// verified by anyone, while one merely awaiting a judge can.
func TestVerificationStats_ReportsTheGateNumber(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	mkFact(t, d, ctx, doc, root, "f:1", "Claim one.", "quote one", "supported")
	mkFact(t, d, ctx, doc, root, "f:2", "Claim two.", "quote two", "unsupported")
	mkFact(t, d, ctx, doc, root, "f:3", "Claim three.", "quote three", "")
	mkFact(t, d, ctx, doc, root, "f:4", "Claim four.", "", "")

	out, r := docExec(t, d, ctx, `{"op":"verification_stats","scope":"user"}`)
	if r.IsError {
		t.Fatalf("verification_stats: %s", r.Text)
	}
	for field, want := range map[string]float64{
		"facts": 4, "with_span": 3, "judged": 2, "supported": 1, "withheld": 1,
		"unverifiable_no_span": 1, "awaiting_judge": 1,
	} {
		if got, _ := out[field].(float64); got != want {
			t.Errorf("%s = %v, want %v (full report: %v)", field, out[field], want, out)
		}
	}
	if got, _ := out["verified_share"].(float64); got != 0.25 {
		t.Errorf("verified_share = %v, want 0.25", got)
	}
}

// TestVerificationStats_EmptyStoreReportsZerosNotNulls.
//
// `sum()` over zero rows is NULL, not 0, on both tiers — so a scope with no facts is
// exactly where a counting query returns a shape nobody expected. An operator checking
// the gate on a fresh deployment must get zeros and no division by zero.
func TestVerificationStats_EmptyStoreReportsZerosNotNulls(t *testing.T) {
	d, ctx, _, _ := verbatimFixture(t)

	out, r := docExec(t, d, ctx, `{"op":"verification_stats","scope":"user"}`)
	if r.IsError {
		t.Fatalf("verification_stats on an empty store: %s", r.Text)
	}
	if got, _ := out["facts"].(float64); got != 0 {
		t.Errorf("facts = %v, want 0", out["facts"])
	}
	for _, field := range []string{"with_span", "judged", "supported", "withheld"} {
		if v, ok := out[field]; ok {
			if n, isNum := v.(float64); !isNum || n != 0 {
				t.Errorf("%s = %v on an empty store, want 0 or absent", field, v)
			}
		}
	}
	// No share at all rather than 0/0. A reported 0.00 would read as "nothing is
	// verified", which is a different claim from "there is nothing to verify".
	if _, has := out["verified_share"]; has {
		t.Errorf("an empty store reported a verified_share (%v); there is no share of nothing",
			out["verified_share"])
	}
}

// TestVerbatim_EmptyStoreSaysSoRatherThanFailing. The first thing a consumer will do is
// call this against a store that has nothing yet.
func TestVerbatim_EmptyStoreSaysSoRatherThanFailing(t *testing.T) {
	d, ctx, _, _ := verbatimFixture(t)

	out, r := docExec(t, d, ctx, `{"op":"verbatim_answer","scope":"user","query":"github username"}`)
	if r.IsError {
		t.Fatalf("verbatim_answer on an empty store errored: %s", r.Text)
	}
	if ok, _ := out["answered"].(bool); ok {
		t.Fatalf("an empty store produced an answer: %v", out)
	}
	if reason, _ := out["reason"].(string); !strings.Contains(reason, "no stored fact") {
		t.Errorf("the refusal does not say the store is empty: %q", reason)
	}
}

// TestVerificationStats_PostgresTier.
//
// The counting query is where the tiers diverge without saying so. `sum(CASE WHEN ...)`
// comes back as a different SQL type on Postgres than on sqlite, and a scan that quietly
// failed to read it would report ZEROS — not an error, a plausible-looking gate number
// saying nothing in the store is verified. A decision gets made on that number.
//
// The float literals in the CASE arms are the second reason: they are rendered from the
// confidence constants into SQL text, and Postgres compares a double against a numeric
// literal by its own rules.
func TestVerificationStats_PostgresTier(t *testing.T) {
	d, ctx, docID, root := pgVerbatimFixture(t)
	mkFact(t, d, ctx, docID, root, "f:1", "Claim one.", "quote one", "supported")
	mkFact(t, d, ctx, docID, root, "f:2", "Claim two.", "quote two", "unsupported")
	mkFact(t, d, ctx, docID, root, "f:3", "Claim three.", "quote three", "")
	mkFact(t, d, ctx, docID, root, "f:4", "Claim four.", "", "")

	out, r := docExec(t, d, ctx, `{"op":"verification_stats","scope":"user"}`)
	if r.IsError {
		t.Fatalf("verification_stats(pg): %s", r.Text)
	}
	for field, want := range map[string]float64{
		"facts": 4, "with_span": 3, "judged": 2, "supported": 1, "withheld": 1,
		"unverifiable_no_span": 1, "awaiting_judge": 1,
	} {
		if got, _ := out[field].(float64); got != want {
			t.Errorf("postgres: %s = %v, want %v (full report: %v)", field, out[field], want, out)
		}
	}
	if got, _ := out["verified_share"].(float64); got != 0.25 {
		t.Errorf("postgres: verified_share = %v, want 0.25", got)
	}
}

// mkIdentityNode writes an entity IDENTITY node — the subject a fact is about — and
// links a fact to it with the `about` edge the consolidation pass uses. It carries no
// span, because a subject is a name and there is nothing in it for a quote to support.
func mkIdentityNode(t *testing.T, d *Document, ctx context.Context, doc, root, factID, nk, title string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","scope":"user","document_id":%q,"parent_id":%q,`+
			`"natural_key":%q,"title":%q,"type":"object","subject":%q}`, doc, root, nk, title, title))
	if r.IsError {
		t.Fatalf("upsert identity %s: %s", nk, r.Text)
	}
	id := out["id"].(string)
	if _, lr := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"about"}`,
		factID, id)); lr.IsError {
		t.Fatalf("link about %s: %s", nk, lr.Text)
	}
	return id
}

// TestVerificationStats_IgnoresEntityIdentityNodes.
//
// The consolidation pass writes an identity node beside each fact, and both carry entity
// metadata, so both landed in the count. Because an identity node can NEVER carry a span,
// every new subject permanently lowered the reported share — the number got worse as the
// store got richer. Measured on a live store before the fix: 0.579 reported where 0.846
// was true, and 7 facts called impossible to verify where the answer was 1.
func TestVerificationStats_IgnoresEntityIdentityNodes(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	f1 := mkFact(t, d, ctx, doc, root, "memory/fact/1", "Claim one.", "quote one", "supported")
	f2 := mkFact(t, d, ctx, doc, root, "memory/fact/2", "Claim two.", "quote two", "")
	mkIdentityNode(t, d, ctx, doc, root, f1, "object:thing-one", "thing one")
	mkIdentityNode(t, d, ctx, doc, root, f2, "object:thing-two", "thing two")

	out, r := docExec(t, d, ctx, `{"op":"verification_stats","scope":"user"}`)
	if r.IsError {
		t.Fatalf("verification_stats: %s", r.Text)
	}
	for field, want := range map[string]float64{
		"facts": 2, "with_span": 2, "judged": 1, "supported": 1,
		// The number the bug inflated most: both identity nodes looked like facts whose
		// evidence had been lost.
		"unverifiable_no_span": 0, "awaiting_judge": 1,
	} {
		if got, _ := out[field].(float64); got != want {
			t.Errorf("%s = %v, want %v (full report: %v)", field, out[field], want, out)
		}
	}
	if got, _ := out["verified_share"].(float64); got != 0.5 {
		t.Errorf("verified_share = %v, want 0.5 (1 of 2 claims, not 1 of 4 rows)", got)
	}
}

// TestVerificationStats_CountsAFactWhoseChunkTypeIsNotFact.
//
// The trap that makes the obvious fix wrong, pinned as a test. A distilled fact's chunk
// type is the constant "fact", so `type = 'fact'` reads like the natural way to exclude
// identity nodes — but `remember` stamps the CALLER's type, and one operator-remembered
// fact on a live store landed as type `object` while carrying a span. Filtering on the
// type would have dropped a verified fact out of the coverage figure, which is the error
// direction that loses data rather than the one that merely dilutes.
func TestVerificationStats_CountsAFactWhoseChunkTypeIsNotFact(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	// A remembered fact: typed by its caller, cites itself, and nothing is `about` it.
	if _, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","scope":"user","document_id":%q,"parent_id":%q,`+
			`"natural_key":"memory/operator/remembered","title":"Remembered.","body":"Remembered.",`+
			`"source_quote":"Remembered.","type":"object","class":"evidential"}`, doc, root)); r.IsError {
		t.Fatalf("upsert remembered fact: %s", r.Text)
	}
	out, r := docExec(t, d, ctx, `{"op":"verification_stats","scope":"user"}`)
	if r.IsError {
		t.Fatalf("verification_stats: %s", r.Text)
	}
	if got, _ := out["facts"].(float64); got != 1 {
		t.Errorf("facts = %v, want 1 — a remembered fact typed `object` is still a fact (report: %v)",
			out["facts"], out)
	}
	if got, _ := out["with_span"].(float64); got != 1 {
		t.Errorf("with_span = %v, want 1 — it cites itself", out["with_span"])
	}
}

// TestListFacts_ClaimsOnlyIsOptInAndTheDefaultStaysWIDE.
//
// Both directions in one test on purpose. list_facts' contract is "chunks that carry
// entity metadata", and the document-federation reconcile depends on that breadth — it
// syncs identity nodes too — so narrowing the DEFAULT would silently stop federating
// them. The opt-in is for the surfaces that read facts to a person or count coverage.
func TestListFacts_ClaimsOnlyIsOptInAndTheDefaultStaysWIDE(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	f1 := mkFact(t, d, ctx, doc, root, "memory/fact/1", "Claim one.", "quote one", "supported")
	mkIdentityNode(t, d, ctx, doc, root, f1, "object:thing-one", "thing one")

	out, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if r.IsError {
		t.Fatalf("list_facts: %s", r.Text)
	}
	if got := len(out["facts"].([]any)); got != 2 {
		t.Errorf("default list_facts returned %d rows, want 2 (the claim AND the identity node — sync needs both)", got)
	}

	out, r = docExec(t, d, ctx, `{"op":"list_facts","scope":"user","claims_only":true}`)
	if r.IsError {
		t.Fatalf("list_facts claims_only: %s", r.Text)
	}
	rows := out["facts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("claims_only returned %d rows, want 1", len(rows))
	}
	if title, _ := rows[0].(map[string]any)["title"].(string); title != "Claim one." {
		t.Errorf("claims_only kept %q, want the claim", title)
	}
}

// TestVerificationStats_PostgresTier_IgnoresEntityIdentityNodes.
//
// The exclusion is a correlated NOT EXISTS subquery, which is exactly the kind of SQL
// that can behave differently per tier — and the failure mode is a plausible number, not
// an error: an anti-join that matched nothing would report the pre-fix figure and an
// anti-join that matched everything would report zero facts on a full store.
func TestVerificationStats_PostgresTier_IgnoresEntityIdentityNodes(t *testing.T) {
	d, ctx, docID, root := pgVerbatimFixture(t)
	f1 := mkFact(t, d, ctx, docID, root, "memory/fact/1", "Claim one.", "quote one", "supported")
	f2 := mkFact(t, d, ctx, docID, root, "memory/fact/2", "Claim two.", "quote two", "")
	mkIdentityNode(t, d, ctx, docID, root, f1, "object:thing-one", "thing one")
	mkIdentityNode(t, d, ctx, docID, root, f2, "object:thing-two", "thing two")

	out, r := docExec(t, d, ctx, `{"op":"verification_stats","scope":"user"}`)
	if r.IsError {
		t.Fatalf("verification_stats(pg): %s", r.Text)
	}
	for field, want := range map[string]float64{
		"facts": 2, "with_span": 2, "judged": 1, "supported": 1, "unverifiable_no_span": 0,
	} {
		if got, _ := out[field].(float64); got != want {
			t.Errorf("postgres: %s = %v, want %v (full report: %v)", field, out[field], want, out)
		}
	}
	if got, _ := out["verified_share"].(float64); got != 0.5 {
		t.Errorf("postgres: verified_share = %v, want 0.5", got)
	}
}
