package snapshot

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
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
		// The promoter's subject, whose access-control set binds the walks:
		// a restore that dropped it would walk the team unnarrowed.
		"owned": {Isolated: true, ACLOwner: &authz.SubjectRef{TenantID: "acme", Subject: "dev", TokenDefID: "def_dev", Class: "member"}},
		// Promoted before the owner was recorded: it must come back unknown,
		// not as a recorded "nobody".
		"owner-unrecorded": {OperatorKeyRestricted: true, ACLOwnerUnrecorded: true},
	}
	for name, p := range promoters {
		row, err := src.TeamDefCreate(ctx, store.TeamDefRow{DefID: "tdf_" + name, Name: name, Definition: json.RawMessage(`{"entry":"a"}`)})
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if p == nil || p.ACLOwnerUnrecorded {
			_, err = src.SnapshotRestoreTeamDefActive(ctx, store.TeamDefActiveEntry{Name: name, DefID: row.DefID, PromotedAt: time.Now(), Promoter: p})
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
		case want != nil && (n.ActivePromoter == nil || !samePromoter(*n.ActivePromoter, *want)):
			t.Errorf("%s: restored with capture %+v, want %+v", n.Name, n.ActivePromoter, *want)
		}
	}
}

// samePromoter compares two captures by value, the owner included.
func samePromoter(a, b store.TeamDefPromoter) bool {
	if (a.ACLOwner == nil) != (b.ACLOwner == nil) || (a.ACLOwner != nil && *a.ACLOwner != *b.ACLOwner) {
		return false
	}
	a.ACLOwner, b.ACLOwner = nil, nil
	return a == b
}

// A snapshot taken before the promoter's subject was recorded carries
// neither acl_owner nor acl_owner_recorded on a captured promoter. It restores
// a pointer whose owner is UNKNOWN — what the store reads for a pointer
// promoted before migration 0121 — never a recorded "nobody".
func TestRestore_APromoterFromAnOlderSnapshotHasAnUnrecordedOwner(t *testing.T) {
	var old TeamDefPromoterEntry
	if err := json.Unmarshal([]byte(`{"operator_key_restricted":true,"isolated":false}`), &old); err != nil {
		t.Fatal(err)
	}
	got := promoterRow(&old)
	if got == nil || !got.ACLOwnerUnrecorded || got.ACLOwner != nil || !got.OperatorKeyRestricted {
		t.Errorf("an older snapshot's promoter restores as %+v, want the bits with an unrecorded owner", got)
	}
	// An owner present is kept even with the flag absent: dropping it would
	// walk the team unnarrowed.
	owner := &authz.SubjectRef{TenantID: "acme", Subject: "dev", Class: "member"}
	if got := promoterRow(&TeamDefPromoterEntry{ACLOwner: owner}); got == nil || got.ACLOwner != owner || got.ACLOwnerUnrecorded {
		t.Errorf("an entry with an owner and no flag restores as %+v, want the owner recorded", got)
	}
}
