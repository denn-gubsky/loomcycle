package snapshot

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestRoundTrip_AuthorshipSurvivesCaptureAndRestore closes a gap that shipped
// with the flag itself: operator_authored was added to both def planes but
// carried through neither snapshot section, so a capture→restore silently
// DEMOTED every operator-authored definition.
//
// The direction is safe — authority is lost, never gained — but the effect is
// an outage with extra steps. The flag gates the widened prompt-expansion
// families, so an operator restoring onto a new deployment would find their own
// definitions no longer resolving bindings that worked before the capture, with
// nothing in the restore result saying why.
//
// Both planes are checked in one test on purpose: they are the same field with
// the same meaning, and the failure mode is that somebody wires one and not the
// other.
func TestRoundTrip_AuthorshipSurvivesCaptureAndRestore(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()

	// One operator-authored and one agent-authored row on each plane, so the
	// test can tell "carried through" from "defaulted to true".
	agentRows := []store.AgentDefRow{
		{DefID: "adf_op", Name: "op-agent", Definition: json.RawMessage(`{"tier":"middle"}`), OperatorAuthored: true},
		{DefID: "adf_ag", Name: "ag-agent", Definition: json.RawMessage(`{"tier":"middle"}`)},
	}
	for _, r := range agentRows {
		if _, err := src.AgentDefCreate(ctx, r); err != nil {
			t.Fatalf("seed agent def %s: %v", r.DefID, err)
		}
	}
	teamRows := []store.TeamDefRow{
		{DefID: "tdf_op", Name: "op-team", Definition: json.RawMessage(`{"entry":"a"}`), OperatorAuthored: true},
		{DefID: "tdf_ag", Name: "ag-team", Definition: json.RawMessage(`{"entry":"a"}`)},
	}
	for _, r := range teamRows {
		if _, err := src.TeamDefCreate(ctx, r); err != nil {
			t.Fatalf("seed team def %s: %v", r.DefID, err)
		}
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for _, want := range agentRows {
		got, err := dst.AgentDefGet(ctx, want.DefID)
		if err != nil {
			t.Fatalf("restored agent def %s: %v", want.DefID, err)
		}
		if got.OperatorAuthored != want.OperatorAuthored {
			t.Errorf("agent def %s: operator_authored = %v after restore, want %v — a snapshot "+
				"that demotes a definition breaks bindings that worked before the capture",
				want.DefID, got.OperatorAuthored, want.OperatorAuthored)
		}
	}
	for _, want := range teamRows {
		got, err := dst.TeamDefGet(ctx, want.DefID)
		if err != nil {
			t.Fatalf("restored team def %s: %v", want.DefID, err)
		}
		if got.OperatorAuthored != want.OperatorAuthored {
			t.Errorf("team def %s: operator_authored = %v after restore, want %v",
				want.DefID, got.OperatorAuthored, want.OperatorAuthored)
		}
	}
}
