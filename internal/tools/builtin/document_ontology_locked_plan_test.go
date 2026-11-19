package builtin

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// TestOntologyPlan_LockNotesFollowTheRequestsEdits: the lock a plan reports is the one
// the request's own edits leave — an edit that removes @locked clears the note on a
// retire of the entry above, one that adds it raises it — and an edit whose body
// heading renames an untitled locked entry is noted.
func TestOntologyPlan_LockNotesFollowTheRequestsEdits(t *testing.T) {
	f := newLockedFixture(t)
	internal, policy, memo, co := f.chunk("Internal Policy"), f.chunk("Policy"), f.chunk("Memo"), f.chunk("co_supervises")
	if text := f.update(f.ctx, co, map[string]any{"title": "", "body": "# co_supervises\n@locked"}); text != "" {
		t.Fatalf("untitle co_supervises: %s", text)
	}
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{
		{Op: connector.OntologyPlanEdit, Entry: internal, Body: "An internal rule, no longer locked."},
		{Op: connector.OntologyPlanRetire, Entry: policy},
		{Op: connector.OntologyPlanEdit, Entry: memo, Body: "@locked\nA memo."},
		{Op: connector.OntologyPlanDelete, Entry: memo},
		{Op: connector.OntologyPlanEdit, Entry: co, Body: "# co_oversees\n@locked"},
	})
	if err != nil {
		t.Fatal(err)
	}
	note := func(i int) string { return strings.Join(plan.Changes[i].Notes, "\n") }
	if strings.Contains(note(1), entryLockedCode) {
		t.Errorf("retire of Policy after the edit that unlocks Internal Policy carries a lock note: %q", note(1))
	}
	wantLocked(t, "delete of Memo after the edit that locks it", note(3), `class "Memo" carries @locked`)
	wantLocked(t, "an edit whose heading renames co_supervises", note(4), `relation "co_supervises" carries @locked`)
	if strings.Contains(note(0), entryLockedCode) || strings.Contains(note(2), entryLockedCode) {
		t.Errorf("an edit that renames nothing carries a lock note: %q / %q", note(0), note(2))
	}
}
