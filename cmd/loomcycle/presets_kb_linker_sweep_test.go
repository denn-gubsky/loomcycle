package main

import (
	"strings"
	"testing"
	"time"
)

// The maintenance sweep and the purge (RFC FP Phase D, owner decision D1: the purge
// is reported always and deletes only on purge: true).

// sweepFixture: the target act sits at /acts/target, the corpus law at /laws/act.
//   - st-old  (c1 → x1, verified at 10) and st-new (c1 → x2, verified at 500): under /acts;
//   - st-flag (c1 → x1 by extractor, flagged): under /acts, waiting for a person;
//   - st-out  (x1 → x2): both ends under /laws, outside the sweep.
func sweepFixture() *kbFixture {
	f := maintainFixture()
	old := citeStatement("st-old", "c1", "x1", "report a material incident")
	old.VerifiedAt = 10
	newer := citeStatement("st-new", "c1", "x2", "to the regulator")
	newer.VerifiedAt = 500
	flagged := citeStatement("st-flag", "c1", "x1", "within 24 hours")
	flagged.Kind, flagged.Method, flagged.Attributes = "uses_term", "extractor:relate", map[string]any{"needs_review": true}
	out := citeStatement("st-out", "x1", "x2")
	f.statements = []*kbStatement{newer, flagged, old, out}
	return f
}

// TestKBLinker_SweepChecksTheSubtreeLeastRecentlyVerifiedFirst: a sweep over /acts
// checks the statements with an end there, least recently verified first and no more
// than max_statements, says it did not finish, passes over a statement already
// flagged for a person, and never touches one outside the subtree.
func TestKBLinker_SweepChecksTheSubtreeLeastRecentlyVerifiedFirst(t *testing.T) {
	f := sweepFixture()

	rep, _ := runLinker(t, f, `{"mode":"sweep","path":"/acts","write":"propose","max_statements":2}`)

	if got := actionsOf(f, "st-old"); got != "verify" {
		t.Errorf("st-old actions = %q, want verify (the least recently verified)", got)
	}
	for _, id := range []string{"st-new", "st-out", "st-flag"} {
		if got := actionsOf(f, id); got != "" {
			t.Errorf("%s actions = %q, want none", id, got)
		}
	}
	L := layerReport(t, rep, "L6")
	sw, _ := L["sweep"].(map[string]any)
	if sw["path"] != "/acts" || sw["exhausted"] != false {
		t.Errorf("sweep = %v, want path /acts and not exhausted", sw)
	}
	if n := layerCount(t, rep, "L6", "skipped_flagged"); n != 1 {
		t.Errorf("skipped_flagged = %v, want 1", n)
	}
	for _, c := range f.callsOf("Document", "list_statements") {
		if c.Input["under_path"] != "/acts" || c.Input["document_id"] != nil {
			t.Errorf("list_statements = %v, want under_path /acts only", c.Input)
		}
	}
}

// purgeFixture holds retired statements under /acts:
//   - st-gone-old (rule, expired in 1970): a purge candidate;
//   - st-gone-new (rule, expired now): too recent;
//   - st-gone-hand (manual, expired in 1970): a person's, never purged;
//   - st-rejected (rule, rejected): an operator's ruling, never listed for purge.
func purgeFixture() *kbFixture {
	f := maintainFixture()
	mk := func(id, method string, at int64) *kbStatement {
		s := citeStatement(id, "c1", "x1", "report a material incident")
		s.Method, s.Expired, s.ExpiredAt, s.Status = method, true, at, "proposed"
		return s
	}
	rejected := citeStatement("st-rejected", "c1", "x2", "to the regulator")
	rejected.Status = "rejected"
	f.statements = []*kbStatement{
		mk("st-gone-old", "rule:citation", 1),
		mk("st-gone-new", "rule:citation", time.Now().UnixNano()),
		mk("st-gone-hand", "manual", 1),
		rejected,
	}
	return f
}

// TestKBLinker_SweepPurgeReportsAndDeletesOnlyWhenAsked: a sweep always reports what
// a purge would delete, and deletes it only with purge: true — and then only a
// code-made statement retired before purge_after_days, never a person's, a recent
// one or a rejected one.
func TestKBLinker_SweepPurgeReportsAndDeletesOnlyWhenAsked(t *testing.T) {
	f := purgeFixture()
	rep, _ := runLinker(t, f, `{"mode":"sweep","path":"/acts","write":"propose"}`)
	if n := len(f.callsOf("Document", "unlink_chunks")); n != 0 {
		t.Errorf("unlink calls without purge = %d, want 0", n)
	}
	P, _ := layerReport(t, rep, "L6")["purge"].(map[string]any)
	if P["enabled"] != false || P["candidates"] != float64(1) || P["purged"] != float64(0) || P["kept_person"] != float64(1) {
		t.Errorf("purge = %v, want 1 candidate, 0 purged, 1 kept for a person, not enabled", P)
	}

	g := purgeFixture()
	rep, _ = runLinker(t, g, `{"mode":"sweep","path":"/acts","write":"propose","purge":true}`)
	calls := g.callsOf("Document", "unlink_chunks")
	if len(calls) != 1 || calls[0].Input["id"] != "st-gone-old" {
		t.Errorf("unlink calls = %v, want only st-gone-old", calls)
	}
	if P, _ := layerReport(t, rep, "L6")["purge"].(map[string]any); P["purged"] != float64(1) {
		t.Errorf("purge = %v, want 1 purged", P)
	}
	if g.statementByID("st-rejected") == nil || g.statementByID("st-gone-hand") == nil || g.statementByID("st-gone-new") == nil {
		t.Error("a statement that must be kept was deleted")
	}
}

// TestKBLinker_SweepAndPurgeInputsAreChecked: a sweep names a subtree, never a
// document; the purge belongs to a sweep and, since it deletes, to a propose run.
func TestKBLinker_SweepAndPurgeInputsAreChecked(t *testing.T) {
	for _, in := range []string{
		`{"mode":"sweep"}`,
		`{"mode":"sweep","document_id":"doc-target"}`,
		`{"mode":"sweep","path":"/acts","purge":true}`,
		`{"mode":"maintain","document_id":"doc-target","write":"propose","purge":true}`,
		`{"mode":"maintain","document_id":"doc-target","purge_after_days":30}`,
		`{"mode":"sweep","path":"/acts","purge_after_days":0}`,
	} {
		f := sweepFixture()
		rep, _ := runLinker(t, f, in)
		if rep["status"] != "refused" || len(f.calls) != 0 {
			t.Errorf("%s: status %v after %d calls, want refused before any", in, rep["status"], len(f.calls))
		}
		if r, _ := rep["reason"].(string); strings.TrimSpace(r) == "" {
			t.Errorf("%s: no reason given", in)
		}
	}
}
