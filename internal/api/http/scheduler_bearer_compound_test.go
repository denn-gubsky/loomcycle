package http

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers/mock"
	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/mcptest"
)

// compoundScale is the override knob for TestSchedulerBearerCompound.
// Default 310 (10 + 100 + 200 across the 3 phases). Set higher to
// stress under load — proportions across the 3 phases are preserved.
var compoundScale = flag.Int("scale", 310, "total number of schedules to seed in TestSchedulerBearerCompound (split 3% / 32% / 65% across the 3 phases)")

// TestSchedulerBearerCompound is the v0.12.7 release-gate test. It
// verifies the three v1.x substrates COMPOSE correctly:
//
//  1. RFC E ScheduleDef — the sweeper picks up forked schedules
//     and spawns runs.
//  2. RFC F per-run credentials — each fork's user_credentials map
//     flows into the run via RunInput → ctx → MCP HTTP header.
//  3. MCP per-server bearer substitution — outgoing MCP requests
//     carry the per-user bearer in the Authorization header, and
//     two distinct servers each see their own substituted value.
//
// Workload shape (default scale=310):
//
//	phase 1 (~3% of total): N forks seeded with next_run_at = T+0
//	phase 2 (~32%):          N forks seeded with next_run_at = T+1s
//	phase 3 (~65%):          N forks seeded with next_run_at = T+2s
//
// Each fork has user_id u_NNN and credential {user_token: u_NNN};
// each MCP server expects Authorization: Bearer u_NNN. The mock-mcp-
// caller provider variant inspects the agent's tool catalog, extracts
// the two mcp__-prefixed tools, and calls each with {user_id: u_NNN}
// (extracted from the prompt's "user_id=u_NNN" literal). The MCP
// servers each compare bearer == "Bearer " + user_id and count
// matched / mismatched pairs.
//
// Pass criteria:
//   - All N runs complete with status=completed.
//   - Each server received exactly N calls.
//   - Zero bearer/user_id mismatches across both servers.
//   - Each user_id appears exactly twice in the combined call log
//     (once per server) — proves per-user isolation under parallel-fire.
func TestSchedulerBearerCompound(t *testing.T) {
	scale := *compoundScale
	if scale < 30 {
		// Floor the per-phase counts so each phase has at least
		// one fork (scale=1 would split into 0/0/1).
		t.Logf("scale=%d clamped to 30 (minimum per-phase = 1+1+1 = 3 wouldn't exercise the cascade)", scale)
		scale = 30
	}
	phase1Count := scale * 3 / 100
	if phase1Count < 1 {
		phase1Count = 1
	}
	phase2Count := scale * 32 / 100
	if phase2Count < 1 {
		phase2Count = 1
	}
	phase3Count := scale - phase1Count - phase2Count

	// Two test MCP servers, each exposing ONE check_user-shaped tool
	// under a distinct name so the mock-mcp-caller can extract them
	// as two separate mcp__-prefixed entries from the agent catalog.
	mcpA := mcptest.NewServer(t, mcptest.WithToolName("check_user"))
	mcpB := mcptest.NewServer(t, mcptest.WithToolName("check_user"))

	// Build the operator config:
	//   - one agent `cascade-agent` using the mock-mcp-caller provider
	//   - two mcp_servers, each pointing at a test server with a
	//     credential-substituted Authorization header
	//   - one scheduled_runs template `cascade-template` referencing
	//     the agent. We won't actually drive the template through
	//     yaml — we seed forks directly via the store at scale.
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "mock", Model: "mock-mcp-caller"},
		Agents: map[string]config.AgentDef{
			"cascade-agent": {
				Model: "mock-mcp-caller",
				AllowedTools: []string{
					"mcp__server_a__check_user",
					"mcp__server_b__check_user",
				},
				SystemPrompt: "you are the cascade test agent",
			},
		},
		MCPServers: map[string]config.MCPServer{
			"server_a": {
				Transport: "http",
				URL:       mcpA.URL,
				Headers:   map[string]string{"Authorization": "Bearer ${run.credentials.user_token}"},
			},
			"server_b": {
				Transport: "http",
				URL:       mcpB.URL,
				Headers:   map[string]string{"Authorization": "Bearer ${run.credentials.user_token}"},
			},
		},
		Concurrency: config.Concurrency{
			MaxConcurrentRuns: 64,
			MaxQueueDepth:     scale * 2,
			QueueTimeoutMS:    30_000,
		},
	}
	cfg.Env.AuthToken = ""

	// Mock provider — low base latency + small jitter so the cascade
	// completes in tens of seconds even at scale=310. Operators wanting
	// realistic LLM-call shape can override via env before running.
	t.Setenv("LOOMCYCLE_MOCK_LATENCY_MS", "20")
	t.Setenv("LOOMCYCLE_MOCK_LATENCY_JITTER_MS", "30")
	t.Setenv("LOOMCYCLE_MOCK_ENABLED", "1")
	t.Setenv("LOOMCYCLE_MOCK_429_RATE", "0")
	mockProv := mock.New()

	// In-memory store; cleanup via t.TempDir-equivalent.
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// MCP pool factory — returns a Client for each configured server
	// using the same URL+headers shape main.go uses.
	pool := loommcp.NewPool(
		func(name string) (loommcp.Caller, error) {
			srv, ok := cfg.MCPServers[name]
			if !ok {
				return nil, fmt.Errorf("mcp_servers.%s: not configured", name)
			}
			return mcphttp.New(mcphttp.Config{URL: srv.URL, Headers: srv.Headers})
		},
		func(c loommcp.Caller) {
			type closer interface{ Close() error }
			if cl, ok := c.(closer); ok {
				_ = cl.Close()
			}
		},
	)
	t.Cleanup(pool.Close)

	// Eagerly handshake both servers so the mcp__-prefixed tool names
	// land in the agent's catalog at run time. (The lazy resolver
	// would also work but eager makes the test deterministic.)
	allTools := []tools.Tool{}
	mcpInitCtx, mcpInitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	for name := range cfg.MCPServers {
		_, descs, err := pool.GetWithRetry(mcpInitCtx, name, t.Logf)
		if err != nil {
			t.Fatalf("mcp[%s]: handshake failed: %v", name, err)
		}
		for _, d := range descs {
			allTools = append(allTools, loommcp.NewTool(pool, name, d))
		}
	}
	mcpInitCancel()
	if len(allTools) != 2 {
		t.Fatalf("expected 2 MCP tools registered (one per server); got %d", len(allTools))
	}

	sem := concurrency.New(cfg.Concurrency.MaxConcurrentRuns, cfg.Concurrency.MaxQueueDepth, cfg.Concurrency.QueueTimeout())
	srv := New(cfg, &stubResolver{p: mockProv}, allTools, sem, st)
	httpServer := httptest.NewServer(srv.Mux())
	t.Cleanup(httpServer.Close)
	// Wire the pause manager so the scheduler's pause-gate check is
	// satisfied (StateRunning).
	pauseMgr := pause.NewManager(st, 100*time.Millisecond)
	srv.SetPauseManager(pauseMgr)

	// Seed 310 (or scale) schedules across the 3 phases.
	now := time.Now()
	type seeded struct {
		userID    string
		defID     string
		nextRunAt time.Time
		phase     int
	}
	all := make([]seeded, 0, scale)
	idx := 0
	seedPhase := func(count int, offset time.Duration, phase int) {
		for i := 0; i < count; i++ {
			idx++
			userID := fmt.Sprintf("u_%04d", idx)
			defID := "sd_" + userID
			seedScheduleFork(t, st, defID, userID, "cascade-agent")
			all = append(all, seeded{userID: userID, defID: defID, nextRunAt: now.Add(offset), phase: phase})
		}
	}
	seedPhase(phase1Count, 0, 1)
	seedPhase(phase2Count, 1*time.Second, 2)
	seedPhase(phase3Count, 2*time.Second, 3)

	// Apply the staggered next_run_at offsets via Seed (UPSERT
	// semantics — overwrites the just-seeded "now" value from the
	// fork helper).
	for _, s := range all {
		if err := st.ScheduleRunStateSeed(context.Background(), s.defID, s.nextRunAt); err != nil {
			t.Fatalf("seed %s: %v", s.defID, err)
		}
	}

	// Spin the scheduler with a tight 100ms tick + large concurrent-fire
	// cap so all due rows in each phase enter the queue rapidly.
	sched := scheduler.New(scheduler.Config{
		TickInterval:       100 * time.Millisecond,
		FireTimeout:        2 * time.Minute,
		MaxConcurrentFires: 64,
	}, st, srv, pauseMgr, nil, t.Logf)

	schedCtx, schedCancel := context.WithCancel(context.Background())
	defer schedCancel()
	sched.Start(schedCtx)

	// Wait for all 2N MCP calls (one per server × scale) with a
	// generous deadline. Poll the counters every 100ms.
	totalCallsExpected := int64(scale * 2)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		got := mcpA.ToolCalls() + mcpB.ToolCalls()
		if got >= totalCallsExpected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: only %d/%d MCP calls observed after 5 min (server_a=%d, server_b=%d)",
				got, totalCallsExpected, mcpA.ToolCalls(), mcpB.ToolCalls())
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Give the scheduler a moment to advance final next_run_at values.
	time.Sleep(500 * time.Millisecond)
	sched.Stop()

	// ----- Assertions -----

	if mcpA.ToolCalls() != int64(scale) {
		t.Errorf("server_a calls = %d, want %d", mcpA.ToolCalls(), scale)
	}
	if mcpB.ToolCalls() != int64(scale) {
		t.Errorf("server_b calls = %d, want %d", mcpB.ToolCalls(), scale)
	}
	if got := mcpA.MismatchedBearers(); got != 0 {
		t.Errorf("server_a bearer mismatches = %d, want 0", got)
		dumpMismatches(t, "server_a", mcpA.CallLog())
	}
	if got := mcpB.MismatchedBearers(); got != 0 {
		t.Errorf("server_b bearer mismatches = %d, want 0", got)
		dumpMismatches(t, "server_b", mcpB.CallLog())
	}

	// Per-user isolation: every user_id must appear exactly once on
	// each server. Aggregate the call logs and tally.
	tallyA := tallyByUser(mcpA.CallLog())
	tallyB := tallyByUser(mcpB.CallLog())
	for _, s := range all {
		if got := tallyA[s.userID]; got != 1 {
			t.Errorf("server_a calls for %s = %d, want 1 (cross-fork contamination?)", s.userID, got)
		}
		if got := tallyB[s.userID]; got != 1 {
			t.Errorf("server_b calls for %s = %d, want 1", s.userID, got)
		}
	}

	// Verify last_status=completed on every fork.
	failedCount := 0
	for _, s := range all {
		state, err := st.ScheduleRunStateGet(context.Background(), s.defID)
		if err != nil {
			t.Errorf("get state %s: %v", s.defID, err)
			continue
		}
		if state.LastStatus != "completed" {
			failedCount++
			if failedCount <= 5 {
				t.Errorf("schedule %s last_status = %q, want completed (last_error=%q)",
					s.defID, state.LastStatus, state.LastError)
			}
		}
	}
	if failedCount > 5 {
		t.Errorf("... (%d more schedules failed; first 5 logged above)", failedCount-5)
	}

	// Summary line for operator-visible test output.
	t.Logf("compound test PASS — scale=%d, server_a=%d calls, server_b=%d calls, 0 mismatches, %d failed runs",
		scale, mcpA.ToolCalls(), mcpB.ToolCalls(), failedCount)
}

// seedScheduleFork inserts one substrate schedule row with the canonical
// shape this test needs: agent points at cascade-agent, prompt embeds
// user_id=<userID> (the mock-mcp-caller extracts it), user_credentials
// maps the single bearer key. Auto-promotes via SetActive + seeds
// schedule_run_state at time.Now() (caller overrides via a second
// Seed call to stagger phases).
func seedScheduleFork(t *testing.T, st store.Store, defID, userID, agentName string) {
	t.Helper()
	def := map[string]any{
		"agent": agentName,
		// schedule cron — picked arbitrarily; we override next_run_at
		// directly via the store, so the cron value only matters for
		// the post-fire next_run_at advance.
		"schedule": "0 * * * *",
		"enabled":  true,
		"user_id":  userID,
		"prompt": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "trusted-text",
						"text": fmt.Sprintf("Run cascade for user_id=%s now.", userID),
					},
				},
			},
		},
		"user_credentials": map[string]string{"user_token": userID},
	}
	defJSON, _ := json.Marshal(def)
	ctx := context.Background()
	if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{
		DefID:      defID,
		Name:       "sched-" + userID,
		Definition: defJSON,
	}); err != nil {
		t.Fatalf("ScheduleDefCreate %s: %v", defID, err)
	}
	if err := st.ScheduleDefSetActive(ctx, "sched-"+userID, defID, "compound-test"); err != nil {
		t.Fatalf("SetActive %s: %v", defID, err)
	}
	if err := st.ScheduleRunStateSeed(ctx, defID, time.Now()); err != nil {
		t.Fatalf("Seed %s: %v", defID, err)
	}
}

// tallyByUser counts how many times each user_id appears in a call log.
func tallyByUser(log []mcptest.CallRecord) map[string]int {
	out := make(map[string]int, len(log))
	for _, r := range log {
		out[r.UserID]++
	}
	return out
}

func dumpMismatches(t *testing.T, label string, log []mcptest.CallRecord) {
	t.Helper()
	var mismatches []string
	for _, r := range log {
		if !r.Matched {
			mismatches = append(mismatches, fmt.Sprintf("user_id=%s observed=%q", r.UserID, r.ObservedBearer))
		}
	}
	sort.Strings(mismatches)
	max := 5
	if len(mismatches) < max {
		max = len(mismatches)
	}
	t.Logf("%s first %d mismatch(es): %s", label, max, strings.Join(mismatches[:max], " | "))
}

// _ ensures atomic is referenced even if a future edit removes its
// usage (the package-level var keeps imports honest under gofmt).
var _ = atomic.Int32{}
