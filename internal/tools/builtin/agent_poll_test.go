package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// gatedPollChildren is a RunDetailed whose children each wait for their own
// gate (or ctx), recording when each started and ended. A child answers with
// the run id it was handed.
type gatedPollChildren struct {
	mu      sync.Mutex
	gates   map[string]chan struct{} // by prompt
	started map[string]time.Time
	ended   map[string]time.Time
	runIDs  map[string]string // prompt → child run id it ran under
	fail    map[string]bool
}

func newGated(prompts ...string) *gatedPollChildren {
	g := &gatedPollChildren{gates: map[string]chan struct{}{}, started: map[string]time.Time{}, ended: map[string]time.Time{}, runIDs: map[string]string{}, fail: map[string]bool{}}
	for _, p := range prompts {
		g.gates[p] = make(chan struct{})
	}
	return g
}

func (g *gatedPollChildren) run(ctx context.Context, name, prompt, _ string) (string, map[string]any, string, error) {
	g.mu.Lock()
	gate := g.gates[prompt]
	g.started[prompt] = time.Now()
	g.runIDs[prompt] = tools.ChildRunID(ctx)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.ended[prompt] = time.Now()
		g.mu.Unlock()
	}()
	select {
	case <-gate:
	case <-ctx.Done():
		return "", nil, tools.ChildRunID(ctx), context.Cause(ctx)
	}
	if g.fail[prompt] {
		return "", nil, tools.ChildRunID(ctx), errors.New("sub-agent " + name + " failed")
	}
	return "answer to " + prompt, nil, tools.ChildRunID(ctx), nil
}

func (g *gatedPollChildren) open(prompt string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.gates[prompt])
}

func (g *gatedPollChildren) startedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.started)
}

func pollTool(g *gatedPollChildren) *AgentTool {
	return &AgentTool{
		Run:          func(context.Context, string, string, string) (string, error) { return "", nil },
		RunDetailed:  g.run,
		LiveChildren: tools.NewLiveChildren(func() int { return 32 }),
	}
}

// pollCtx is a parent run's tool ctx: its run id, its background table and an
// iteration budget with turns left.
func pollCtx() (context.Context, *tools.Background) {
	bg := tools.NewBackground(context.Background())
	ctx := tools.WithRunID(context.Background(), "r_parent")
	ctx = tools.WithBackground(ctx, bg)
	ctx = tools.WithIterationBudget(ctx, 2, 10, false)
	return ctx, bg
}

func execJSON(t *testing.T, a *AgentTool, ctx context.Context, in string) tools.Result {
	t.Helper()
	res, err := a.Execute(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

type pollAnswer struct {
	Children []pollRow `json:"children"`
	Pending  int       `json:"pending"`
}

func decodePoll(t *testing.T, res tools.Result) pollAnswer {
	t.Helper()
	if res.IsError {
		t.Fatalf("poll failed: %s", res.Text)
	}
	var p pollAnswer
	if err := json.Unmarshal([]byte(res.Text), &p); err != nil {
		t.Fatalf("poll answer %q: %v", res.Text, err)
	}
	return p
}

type batchAnswer struct {
	BatchID  string         `json:"batch_id"`
	Children []pollStartRow `json:"children"`
}

func decodeBatch(t *testing.T, res tools.Result) batchAnswer {
	t.Helper()
	if res.IsError {
		t.Fatalf("parallel_spawn failed: %s", res.Text)
	}
	var b batchAnswer
	if err := json.Unmarshal([]byte(res.Text), &b); err != nil {
		t.Fatalf("batch answer %q: %v", res.Text, err)
	}
	return b
}

// A parent spawns three children in poll mode and the calls return at once,
// with every child running; the parent makes two more calls while they work,
// and a poll waiting for all of them collects all three results.
func TestAgentPoll_ChildrenRunWhileTheParentWorks(t *testing.T) {
	g := newGated("a", "b", "c")
	a := pollTool(g)
	ctx, _ := pollCtx()

	spawn := execJSON(t, a, ctx, `{"op":"spawn","name":"w","prompt":"a","mode":"poll"}`)
	var one pollStartRow
	if err := json.Unmarshal([]byte(spawn.Text), &one); err != nil || !strings.HasPrefix(one.ChildRunID, "r_") || one.Agent != "w" || one.State != "running" {
		t.Fatalf("spawn answer = %s (%v), want {child_run_id, agent w, state running}", spawn.Text, err)
	}
	batch := decodeBatch(t, execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"w","prompt":"b"},{"name":"w","prompt":"c"}]}`))
	if !strings.HasPrefix(batch.BatchID, "fan_") || len(batch.Children) != 2 || *batch.Children[1].Index != 1 {
		t.Fatalf("batch answer = %+v", batch)
	}
	waitUntil(t, func() bool { return g.startedCount() == 3 })

	// Two more calls of the parent's own while its children work.
	var parentCalls []time.Time
	for range 2 {
		p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll"}`))
		if p.Pending != 3 {
			t.Fatalf("pending = %d, want all three still running", p.Pending)
		}
		parentCalls = append(parentCalls, time.Now())
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		g.open("a")
		g.open("b")
		g.open("c")
	}()
	p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll","wait":"all","wait_ms":10000}`))
	if p.Pending != 0 || len(p.Children) != 3 {
		t.Fatalf("poll wait all = %+v, want three ended", p)
	}
	for _, r := range p.Children {
		if r.State != tools.ChildCompleted || !strings.HasPrefix(r.Output, "answer to ") {
			t.Errorf("row = %+v, want completed with its answer", r)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for prompt, start := range g.started {
		if !start.Before(parentCalls[0]) || !g.ended[prompt].After(parentCalls[1]) {
			t.Errorf("child %s ran %v..%v, not across the parent's calls at %v", prompt, start, g.ended[prompt], parentCalls)
		}
		if g.runIDs[prompt] == "" {
			t.Errorf("child %s ran with no minted run id", prompt)
		}
	}
	if g.runIDs["a"] != one.ChildRunID {
		t.Errorf("child a ran under %q, was handed out as %q", g.runIDs["a"], one.ChildRunID)
	}
}

// Wait mode is unchanged by the mode field: an explicit "wait" returns the
// same envelope as no mode, its children's results and run ids included.
func TestAgentPoll_WaitModeEnvelopeIsUnchanged(t *testing.T) {
	a := &AgentTool{
		Run: func(context.Context, string, string, string) (string, error) { return "", nil },
		RunDetailed: func(_ context.Context, name, prompt, _ string) (string, map[string]any, string, error) {
			return "out " + prompt, nil, "r_" + prompt, nil
		},
	}
	ctx, bg := pollCtx()
	plain := execJSON(t, a, ctx, `{"op":"parallel_spawn","spawns":[{"name":"w","prompt":"x"},{"name":"w","prompt":"y"}]}`)
	explicit := execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"wait","spawns":[{"name":"w","prompt":"x"},{"name":"w","prompt":"y"}]}`)
	const want = `{"results":[{"index":0,"agent":"w","ok":true,"output":"out x","run_id":"r_x"},{"index":1,"agent":"w","ok":true,"output":"out y","run_id":"r_y"}]}`
	if plain.Text != want || explicit.Text != want {
		t.Errorf("envelopes:\n%s\n%s\nwant\n%s", plain.Text, explicit.Text, want)
	}
	if got, _ := bg.Select(nil, ""); len(got) != 0 {
		t.Errorf("wait-mode children were filed as background children: %+v", got)
	}
}

// wait "any" returns once one child has ended; a bare poll hands each result
// over once (unread), a poll by id returns it every time, and an id this run
// did not start — or another run's — is refused like an unknown one.
func TestAgentPoll_WaitAnyUnreadOnceAndIdempotentByID(t *testing.T) {
	g := newGated("fast", "slow")
	a := pollTool(g)
	ctx, _ := pollCtx()
	batch := decodeBatch(t, execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"w","prompt":"fast"},{"name":"w","prompt":"slow"}]}`))
	fast, slow := batch.Children[0].ChildRunID, batch.Children[1].ChildRunID
	go func() { time.Sleep(20 * time.Millisecond); g.open("fast") }()
	p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll","batch_id":"`+batch.BatchID+`","wait":"any","wait_ms":10000}`))
	if p.Pending != 1 || p.Children[0].State != tools.ChildCompleted || p.Children[1].State != tools.ChildRunning {
		t.Fatalf("wait any = %+v, want fast completed and slow running", p)
	}
	// fast was read by that poll: a bare poll now lists only slow.
	if p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll"}`)); len(p.Children) != 1 || p.Children[0].ChildRunID != slow {
		t.Errorf("bare poll = %+v, want only the unread slow child", p)
	}
	for range 2 {
		p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll","child_run_ids":["`+fast+`"]}`))
		if len(p.Children) != 1 || p.Children[0].Output != "answer to fast" {
			t.Errorf("poll by id = %+v, want fast's result every time", p)
		}
	}
	other, _ := pollCtx() // another run's table
	for _, c := range []context.Context{ctx, other} {
		res := execJSON(t, a, c, `{"op":"poll","child_run_ids":["r_nobody"]}`)
		if !res.IsError || !strings.Contains(res.Text, "not a child of this run: r_nobody") {
			t.Errorf("unknown id = %+v", res)
		}
	}
	foreign := execJSON(t, a, other, `{"op":"poll","child_run_ids":["`+fast+`"]}`)
	unknown := execJSON(t, a, other, `{"op":"poll","child_run_ids":["r_nobody"]}`)
	if foreign.IsError != unknown.IsError || strings.Replace(foreign.Text, fast, "r_nobody", 1) != unknown.Text || *foreign.Error != *unknown.Error {
		t.Errorf("another run's child answered %+v, an unknown id %+v; want the same answer", foreign, unknown)
	}
	g.open("slow")
}

// Cancelling one child of a batch ends it cancelled and leaves its siblings
// running.
func TestAgentPoll_CancelOneChildOfABatchLeavesTheRest(t *testing.T) {
	g := newGated("x", "y", "z")
	a := pollTool(g)
	ctx, _ := pollCtx()
	batch := decodeBatch(t, execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"w","prompt":"x"},{"name":"w","prompt":"y"},{"name":"w","prompt":"z"}]}`))
	waitUntil(t, func() bool { return g.startedCount() == 3 })
	y := batch.Children[1].ChildRunID
	res := execJSON(t, a, ctx, `{"op":"cancel","child_run_ids":["`+y+`"]}`)
	var c pollAnswer
	if err := json.Unmarshal([]byte(res.Text), &c); err != nil || len(c.Children) != 1 || c.Children[0].State != tools.ChildCancelled {
		t.Fatalf("cancel = %s, want y cancelled", res.Text)
	}
	p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll","batch_id":"`+batch.BatchID+`"}`))
	got := []string{p.Children[0].State, p.Children[1].State, p.Children[2].State}
	if strings.Join(got, ",") != "running,cancelled,running" {
		t.Errorf("states = %v, want only y cancelled", got)
	}
	g.open("x")
	g.open("z")
}

// A parallel_spawn wider than its concurrency answers the extra children
// queued; the queue keeps draining after the call has returned.
func TestAgentPoll_QueuedChildrenStartAfterTheCallReturns(t *testing.T) {
	g := newGated("first", "second")
	a := pollTool(g)
	a.CapLookup = func(context.Context, string) int { return 1 }
	ctx, _ := pollCtx()
	ctx = tools.WithAgentName(ctx, "lead")
	batch := decodeBatch(t, execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"w","prompt":"first"},{"name":"w","prompt":"second"}]}`))
	if batch.Children[0].State != tools.ChildRunning || batch.Children[1].State != tools.ChildQueued {
		t.Fatalf("states = %+v, want running then queued", batch.Children)
	}
	g.open("first")
	g.open("second")
	p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll","wait":"all","wait_ms":10000}`))
	if p.Pending != 0 || p.Children[1].Output != "answer to second" {
		t.Errorf("poll = %+v, want the queued child run and completed", p)
	}
}

// Poll mode is refused on the run's last iteration — no turn would be left
// to collect the children — and nothing is started.
func TestAgentPoll_RefusedOnTheLastIteration(t *testing.T) {
	g := newGated("x")
	a := pollTool(g)
	ctx, bg := pollCtx()
	ctx = tools.WithIterationBudget(ctx, 10, 10, false)
	for _, in := range []string{
		`{"op":"spawn","name":"w","prompt":"x","mode":"poll"}`,
		`{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"w","prompt":"x"}]}`,
	} {
		res := execJSON(t, a, ctx, in)
		if !res.IsError || !strings.Contains(res.Text, "last iteration") {
			t.Errorf("%s: %+v, want a last-iteration refusal", in, res)
		}
	}
	if got, _ := bg.Select(nil, ""); len(got) != 0 || a.LiveChildren.Alive("r_parent") != 0 {
		t.Errorf("a refused call started children: %+v", got)
	}
	// An unbounded run has no last iteration to refuse on.
	ctx = tools.WithIterationBudget(ctx, 10, 10, true)
	if res := execJSON(t, a, ctx, `{"op":"spawn","name":"w","prompt":"x","mode":"poll"}`); res.IsError {
		t.Errorf("an unbounded run was refused: %s", res.Text)
	}
	g.open("x")
}

// A poll-mode child holds its live-children slot until it ENDS, not until the
// call that started it returns.
func TestAgentPoll_LiveChildrenCountUntilTheyEnd(t *testing.T) {
	g := newGated("x", "y")
	a := pollTool(g)
	ctx, _ := pollCtx()
	execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"w","prompt":"x"},{"name":"w","prompt":"y"}]}`)
	if n := a.LiveChildren.Alive("r_parent"); n != 2 {
		t.Fatalf("%d alive after the call returned, want 2", n)
	}
	g.open("x")
	waitUntil(t, func() bool { return a.LiveChildren.Alive("r_parent") == 1 })
	g.open("y")
	waitUntil(t, func() bool { return a.LiveChildren.Alive("r_parent") == 0 })
}

// notify and on_parent_end reach the table; they apply to poll mode only.
func TestAgentPoll_NotifyAndOnParentEndReachTheTable(t *testing.T) {
	g := newGated("quiet", "bound")
	a := pollTool(g)
	ctx, bg := pollCtx()
	quiet := decodeBatch(t, execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"poll","notify":false,"spawns":[{"name":"w","prompt":"quiet"}]}`)).Children[0].ChildRunID
	bound := decodeBatch(t, execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"poll","on_parent_end":"cancel","spawns":[{"name":"w","prompt":"bound"}]}`)).Children[0].ChildRunID
	if v, _ := bg.Lookup(quiet); v.Notify {
		t.Error("notify:false was filed as notify")
	}
	if v, _ := bg.Lookup(bound); !v.Notify || !v.CancelOnParentEnd {
		t.Errorf("on_parent_end:cancel child = %+v", v.ChildSpec)
	}
	if res := execJSON(t, a, ctx, `{"op":"spawn","name":"w","prompt":"x","notify":false}`); !res.IsError || !strings.Contains(res.Text, "poll") {
		t.Errorf("notify in wait mode = %+v, want refused", res)
	}
	g.open("quiet")
	g.open("bound")
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
	}
}
