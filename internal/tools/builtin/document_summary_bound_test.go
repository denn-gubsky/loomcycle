package builtin

import (
	"encoding/json"
	"fmt"
	"testing"
)

// documents_summary must bound its response (RFC CV OQ2).
//
// under_path resolves to EVERY document under a subtree, and subject-homed facts
// make /facts exactly as large as the tenant's entity count — so an unbounded call
// built a ten-thousand-element IN list and returned ten thousand rows. Postgres
// serves that; an agent's context window does not, which is the same reason
// `path op=ls` is paged.

// TestDocumentsSummary_BoundsAnUnboundedSubtree.
func TestDocumentsSummary_BoundsAnUnboundedSubtree(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	const n = 12
	for i := 0; i < n; i++ {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_document","scope":"user","title":"subject-%02d","path":"/facts/subject-%02d"}`, i, i)); r.IsError {
			t.Fatalf("create_document %d: %s", i, r.Text)
		}
	}

	// Unbounded by default? No — but the default is far above this fixture, so the
	// bound is exercised with an explicit small limit AND the default is checked to
	// return everything, which is what says the default did not silently clip.
	out, r := docExec(t, d, ctx, `{"op":"documents_summary","scope":"user","under_path":"/facts"}`)
	if r.IsError {
		t.Fatalf("documents_summary: %s", r.Text)
	}
	var all struct {
		Documents []json.RawMessage `json:"documents"`
		Truncated bool              `json:"truncated"`
	}
	_ = json.Unmarshal([]byte(r.Text), &all)
	if len(all.Documents) != n {
		t.Errorf("default returned %d of %d documents — the default bound must be well above "+
			"an ordinary subtree, or it clips silently; out=%v", len(all.Documents), n, out)
	}
	if all.Truncated {
		t.Errorf("a subtree inside the default bound reported truncated — the flag must mean " +
			"something was actually clipped")
	}

	// Now the bound itself.
	_, r = docExec(t, d, ctx, `{"op":"documents_summary","scope":"user","under_path":"/facts","limit":5}`)
	if r.IsError {
		t.Fatalf("documents_summary limited: %s", r.Text)
	}
	var clipped struct {
		Documents []json.RawMessage `json:"documents"`
		Truncated bool              `json:"truncated"`
		Note      string            `json:"note"`
	}
	_ = json.Unmarshal([]byte(r.Text), &clipped)
	if len(clipped.Documents) != 5 {
		t.Errorf("limit=5 returned %d documents", len(clipped.Documents))
	}
	// REPORTED, not silent. A bound a caller cannot detect is indistinguishable
	// from a store that holds less than it does.
	if !clipped.Truncated {
		t.Error("the response was clipped without saying so — that is a lie, not a bound")
	}
	if clipped.Note == "" {
		t.Error("truncation carries no note saying how to get the rest")
	}
}

// TestDocumentsSummary_ExplicitIdsAreStillHonoured: the explorer passes a page of
// ids it already holds, and bounding must not break that path — which is why the
// bound is a default rather than a refusal to answer an unbounded call.
func TestDocumentsSummary_ExplicitIdsAreStillHonoured(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	var ids []string
	for i := 0; i < 3; i++ {
		out, r := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_document","scope":"user","title":"d-%d","path":"/docs/d-%d"}`, i, i))
		if r.IsError {
			t.Fatalf("create_document: %s", r.Text)
		}
		ids = append(ids, asStr(out["document_id"]))
	}
	body, _ := json.Marshal(map[string]any{
		"op": "documents_summary", "scope": "user", "document_ids": ids,
	})
	_, r := docExec(t, d, ctx, string(body))
	if r.IsError {
		t.Fatalf("documents_summary by ids: %s", r.Text)
	}
	var got struct {
		Documents []json.RawMessage `json:"documents"`
		Truncated bool              `json:"truncated"`
	}
	_ = json.Unmarshal([]byte(r.Text), &got)
	if len(got.Documents) != len(ids) {
		t.Errorf("asked for %d ids, got %d — bounding must not clip a caller's own page", len(ids), len(got.Documents))
	}
	if got.Truncated {
		t.Error("a caller's own page reported truncated")
	}
}
