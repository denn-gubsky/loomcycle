package snapshot

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestRoundTrip_TeamPromoterSurvivesCaptureAndRestore: a team's active pointer
// carries its promoter's confinement, which is what an armed subscription's
// walks run under. A restore that dropped it would re-arm every captured team
// with no capture — fully confined until re-promoted, an outage — and one that
// defaulted it would re-arm a confined promoter's team unrestricted. An
// uncaptured pointer must come back uncaptured, not as a captured zero.
func TestRoundTrip_TeamPromoterSurvivesCaptureAndRestore(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()

	promoters := map[string]*store.TeamDefPromoter{
		"confined": {OperatorKeyRestricted: true, Isolated: true},
		"open":     {},
		"legacy":   nil, // promoted before the capture existed
	}
	for name, p := range promoters {
		row, err := src.TeamDefCreate(ctx, store.TeamDefRow{DefID: "tdf_" + name, Name: name, Definition: json.RawMessage(`{"entry":"a"}`)})
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if p == nil {
			_, err = src.SnapshotRestoreTeamDefActive(ctx, store.TeamDefActiveEntry{Name: name, DefID: row.DefID, PromotedAt: time.Now()})
		} else {
			err = src.TeamDefSetActive(ctx, "", name, row.DefID, "a_test", *p)
		}
		if err != nil {
			t.Fatalf("promote %s: %v", name, err)
		}
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	names, err := dst.TeamDefListNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != len(promoters) {
		t.Fatalf("restored %d teams, want %d", len(names), len(promoters))
	}
	for _, n := range names {
		want := promoters[n.Name]
		switch {
		case want == nil && n.ActivePromoter != nil:
			t.Errorf("%s: restored with capture %+v, want none", n.Name, *n.ActivePromoter)
		case want != nil && (n.ActivePromoter == nil || *n.ActivePromoter != *want):
			t.Errorf("%s: restored with capture %+v, want %+v", n.Name, n.ActivePromoter, *want)
		}
	}
}
