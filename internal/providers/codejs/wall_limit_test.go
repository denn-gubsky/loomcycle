package codejs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Waits do not spend a code agent's budget, so the budget alone cannot end a
// run that does nothing but wait. The wall limit — its whole lifetime, waits
// included — does, and it ends the run by cancelling it: whatever it is
// waiting on is stopped too, not just its next turn.

const wallLimit = 2 * time.Second

// runWallLimited runs js under the wall limit, with opts adjusted by tweak.
func runWallLimited(t *testing.T, ctx context.Context, js string, tweak func(*loop.RunOptions), ts ...tools.Tool) (time.Duration, error) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "orch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := codejs.New(codejs.Config{CodeRoot: root, RunTimeout: time.Second, MaxWall: wallLimit})
	opts := loop.RunOptions{
		Provider:   prov,
		Model:      "code-js",
		AgentName:  "orch",
		Tools:      ts,
		Dispatcher: tools.NewDispatcher(ts),
		Segments:   []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	}
	if tweak != nil {
		tweak(&opts)
	}
	start := time.Now()
	_, err := loop.Run(ctx, opts)
	return time.Since(start), err
}

// channelFixture is a Channel tool over in-memory SQLite and a live bus, and a
// ctx that may subscribe to channel "inbox", which nothing ever publishes to.
func channelFixture(t *testing.T) (*builtin.Channel, context.Context) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tool := &builtin.Channel{Store: s, Bus: channels.NewBus(), MaxValueBytes: 65536, LongPollCapMS: 30000}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "alice", AgentID: "a_orch"})
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Subscribe: []string{"inbox"},
		Channels:  map[string]tools.ChannelDef{"inbox": {Name: "inbox", Scope: "global", Semantic: "queue"}},
	})
	return tool, ctx
}

// A script looping Channel.await spends almost no budget; the wall limit
// stops it, cancelling the 30s await it is in rather than waiting it out.
func TestCodeJSOrchestrator_WaitLoopIsStoppedAtTheWallLimit(t *testing.T) {
	ch, ctx := channelFixture(t)
	js := `
function run() {
  while (true) { Channel.await({channels: ["inbox"], wait_ms: 30000}); }
}`
	took, err := runWallLimited(t, ctx, js, nil, ch)
	if err == nil || !strings.HasPrefix(err.Error(), "code_agent_wall_limit") {
		t.Fatalf("a run that only waits must end at its wall limit with code_agent_wall_limit; got %v", err)
	}
	if took > wallLimit+2*time.Second {
		t.Errorf("the run ended after %s — the await it was in was not cancelled at the %s limit", took, wallLimit)
	}
}

// The children of a run stopped at its wall limit are cancelled with it.
func TestCodeJSOrchestrator_WallLimitCancelsItsChildren(t *testing.T) {
	var cancelled atomic.Bool
	forever := func(ctx context.Context, _, _, _ string) (string, error) {
		<-ctx.Done()
		cancelled.Store(true)
		return "", ctx.Err()
	}
	js := `
function run() {
  Agent.spawn({op: "parallel_spawn", spawns: [{name: "kid", prompt: "a"}, {name: "kid", prompt: "b"}]});
  return { final_text: "never" };
}`
	took, err := runWallLimited(t, context.Background(), js, nil, &builtin.AgentTool{Run: forever})
	if err == nil || !strings.HasPrefix(err.Error(), "code_agent_wall_limit") {
		t.Fatalf("want code_agent_wall_limit; got %v", err)
	}
	if !cancelled.Load() {
		t.Error("the run ended at its wall limit but its children were never cancelled")
	}
	if took > wallLimit+2*time.Second {
		t.Errorf("the run ended after %s, not at its %s limit", took, wallLimit)
	}
}

// The per-run and per-agent knobs set the BUDGET; none of them reaches the
// operator's wall limit, so an agent author cannot lift it.
func TestCodeJSOrchestrator_RunTimeoutOverrideCannotLiftTheWallLimit(t *testing.T) {
	ch, ctx := channelFixture(t)
	js := `
function run() {
  while (true) { Channel.await({channels: ["inbox"], wait_ms: 30000}); }
}`
	took, err := runWallLimited(t, ctx, js, func(o *loop.RunOptions) { o.RunTimeoutSeconds = 1_000_000 }, ch)
	if err == nil || !strings.HasPrefix(err.Error(), "code_agent_wall_limit") || took > wallLimit+2*time.Second {
		t.Fatalf("a huge run_timeout_seconds lifted the wall limit: ended after %s with %v", took, err)
	}
}

// A resumed run continues its lifetime rather than starting a new one: one
// that had lived all but half a second of its limit before it paused is
// stopped about half a second in.
func TestCodeJSOrchestrator_ResumeDoesNotResetTheWallLimit(t *testing.T) {
	ch, ctx := channelFixture(t)
	js := `
function run() {
  while (true) { Channel.await({channels: ["inbox"], wait_ms: 30000}); }
}`
	carry := providers.RunClockState{Wall: wallLimit - 500*time.Millisecond}
	took, err := runWallLimited(t, ctx, js, func(o *loop.RunOptions) { o.RunClockCarry = carry }, ch)
	if err == nil || !strings.HasPrefix(err.Error(), "code_agent_wall_limit") {
		t.Fatalf("want code_agent_wall_limit; got %v", err)
	}
	if took > wallLimit-500*time.Millisecond+time.Second {
		t.Errorf("the resumed run lived %s more; it had %s of its lifetime left", took, 500*time.Millisecond)
	}
}
