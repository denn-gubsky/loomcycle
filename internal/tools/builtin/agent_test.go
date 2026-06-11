package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Happy path: input parses, runner is called with the right args, the
// returned text is wrapped in a successful Result.
func TestAgentTool_HappyPath(t *testing.T) {
	var gotName, gotPrompt string
	a := &AgentTool{Run: func(_ context.Context, name, prompt, _ string) (string, error) {
		gotName, gotPrompt = name, prompt
		return "sub-agent output", nil
	}}

	res, err := a.Execute(context.Background(),
		json.RawMessage(`{"name":"cv-adapter","prompt":"Generate CV"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Errorf("expected success, got IsError=true: %s", res.Text)
	}
	if res.Text != "sub-agent output" {
		t.Errorf("Text = %q", res.Text)
	}
	if gotName != "cv-adapter" || gotPrompt != "Generate CV" {
		t.Errorf("runner got (%q, %q)", gotName, gotPrompt)
	}
}

// Missing required fields surface as IsError tool_results so the model
// can self-correct, NOT as Go errors that tear down the run.
func TestAgentTool_MissingName(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		t.Fatal("runner should not be called when name is missing")
		return "", nil
	}}
	res, err := a.Execute(context.Background(), json.RawMessage(`{"prompt":"X"}`))
	if err != nil {
		t.Fatalf("Execute returned hard error: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError=true on missing name")
	}
	if !strings.Contains(res.Text, "name") {
		t.Errorf("error should name the missing field: %q", res.Text)
	}
}

func TestAgentTool_MissingPrompt(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		t.Fatal("runner should not be called when prompt is missing")
		return "", nil
	}}
	res, _ := a.Execute(context.Background(), json.RawMessage(`{"name":"foo"}`))
	if !res.IsError || !strings.Contains(res.Text, "prompt") {
		t.Errorf("expected missing-prompt IsError, got %+v", res)
	}
}

// Whitespace-only name is treated as missing — guards against the model
// emitting `"name": "  "` and getting a confusing "unknown agent" error
// from the runner instead of a clean "missing field" response.
func TestAgentTool_WhitespaceNameRejected(t *testing.T) {
	called := false
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		called = true
		return "", nil
	}}
	res, _ := a.Execute(context.Background(), json.RawMessage(`{"name":"   ","prompt":"X"}`))
	if called {
		t.Error("runner should not be called with whitespace-only name")
	}
	if !res.IsError {
		t.Error("expected IsError on whitespace name")
	}
}

// Malformed JSON input is also a model-correctable error, not a hard
// crash. The model gets feedback "invalid input JSON" and can retry.
func TestAgentTool_MalformedJSON(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		t.Fatal("runner should not be called on malformed JSON")
		return "", nil
	}}
	res, err := a.Execute(context.Background(), json.RawMessage(`{not json`))
	if err != nil {
		t.Fatalf("Execute returned hard error: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError=true on malformed JSON")
	}
}

// Runner errors propagate to the model as IsError tool_results — the
// parent run continues so the model can decide how to recover (try a
// different sub-agent, fall back, give up gracefully).
func TestAgentTool_RunnerError(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		return "", errors.New("provider returned 500")
	}}
	res, err := a.Execute(context.Background(),
		json.RawMessage(`{"name":"x","prompt":"y"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError=true when runner fails")
	}
	if !strings.Contains(res.Text, "provider returned 500") {
		t.Errorf("error message lost: %q", res.Text)
	}
}

// A sub-agent that ends_turn with no text is rare but possible (made
// only tool calls, then stopped). Surface a hint so the parent's
// model has something concrete to read instead of empty Text.
func TestAgentTool_EmptyOutputHint(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		return "", nil
	}}
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"name":"silent-agent","prompt":"x"}`))
	if res.IsError {
		t.Errorf("empty output is not an error: %+v", res)
	}
	if !strings.Contains(res.Text, "silent-agent") || !strings.Contains(res.Text, "no final text") {
		t.Errorf("hint should name the agent and explain: %q", res.Text)
	}
}

// Recursion guard: a context already at MaxAgentDepth refuses to spawn.
// EMPIRICAL: with the depth check disabled, this test fails because
// the runner gets called instead of the IsError path.
func TestAgentTool_MaxDepthGuard(t *testing.T) {
	called := false
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		called = true
		return "should not run", nil
	}}
	ctx := context.Background()
	for i := 0; i < MaxAgentDepth; i++ {
		ctx = IncrementAgentDepth(ctx)
	}
	res, _ := a.Execute(ctx, json.RawMessage(`{"name":"x","prompt":"y"}`))
	if called {
		t.Error("runner should not have been invoked at max depth")
	}
	if !res.IsError || !strings.Contains(res.Text, "max sub-agent recursion depth") {
		t.Errorf("expected max-depth IsError, got %+v", res)
	}
}

// One depth below the cap is still allowed — the guard fires at >=,
// not >. Worth pinning so a future refactor doesn't shift the
// boundary by one.
func TestAgentTool_DepthBelowCapAllowed(t *testing.T) {
	called := false
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		called = true
		return "ok", nil
	}}
	ctx := context.Background()
	for i := 0; i < MaxAgentDepth-1; i++ {
		ctx = IncrementAgentDepth(ctx)
	}
	res, _ := a.Execute(ctx, json.RawMessage(`{"name":"x","prompt":"y"}`))
	if !called {
		t.Error("runner should run at depth = MaxAgentDepth-1")
	}
	if res.IsError {
		t.Errorf("unexpected error: %s", res.Text)
	}
}

// AgentDepth on a fresh context returns 0 — the top-level run is depth 0.
func TestAgentDepth_DefaultsToZero(t *testing.T) {
	if d := AgentDepth(context.Background()); d != 0 {
		t.Errorf("AgentDepth(empty ctx) = %d, want 0", d)
	}
}

// IncrementAgentDepth chains: each call increments by exactly one.
func TestIncrementAgentDepth_Chains(t *testing.T) {
	ctx := context.Background()
	for want := 1; want <= 5; want++ {
		ctx = IncrementAgentDepth(ctx)
		if got := AgentDepth(ctx); got != want {
			t.Errorf("after %d increments: depth = %d, want %d", want, got, want)
		}
	}
}

// Defensive: tool with nil Run surfaces a clear error so a misconfigured
// server doesn't silently accept Agent calls.
func TestAgentTool_NilRunner(t *testing.T) {
	a := &AgentTool{} // Run is nil
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"name":"x","prompt":"y"}`))
	if !res.IsError || !strings.Contains(res.Text, "not wired") {
		t.Errorf("expected nil-runner IsError, got %+v", res)
	}
}

// v0.8.5 PR 5: optional def_id pins this sub-run to a specific
// agent_defs row. Test only that the tool propagates the field
// through to the runner — actual lookup + overlay is wired in the
// HTTP server's runSubAgent and tested at that layer.
func TestAgentTool_DefIDPassthrough(t *testing.T) {
	var gotDef string
	a := &AgentTool{Run: func(_ context.Context, _, _, defID string) (string, error) {
		gotDef = defID
		return "ok", nil
	}}
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"name":"x","prompt":"y","def_id":"def_abc"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if gotDef != "def_abc" {
		t.Errorf("def_id passthrough lost: %q", gotDef)
	}
}

// Backwards-compat: omitting def_id leaves it empty (zero-value).
func TestAgentTool_DefIDOmittedDefaultsEmpty(t *testing.T) {
	var gotDef string
	a := &AgentTool{Run: func(_ context.Context, _, _, defID string) (string, error) {
		gotDef = defID
		return "ok", nil
	}}
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"name":"x","prompt":"y"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if gotDef != "" {
		t.Errorf("def_id should default empty, got %q", gotDef)
	}
}

// ---- v0.11.8 parallel_spawn op tests ----

// TestAgentTool_ParallelSpawn_HappyPath: 3 children spawn concurrently,
// envelope preserves input order, all marked ok=true.
func TestAgentTool_ParallelSpawn_HappyPath(t *testing.T) {
	a := &AgentTool{Run: func(_ context.Context, name, prompt, _ string) (string, error) {
		return "result-" + name + "-" + prompt, nil
	}}
	res, err := a.Execute(context.Background(), json.RawMessage(`{
		"op": "parallel_spawn",
		"spawns": [
			{"name": "researcher", "prompt": "topic-A"},
			{"name": "researcher", "prompt": "topic-B"},
			{"name": "summarizer", "prompt": "X"}
		]
	}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got IsError=true: %s", res.Text)
	}
	var env struct {
		Results []parallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil {
		t.Fatalf("envelope unmarshal: %v; raw=%s", err, res.Text)
	}
	if len(env.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(env.Results))
	}
	for i, r := range env.Results {
		if r.Index != i {
			t.Errorf("results[%d].Index = %d, want %d", i, r.Index, i)
		}
		if !r.Ok || r.Error != "" {
			t.Errorf("results[%d] not ok: %+v", i, r)
		}
	}
	if env.Results[0].Output != "result-researcher-topic-A" {
		t.Errorf("results[0].Output = %q", env.Results[0].Output)
	}
	if env.Results[2].Agent != "summarizer" {
		t.Errorf("results[2].Agent = %q", env.Results[2].Agent)
	}
}

// TestAgentTool_ParallelSpawn_ChildErrorCaptured: per-child error
// shows up inside the envelope, NOT as a tool-level IsError. The
// other children still run + report success.
func TestAgentTool_ParallelSpawn_ChildErrorCaptured(t *testing.T) {
	a := &AgentTool{Run: func(_ context.Context, name, _, _ string) (string, error) {
		if name == "boom" {
			return "", errors.New("backend unreachable")
		}
		return "ok-" + name, nil
	}}
	res, err := a.Execute(context.Background(), json.RawMessage(`{
		"op": "parallel_spawn",
		"spawns": [
			{"name": "good", "prompt": "x"},
			{"name": "boom", "prompt": "y"},
			{"name": "good", "prompt": "z"}
		]
	}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool-level error should NOT escalate per-child failures: %s", res.Text)
	}
	var env struct {
		Results []parallelSpawnResult `json:"results"`
	}
	_ = json.Unmarshal([]byte(res.Text), &env)
	if !env.Results[0].Ok || !env.Results[2].Ok {
		t.Errorf("good children should succeed: %+v", env.Results)
	}
	if env.Results[1].Ok {
		t.Errorf("boom child should not be ok: %+v", env.Results[1])
	}
	if !strings.Contains(env.Results[1].Error, "backend unreachable") {
		t.Errorf("boom child should surface backend error: %q", env.Results[1].Error)
	}
}

// TestAgentTool_ParallelSpawn_CapEnforced verifies the per-agent cap
// throttles concurrent goroutines. Each child sleeps long enough that
// we can measure how many were running at peak. With cap=2 + 4 spawns,
// peak concurrency should never exceed 2.
func TestAgentTool_ParallelSpawn_CapEnforced(t *testing.T) {
	var active, peak int32
	var mu sync.Mutex
	a := &AgentTool{
		CapLookup: func(_ context.Context, callingAgent string) int {
			if callingAgent == "parent-with-cap" {
				return 2
			}
			return 0
		},
		Run: func(_ context.Context, _, _, _ string) (string, error) {
			n := atomic.AddInt32(&active, 1)
			mu.Lock()
			if n > peak {
				peak = n
			}
			mu.Unlock()
			time.Sleep(25 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			return "ok", nil
		},
	}
	ctx := tools.WithAgentName(context.Background(), "parent-with-cap")
	res, err := a.Execute(ctx, json.RawMessage(`{
		"op": "parallel_spawn",
		"spawns": [
			{"name": "child", "prompt": "a"},
			{"name": "child", "prompt": "b"},
			{"name": "child", "prompt": "c"},
			{"name": "child", "prompt": "d"}
		]
	}`))
	if err != nil || res.IsError {
		t.Fatalf("unexpected error: err=%v text=%s", err, res.Text)
	}
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want <= 2 (cap should throttle)", peak)
	}
	if peak < 2 {
		// Not a hard failure (timing-dependent) but worth flagging —
		// if peak is 1 we never actually parallelized.
		t.Logf("peak concurrency = %d (parallelism may be inhibited by environment)", peak)
	}
}

// TestAgentTool_ParallelSpawn_DefaultCapWhenLookupNil: no CapLookup
// wired + no per-agent override → DefaultMaxConcurrentChildren (4)
// applies. 4 spawns with a sleeper runner should peak at exactly 4.
func TestAgentTool_ParallelSpawn_DefaultCapWhenLookupNil(t *testing.T) {
	var active, peak int32
	var mu sync.Mutex
	a := &AgentTool{Run: func(_ context.Context, _, _, _ string) (string, error) {
		n := atomic.AddInt32(&active, 1)
		mu.Lock()
		if n > peak {
			peak = n
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return "ok", nil
	}}
	res, _ := a.Execute(context.Background(), json.RawMessage(`{
		"op": "parallel_spawn",
		"spawns": [
			{"name": "c", "prompt": "a"},
			{"name": "c", "prompt": "b"},
			{"name": "c", "prompt": "c"},
			{"name": "c", "prompt": "d"}
		]
	}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if peak > DefaultMaxConcurrentChildren {
		t.Errorf("peak = %d, exceeded default cap %d", peak, DefaultMaxConcurrentChildren)
	}
}

// TestAgentTool_ParallelSpawn_DepthGuard: at MaxAgentDepth, refuse to
// spawn at all (the guard fires once per call, not per-child — the
// envelope shape would otherwise be misleading).
func TestAgentTool_ParallelSpawn_DepthGuard(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		t.Fatal("runner should not be called when depth cap is hit")
		return "", nil
	}}
	ctx := context.Background()
	for i := 0; i < MaxAgentDepth; i++ {
		ctx = IncrementAgentDepth(ctx)
	}
	res, _ := a.Execute(ctx, json.RawMessage(`{
		"op": "parallel_spawn",
		"spawns": [{"name": "x", "prompt": "y"}]
	}`))
	if !res.IsError {
		t.Errorf("expected IsError=true at depth %d", AgentDepth(ctx))
	}
	if !strings.Contains(res.Text, "depth") {
		t.Errorf("error should mention depth: %q", res.Text)
	}
}

// TestAgentTool_ParallelSpawn_EmptySpawnsRejected: explicit op with no
// spawns is a malformed input — refuse up-front, not as a zero-results
// envelope.
func TestAgentTool_ParallelSpawn_EmptySpawnsRejected(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		return "", nil
	}}
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"op":"parallel_spawn","spawns":[]}`))
	if !res.IsError {
		t.Errorf("empty spawns array should refuse, got: %s", res.Text)
	}
}

// TestAgentTool_ParallelSpawn_TooManySpawns: ceiling at
// MaxParallelSpawns; refuse the call rather than silently truncate.
func TestAgentTool_ParallelSpawn_TooManySpawns(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		t.Fatal("runner should not be called for over-cap input")
		return "", nil
	}}
	// Build a spawns array with MaxParallelSpawns + 1 entries.
	parts := make([]string, MaxParallelSpawns+1)
	for i := range parts {
		parts[i] = `{"name":"x","prompt":"y"}`
	}
	body := `{"op":"parallel_spawn","spawns":[` + strings.Join(parts, ",") + `]}`
	res, _ := a.Execute(context.Background(), json.RawMessage(body))
	if !res.IsError {
		t.Errorf("over-cap should refuse, got: %s", res.Text)
	}
	if !strings.Contains(res.Text, "ceiling") {
		t.Errorf("error should mention the ceiling: %q", res.Text)
	}
}

// TestAgentTool_ParallelSpawn_TopLevelFieldsRejected: mixing the
// single-spawn shape with parallel_spawn (e.g. forgetting to clear
// `name` when adding `op: parallel_spawn`) is a footgun — refuse so
// the model gets a clear error rather than a silently-wrong result.
func TestAgentTool_ParallelSpawn_TopLevelFieldsRejected(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		return "", nil
	}}
	res, _ := a.Execute(context.Background(), json.RawMessage(`{
		"op": "parallel_spawn",
		"name": "stray",
		"spawns": [{"name":"x","prompt":"y"}]
	}`))
	if !res.IsError {
		t.Errorf("stray top-level name should refuse, got: %s", res.Text)
	}
}

// TestAgentTool_ParallelSpawn_PerEntryValidation: a malformed entry
// (missing name) fails the whole call, not just the bad row.
func TestAgentTool_ParallelSpawn_PerEntryValidation(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		t.Fatal("runner should not be called when an entry is malformed")
		return "", nil
	}}
	res, _ := a.Execute(context.Background(), json.RawMessage(`{
		"op": "parallel_spawn",
		"spawns": [
			{"name": "good", "prompt": "x"},
			{"prompt": "no name"}
		]
	}`))
	if !res.IsError {
		t.Errorf("malformed entry should refuse the whole call, got: %s", res.Text)
	}
	if !strings.Contains(res.Text, "spawns[1]") {
		t.Errorf("error should pinpoint the bad index: %q", res.Text)
	}
}

// TestAgentTool_ParallelSpawn_DefIDPassthrough: per-child def_id
// reaches the runner.
func TestAgentTool_ParallelSpawn_DefIDPassthrough(t *testing.T) {
	gotDefs := make([]string, 0, 2)
	var mu sync.Mutex
	a := &AgentTool{Run: func(_ context.Context, _, _, def string) (string, error) {
		mu.Lock()
		gotDefs = append(gotDefs, def)
		mu.Unlock()
		return "ok", nil
	}}
	res, _ := a.Execute(context.Background(), json.RawMessage(`{
		"op": "parallel_spawn",
		"spawns": [
			{"name": "x", "prompt": "a", "def_id": "def_aaa"},
			{"name": "x", "prompt": "b"}
		]
	}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	// Order isn't guaranteed across goroutines; check the set.
	got := map[string]bool{}
	for _, d := range gotDefs {
		got[d] = true
	}
	if !got["def_aaa"] || !got[""] {
		t.Errorf("expected def_id passthrough; got %v", gotDefs)
	}
}

// TestAgentTool_ParallelSpawn_UnknownOpRejected: typo in `op` (e.g.
// "parallel" instead of "parallel_spawn") surfaces a clear error
// instead of silently routing to the spawn path.
func TestAgentTool_ParallelSpawn_UnknownOpRejected(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		return "", nil
	}}
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"op":"parallel","spawns":[{"name":"x","prompt":"y"}]}`))
	if !res.IsError {
		t.Errorf("unknown op should refuse, got: %s", res.Text)
	}
	if !strings.Contains(res.Text, "unknown op") {
		t.Errorf("error should mention 'unknown op': %q", res.Text)
	}
}

// TestAgentTool_Spawn_RejectsSpawnsArray: belt-and-suspenders — if the
// model sets op=spawn but also includes a `spawns` array, refuse so
// the wrong path doesn't silently consume the array.
func TestAgentTool_Spawn_RejectsSpawnsArray(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		return "ok", nil
	}}
	res, _ := a.Execute(context.Background(), json.RawMessage(`{
		"op": "spawn",
		"name": "x", "prompt": "y",
		"spawns": [{"name":"z","prompt":"w"}]
	}`))
	if !res.IsError {
		t.Errorf("op=spawn with spawns array should refuse, got: %s", res.Text)
	}
}

// ---- RFC X Phase 3: parallel_spawn park watcher + spawn ledger ----

// fanoutFakeGate is a tools.PauseGate stub for the parallel_spawn park-watcher
// test. triggerPause closes the current PauseCh (one pause cycle); Park rearms a
// fresh open channel BEFORE blocking (mirrors the real gate's
// fresh-channel-per-resume contract) so the watcher's re-watch select blocks
// rather than spinning on a permanently-closed channel.
type fanoutFakeGate struct {
	mu        sync.Mutex
	ch        chan struct{}
	parkCalls int32
	parked    chan struct{}
	parkOnce  sync.Once
	release   chan struct{}
}

func newFanoutFakeGate() *fanoutFakeGate {
	return &fanoutFakeGate{ch: make(chan struct{}), parked: make(chan struct{}), release: make(chan struct{})}
}

func (g *fanoutFakeGate) PauseRequested() bool { return false }

func (g *fanoutFakeGate) PauseCh() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ch
}

func (g *fanoutFakeGate) triggerPause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.ch)
}

func (g *fanoutFakeGate) Park(ctx context.Context) error {
	atomic.AddInt32(&g.parkCalls, 1)
	g.parkOnce.Do(func() { close(g.parked) })
	g.mu.Lock()
	g.ch = make(chan struct{}) // rearm so the watcher re-watch blocks
	g.mu.Unlock()
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestAgentTool_ParallelSpawn_ParksParentOnPause: when a pause is declared while
// the fan-out parent is blocked in wg.Wait, the Phase-3 watcher parks the parent
// (gate.Park) WITHOUT disturbing the in-flight child; on resume the fan-out
// completes and the envelope is correct. Fail-before: pre-Phase-3 there was no
// watcher, so the parent never parked mid-tool-call.
func TestAgentTool_ParallelSpawn_ParksParentOnPause(t *testing.T) {
	gate := newFanoutFakeGate()
	childStarted := make(chan struct{})
	childRelease := make(chan struct{})
	var startedOnce sync.Once

	a := &AgentTool{
		SpawnLedger: true,
		RunDetailed: func(_ context.Context, name, _, _ string) (string, string, error) {
			startedOnce.Do(func() { close(childStarted) })
			<-childRelease // simulate a long child still running when the pause hits
			return "child-out-" + name, "run_child_0", nil
		},
	}
	// Run is a thin wrapper so a.runChild prefers RunDetailed.
	a.Run = func(ctx context.Context, name, prompt, defID string) (string, error) {
		out, _, err := a.RunDetailed(ctx, name, prompt, defID)
		return out, err
	}

	var emu sync.Mutex
	var events []providers.Event
	emit := func(ev providers.Event) { emu.Lock(); events = append(events, ev); emu.Unlock() }

	ctx := tools.WithPauseGate(context.Background(), gate)
	ctx = tools.WithToolUseID(ctx, "tu_fan")
	ctx = tools.WithEventEmitter(ctx, emit)

	resCh := make(chan tools.Result, 1)
	go func() {
		res, _ := a.Execute(ctx, json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"solver","prompt":"p"}]}`))
		resCh <- res
	}()

	<-childStarted      // child is running
	gate.triggerPause() // declare a pause
	select {
	case <-gate.parked:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never parked the fan-out parent on pause")
	}
	if got := atomic.LoadInt32(&gate.parkCalls); got != 1 {
		t.Fatalf("park called %d times, want exactly 1", got)
	}

	close(childRelease) // child returns → wg.Wait completes → Execute returns via defer
	close(gate.release) // let Park return so the watcher exits + is joined

	var res tools.Result
	select {
	case res = <-resCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return after resume (watcher join hung?)")
	}
	if res.IsError {
		t.Fatalf("envelope IsError after resume: %s", res.Text)
	}
	var env struct {
		Results []parallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil {
		t.Fatalf("envelope unmarshal: %v; raw=%s", err, res.Text)
	}
	if len(env.Results) != 1 || !env.Results[0].Ok || env.Results[0].Output != "child-out-solver" {
		t.Fatalf("post-resume envelope wrong: %+v", env.Results)
	}
	// The ledger recorded the completed child (spawn_child_result on the parent).
	emu.Lock()
	defer emu.Unlock()
	var sawResult bool
	for _, ev := range events {
		if ev.Type == providers.EventSpawnChildResult && ev.SpawnChild != nil && ev.SpawnChild.ToolUseID == "tu_fan" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Errorf("expected a spawn_child_result ledger event for tu_fan; got %d events", len(events))
	}
}

// TestAgentTool_ParallelSpawn_NoWatcherNoLedgerWhenFlagOff: with SpawnLedger
// off (the default), parallel_spawn emits NO ledger events and starts NO park
// watcher even when a gate + emitter are present — proving the Phase-3 capture
// side is byte-identical to pre-Phase-3 until the operator opts in.
func TestAgentTool_ParallelSpawn_NoWatcherNoLedgerWhenFlagOff(t *testing.T) {
	gate := newFanoutFakeGate()
	a := &AgentTool{
		// SpawnLedger defaults false.
		RunDetailed: func(_ context.Context, name, _, _ string) (string, string, error) {
			return "out-" + name, "run_x", nil
		},
	}
	a.Run = func(ctx context.Context, name, prompt, defID string) (string, error) {
		out, _, err := a.RunDetailed(ctx, name, prompt, defID)
		return out, err
	}
	var emu sync.Mutex
	var events []providers.Event
	emit := func(ev providers.Event) { emu.Lock(); events = append(events, ev); emu.Unlock() }
	ctx := tools.WithPauseGate(context.Background(), gate)
	ctx = tools.WithToolUseID(ctx, "tu_fan")
	ctx = tools.WithEventEmitter(ctx, emit)

	res, err := a.Execute(ctx, json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"solver","prompt":"p"}]}`))
	if err != nil || res.IsError {
		t.Fatalf("Execute failed: err=%v isErr=%v text=%s", err, res.IsError, res.Text)
	}
	// Trigger a pause AFTER completion — a watcher (if it had wrongly started)
	// would call Park; a correctly-disabled one never does.
	gate.triggerPause()
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&gate.parkCalls); got != 0 {
		t.Errorf("park called %d times with SpawnLedger off, want 0", got)
	}
	emu.Lock()
	defer emu.Unlock()
	if len(events) != 0 {
		t.Errorf("expected no ledger events with SpawnLedger off, got %d", len(events))
	}
}
