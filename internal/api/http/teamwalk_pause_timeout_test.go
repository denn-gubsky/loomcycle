package http

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// timedWalkTeam is one agent state bounded by timeout_ms.
const timedWalkTeam = `{"entry":"work","states":[` +
	`{"state":"work","handler":{"kind":"agent","agent":"writer","timeout_ms":800}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"work","to":"done","on":"success"}]}`

// A runtime pause does not spend a team state's timeout_ms. The member here is
// in its model call when the operator pauses and the runtime stays paused for
// twice the bound: the walk does not time out meanwhile, and once the runtime
// resumes the state runs out only the time it had left.
func TestTeamWalkTimeout_APausedRuntimeDoesNotSpendTheStatesBound(t *testing.T) {
	const bound = 800 * time.Millisecond
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"writer": {Model: "stub-model", SystemPrompt: "write"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "walkpause.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: hangingProvider{}}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	mgr := pause.NewManager(st, time.Second)
	srv.SetPauseManager(mgr)
	defer cancelAllRuns(srv)
	seedTenantTeam(t, st, "acme", "timed", timedWalkTeam)
	h := &walkHarness{t: t, srv: srv, st: st}

	start := time.Now()
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"timed","input":"draft","mode":"detach"}`)
	time.Sleep(200 * time.Millisecond)
	if _, err := mgr.Pause(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("pause: %v", err)
	}
	paused := time.Now()
	time.Sleep(2 * bound)
	if run, err := st.GetRun(context.Background(), walkID); err != nil || run.Status != store.RunRunning {
		t.Fatalf("walk at the resume = %+v (%v), want still running: the pause spent its bound", run, err)
	}
	resumed := time.Now()
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	awaitWalkEnd(t, st, walkID)
	ended := time.Now()
	run, err := st.GetRun(context.Background(), walkID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunFailed || !strings.Contains(run.ErrorMsg, `state "work" timed out: timeout_ms=800`) {
		t.Fatalf("walk ended %s (%s), want failed with the state's timeout", run.Status, run.ErrorMsg)
	}
	left := bound - paused.Sub(start)
	if got := ended.Sub(resumed); got < left-150*time.Millisecond || got > left+2*time.Second {
		t.Errorf("the walk timed out %v after the resume, want about the %v it had left", got, left)
	}
}
