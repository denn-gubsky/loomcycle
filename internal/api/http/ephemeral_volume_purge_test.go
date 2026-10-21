package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// ephemeralPurgeServer builds a minimal Server with a sqlite store + a
// dynamic_root volume pointing at a real temp dir, and returns it plus the
// resolved root.
func ephemeralPurgeServer(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Volumes: map[string]config.Volume{
			"pool": {Path: root, Mode: "rw", DynamicRoot: true},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	cfg.Env.AuthToken = ""
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "ephemeral_purge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: &scriptedProvider{}}, nil, concurrency.New(4, 4, time.Second), st)
	return srv, st, root
}

// seedEphemeral creates one run + an ephemeral volume row + the on-disk dir
// under <root>/_ephemeral/<runID>/<name>, returning the run id + the dir.
func seedEphemeral(t *testing.T, st store.Store, root, name string) (runID, dir string) {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "t", "a", "u")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_" + name})
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(root, "_ephemeral", run.ID, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": dir, "mode": "rw"})
	if _, err := st.EphemeralVolumeCreate(ctx, store.EphemeralVolumeDefRow{
		RootRunID: run.ID, Name: name, Definition: body,
	}); err != nil {
		t.Fatal(err)
	}
	return run.ID, dir
}

// A TOP-LEVEL run completing purges its ephemeral subtree (dir gone + rows
// deleted) via finishRunWithCancel.
func TestFinishRun_TopLevelPurgesEphemeralTree(t *testing.T) {
	srv, st, root := ephemeralPurgeServer(t)
	runID, dir := seedEphemeral(t, st, root, "work")

	meta := runStateMeta{RunID: runID, IsTopLevel: true, RootRunID: runID}
	// A non-cancelled runCtx routes to finishRun; the deferred top-level
	// purge then fires.
	srv.finishRunWithCancel(context.Background(), context.Background(), runID, loop.RunResult{}, nil, meta)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("ephemeral dir survived a top-level completion: %q (err=%v)", dir, err)
	}
	rows, _ := st.EphemeralVolumeListByRun(context.Background(), runID)
	if len(rows) != 0 {
		t.Errorf("ephemeral rows survived a top-level completion: %+v", rows)
	}
}

// A SUB-AGENT completing (IsTopLevel=false) must NOT purge — the tree
// belongs to the top-level run, which its parent + siblings still use.
func TestFinishRun_SubAgentDoesNotPurgeEphemeralTree(t *testing.T) {
	srv, st, root := ephemeralPurgeServer(t)
	runID, dir := seedEphemeral(t, st, root, "work")

	subMeta := runStateMeta{RunID: runID, IsTopLevel: false, RootRunID: runID}
	srv.finishRunWithCancel(context.Background(), context.Background(), runID, loop.RunResult{}, nil, subMeta)

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("ephemeral dir wrongly removed by a SUB-agent completion: %q err=%v", dir, err)
	}
	rows, _ := st.EphemeralVolumeListByRun(context.Background(), runID)
	if len(rows) != 1 {
		t.Errorf("ephemeral rows wrongly deleted by a SUB-agent completion: %+v", rows)
	}
}

// A TOP-LEVEL run that FAILS (finishRunFailedReason — incl. mid-flight panic
// paths) must still purge its ephemeral subtree, not just a clean completion.
func TestFinishRun_TopLevelFailedPurgesEphemeralTree(t *testing.T) {
	srv, st, root := ephemeralPurgeServer(t)
	runID, dir := seedEphemeral(t, st, root, "work")

	srv.finishRunFailedReason(runID, "boom", runStateMeta{RunID: runID, IsTopLevel: true, RootRunID: runID})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("ephemeral dir survived a top-level FAILURE: %q (err=%v)", dir, err)
	}
	if rows, _ := st.EphemeralVolumeListByRun(context.Background(), runID); len(rows) != 0 {
		t.Errorf("ephemeral rows survived a top-level FAILURE: %+v", rows)
	}
}

// rehydrateEphemeralVolumes rebuilds the in-memory set from persisted rows so
// a resumed paused run keeps resolving its own ephemeral volumes.
func TestRehydrateEphemeralVolumes_RoundTrip(t *testing.T) {
	srv, st, root := ephemeralPurgeServer(t)
	runID, dir := seedEphemeral(t, st, root, "work")

	set := srv.rehydrateEphemeralVolumes(context.Background(), runID)
	ref, ok := set.Get("work")
	if !ok || ref.Root != dir || ref.ReadOnly {
		t.Errorf("rehydrated set.Get(work) = %+v ok=%v, want root=%q ro=false", ref, ok, dir)
	}
}

// A volume made read-only before a pause comes back read-only: the resumed
// run rebuilds its set from the rows, and set_mode wrote the row.
func TestRehydrateEphemeralVolumes_KeepsAVolumeReadOnly(t *testing.T) {
	srv, st, root := ephemeralPurgeServer(t)
	runID, dir := seedEphemeral(t, st, root, "work")
	if found, err := st.EphemeralVolumeSetReadOnly(context.Background(), runID, "work"); err != nil || !found {
		t.Fatalf("set read-only: found=%v err=%v", found, err)
	}
	ref, ok := srv.rehydrateEphemeralVolumes(context.Background(), runID).Get("work")
	if !ok || ref.Root != dir || !ref.ReadOnly {
		t.Errorf("rehydrated = %+v ok=%v, want root=%q read-only", ref, ok, dir)
	}
}

// The resumed runs of one tree share one set, so a volume one of them makes
// read-only after the resume is read-only for the others. Each used to get a
// set of its own, and a resumed child kept writing to a volume its parent
// had closed.
func TestTreeVolumeSets_RunsOfOneTreeShareOneSet(t *testing.T) {
	srv, st, root := ephemeralPurgeServer(t)
	runID, dir := seedEphemeral(t, st, root, "work")
	ctx := context.Background()

	parentSet, releaseParent := srv.treeSets.acquire(runID)
	srv.loadEphemeralVolumes(ctx, runID, parentSet)
	childSet, releaseChild := srv.treeSets.acquire(runID)
	srv.loadEphemeralVolumes(ctx, runID, childSet)
	if parentSet != childSet {
		t.Fatal("two resumed runs of one tree got different sets")
	}
	parentSet.SetReadOnly("work")
	if ref, ok := childSet.Get("work"); !ok || ref.Root != dir || !ref.ReadOnly {
		t.Errorf("the other run sees %+v ok=%v, want read-only", ref, ok)
	}

	// Loading the rows again, where the row still says rw, gives nothing back.
	srv.loadEphemeralVolumes(ctx, runID, parentSet)
	if ref, _ := parentSet.Get("work"); !ref.ReadOnly {
		t.Error("loading the stored rows made a read-only volume writable")
	}

	// Another tree gets its own set.
	otherSet, releaseOther := srv.treeSets.acquire("another-root")
	defer releaseOther()
	if otherSet == parentSet {
		t.Error("two trees share a set")
	}

	// The entry goes with the last run of the tree, and release counts once.
	releaseParent()
	releaseParent()
	if again, release := srv.treeSets.acquire(runID); again != parentSet {
		t.Error("the set was dropped while a run of the tree still held it")
	} else {
		release()
	}
	releaseChild()
	if fresh, release := srv.treeSets.acquire(runID); fresh == parentSet {
		t.Error("the set outlived the last run of the tree")
	} else {
		release()
	}
}

// A run that resume starts again joins the set of its tree's live top-level
// run, and the tree's purge forgets the set.
func TestTreeVolumeSets_AResumedRunJoinsTheLiveTreeAndPurgeForgetsIt(t *testing.T) {
	srv, st, root := ephemeralPurgeServer(t)
	runID, _ := seedEphemeral(t, st, root, "work")
	ctx := context.Background()

	live, _ := srv.treeSets.acquire(runID) // what a top-level start does
	srv.loadEphemeralVolumes(ctx, runID, live)
	resumed, release := srv.treeSets.acquire(runID) // what resume does for a child
	defer release()
	if resumed != live {
		t.Fatal("a resumed run of a live tree got a set of its own")
	}
	resumed.SetReadOnly("work")
	if ref, _ := live.Get("work"); !ref.ReadOnly {
		t.Error("read-only set by the resumed run is not seen by the live one")
	}

	srv.purgeEphemeralVolumesForRun(runID)
	if after, rel := srv.treeSets.acquire(runID); after == live {
		t.Error("the set outlived the tree's purge")
	} else {
		rel()
	}
}

// Two replicas each hold their own set for one tree; the rows are what they
// share. A volume made read-only through one must be read-only for a write
// made through the other, whose set still says writable.
func TestEphemeralModeCheck_AnotherReplicasSetModeStopsAWriteHere(t *testing.T) {
	srv, st, root := ephemeralPurgeServer(t)
	runID, dir := seedEphemeral(t, st, root, "work")
	ctx := context.Background()

	// This replica: its own set, loaded from the rows while they said rw.
	here := srv.loadEphemeralVolumes(ctx, runID, tools.NewEphemeralVolumeSet())
	// A run its entry point granted ephemeral write, as every run without
	// `ephemeral_volumes: ro` is.
	runCtx := tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.EphemeralWritable = true })
	runCtx = tools.WithEphemeralModeCheck(tools.WithEphemeralVolumes(runCtx, here), srv.ephemeralModeCheck(runID))
	write := func() tools.Result {
		in, _ := json.Marshal(map[string]any{"volume": "work", "path": "a.txt", "content": "x"})
		res, err := (&builtin.Write{}).Execute(runCtx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := write(); res.IsError {
		t.Fatalf("before the other replica's set_mode the write must work: %s", res.Text)
	}

	// The other replica's set_mode: it writes the row and its OWN set.
	if found, err := st.EphemeralVolumeSetReadOnly(ctx, runID, "work"); err != nil || !found {
		t.Fatalf("set read-only: found=%v err=%v", found, err)
	}
	if ref, _ := here.Get("work"); ref.ReadOnly {
		t.Fatal("this replica's set changed by itself; the test would prove nothing")
	}

	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	res := write()
	if !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Errorf("the write went through a set that had not heard of the set_mode: error=%v %q", res.IsError, res.Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err == nil {
		t.Error("the file was written")
	}

	// And once the tree's volumes are purged, the rows are gone: a late write
	// is refused, not waved through.
	srv.purgeEphemeralVolumesForRun(runID)
	if res := write(); !res.IsError {
		t.Error("a write after the tree's purge was accepted")
	}
}

// rehydrate is scoped to its own run — it must NOT load another run's ephemeral
// volumes (a regression to list-all would breach run isolation).
func TestRehydrateEphemeralVolumes_DoesNotLeakOtherRun(t *testing.T) {
	srv, st, root := ephemeralPurgeServer(t)
	runID, _ := seedEphemeral(t, st, root, "mine")
	seedEphemeral(t, st, root, "theirs") // a DIFFERENT run's volume

	set := srv.rehydrateEphemeralVolumes(context.Background(), runID)
	if _, ok := set.Get("mine"); !ok {
		t.Error("rehydrate should load the run's own volume")
	}
	if _, ok := set.Get("theirs"); ok {
		t.Error("rehydrate leaked a DIFFERENT run's ephemeral volume — run isolation breach")
	}
}
