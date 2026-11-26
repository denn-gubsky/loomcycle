package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC GW P12 plan §3.6, §5 (PR 6): a workspace's row travels in a
// snapshot, its files never. A restored row points at nothing it was not
// given, keeps tenants and users apart, and is lost until its worker says
// it holds the directory.

// workspaceEntryValidator stands in for the server's: a name and an id.
func workspaceEntryValidator(body json.RawMessage) error {
	var e WorkspaceEntry
	if err := json.Unmarshal(body, &e); err != nil {
		return err
	}
	if e.Name == "" || strings.ContainsAny(e.Name, "/@ ") || !strings.HasPrefix(e.ID, "ws_") {
		return errors.New("bad name or id")
	}
	return nil
}

func workspaceRestoreOptions(routes ...string) RestoreOptions {
	return RestoreOptions{
		Validators:            map[string]func(json.RawMessage) error{migrations.SectionWorkspaces: workspaceEntryValidator},
		WorkspaceEgressRoutes: routes,
	}
}

type wsSource struct {
	st                store.Store
	alice, bob, ended string
}

func newWSSource(t *testing.T) (*wsSource, func()) {
	t.Helper()
	ctx := context.Background()
	src, closeSrc := newTestStore(t)
	sess, err := src.CreateSession(ctx, "acme", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(agent, user string) string {
		r, err := src.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{AgentID: agent, UserID: user, TenantID: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	ws := &wsSource{st: src, alice: mk("a_alice", "alice"), bob: mk("a_bob", "bob")}
	// A run that ended does not travel (only paused and queued runs do).
	ended, err := src.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_ended", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.FinishRun(ctx, ended.ID, store.RunCompleted, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	ws.ended = ended.ID
	return ws, closeSrc
}

func (w *wsSource) add(t *testing.T, row store.WorkspaceRow, state store.WorkspaceState) {
	t.Helper()
	if row.State == "" {
		row.State = store.WorkspaceOpening
	}
	if row.UserID == "" {
		row.UserID = "alice"
	}
	if row.TenantID == "" {
		row.TenantID = "acme"
	}
	if row.WorkerID == "" {
		row.WorkerID = "tenant:acme/w1"
	}
	if _, err := w.st.WorkspaceCreate(context.Background(), row); err != nil {
		t.Fatalf("create %s: %v", row.ID, err)
	}
	if state != store.WorkspaceOpening {
		if ok, err := w.st.WorkspaceSetState(context.Background(), row.TenantID, row.ID, store.WorkspaceOpening, state); err != nil || !ok {
			t.Fatalf("state %s: %v %v", row.ID, ok, err)
		}
	}
}

func restoreInto(t *testing.T, src store.Store, dst store.Store, opts RestoreOptions) (RestoreResult, []byte) {
	t.Helper()
	ctx := context.Background()
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Restore(ctx, dst, raw, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res, raw
}

func wsRow(t *testing.T, st store.Store, id string) (store.WorkspaceRow, bool) {
	t.Helper()
	r, err := st.WorkspaceGet(context.Background(), "acme", id)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return store.WorkspaceRow{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return r, true
}

// What a restored row may point at: its own tenant's worker, a run of its
// own tenant and user, shares to such runs, the routes the target defines.
// Every restored row is lost until its worker lists it.
func TestSnapshotWorkspaces_ARestoredRowPointsOnlyAtItsOwn(t *testing.T) {
	w, closeSrc := newWSSource(t)
	defer closeSrc()
	good := store.WorkspaceRow{ID: "ws_good", Name: "build", OwnerRunID: w.alice, OwnerAgentID: "a_alice", RootRunID: w.alice,
		Egress: []string{"pypi", "ghost"},
		Shares: []store.WorkspaceShare{{RunID: w.bob, AgentID: "a_bob", Access: store.WorkspaceShareRead}, {RunID: "run-missing", AgentID: "a_x", Access: store.WorkspaceShareRead}}}
	w.add(t, good, store.WorkspaceReady)
	w.add(t, store.WorkspaceRow{ID: "ws_foreign_worker", Name: "fw", OwnerRunID: w.alice, WorkerID: "tenant:beta/w1"}, store.WorkspaceReady)
	w.add(t, store.WorkspaceRow{ID: "ws_other_users_run", Name: "ou", OwnerRunID: w.bob}, store.WorkspaceReady)
	w.add(t, store.WorkspaceRow{ID: "ws_ended_eph", Name: "ee", OwnerRunID: w.ended}, store.WorkspaceReady)
	w.add(t, store.WorkspaceRow{ID: "ws_ended_dur", Name: "ed", OwnerRunID: w.ended, Lifetime: store.WorkspaceLifetimeDurable}, store.WorkspaceReady)
	w.add(t, store.WorkspaceRow{ID: "ws_lost", Name: "lo", OwnerRunID: w.alice}, store.WorkspaceLost)
	if _, err := w.st.WorkspaceExposureCreate(context.Background(), store.WorkspaceExposure{ID: "e0123456789abcdef0123456", TenantID: "acme", UserID: "alice", WorkspaceID: "ws_good", Port: 8000, Kind: store.WorkspaceExposurePage}, 4); err != nil {
		t.Fatal(err)
	}

	dst, closeDst := newTestStore(t)
	defer closeDst()
	res, _ := restoreInto(t, w.st, dst, workspaceRestoreOptions("pypi"))

	r, ok := wsRow(t, dst, "ws_good")
	if !ok {
		t.Fatalf("the good row was not restored; warnings %v", res.Warnings)
	}
	if r.State != store.WorkspaceLost || r.OwnerRunID != w.alice || r.UserID != "alice" || r.WorkerID != "tenant:acme/w1" {
		t.Errorf("restored = %+v, want lost, alice's run and worker", r)
	}
	if !slices.Equal(r.Egress, []string{"pypi"}) {
		t.Errorf("egress = %v, want only the route the target defines", r.Egress)
	}
	if len(r.Shares) != 0 {
		t.Errorf("shares = %+v, want none (another user's run, and a run that did not travel)", r.Shares)
	}
	if l, _ := dst.WorkspaceExposureListByWorkspace(context.Background(), "acme", "ws_good"); len(l) != 0 {
		t.Errorf("an exposure travelled: %+v", l)
	}
	for _, id := range []string{"ws_foreign_worker", "ws_other_users_run", "ws_ended_eph"} {
		if _, ok := wsRow(t, dst, id); ok {
			t.Errorf("%s was restored", id)
		}
	}
	d, ok := wsRow(t, dst, "ws_ended_dur")
	if !ok || d.OwnerKind != store.WorkspaceOwnerParked || d.OwnerRunID != store.WorkspaceParkedKey("ws_ended_dur") || d.State != store.WorkspaceLost {
		t.Errorf("a durable row of a run that did not travel = %+v (%v), want parked and lost", d, ok)
	}
	if l, ok := wsRow(t, dst, "ws_lost"); !ok || l.State != store.WorkspaceLost {
		t.Errorf("a lost row = %+v (%v)", l, ok)
	}
	if res.WorkspacesRestored != 3 || res.WorkspacesSkipped != 3 {
		t.Errorf("restored %d, skipped %d; want 3 and 3", res.WorkspacesRestored, res.WorkspacesSkipped)
	}
}

// Never captured: a workspace opened over the MCP server (its token
// definition never travels), opening and releasing rows. No secret is in the
// envelope.
func TestSnapshotWorkspaces_PrincipalOpeningAndReleasingRowsDoNotTravel(t *testing.T) {
	w, closeSrc := newWSSource(t)
	defer closeSrc()
	w.add(t, store.WorkspaceRow{ID: "ws_mcp", Name: "mcp", OwnerRunID: "principal:abc", OwnerKind: store.WorkspaceOwnerPrincipal, OpenedToken: "tokdef-do-not-travel"}, store.WorkspaceReady)
	w.add(t, store.WorkspaceRow{ID: "ws_opening", Name: "op", OwnerRunID: w.alice}, store.WorkspaceOpening)
	w.add(t, store.WorkspaceRow{ID: "ws_rel", Name: "rel", OwnerRunID: w.alice}, store.WorkspaceReleasing)
	dst, closeDst := newTestStore(t)
	defer closeDst()
	_, raw := restoreInto(t, w.st, dst, workspaceRestoreOptions())
	for _, s := range []string{"ws_mcp", "tokdef-do-not-travel", "ws_opening", "ws_rel"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("the envelope carries %q", s)
		}
	}
}

// A row the target already has, by id or by its durable name, is skipped,
// never merged; a restore twice changes nothing.
func TestSnapshotWorkspaces_AColliderIsSkipped(t *testing.T) {
	w, closeSrc := newWSSource(t)
	defer closeSrc()
	w.add(t, store.WorkspaceRow{ID: "ws_dur", Name: "keep", OwnerRunID: w.alice, Lifetime: store.WorkspaceLifetimeDurable}, store.WorkspaceReady)
	dst, closeDst := newTestStore(t)
	defer closeDst()
	if _, err := dst.WorkspaceCreate(context.Background(), store.WorkspaceRow{ID: "ws_mine", TenantID: "acme", UserID: "alice", Name: "keep",
		WorkerID: "tenant:acme/w9", OwnerRunID: "run-target", Lifetime: store.WorkspaceLifetimeDurable}); err != nil {
		t.Fatal(err)
	}
	res, _ := restoreInto(t, w.st, dst, workspaceRestoreOptions())
	if _, ok := wsRow(t, dst, "ws_dur"); ok {
		t.Error("a row whose durable name the target holds was restored")
	}
	if mine, _ := wsRow(t, dst, "ws_mine"); mine.WorkerID != "tenant:acme/w9" {
		t.Errorf("the target's own row changed: %+v", mine)
	}
	if res.WorkspacesSkipped != 1 {
		t.Errorf("skipped %d, want 1", res.WorkspacesSkipped)
	}
}

// Without the server's validator the section is not restored at all.
func TestSnapshotWorkspaces_NoValidatorRestoresNothing(t *testing.T) {
	w, closeSrc := newWSSource(t)
	defer closeSrc()
	w.add(t, store.WorkspaceRow{ID: "ws_good", Name: "build", OwnerRunID: w.alice}, store.WorkspaceReady)
	dst, closeDst := newTestStore(t)
	defer closeDst()
	res, _ := restoreInto(t, w.st, dst, RestoreOptions{})
	if _, ok := wsRow(t, dst, "ws_good"); ok {
		t.Error("a row was restored with no validator")
	}
	if !slices.ContainsFunc(res.Warnings, func(s string) bool { return strings.Contains(s, "workspaces") }) {
		t.Errorf("warnings %v, want one about the section", res.Warnings)
	}
}

var _ = time.Second

// A git workspace's record travels with its credential as a reference; an
// envelope edited to carry anything else in its place is refused.
func TestSnapshotWorkspaces_AGitCredentialTravelsAsAReferenceOnly(t *testing.T) {
	w, closeSrc := newWSSource(t)
	defer closeSrc()
	w.add(t, store.WorkspaceRow{ID: "ws_repo", Name: "repo", OwnerRunID: w.alice, Kind: store.WorkspaceKindGit}, store.WorkspaceReady)
	if ok, err := w.st.WorkspaceSetGit(context.Background(), "acme", "ws_repo", store.WorkspaceGit{URL: "https://forge.example/acme/repo.git", Branch: "loom/repo", Credential: "$cred:forge", Mirror: "m1"}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ctx := context.Background()
	_, raw, err := Capture(ctx, w.st, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dst, closeDst := newTestStore(t)
	defer closeDst()
	if _, err := Restore(ctx, dst, raw, workspaceRestoreOptions()); err != nil {
		t.Fatal(err)
	}
	if r, ok := wsRow(t, dst, "ws_repo"); !ok || r.Git == nil || r.Git.Credential != "$cred:forge" || r.Git.URL != "https://forge.example/acme/repo.git" {
		t.Fatalf("restored git = %+v (%v)", r.Git, ok)
	}
	edited := strings.Replace(string(raw), `"credential":"$cred:forge"`, `"credential":"ghp_literal-token-value"`, 1)
	if edited == string(raw) {
		t.Fatal("the envelope does not carry the credential where the test looks")
	}
	// A hand-edited file has no checksum (it is verified only if present):
	// drop it, so the edit reaches the section's own checks.
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(edited), &env); err != nil {
		t.Fatal(err)
	}
	delete(env, "checksum")
	unsealed, _ := json.Marshal(env)
	other, closeOther := newTestStore(t)
	defer closeOther()
	res, err := Restore(ctx, other, unsealed, workspaceRestoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wsRow(t, other, "ws_repo"); ok {
		t.Errorf("a row whose git credential is not a reference was restored (warnings %v)", res.Warnings)
	}
}
