package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// export_canvas draws what holds: a proposal is not exported as a plain edge,
// where import_canvas would assert it.
func TestExportCanvas_LeavesOutAStatementThatIsNotLive(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, a, b, c := stmtDoc(t, d, ctx, "canvas")
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b)); r.IsError {
		t.Fatal(r.Text)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"blocks"}`, a, c)); r.IsError {
		t.Fatal(r.Text)
	}
	_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"export_canvas","scope":"user","document_id":%q}`, docID))
	if r.IsError {
		t.Fatal(r.Text)
	}
	if !strings.Contains(r.Text, `"cites"`) || strings.Contains(r.Text, `"blocks"`) {
		t.Errorf("canvas edges = %s, want the asserted cites and not the proposed blocks", r.Text)
	}
}

// An `about` statement an operator judged unsupported no longer makes a fact
// "about" its subject: not in list_facts about, not in the subject's references,
// and the claim it pointed at is a claim again.
func TestAboutReaders_IgnoreARefutedAboutStatement(t *testing.T) {
	assertAboutReadersDropTheFact(t, "a refuted", func(t *testing.T, d *Document, ctx context.Context, id string) {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"unsupported","reason":"the fact is about another Ada"}`, id)); r.IsError {
			t.Fatal(r.Text)
		}
	})
}

// An `about` statement retired when its fact was corrected (expired_at set, the
// row kept) no longer files the fact under its old subject.
func TestAboutReaders_IgnoreAnExpiredAboutStatement(t *testing.T) {
	assertAboutReadersDropTheFact(t, "an expired", func(t *testing.T, d *Document, ctx context.Context, id string) {
		key, _, _ := d.resolveScope(ctx, "user")
		if err := d.exec(ctx, key, `UPDATE chunk_edges SET expired_at = ? WHERE id = ?`, time.Now().UnixNano(), id); err != nil {
			t.Fatal(err)
		}
	})
}

// assertAboutReadersDropTheFact files a fact about a subject, retires its `about`
// statement with retire, and checks that list_facts about and the subject's
// references stop listing the fact.
func assertAboutReadersDropTheFact(t *testing.T, how string, retire func(*testing.T, *Document, context.Context, string)) {
	t.Helper()
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		ada := integritySubject(t, d, ctx, "ada")
		facts, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts","path":"/facts/misc"}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"m/ada","title":"Ada wrote notes",
			"body":"Ada wrote notes.","about_ids":[%q]}`, facts["document_id"], ada))
		if r.IsError {
			t.Fatal(r.Text)
		}
		fact := out["id"].(string)
		mentions := func(what, body string) bool {
			t.Helper()
			_, r := docExec(t, d, ctx, body)
			if r.IsError {
				t.Fatalf("%s: %s", what, r.Text)
			}
			return strings.Contains(r.Text, fact)
		}
		listAbout := fmt.Sprintf(`{"op":"list_facts","scope":"user","about":%q}`, ada)
		getSubject := `{"op":"get_document","scope":"user","path":"/facts/ada"}`
		if !mentions("list_facts", listAbout) || !mentions("get_document", getSubject) {
			t.Fatalf("staging: the fact is not about ada yet")
		}
		key, _, _ := d.resolveScope(ctx, "user")
		st, err := d.statementByKey(ctx, key, fact, ada, aboutEdgeKind, nil, "")
		if err != nil || st == nil {
			t.Fatalf("about statement: %v %v", st, err)
		}
		retire(t, d, ctx, st.ID)
		if mentions("list_facts", listAbout) {
			t.Errorf("list_facts about still follows %s about statement", how)
		}
		if mentions("get_document", getSubject) {
			t.Errorf("the subject's references still list a fact over %s about statement", how)
		}
	})
}

// A claim an `about` statement wrongly pointed at is a claim again once that
// statement is refuted: claims_only does not drop it as an identity node.
func TestClaimsOnly_KeepsAClaimWhoseAboutStatementIsRefuted(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	facts, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts","path":"/facts/claims"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	upsert := func(nk, body string, about ...string) string {
		t.Helper()
		ids := "[]"
		if len(about) > 0 {
			ids = fmt.Sprintf("[%q]", about[0])
		}
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":%q,"title":%q,"body":%q,"about_ids":%s}`,
			facts["document_id"], nk, body, body, ids))
		if r.IsError {
			t.Fatal(r.Text)
		}
		return out["id"].(string)
	}
	claim := upsert("m/two", "The archive closed in 1990.")
	wrong := upsert("m/one", "The archive moved.", claim)
	listed := func() bool {
		t.Helper()
		_, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","claims_only":true}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		return strings.Contains(r.Text, claim)
	}
	if listed() {
		t.Fatalf("staging: the claim should read as an identity node while the about statement holds")
	}
	key, _, _ := d.resolveScope(ctx, "user")
	st, err := d.statementByKey(ctx, key, wrong, claim, aboutEdgeKind, nil, "")
	if err != nil || st == nil {
		t.Fatalf("about statement: %v %v", st, err)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"unsupported","reason":"not about that"}`, st.ID)); r.IsError {
		t.Fatal(r.Text)
	}
	if !listed() {
		t.Errorf("claims_only still drops a claim whose only about statement was refuted")
	}
}
