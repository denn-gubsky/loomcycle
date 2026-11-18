package builtin

// Regression tests for the review findings on the @focus mirror (RFC GM P1.1): which
// existing statements stand in for the ontology's, and what a retire or revive tells.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// linkFocusByHand writes a person's focus statement from the provider concept to the
// provider class, with extra fields merged into the request, and returns its id.
func linkFocusByHand(t *testing.T, d *Document, ctx context.Context, ids focusFixtureIDs, extra string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"focus"%s}`,
		ids.providerConcept, ids.providerClass, extra))
	if r.IsError {
		t.Fatalf("a person's focus link: %s", refusalText(r))
	}
	return out["id"].(string)
}

func hasPrefixed(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// TestOntologyConfirm_DatedForeignFocusDoesNotCountAsKept: a person's focus statement
// for the same pair but with a valid_at has a different key, so it does not stand in
// for the ontology's undated one — the confirm writes the ontology's own statement.
func TestOntologyConfirm_DatedForeignFocusDoesNotCountAsKept(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	ids := focusIDs(t, d, ctx)
	linkFocusByHand(t, d, ctx, ids, `,"valid_at":1700000000000000000`)

	rep := confirmFocus(t, d, ctx)
	if rep.Kept != 0 || rep.Written != 2 || len(rep.Errors) != 0 {
		t.Errorf("report = %#v, want both foci written and nothing kept", rep)
	}
	if live := liveFocusEdges(ontologyFocusEdges(t, d, ctx)); live[ids.providerConcept] != ids.providerClass {
		t.Errorf("the ontology's own provider focus was not written: %v", live)
	}
}

// TestOntologyConfirm_ProposedForeignFocusIsAnError: a person's focus statement on the
// same key that is only proposed is not in force, so it is neither kept nor written
// over: the confirm reports it as an error, on the dry run too.
func TestOntologyConfirm_ProposedForeignFocusIsAnError(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	ids := focusIDs(t, d, ctx)
	id := linkFocusByHand(t, d, ctx, ids, "")
	if err := d.exec(ctx, mustTenantKey(t, d, ctx), `UPDATE chunk_edges SET status = ? WHERE id = ?`, edgeStatusProposed, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AdoptInstances(ctx, false); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		rep, err := d.MirrorSchemeFocus(ctx, dry)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Kept != 0 || rep.Written != 1 || !hasPrefixed(rep.Errors, "components/provider ") {
			t.Errorf("dry=%v report = %#v, want the proposed statement reported as an error", dry, rep)
		}
	}
}

// TestOntologyConfirm_RetiredForeignFocusIsAnErrorNotWritten: a person's retired focus
// statement on the same key is handed back by link_chunks unchanged, so writing would
// report a change that never happens, on every confirm. It is an error instead.
func TestOntologyConfirm_RetiredForeignFocusIsAnErrorNotWritten(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	ids := focusIDs(t, d, ctx)
	id := linkFocusByHand(t, d, ctx, ids, "")
	if err := d.exec(ctx, mustTenantKey(t, d, ctx), `UPDATE chunk_edges SET expired_at = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rep := confirmFocus(t, d, ctx)
		wantWritten := 1 // the Acme focus, first confirm only
		if i > 0 {
			wantWritten = 0
		}
		if rep.Written != wantWritten || rep.Kept != 0 || !hasPrefixed(rep.Errors, "components/provider ") {
			t.Errorf("confirm %d = %#v, want the retired statement reported as an error", i+1, rep)
		}
		if hasPrefixed(rep.Stale, "components/provider ") {
			t.Errorf("confirm %d listed the provider focus as a change: %v", i+1, rep.Stale)
		}
	}
}

// TestOntologyConfirm_RejectedFocusIsNotRevived: re-declaring a @focus whose ontology
// statement an operator rejected does not bring the rejected statement back as written.
func TestOntologyConfirm_RejectedFocusIsNotRevived(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	confirmFocus(t, d, ctx)
	ids := focusIDs(t, d, ctx)
	key := mustTenantKey(t, d, ctx)
	if err := d.exec(ctx, key, `UPDATE chunk_edges SET status = ? WHERE from_id = ? AND kind = 'focus'`,
		edgeStatusRejected, ids.acmeConcept); err != nil {
		t.Fatal(err)
	}
	setConceptBody(t, d, ctx, ids.acmeConcept, "No focus any more.")
	if rep := confirmFocus(t, d, ctx); rep.Retired != 1 {
		t.Fatalf("removal = %#v", rep)
	}
	setConceptBody(t, d, ctx, ids.acmeConcept, "@focus Acme Bank")
	rep := confirmFocus(t, d, ctx)
	if rep.Written != 0 || !hasPrefixed(rep.Errors, "components/acme-work ") {
		t.Errorf("re-declare of a rejected focus = %#v, want an error and nothing written", rep)
	}
	if live := liveFocusEdges(ontologyFocusEdges(t, d, ctx)); live[ids.acmeConcept] != "" {
		t.Errorf("the rejected statement was revived: %v", live)
	}
}

// focusEdgeEvents returns the statuses published for one statement, per change feed in
// publish order (the snapshot read orders by channel first).
func focusEdgeEvents(t *testing.T, st store.Store, edgeID string) map[string][]string {
	t.Helper()
	msgs, err := st.SnapshotReadChannelMessages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, m := range msgs {
		if !IsDocumentFeedChannel(m.Channel) {
			continue
		}
		var ev map[string]any
		if json.Unmarshal(m.Payload, &ev) == nil && ev["edge_id"] == edgeID {
			out[m.Channel] = append(out[m.Channel], fmt.Sprint(ev["status"]))
		}
	}
	return out
}

// TestOntologyConfirm_RetireAndReviveArePublished: retiring and reviving an ontology
// focus statement tell the change feeds, as every other statement change does.
func TestOntologyConfirm_RetireAndReviveArePublished(t *testing.T) {
	d, base, st := documentFixture(t)
	ctx := installTenantOntology(t, d, base, focusOntologyMD, memrank.OntologyConfirmedStatus)
	confirmFocus(t, d, ctx)
	ids := focusIDs(t, d, ctx)
	var acmeEdge string
	for _, e := range ontologyFocusEdges(t, d, ctx) {
		if e.from == ids.acmeConcept {
			acmeEdge = e.id
		}
	}
	before := focusEdgeEvents(t, st, acmeEdge)
	if len(before) == 0 {
		t.Fatal("the written statement was not published")
	}
	setConceptBody(t, d, ctx, ids.acmeConcept, "No focus any more.")
	confirmFocus(t, d, ctx)
	setConceptBody(t, d, ctx, ids.acmeConcept, "@focus Acme Bank")
	confirmFocus(t, d, ctx)
	for ch, got := range focusEdgeEvents(t, st, acmeEdge) {
		if added := strings.Join(got[len(before[ch]):], ","); added != "expired,asserted" {
			t.Errorf("%s: events after retire and revive = %q, want expired then asserted", ch, added)
		}
	}
}
