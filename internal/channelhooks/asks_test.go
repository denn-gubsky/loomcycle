package channelhooks

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/hooks/codehook"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// fakeRuns opens real run rows (an ask is filed under one) and records what
// happens to them.
type fakeRuns struct {
	st       store.Store
	mu       sync.Mutex
	opened   []string
	agents   []string
	finished map[string]string
	events   map[string][]providers.Event
}

func (r *fakeRuns) Open(ctx context.Context, tenant, agent, runID string) (context.Context, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if runID == "" {
		sess, err := r.st.CreateSession(ctx, tenant, agent, "_system")
		if err != nil {
			return nil, "", err
		}
		run, err := r.st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agent, UserID: "_system", TenantID: tenant})
		if err != nil {
			return nil, "", err
		}
		runID = run.ID
		r.opened = append(r.opened, runID)
		r.agents = append(r.agents, agent)
	}
	id := runID
	rctx := tools.WithRunIdentity(tools.WithRunID(ctx, id), tools.RunIdentityValue{TenantID: tenant, UserID: "_system", AgentID: agent})
	rctx = tools.WithEventEmitter(rctx, func(ev providers.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events[id] = append(r.events[id], ev)
	})
	return rctx, runID, nil
}

func (r *fakeRuns) Finish(runID string, _ store.RunStatus, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished[runID] = reason
}

func (r *fakeRuns) snapshot() (opened []string, finished map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := map[string]string{}
	for k, v := range r.finished {
		f[k] = v
	}
	return append([]string(nil), r.opened...), f
}

// askFixture is a fixture whose worker can ask a person: the real
// Interruption tool, over the same store and bus.
func askFixture(t *testing.T, timeout time.Duration) (*fixture, *fakeRuns, *channels.Bus) {
	f := newFixture(t)
	bus := f.writer.Bus
	runs := &fakeRuns{st: f.st, finished: map[string]string{}, events: map[string][]providers.Event{}}
	it := &builtin.Interruption{Store: f.st, Bus: bus, DefaultTimeout: timeout, ResolvePollInterval: 10 * time.Millisecond}
	f.w.cfg.Interruption = it
	f.w.cfg.Runs = runs
	// A code body asks through the same tool, as the server wires it.
	disp := hooks.NewDispatcher(nil, nil)
	disp.SetCodeRunner(codehook.New(it))
	f.w.cfg.Dispatcher = disp
	return f, runs, bus
}

// pendingAsk waits for the one pending ask of a run to appear.
func pendingAsk(t *testing.T, st store.Store, runs *fakeRuns) store.InterruptRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		opened, _ := runs.snapshot()
		for _, id := range opened {
			if rows, _ := st.InterruptListByRun(context.Background(), id, store.InterruptStatusPending); len(rows) > 0 {
				return rows[0]
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no ask became pending")
	return store.InterruptRow{}
}

func resolveAsk(t *testing.T, st store.Store, bus *channels.Bus, row store.InterruptRow, a string) {
	t.Helper()
	if err := st.InterruptResolve(context.Background(), row.InterruptID, a, store.InterruptResolvedByAPI, nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	bus.Notify("intr:" + row.InterruptID)
}

// runAsync runs one claim round in the background and returns a wait func.
func runAsync(f *fixture) func() {
	done := make(chan struct{})
	go func() { f.w.claim(context.Background()); f.w.wg.Wait(); close(done) }()
	return func() {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			f.t.Fatal("the worker did not finish")
		}
	}
}

// A hold is put to a person, as a pending ask under a `hook:<name>` run the
// hook's first ask opens — no run exists for a message nobody had to be asked
// about. The answer decides, and the run ends when the message is decided.
func TestWorker_AHoldAsksAPersonUnderAHookRun(t *testing.T) {
	for _, tc := range []struct {
		answer    string
		delivered int
	}{{"release", 1}, {"drop", 0}} {
		t.Run(tc.answer, func(t *testing.T) {
			f, runs, bus := askFixture(t, 5*time.Second)
			hold, _ := answer(t, `{"decision":"hold","reason":"needs a look"}`)
			f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", hold.URL))})
			f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, nil)
			wait := runAsync(f)
			row := pendingAsk(t, f.st, runs)
			if row.UserID != "_system" || !strings.Contains(row.Question, "gate") || !strings.Contains(row.ContextData, `"n":1`) {
				t.Fatalf("the ask = %+v", row)
			}
			resolveAsk(t, f.st, bus, row, tc.answer)
			wait()
			if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != tc.delivered {
				t.Fatalf("delivered %d, want %d", got, tc.delivered)
			}
			opened, finished := runs.snapshot()
			if len(opened) != 1 || runs.agents[0] != "hook:gate" || finished[opened[0]] == "" {
				t.Fatalf("runs opened %v (%v), finished %v", opened, runs.agents, finished)
			}
		})
	}
}

// A message nobody had to be asked about opens no run.
func TestWorker_NoRunUnlessAHookAsks(t *testing.T) {
	f, runs, _ := askFixture(t, time.Second)
	rel, _ := answer(t, ``)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("screen", rel.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	f.drain()
	if opened, _ := runs.snapshot(); len(opened) != 0 {
		t.Fatalf("opened %v for a message no hook asked about", opened)
	}
}

// A hold nobody answers in time drops the message: a hold means a person must
// approve.
func TestWorker_AnUnansweredHoldDrops(t *testing.T) {
	f, _, _ := askFixture(t, 50*time.Millisecond)
	hold, _ := answer(t, `{"decision":"hold"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", hold.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	runAsync(f)()
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 0 {
		t.Fatalf("delivered %d past an unanswered hold", got)
	}
	if f.w.Stats().Decisions["drop"] != 1 {
		t.Fatalf("stats = %+v", f.w.Stats())
	}
}

// An ask left pending by a worker that died is cancelled when the message is
// claimed again, and the person is asked afresh; the new answer decides.
func TestWorker_RestartMidAskCancelsAndReasks(t *testing.T) {
	f, runs, bus := askFixture(t, 5*time.Second)
	hold, _ := answer(t, `{"decision":"hold"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", hold.URL))})
	res := f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)

	// What a dead worker leaves: its lease (now expired), the message's run,
	// and a pending ask nobody will wait for.
	ctx := context.Background()
	_, deadRun, err := runs.Open(ctx, "", "hook:gate", "")
	if err != nil {
		t.Fatal(err)
	}
	stale := store.InterruptRow{InterruptID: store.MintInterruptID(time.Now()), RunID: deadRun, Kind: store.InterruptKindQuestion,
		Status: store.InterruptStatusPending, Question: "old", CreatedAt: time.Now(), UserID: "_system"}
	if _, err := f.st.InterruptCreate(ctx, stale); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	items, _ := f.st.ChannelHookClaim(ctx, "dead", now, now.Add(time.Millisecond), 1)
	if len(items) != 1 {
		t.Fatal("setup claim")
	}
	key := keyOf(res.Message)
	key.TenantID = items[0].Message.TenantID
	if ok, err := f.st.ChannelHookSaveProgress(ctx, key, items[0].Lease, store.ChannelHookProgress{RunID: deadRun}, now.Add(time.Millisecond)); err != nil || !ok {
		t.Fatalf("setup progress: %v %v", ok, err)
	}
	time.Sleep(5 * time.Millisecond)

	wait := runAsync(f)
	var fresh store.InterruptRow
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := f.st.InterruptListByRun(ctx, deadRun, store.InterruptStatusPending)
		if len(rows) == 1 && rows[0].InterruptID != stale.InterruptID {
			fresh = rows[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fresh.InterruptID == "" {
		t.Fatal("no fresh ask under the message's run")
	}
	if old, _ := f.st.InterruptGet(ctx, stale.InterruptID); old.Status != store.InterruptStatusCancelled || old.ResolvedBy != store.InterruptResolvedByHookRestart {
		t.Fatalf("the stale ask is %s by %s", old.Status, old.ResolvedBy)
	}
	resolveAsk(t, f.st, bus, fresh, "release")
	wait()
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 1 {
		t.Fatalf("delivered %d", got)
	}
	if opened, _ := runs.snapshot(); len(opened) != 1 {
		t.Fatalf("a new run was opened (%v) instead of the message's own", opened)
	}
}

// The hook run's transcript records the decisions made on the message.
func TestWorker_TheHookRunRecordsTheDecisions(t *testing.T) {
	f, runs, bus := askFixture(t, 5*time.Second)
	hold, _ := answer(t, `{"decision":"hold"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", hold.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	wait := runAsync(f)
	resolveAsk(t, f.st, bus, pendingAsk(t, f.st, runs), "release")
	wait()
	opened, _ := runs.snapshot()
	runs.mu.Lock()
	evs := runs.events[opened[0]]
	runs.mu.Unlock()
	var released bool
	for _, ev := range evs {
		if ev.Type == providers.EventHookDecision && ev.HookDecision.Decision == "release" && ev.HookDecision.MessageID != "" {
			released = true
		}
	}
	if !released {
		t.Fatalf("the hook run's transcript has no release decision: %+v", evs)
	}
}

// A person's time does not starve the worker: a message waiting on an answer
// gives up its slot, and the next message is decided meanwhile.
func TestWorker_BlockedAskDoesNotStarveTheSemaphore(t *testing.T) {
	f, runs, bus := askFixture(t, 5*time.Second)
	f.w = New(Config{Store: f.st, Dispatcher: f.w.cfg.Dispatcher, Writer: f.writer, Bus: f.writer.Bus, Owner: "w1",
		Lookup: f.w.cfg.Lookup, Defs: f.w.cfg.Defs, Interruption: f.w.cfg.Interruption, Runs: runs, Concurrency: 1, PerChannel: 1})
	hold, _ := answer(t, `{"decision":"hold"}`)
	rel, _ := answer(t, ``)
	f.setDef("", "held", Def{Hooks: chain(webhookEntry("gate", hold.URL))})
	f.setDef("", "open", Def{Hooks: chain(webhookEntry("screen", rel.URL))})
	f.publish("held", "", store.MemoryScopeGlobal, `{}`, nil)
	wait := runAsync(f)
	row := pendingAsk(t, f.st, runs)
	f.publish("open", "", store.MemoryScopeGlobal, `{}`, nil)
	f.w.claim(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(f.peek("open", "", store.MemoryScopeGlobal)) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(f.peek("open", "", store.MemoryScopeGlobal)) != 1 {
		t.Fatal("the second message waited behind the first one's person")
	}
	resolveAsk(t, f.st, bus, row, "release")
	wait()
}

// A code body's own ask goes through the message's session: filed under the
// hook run, answered, and the body decides on the answer.
func TestWorker_ACodeHooksAskIsFiledUnderTheHookRun(t *testing.T) {
	f, runs, bus := askFixture(t, 5*time.Second)
	f.hdefs["|review"] = hooks.Def{Event: hooks.PhaseChannelPublish, Body: hooks.DefBody{Kind: hooks.BodyKindCode,
		Code: `function hook(ev){ var a = Interruption.ask({question: "ship " + ev.message_id + "?", options: ["yes","no"]}); return a === "yes" ? {} : {decision: "drop"}; }`}}
	f.setDef("", "inbox", Def{Hooks: chain(hooks.Entry{Ref: "review"})})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	wait := runAsync(f)
	row := pendingAsk(t, f.st, runs)
	resolveAsk(t, f.st, bus, row, "yes")
	wait()
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 1 {
		t.Fatalf("delivered %d", got)
	}
	if opened, _ := runs.snapshot(); len(opened) != 1 || runs.agents[0] != "hook:review" {
		t.Fatalf("runs %v (%v)", opened, runs.agents)
	}
}

// countingTool answers every ask with the same answer and counts the asks.
type countingTool struct {
	answer string
	calls  int
}

func (c *countingTool) Name() string                 { return "Interruption" }
func (c *countingTool) Description() string          { return "" }
func (c *countingTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (c *countingTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	c.calls++
	return tools.Result{Text: `{"interrupt_id":"int_x","answer":"` + c.answer + `"}`}, nil
}

// A hold's answer is kept before it is acted on, and a worker that picks the
// message up again replays it rather than asking the person twice — even for
// a message with a TTL, whose ask carries a timeout that is different by the
// time the second worker gets there.
func TestWorker_AnAnsweredHoldIsNotAskedAgain(t *testing.T) {
	f, runs, _ := askFixture(t, time.Second)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", "https://unused.example"))})
	res := f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, func(r *channels.WriteRequest) { r.ExpiresAt = time.Now().Add(time.Hour) })
	now := time.Now()
	items, _ := f.st.ChannelHookClaim(context.Background(), "w1", now, now.Add(time.Minute), 1)
	if len(items) != 1 {
		t.Fatal("claim")
	}
	first := &countingTool{answer: "release"}
	f.w.cfg.Interruption, f.w.cfg.Runs = first, runs
	j := &job{w: f.w, msg: items[0].Message, key: keyOf(items[0].Message), lease: items[0].Lease, progress: items[0].Progress, chSem: make(chan struct{}, 1)}
	j.chSem <- struct{}{}
	f.w.sem <- struct{}{}
	j.loadJournal()
	if got, err := j.askHold(context.Background(), 0, "gate", "channel:inbox/gate", "why", res.Message.Payload, nil); err != nil || got != "release" || first.calls != 1 {
		t.Fatalf("first ask: %q %v (%d calls)", got, err, first.calls)
	}

	// Another worker, the journal as the store kept it.
	time.Sleep(10 * time.Millisecond)
	later := time.Now().Add(2 * time.Minute)
	again, _ := f.st.ChannelHookClaim(context.Background(), "w2", later, later.Add(time.Minute), 1)
	if len(again) != 1 || len(again[0].Progress.Journal) == 0 {
		t.Fatalf("reclaim: %+v", again)
	}
	second := &countingTool{answer: "drop"}
	f.w.cfg.Interruption = second
	j2 := &job{w: f.w, msg: again[0].Message, key: keyOf(again[0].Message), lease: again[0].Lease, progress: again[0].Progress, chSem: make(chan struct{}, 1)}
	j2.chSem <- struct{}{} // the slots a job holds, which an ask it did make would give up
	j2.loadJournal()
	if got, err := j2.askHold(context.Background(), 0, "gate", "channel:inbox/gate", "why", res.Message.Payload, nil); err != nil || got != "release" || second.calls != 0 {
		t.Fatalf("replay: %q %v (%d calls)", got, err, second.calls)
	}
}

// A worker whose lease passed to another while a person was answering must
// not act on the answer: the new owner decides the message. Before, the lost
// lease surfaced as a failed hook, fail-open let the chain go on, and the
// message was delivered past the person's "no".
func TestWorker_ALostLeaseSettlesNothing(t *testing.T) {
	f, runs, bus := askFixture(t, 5*time.Second)
	f.hdefs["|review"] = hooks.Def{Event: hooks.PhaseChannelPublish, Body: hooks.DefBody{Kind: hooks.BodyKindCode,
		Code: `function hook(ev){ var a = Interruption.ask({question: "ship?", options: ["yes","no"]}); return a === "yes" ? {} : {decision: "drop"}; }`}}
	f.setDef("", "inbox", Def{Hooks: chain(hooks.Entry{Ref: "review"})})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	wait := runAsync(f)
	row := pendingAsk(t, f.st, runs)
	// Another worker takes the message over (as after a lease that lapsed).
	later := time.Now().Add(time.Hour)
	if w, _ := f.st.ChannelHookClaim(context.Background(), "w2", later, later.Add(time.Hour), 1); len(w) != 1 {
		t.Fatal("w2 did not take the lease")
	}
	resolveAsk(t, f.st, bus, row, "no")
	wait()
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 0 {
		t.Fatalf("delivered %d past the person's no", got)
	}
	items, _ := f.st.ChannelHookClaim(context.Background(), "w3", later.Add(2*time.Hour), later.Add(3*time.Hour), 1)
	if len(items) != 1 {
		t.Fatal("the message is no longer waiting for its new owner")
	}
	if _, finished := runs.snapshot(); len(finished) != 0 {
		t.Fatalf("the old worker ended the hook run the new owner uses: %v", finished)
	}
}

// A worker that lost its lease stops deciding: a hook whose answer could not
// be kept fails, and even a fail-open one must not let the chain run on — the
// hooks after it are the new owner's to call, not the old worker's too.
func TestWorker_ALostLeaseRunsNoFurtherHook(t *testing.T) {
	f, runs, bus := askFixture(t, 5*time.Second)
	f.hdefs["|review"] = hooks.Def{Event: hooks.PhaseChannelPublish, FailMode: hooks.FailOpen, Body: hooks.DefBody{Kind: hooks.BodyKindCode,
		Code: `function hook(ev){ Interruption.ask({question: "ship?", options: ["yes","no"]}); return {}; }`}}
	after, calls := answer(t, ``)
	f.setDef("", "inbox", Def{Hooks: chain(hooks.Entry{Ref: "review"}, webhookEntry("after", after.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	wait := runAsync(f)
	row := pendingAsk(t, f.st, runs)
	later := time.Now().Add(time.Hour)
	if w, _ := f.st.ChannelHookClaim(context.Background(), "w2", later, later.Add(time.Hour), 1); len(w) != 1 {
		t.Fatal("w2 did not take the lease")
	}
	resolveAsk(t, f.st, bus, row, "yes")
	wait()
	if n := calls.Load(); n != 0 {
		t.Fatalf("the old worker called the next hook %d time(s) after losing the lease", n)
	}
}

// A body that puts the time or a random number in its question replays that
// question after a restart: the seed comes from the hook, not from the ids a
// fresh resolution mints, and the clock from the message's journal. Before,
// the second worker's body asked a different question, the replay reported a
// divergence, and the person's earlier answer was lost with the hook failing.
func TestWorker_AReplayAfterARestartIsDeterministic(t *testing.T) {
	f, runs, bus := askFixture(t, 5*time.Second)
	f.hdefs["|review"] = hooks.Def{Event: hooks.PhaseChannelPublish, FailMode: hooks.FailClosed, Body: hooks.DefBody{Kind: hooks.BodyKindCode,
		Code: `function hook(ev){
		  var a = Interruption.ask({question: "first " + Date.now() + " " + Math.random(), options: ["yes","no"]});
		  var b = Interruption.ask({question: "second", options: ["yes","no"]});
		  return a === "yes" && b === "yes" ? {} : {decision: "drop"};
		}`}}
	f.setDef("", "inbox", Def{Hooks: chain(hooks.Entry{Ref: "review"})})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)

	// The first worker gets the first answer, then stops while the second
	// question is pending.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.w.claim(ctx); f.w.wg.Wait(); close(done) }()
	first := pendingAsk(t, f.st, runs)
	resolveAsk(t, f.st, bus, first, "yes")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows, _ := f.st.InterruptListByRun(context.Background(), first.RunID, store.InterruptStatusPending); len(rows) == 1 && rows[0].InterruptID != first.InterruptID {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	// A fresh worker takes the message over once the lease has run out.
	w2 := New(f.w.cfg)
	w2.cfg.Owner = "w2"
	w2.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	wait := make(chan struct{})
	go func() { w2.claim(context.Background()); w2.wg.Wait(); close(wait) }()
	var second store.InterruptRow
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && second.InterruptID == "" {
		rows, _ := f.st.InterruptListByRun(context.Background(), first.RunID, store.InterruptStatusPending)
		for _, r := range rows {
			if r.Question == "second" {
				second = r
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if second.InterruptID == "" {
		t.Fatal("the fresh worker did not get to the second question — the first did not replay")
	}
	resolveAsk(t, f.st, bus, second, "yes")
	<-wait
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 1 {
		t.Fatalf("delivered %d", got)
	}
}
