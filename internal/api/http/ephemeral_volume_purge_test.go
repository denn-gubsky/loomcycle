package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
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
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
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
