package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// armT0 is the instant the team webhook lease tests arm at.
var armT0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// runningWalk opens a running run in tenant acme, standing for a walk.
func runningWalk(t *testing.T, s store.Store) string {
	t.Helper()
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "acme", "team:hooked", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "team:hooked", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// leaseOf is walk's lease on team hooked's webhook name, armed at armedAt and
// held for ttl.
func leaseOf(walk, name string, armedAt time.Time, ttl time.Duration) store.TeamWebhookArm {
	return store.TeamWebhookArm{
		TenantID: "acme", Team: "hooked", Name: name, WalkRunID: walk,
		DefID: "tdf_hooked_1", UserID: "alice", ArmedAt: armedAt, ExpiresAt: armedAt.Add(ttl),
	}
}

func liveWalk(t *testing.T, s store.Store, tenant, team, name string, now time.Time) string {
	t.Helper()
	got, ok, err := s.TeamWebhookArmLive(context.Background(), tenant, team, name, now)
	if err != nil {
		t.Fatalf("TeamWebhookArmLive(%s/%s/%s): %v", tenant, team, name, err)
	}
	if !ok {
		return ""
	}
	return got.WalkRunID
}

// The live lease of a webhook is the earliest-armed one, with every field as
// written, and only for the exact (tenant, team, name).
func testTeamWebhookArmLiveIsTheEarliestArmedLease(t *testing.T, s store.Store) {
	ctx := context.Background()
	first, second := runningWalk(t, s), runningWalk(t, s)
	if err := s.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{
		leaseOf(second, "github", armT0.Add(time.Second), time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{
		leaseOf(first, "github", armT0, time.Minute), leaseOf(first, "gitlab", armT0, time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.TeamWebhookArmLive(ctx, "acme", "hooked", "github", armT0.Add(time.Second))
	if err != nil || !ok {
		t.Fatalf("live lease: ok=%v err=%v", ok, err)
	}
	want := leaseOf(first, "github", armT0, time.Minute)
	if got.WalkRunID != want.WalkRunID || got.DefID != want.DefID || got.UserID != want.UserID ||
		got.TenantID != "acme" || got.Team != "hooked" || got.Name != "github" ||
		!got.ArmedAt.Equal(want.ArmedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("live lease = %+v, want the earliest-armed %+v", got, want)
	}
	for what, k := range map[string][3]string{
		"another name":   {"acme", "hooked", "jira"},
		"another team":   {"acme", "other", "github"},
		"another tenant": {"globex", "hooked", "github"},
		"shared tenant":  {"", "hooked", "github"},
	} {
		if w := liveWalk(t, s, k[0], k[1], k[2], armT0); w != "" {
			t.Errorf("%s: live lease of walk %s, want none", what, w)
		}
	}
}

// A lease ending at or before now is absent; renewing it (the same Put, a
// later expiry) brings it back.
func testTeamWebhookArmLapsedLeaseIsAbsentUntilRenewed(t *testing.T, s store.Store) {
	ctx := context.Background()
	walk := runningWalk(t, s)
	if err := s.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{leaseOf(walk, "github", armT0, time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if w := liveWalk(t, s, "acme", "hooked", "github", armT0.Add(time.Minute-time.Millisecond)); w != walk {
		t.Fatalf("before the lease ends: %q, want %s", w, walk)
	}
	if w := liveWalk(t, s, "acme", "hooked", "github", armT0.Add(time.Minute)); w != "" {
		t.Errorf("at the lease's end: walk %s is live, want none", w)
	}
	if err := s.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{leaseOf(walk, "github", armT0, 2*time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if w := liveWalk(t, s, "acme", "hooked", "github", armT0.Add(time.Minute)); w != walk {
		t.Errorf("after the renewal: %q, want %s", w, walk)
	}
}

// A lease whose walk run is no longer running — or has no run at all — is
// absent, whatever its expiry.
func testTeamWebhookArmLeaseOfAnEndedWalkIsAbsent(t *testing.T, s store.Store) {
	ctx := context.Background()
	ended, running := runningWalk(t, s), runningWalk(t, s)
	if err := s.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{
		leaseOf("r_no_such_run", "github", armT0.Add(-time.Second), time.Hour),
		leaseOf(ended, "github", armT0, time.Hour),
		leaseOf(running, "github", armT0.Add(time.Second), time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, ended, store.RunFailed, "", store.Usage{}, "crashed"); err != nil {
		t.Fatal(err)
	}
	if w := liveWalk(t, s, "acme", "hooked", "github", armT0.Add(2*time.Second)); w != running {
		t.Errorf("live lease of walk %q, want the running walk %s (not the ended one, not one with no run)", w, running)
	}
}

// Delete removes every lease of that walk on that team, and nothing else.
func testTeamWebhookArmDeleteRemovesOnlyThatWalksLeases(t *testing.T, s store.Store) {
	ctx := context.Background()
	gone, kept := runningWalk(t, s), runningWalk(t, s)
	other := leaseOf(gone, "github", armT0, time.Hour)
	other.Team = "other"
	if err := s.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{
		leaseOf(gone, "github", armT0, time.Hour), leaseOf(gone, "gitlab", armT0, time.Hour),
		leaseOf(kept, "github", armT0.Add(time.Second), time.Hour), other,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.TeamWebhookArmDelete(ctx, "acme", "hooked", gone); err != nil {
		t.Fatal(err)
	}
	now := armT0.Add(2 * time.Second)
	if w := liveWalk(t, s, "acme", "hooked", "github", now); w != kept {
		t.Errorf("github after the delete: %q, want the other walk %s", w, kept)
	}
	if w := liveWalk(t, s, "acme", "hooked", "gitlab", now); w != "" {
		t.Errorf("gitlab after the delete: walk %s, want none", w)
	}
	if w := liveWalk(t, s, "acme", "other", "github", now); w != gone {
		t.Errorf("the same walk's lease on another team: %q, want it kept", w)
	}
}

// Arming a team drops that team's lapsed leases (a crashed walk's), so they do
// not pile up; another team's are left alone.
func testTeamWebhookArmPutSweepsTheTeamsLapsedLeases(t *testing.T, s store.Store) {
	ctx := context.Background()
	crashed, next := runningWalk(t, s), runningWalk(t, s)
	elsewhere := leaseOf(crashed, "github", armT0, time.Minute)
	elsewhere.Team = "other"
	if err := s.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{leaseOf(crashed, "github", armT0, time.Minute), elsewhere}); err != nil {
		t.Fatal(err)
	}
	// Armed after the crashed walk's lease ended. Read back as of a moment
	// both leases were live, the earliest-armed one is reported only if it is
	// still stored.
	if err := s.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{leaseOf(next, "github", armT0.Add(2*time.Minute), time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if w := liveWalk(t, s, "acme", "hooked", "github", armT0.Add(30*time.Second)); w == crashed {
		t.Errorf("the crashed walk's lapsed lease is still stored after the team was armed again")
	}
	if w := liveWalk(t, s, "acme", "other", "github", armT0.Add(30*time.Second)); w != crashed {
		t.Errorf("another team's lease was swept: %q", w)
	}
}

// A lease missing what identifies it is refused, and nothing is written.
func testTeamWebhookArmPutRefusesAnIncompleteLease(t *testing.T, s store.Store) {
	ctx := context.Background()
	walk := runningWalk(t, s)
	for what, mut := range map[string]func(*store.TeamWebhookArm){
		"no team":       func(a *store.TeamWebhookArm) { a.Team = "" },
		"no name":       func(a *store.TeamWebhookArm) { a.Name = "" },
		"no walk":       func(a *store.TeamWebhookArm) { a.WalkRunID = "" },
		"no version":    func(a *store.TeamWebhookArm) { a.DefID = "" },
		"no armed_at":   func(a *store.TeamWebhookArm) { a.ArmedAt = time.Time{} },
		"no expires_at": func(a *store.TeamWebhookArm) { a.ExpiresAt = time.Time{} },
	} {
		bad := leaseOf(walk, "github", armT0, time.Hour)
		mut(&bad)
		if err := s.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{leaseOf(walk, "gitlab", armT0, time.Hour), bad}); err == nil {
			t.Errorf("%s: want a refusal", what)
		}
	}
	if w := liveWalk(t, s, "acme", "hooked", "gitlab", armT0); w != "" {
		t.Errorf("a refused Put wrote its valid lease too")
	}
}
