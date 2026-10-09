package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOpenTeamWalkRun_ASecondWalkWithTheKeyIsRefusedAsHeld: the run store's
// unique index is what makes two racing starts one walk. The second opener
// gets the error op=run answers with the winner for, the winner is the walk
// the lookup then finds, and a key is its sender's: another user, another
// tenant and a walk with no key are all opened.
func TestOpenTeamWalkRun_ASecondWalkWithTheKeyIsRefusedAsHeld(t *testing.T) {
	srv, _ := runReadServer(t)
	as := func(tenant, user string) context.Context {
		return tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: user, TenantID: tenant, AgentID: "caller"})
	}
	open := func(ctx context.Context, key string) (string, error) {
		_, runID, finish, err := srv.openTeamWalkRun(ctx, builtin.WalkRunSpec{Name: "triage", DefID: "tdf_1", Detach: true, IdempotencyKey: key})
		if err == nil {
			t.Cleanup(func() { finish(builtin.WalkEnd{FinalText: "routed", Terminal: "done"}) })
		}
		return runID, err
	}
	alice := as("acme", "alice")

	first, err := open(alice, "k1")
	if err != nil {
		t.Fatalf("the first walk: %v", err)
	}
	if _, err := open(alice, "k1"); !errors.Is(err, builtin.ErrWalkKeyHeld) {
		t.Fatalf("a second walk with the key = %v, want ErrWalkKeyHeld", err)
	}
	held, err := srv.existingTeamWalk(alice, "k1", true)
	if err != nil || held == nil || held.RunID != first || held.Name != "triage" || held.DefID != "tdf_1" || held.Ended {
		t.Fatalf("the walk holding the key = %+v (%v), want the first, still running", held, err)
	}
	for _, other := range []struct {
		ctx context.Context
		key string
	}{{as("acme", "bob"), "k1"}, {as("globex", "alice"), "k1"}, {alice, "k2"}, {alice, ""}, {alice, ""}} {
		if id, err := open(other.ctx, other.key); err != nil || id == first {
			t.Errorf("a walk by another caller or with another key = %q (%v), want its own run", id, err)
		}
		if other.key == "" {
			continue
		}
		if held, err := srv.existingTeamWalk(other.ctx, "unused-"+other.key, false); err != nil || held != nil {
			t.Errorf("a key nobody used is held by %+v (%v)", held, err)
		}
	}
}

// Once the walk has ended, the key answers with how it ended.
func TestExistingTeamWalk_ReportsHowTheWalkEnded(t *testing.T) {
	srv, _ := runReadServer(t)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "alice", TenantID: "acme", AgentID: "caller"})
	_, runID, finish, err := srv.openTeamWalkRun(ctx, builtin.WalkRunSpec{Name: "triage", DefID: "tdf_1", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	finish(builtin.WalkEnd{FinalText: "routed to billing", Terminal: "billing-desk"})
	held, err := srv.existingTeamWalk(ctx, "k1", false)
	if err != nil || held == nil {
		t.Fatalf("existingTeamWalk = %+v, %v", held, err)
	}
	want := builtin.ExistingWalk{RunID: runID, Name: "triage", DefID: "tdf_1", Status: "completed", Ended: true,
		FinalText: "routed to billing", Terminal: "billing-desk"}
	if *held != want {
		t.Errorf("the ended walk = %+v, want %+v", *held, want)
	}
}
