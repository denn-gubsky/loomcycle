package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// gatedWalk is a Spawn whose members wait for release (or their ctx), and
// record the ctx each ran under.
type gatedWalk struct {
	release chan struct{}
	mu      sync.Mutex
	started int
	ended   []error
}

func newGatedWalk() *gatedWalk { return &gatedWalk{release: make(chan struct{})} }

func (g *gatedWalk) spawn() teamrun.SpawnFunc {
	return textSpawn(func(ctx context.Context, agent string, _ teamrun.Prompt, _ string) (string, error) {
		g.mu.Lock()
		g.started++
		g.mu.Unlock()
		var err error
		select {
		case <-g.release:
		case <-ctx.Done():
			err = context.Cause(ctx)
		}
		g.mu.Lock()
		g.ended = append(g.ended, err)
		g.mu.Unlock()
		if err != nil {
			return "", err
		}
		return agent + " reviewed", nil
	})
}

func (g *gatedWalk) counts() (started, ended int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started, len(g.ended)
}

// withParentRun puts a parent run's tool ctx on top of ctx: its run id, its
// background table (alive for as long as lifetime) and an iteration budget
// with turns left.
func withParentRun(ctx, lifetime context.Context) (context.Context, *tools.Background) {
	bg := tools.NewBackground(lifetime)
	ctx = tools.WithRunID(ctx, "r_parent")
	ctx = tools.WithBackground(ctx, bg)
	ctx = tools.WithIterationBudget(ctx, 2, 10, false)
	return ctx, bg
}

// pollFixture is a TeamDef tool with the validTeamGraph team "rev", its runs
// recorded, its members gated, and a parent run's ctx.
func pollFixture(t *testing.T) (*TeamDef, context.Context, *tools.Background, *gatedWalk, *walkRunRecorder, func()) {
	t.Helper()
	tool, ctx, done := teamDefFixture(t)
	createTeam(t, tool, ctx, "rev", validTeamGraph)
	g := newGatedWalk()
	tool.Spawn = g.spawn()
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open
	tool.LiveChildren = tools.NewLiveChildren(func() int { return 32 })
	ctx, bg := withParentRun(ctx, context.Background())
	return tool, ctx, bg, g, rec, done
}

func runTD(t *testing.T, tool *TeamDef, ctx context.Context, in string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

type walkPoll struct {
	Walks   []map[string]any `json:"walks"`
	Pending int              `json:"pending"`
}

func decodeWalkPoll(t *testing.T, res tools.Result) walkPoll {
	t.Helper()
	if res.IsError {
		t.Fatalf("poll failed: %s", res.Text)
	}
	var p walkPoll
	if err := json.Unmarshal([]byte(res.Text), &p); err != nil {
		t.Fatalf("poll answer %q: %v", res.Text, err)
	}
	return p
}

// startPollWalk runs "rev" in poll mode and returns its run id.
func startPollWalk(t *testing.T, tool *TeamDef, ctx context.Context, extra string) string {
	t.Helper()
	res := runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"the diff","mode":"poll"`+extra+`}`)
	if res.IsError {
		t.Fatalf("poll-mode run: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	id, _ := out["run_id"].(string)
	if id == "" || out["state"] != "running" {
		t.Fatalf("poll-mode run answered %v, want {run_id, state: running}", out)
	}
	return id
}

// A poll-mode walk returns at once, while its member is still working; the
// parent makes other calls; a poll that waits for all of it returns exactly
// what a waited-for run of the same team answers.
func TestTeamDefPoll_WalkReturnsAtOnceAndPollReadsTheWaitedForAnswer(t *testing.T) {
	tool, ctx, bg, g, rec, done := pollFixture(t)
	defer done()

	id := startPollWalk(t, tool, ctx, "")
	waitUntil(t, func() bool { s, _ := g.counts(); return s == 1 })
	if _, finished := rec.counts(); finished != 0 {
		t.Fatal("the walk finished before the call returned")
	}
	if v, ok := bg.Lookup(id); !ok || v.State != tools.ChildRunning || v.Kind != tools.ChildKindTeam || v.Agent != "team:rev" {
		t.Fatalf("the walk's row in the table = %+v (%v)", v, ok)
	}
	if !rec.spec.Poll || rec.spec.RunID != id || rec.spec.Detach {
		t.Errorf("the walk's run was opened with %+v, want poll mode under %s", rec.spec, id)
	}
	// The parent keeps working.
	if res := runTD(t, tool, ctx, `{"op":"list","name":"rev"}`); res.IsError {
		t.Fatalf("a call while the walk runs: %s", res.Text)
	}
	if p := decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"poll","run_ids":["`+id+`"]}`)); p.Pending != 1 || p.Walks[0]["state"] != "running" {
		t.Fatalf("poll of a running walk = %+v", p)
	}

	close(g.release)
	p := decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"poll","run_ids":["`+id+`"],"wait":"all","wait_ms":5000}`))
	if p.Pending != 0 || len(p.Walks) != 1 {
		t.Fatalf("poll with wait all = %+v", p)
	}
	got := p.Walks[0]
	if got["state"] != "completed" || got["run_id"] != id || got["name"] != "rev" {
		t.Errorf("walk row = %v", got)
	}

	// The same team, waited for.
	waited := runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"the diff"}`)
	if waited.IsError {
		t.Fatalf("waited-for run: %s", waited.Text)
	}
	want := decodeResult(t, waited.Text)
	for _, k := range []string{"name", "def_id", "status", "final_state", "final_output", "steps"} {
		if !reflect.DeepEqual(got[k], want[k]) {
			t.Errorf("%s: poll %v, waited-for run %v", k, got[k], want[k])
		}
	}
	if got["final_output"] != "reviewer reviewed" {
		t.Errorf("final_output = %v", got["final_output"])
	}
}

// Agent poll reads a walk like any background child — kind "team", its
// final output as output — and Agent cancel ends it: the walk ends
// cancelled and its member's ctx is cancelled with it.
func TestTeamDefPoll_AgentPollReadsAWalkAndAgentCancelEndsIt(t *testing.T) {
	tool, ctx, bg, g, rec, done := pollFixture(t)
	defer done()
	a := pollTool(newGated())

	done1 := startPollWalk(t, tool, ctx, "")
	close(g.release)
	waitUntil(t, func() bool { v, _ := bg.Lookup(done1); return v.Ended() })
	p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll","child_run_ids":["`+done1+`"]}`))
	if r := p.Children[0]; r.Kind != "team" || r.Agent != "team:rev" || r.State != "completed" || r.Output != "reviewer reviewed" {
		t.Errorf("Agent poll row of a completed walk = %+v", r)
	}

	g2 := newGatedWalk()
	tool.Spawn = g2.spawn()
	id := startPollWalk(t, tool, ctx, "")
	waitUntil(t, func() bool { s, _ := g2.counts(); return s == 1 })
	c := decodePoll(t, execJSON(t, a, ctx, `{"op":"cancel","child_run_ids":["`+id+`"]}`))
	if r := c.Children[0]; r.State != "cancelled" || r.Kind != "team" {
		t.Errorf("Agent cancel of a walk = %+v", r)
	}
	if _, ended := g2.counts(); ended != 1 {
		t.Errorf("the walk's member did not end with it")
	}
	if _, err := rec.ended(); err == nil {
		t.Error("the cancelled walk's run was finished without an error")
	}
	v, _ := bg.Lookup(id)
	if !strings.Contains(v.Result.Error, "cancelled") {
		t.Errorf("cancelled walk's error = %q", v.Result.Error)
	}
	tp := decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"poll","run_ids":["`+id+`"]}`))
	if tp.Walks[0]["state"] != "cancelled" {
		t.Errorf("TeamDef poll of the cancelled walk = %v", tp.Walks[0])
	}
}

// A poll-mode walk lives with the calling RUN, not the call: when the run
// ends it is cancelled with the run's cause. A detached walk's ctx is not the
// run's, and runs on. (Only the ctx is pinned here: on a server a detached
// walk's members are also children of the starting agent in the cancel
// registry, whose cascade reaches them — behaviour poll mode leaves as it is.)
func TestTeamDefPoll_TheParentRunEndingCancelsThePollWalkNotADetachedCtx(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	createTeam(t, tool, base, "rev", validTeamGraph)
	g := newGatedWalk()
	tool.Spawn = g.spawn()
	tool.WalkRun = (&walkRunRecorder{}).open
	run, endRun := context.WithCancelCause(context.Background())
	ctx, bg := withParentRun(base, run)
	call, endCall := context.WithCancel(ctx)

	id := startPollWalk(t, tool, call, "")
	det := runTD(t, tool, call, `{"op":"run","name":"rev","input":"the diff","mode":"detach"}`)
	if det.IsError {
		t.Fatalf("detach: %s", det.Text)
	}
	waitUntil(t, func() bool { s, _ := g.counts(); return s == 2 })
	endCall() // the call that started them returned long ago
	if v, _ := bg.Lookup(id); v.Ended() {
		t.Fatal("the walk ended with the call that started it")
	}
	why := errors.New("the parent run was cancelled")
	endRun(why)
	waitUntil(t, func() bool { v, _ := bg.Lookup(id); return v.Ended() })
	if v, _ := bg.Lookup(id); v.State != tools.ChildCancelled || !strings.Contains(v.Result.Error, why.Error()) {
		t.Errorf("walk after its parent run ended = %s %q, want cancelled with the run's cause", v.State, v.Result.Error)
	}
	if _, ended := g.counts(); ended != 1 {
		t.Fatalf("%d members ended, want only the poll-mode walk's", ended)
	}
	close(g.release) // the detached walk's member finishes now
	waitUntil(t, func() bool { _, e := g.counts(); return e == 2 })
}

// A poll-mode walk holds one of its caller's live-children slots until it
// ends, and the limit refuses one past it.
func TestTeamDefPoll_TheWalkCountsAsALiveChildUntilItEnds(t *testing.T) {
	tool, ctx, bg, g, _, done := pollFixture(t)
	defer done()
	tool.LiveChildren = tools.NewLiveChildren(func() int { return 1 })
	id := startPollWalk(t, tool, ctx, "")
	if n := tool.LiveChildren.Alive("r_parent"); n != 1 {
		t.Fatalf("%d alive after the call returned, want 1", n)
	}
	res := runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"again","mode":"poll"}`)
	if !res.IsError || !strings.Contains(res.Text, "at most 1") {
		t.Errorf("a walk past the limit = %+v, want refused", res)
	}
	close(g.release)
	waitUntil(t, func() bool { v, _ := bg.Lookup(id); return v.Ended() })
	if n := tool.LiveChildren.Alive("r_parent"); n != 0 {
		t.Errorf("%d alive once the walk ended, want 0", n)
	}
}

// Poll mode is refused on the run's last iteration, and outside a run with a
// background table; nothing is admitted or opened either way.
func TestTeamDefPoll_RefusedOnTheLastIterationAndOutsideARun(t *testing.T) {
	tool, ctx, bg, g, rec, done := pollFixture(t)
	defer done()
	defer close(g.release)
	last := tools.WithIterationBudget(ctx, 10, 10, false)
	res := runTD(t, tool, last, `{"op":"run","name":"rev","input":"x","mode":"poll"}`)
	if !res.IsError || !strings.Contains(res.Text, "last iteration") {
		t.Errorf("last iteration: %+v", res)
	}
	noTable := tools.WithBackground(ctx, nil)
	res = runTD(t, tool, noTable, `{"op":"run","name":"rev","input":"x","mode":"poll"}`)
	if !res.IsError || !strings.Contains(res.Text, "not available") {
		t.Errorf("no table: %+v", res)
	}
	if opened, _ := rec.counts(); opened != 0 {
		t.Errorf("a refused walk opened %d runs", opened)
	}
	if got, _ := bg.Select(nil, ""); len(got) != 0 || tool.LiveChildren.Alive("r_parent") != 0 {
		t.Errorf("a refused walk was filed: %+v", got)
	}
	if res := runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"x","notify":false}`); !res.IsError || !strings.Contains(res.Text, "poll") {
		t.Errorf("notify without mode poll = %+v, want refused", res)
	}
}

// A walk refused after it was filed — here by a breakpoint naming no state —
// is withdrawn: its run is finished, its slot freed, and nothing reports it.
func TestTeamDefPoll_AWalkRefusedAfterFilingIsWithdrawn(t *testing.T) {
	tool, ctx, bg, g, rec, done := pollFixture(t)
	defer done()
	defer close(g.release)
	res := runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"x","mode":"poll","breakpoints":["nowhere"]}`)
	if !res.IsError {
		t.Fatalf("want a refusal, got %s", res.Text)
	}
	if opened, finished := rec.counts(); opened != 1 || finished != 1 {
		t.Errorf("opened=%d finished=%d, want the refused walk's run opened and closed", opened, finished)
	}
	if got, _ := bg.Select(nil, ""); len(got) != 0 {
		t.Errorf("the refused walk is still in the table: %+v", got)
	}
	if n := tool.LiveChildren.Alive("r_parent"); n != 0 {
		t.Errorf("the refused walk holds %d slots", n)
	}
	if n := bg.TakeNotes(); n != "" {
		t.Errorf("the refused walk was noted: %q", n)
	}
}

// vars and input reach a poll-mode walk as they reach a waited-for one, and
// notify / on_parent_end reach its row in the table.
func TestTeamDefPoll_VarsInputAndOptionsReachThePollModeWalk(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	vals := &valueRecorder{}
	tool.Spawn = vals.spawn()
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open
	createTeam(t, tool, ctx, "vars-poll", varsTeam)
	ctx, bg := withParentRun(ctx, context.Background())

	res := runTD(t, tool, ctx, `{"op":"run","name":"vars-poll","input":"go","mode":"poll","vars":{"lang":"de"},"notify":false,"on_parent_end":"cancel"}`)
	if res.IsError {
		t.Fatalf("poll-mode run: %s", res.Text)
	}
	id := decodeResult(t, res.Text)["run_id"].(string)
	waitUntil(t, func() bool { v, _ := bg.Lookup(id); return v.Ended() })
	if got, _ := vals.at("first"); got != "formal/de" {
		t.Errorf("entry prompt handed %q, want formal/de", got)
	}
	if got, _ := vals.at("second"); got != "set-by-state/de" {
		t.Errorf("later prompt handed %q, want set-by-state/de", got)
	}
	if rec.spec.Input != "go" || !reflect.DeepEqual(rec.spec.Vars, map[string]string{"lang": "de"}) {
		t.Errorf("the walk's run was opened with input %q vars %v", rec.spec.Input, rec.spec.Vars)
	}
	if v, _ := bg.Lookup(id); v.Notify || !v.CancelOnParentEnd {
		t.Errorf("table row = %+v, want notify off and cancel on parent end", v.ChildSpec)
	}
}

// A member held for a review verdict shows the walk "held" in the caller's
// table until the hold ends; so does a breakpoint question the walk asks.
func TestTeamDefPoll_AHoldInsideTheWalkShowsItHeld(t *testing.T) {
	t.Run("member review", func(t *testing.T) {
		tool, ctx, bg, _, _, done := pollFixture(t)
		defer done()
		step := make(chan struct{})
		tool.Spawn = textSpawn(func(ctx context.Context, _ string, _ teamrun.Prompt, _ string) (string, error) {
			hold := teamrun.HoldObserver(ctx)
			hold(true)
			<-step
			hold(false)
			<-step
			return "approved", nil
		})
		id := startPollWalk(t, tool, ctx, "")
		stateOf := func() string { v, _ := bg.Lookup(id); return v.State }
		waitUntil(t, func() bool { return stateOf() == tools.ChildHeld })
		step <- struct{}{}
		waitUntil(t, func() bool { return stateOf() == tools.ChildRunning })
		close(step)
		waitUntil(t, func() bool { return stateOf() == tools.ChildCompleted })
	})
	t.Run("breakpoint question", func(t *testing.T) {
		tool, ctx, _, _, done := breakFixture(t)
		defer done()
		tool.WalkRun = (&walkRunRecorder{}).open
		answer := make(chan string)
		tool.AskHuman = func(context.Context, string) (string, error) { return <-answer, nil }
		ctx, bg := withParentRun(ctx, context.Background())
		res := runTD(t, tool, ctx, `{"op":"run","name":"triage","input":"x","mode":"poll","breakpoints":["wave"]}`)
		if res.IsError {
			t.Fatalf("poll-mode run: %s", res.Text)
		}
		id := decodeResult(t, res.Text)["run_id"].(string)
		stateOf := func() string { v, _ := bg.Lookup(id); return v.State }
		waitUntil(t, func() bool { return stateOf() == tools.ChildHeld })
		answer <- "continue"
		waitUntil(t, func() bool { return stateOf() == tools.ChildCompleted })
	})
}

// TeamDef poll reads walks only: a sub-agent of this run is answered as an id
// the run never started, and a bare poll hands over each unread walk once.
func TestTeamDefPoll_ReadsOnlyWalksEachUnreadOnce(t *testing.T) {
	tool, ctx, bg, g, _, done := pollFixture(t)
	defer done()
	gated := newGated("sub")
	a := pollTool(gated)
	sub := execJSON(t, a, ctx, `{"op":"spawn","name":"w","prompt":"sub","mode":"poll"}`)
	var one pollStartRow
	if err := json.Unmarshal([]byte(sub.Text), &one); err != nil {
		t.Fatal(err)
	}
	if res := runTD(t, tool, ctx, `{"op":"poll","run_ids":["`+one.ChildRunID+`"]}`); !res.IsError || !strings.Contains(res.Text, "not a team walk of this run") {
		t.Errorf("TeamDef poll of a sub-agent = %+v", res)
	}
	id := startPollWalk(t, tool, ctx, "")
	close(g.release)
	gated.open("sub")
	waitUntil(t, func() bool { v, _ := bg.Lookup(id); return v.Ended() })
	p := decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"poll"}`))
	if len(p.Walks) != 1 || p.Walks[0]["run_id"] != id {
		t.Fatalf("bare poll = %+v, want the one walk", p)
	}
	if p := decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"poll"}`)); len(p.Walks) != 0 {
		t.Errorf("a second bare poll handed the walk over again: %+v", p)
	}
	if p := decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"poll","run_ids":["`+id+`"]}`)); p.Walks[0]["state"] != "completed" {
		t.Errorf("poll by id after the walk was read = %+v", p)
	}
}
