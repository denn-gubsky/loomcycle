package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// testTeamDefActivePointerRecordsItsACLOwner: the promoter's subject — whose
// access-control set binds a subscription's walks — rides the active pointer
// in three states, on the name listing the sweep reads AND on the snapshot
// read: recorded (a subject), recorded as nobody (a promote with no subject),
// and UNKNOWN (a pointer promoted before the owner was recorded). Unknown must
// never read back as nobody, nor nobody as unknown, and a restore keeps each;
// every promote replaces the owner, as it replaces the bits.
func testTeamDefActivePointerRecordsItsACLOwner(t *testing.T, s store.Store) {
	ctx := context.Background()
	owner := &authz.SubjectRef{TenantID: "acme", Subject: "dev", TokenDefID: "def_dev", Class: "member"}
	read := func(name string) (listed, snap *store.TeamDefPromoter) {
		t.Helper()
		names, err := s.TeamDefListNames(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if n.Name == name {
				listed = n.ActivePromoter
			}
		}
		entries, err := s.SnapshotReadTeamDefActive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name == name {
				snap = e.Promoter
			}
		}
		if listed == nil || snap == nil {
			t.Fatalf("team %q: listed %+v, snapshot %+v; want a captured promoter on both", name, listed, snap)
		}
		return listed, snap
	}
	check := func(what, name string, wantOwner *authz.SubjectRef, wantUnrecorded bool) {
		t.Helper()
		listed, snap := read(name)
		for side, got := range map[string]*store.TeamDefPromoter{"listed": listed, "snapshot": snap} {
			ownerOK := (got.ACLOwner == nil) == (wantOwner == nil) && (wantOwner == nil || *got.ACLOwner == *wantOwner)
			if !ownerOK || got.ACLOwnerUnrecorded != wantUnrecorded || !got.Isolated {
				t.Errorf("%s (%s): %+v owner %+v; want owner %+v unrecorded=%v, isolated kept", what, side, got, got.ACLOwner, wantOwner, wantUnrecorded)
			}
		}
	}

	row, err := s.TeamDefCreate(ctx, mkTeamDef("td-owner-1", "team-acl-owner", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TeamDefSetActive(ctx, "", "team-acl-owner", row.DefID, "a_dev", store.TeamDefPromoter{Isolated: true, ACLOwner: owner}); err != nil {
		t.Fatal(err)
	}
	check("a promote by a subject", "team-acl-owner", owner, false)

	// A re-promote with no subject replaces the owner with nobody — recorded.
	// It also ignores an Unrecorded flag a caller passed: a promote records.
	if err := s.TeamDefSetActive(ctx, "", "team-acl-owner", row.DefID, "a_open", store.TeamDefPromoter{Isolated: true, ACLOwnerUnrecorded: true}); err != nil {
		t.Fatal(err)
	}
	check("a re-promote with no subject", "team-acl-owner", nil, false)

	// A pointer from before the owner was recorded, as a restore writes it.
	legacy, err := s.TeamDefCreate(ctx, mkTeamDef("td-owner-legacy", "team-acl-owner-legacy", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SnapshotRestoreTeamDefActive(ctx, store.TeamDefActiveEntry{
		Name: "team-acl-owner-legacy", DefID: legacy.DefID, PromotedAt: time.Now(),
		Promoter: &store.TeamDefPromoter{Isolated: true, ACLOwnerUnrecorded: true},
	}); err != nil {
		t.Fatal(err)
	}
	check("a pointer with an unrecorded owner", "team-acl-owner-legacy", nil, true)
	if err := s.TeamDefSetActive(ctx, "", "team-acl-owner-legacy", legacy.DefID, "a_dev", store.TeamDefPromoter{Isolated: true, ACLOwner: owner}); err != nil {
		t.Fatal(err)
	}
	check("re-promoting an unrecorded pointer", "team-acl-owner-legacy", owner, false)

	// Restores keep a recorded owner and a recorded nobody.
	for name, p := range map[string]*store.TeamDefPromoter{
		"team-acl-owner-restored":  {Isolated: true, ACLOwner: owner},
		"team-acl-nobody-restored": {Isolated: true},
	} {
		r, err := s.TeamDefCreate(ctx, mkTeamDef("td-"+name, name, ""))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SnapshotRestoreTeamDefActive(ctx, store.TeamDefActiveEntry{Name: name, DefID: r.DefID, PromotedAt: time.Now(), Promoter: p}); err != nil {
			t.Fatal(err)
		}
		check("a restored pointer", name, p.ACLOwner, false)
	}
}
