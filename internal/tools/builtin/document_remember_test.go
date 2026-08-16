package builtin

import (
	"strings"
	"testing"
)

// TestRemember_StoresAFactThatCitesItself.
//
// The whole design in one assertion. A box labelled "remember X" is a natural-language
// write path into memory — the thing the verified-writes line was built to constrain — and
// written naively it produces a fact with no span: permanently unverifiable, and
// indistinguishable from a model's output once stored.
//
// Treating the operator's sentence as the SOURCE resolves that: the fact cites itself, so
// it is checkable rather than merely asserted.
func TestRemember_StoresAFactThatCitesItself(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	out, r := docExec(t, d, ctx, `{"op":"remember","scope":"user",`+
		`"text":"The user deploys on Fridays."}`)
	if r.IsError {
		t.Fatalf("remember: %s", r.Text)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no chunk id returned: %v", out)
	}

	got, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	if body, _ := got["body"].(string); body != "The user deploys on Fridays." {
		t.Errorf("body = %q, want the statement verbatim", body)
	}
	entity, _ := got["entity"].(map[string]any)
	if entity["source_quote"] != "The user deploys on Fridays." {
		t.Errorf("source_quote = %v, want the statement itself — a remembered fact must "+
			"carry its own evidence", entity["source_quote"])
	}
	// Source material, not a distillation: a person said it, and nothing else records
	// that they did, so age-based pruning must not reclaim it.
	if entity["class"] != "evidential" {
		t.Errorf("class = %v, want evidential", entity["class"])
	}
	// And it is NOT pre-judged. Self-citation makes it checkable, not checked — claiming
	// a verdict nobody reached is the failure this line exists to stop.
	if _, judged := entity["judged_at"]; judged {
		t.Errorf("a remembered fact arrived pre-judged: %v", entity)
	}
}

// TestRemember_DoesNotOverwriteADistilledFact.
//
// The consolidator keys a distilled fact `memory/fact/<slug>`; an operator typing the
// same sentence must land on a different row. The two are different claims about one
// subject — a machine's reading and a person's statement — and collapsing them loses
// whichever was written second, silently.
func TestRemember_DoesNotOverwriteADistilledFact(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"E","path":"/memory/entities"}`)
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)

	// What a pass would have written.
	distilled, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","natural_key":"memory/fact/the-user-deploys-on-fridays",`+
		`"title":"The user deploys on Fridays.","body":"The user deploys on Fridays.",`+
		`"source_quote":"we ship on fridays"}`)
	if r.IsError {
		t.Fatalf("seed: %s", r.Text)
	}
	distilledID, _ := distilled["id"].(string)

	remembered, r := docExec(t, d, ctx, `{"op":"remember","scope":"user",`+
		`"text":"The user deploys on Fridays."}`)
	if r.IsError {
		t.Fatalf("remember: %s", r.Text)
	}
	if remembered["id"] == distilledID {
		t.Fatal("the operator's statement overwrote the distilled fact")
	}
	// The distilled one keeps ITS span — the transcript sentence, not the operator's.
	got, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+distilledID+`"}`)
	entity, _ := got["entity"].(map[string]any)
	if entity["source_quote"] != "we ship on fridays" {
		t.Errorf("the distilled fact's evidence was replaced: %v", entity["source_quote"])
	}
}

// TestRemember_RefusesWhatIsNotAFact.
func TestRemember_RefusesWhatIsNotAFact(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	for _, tc := range []struct{ name, req, want string }{
		{"nothing to remember", `{"op":"remember","scope":"user","text":"   "}`, "missing required field"},
		{"a document, not a fact", `{"op":"remember","scope":"user","text":"` + strings.Repeat("x", 1200) + `"}`, "longer than a fact"},
	} {
		_, r := docExec(t, d, ctx, tc.req)
		if !r.IsError || !strings.Contains(r.Text, tc.want) {
			t.Errorf("%s: err=%v %s", tc.name, r.IsError, r.Text)
		}
	}
}

// TestRemember_KeyIsSafeToInterpolate.
//
// The natural key this mints is interpolated into SQL by the consolidation pass's
// key→id lookup, which guards on [a-z0-9:_/-]. A slug that could carry a quote would
// turn a remembered sentence into an injection site, so the character class is the
// invariant rather than the readability.
func TestRemember_KeyIsSafeToInterpolate(t *testing.T) {
	for _, in := range []string{
		`Robert'); DROP TABLE chunks;--`,
		`The user's "favourite" tool is ` + "`bash`" + `.`,
		"tabs\tand\nnewlines",
		"ünïcodé ïs fïne",
	} {
		got := slugForKey(in)
		for _, r := range got {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				t.Errorf("slugForKey(%q) = %q — %q is outside the safe class", in, got, r)
			}
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("slugForKey(%q) = %q has a stray edge dash", in, got)
		}
	}
}
