package http

import (
	"context"
	"encoding/json"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A bounded child that is inside a tool call for the whole of a runtime pause
// never parks for it, yet its live clock stopped for it. Its pause is on its
// record all the same — open once the pause is declared, so a snapshot taken
// then carries it, and ended at the resume — and a parent that re-arms its
// timeout_ms from the record afterwards (after a restart or a restore) gives
// it the time it had left when the runtime paused.
func TestResumeFanout_AChildBusyThroughAPauseIsNotChargedIt(t *testing.T) {
	const bound, pauseFor = 600 * time.Millisecond, 900 * time.Millisecond
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"breeder": {Provider: "scripted", Model: "stub-model", Tools: []string{"Agent"}}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.ResumeFanout = true
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	mgr := pause.NewManager(srv.store, time.Second)
	srv.SetPauseManager(mgr)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()

	childSess, _ := srv.store.CreateSession(ctx, "", "solver", "alice")
	child, err := srv.store.CreateRun(ctx, childSess.ID, store.RunIdentity{AgentID: "a_child_busy", UserID: "alice", Model: "stub-model", RunConfig: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.cancelReg.Register(cancel.Entry{AgentID: child.AgentID, RunID: child.ID, SessionID: childSess.ID, UserID: "alice", StartedAt: time.Now()},
		func(error) {}); err != nil {
		t.Fatal(err)
	}
	// Live here, as a running loop is, and never at a boundary: it is in a
	// tool call from before the pause to after the resume.
	_, deregister := srv.newPauseGate(child.ID)
	defer deregister()

	pausedAt := time.Now()
	if _, err := mgr.Pause(ctx, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if rec := runRecord(t, srv, child.ID); len(rec.Pauses) != 1 || !rec.pauseOpen() {
		t.Fatalf("pauses once the pause returned = %+v, want one open: a snapshot now carries none", rec.Pauses)
	}
	time.Sleep(pauseFor)
	resumedAt := time.Now()
	if _, err := mgr.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := runRecord(t, srv, child.ID); len(rec.Pauses) != 1 || rec.pauseOpen() {
		t.Fatalf("pauses after the resume = %+v, want the one ended", rec.Pauses)
	}

	// Past start + bound already; not past it once the pause is left out.
	payload, _ := json.Marshal(providers.Event{
		Type:       providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: child.ID, Agent: "solver", TimeoutMs: int(bound / time.Millisecond)},
	})
	events := []store.Event{{Type: string(providers.EventSpawnChildStarted), Payload: payload}}
	msg, err := srv.reconcileFanoutParent(ctx, store.Run{ID: "r_parent"}, events,
		fanoutParkInfo{toolUseID: "tu_fan", input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"solver","prompt":"x","timeout_ms":600}]}`)},
		func(providers.Event) {})
	at := time.Now()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(msg.Content[0].Text), &env); err != nil || len(env.Results) != 1 {
		t.Fatalf("envelope = %s (%v)", msg.Content[0].Text, err)
	}
	if r := env.Results[0]; r.Ok || r.Status != "timeout" {
		t.Fatalf("row = %+v, want the child timed out once its time ran out", r)
	}
	want := child.StartedAt.Add(bound + resumedAt.Sub(pausedAt))
	if early, late := at.Sub(want), at.Sub(want.Add(time.Second)); early < -150*time.Millisecond || late > 0 {
		t.Errorf("the child was cut %v after the resume, want about %v (what it had left when the runtime paused)",
			at.Sub(resumedAt), want.Sub(resumedAt))
	}
}

// runRecord reads the run's configuration record.
func runRecord(t *testing.T, srv *Server, runID string) runConfigRecord {
	t.Helper()
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok {
		t.Fatalf("run %s has no readable record: %s", runID, run.RunConfig)
	}
	return rec
}

// pauseAction is one write to a run's pauses in a generated history: an open
// of the pause that began at since (until zero), or its end at until.
type pauseAction struct {
	at, since, until time.Time
}

// A run that lives through more pauses than its record keeps folds the oldest,
// and a re-armed timeout_ms reads the same deadline, stop and next check from
// the folded record as from the whole history — over histories where review
// holds overlap pauses in every way, a pause is opened twice, ended and opened
// again, or left open by a lost end, and the run started after the first
// pause began. Each fold sees only the holds that had begun by then.
func TestRunConfigRecord_FoldedPausesKeepTheReArmedClock(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	folds := 0
	for seed := int64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewSource(seed))
		start := ms(r.Intn(30))
		var actions []pauseAction
		now := 0
		for i, n := 0, maxRunPauses+5+r.Intn(3*maxRunPauses); i < n; i++ {
			now += 1 + r.Intn(40)
			since := ms(now)
			actions = append(actions, pauseAction{at: since, since: since})
			if r.Intn(4) == 0 { // opened again by the run's own park
				actions = append(actions, pauseAction{at: ms(now + 1), since: since})
			}
			if r.Intn(6) == 0 { // ended early (a cancelled park), opened again
				now += 1 + r.Intn(10)
				actions = append(actions, pauseAction{at: ms(now), since: since, until: ms(now)})
				now += r.Intn(3)
				actions = append(actions, pauseAction{at: ms(now), since: since})
			}
			now += 2 + r.Intn(60)
			if r.Intn(10) != 0 { // else the end is lost
				actions = append(actions, pauseAction{at: ms(now), since: since, until: ms(now)})
			}
		}
		var events []store.Event
		for h, seq := r.Intn(50), int64(1); h < now; seq += 2 {
			events = append(events, store.Event{Seq: seq, Type: string(providers.EventAwaitingReview), Timestamp: ms(h)})
			h += 1 + r.Intn(120)
			if h < now || r.Intn(2) == 0 {
				events = append(events, store.Event{Seq: seq + 1, Type: "user_input", Timestamp: ms(h)})
			}
			h += r.Intn(90)
		}
		sort.SliceStable(actions, func(i, j int) bool { return actions[i].at.Before(actions[j].at) })
		heldBy := func(t time.Time) resumedChildClock {
			var c resumedChildClock
			for _, ev := range events {
				if !ev.Timestamp.After(t) {
					c.observe([]store.Event{ev})
				}
			}
			return c
		}

		var whole, kept runConfigRecord
		for _, a := range actions {
			if a.until.IsZero() {
				whole.openPause(a.since)
				kept.openPause(a.since)
				kept.foldPauses(start, heldBy(a.at))
			} else {
				_ = whole.endPause(a.since, a.until)
				_ = kept.endPause(a.since, a.until)
			}
			if len(kept.Pauses) > maxRunPauses {
				t.Fatalf("seed %d: %d pauses kept, want at most %d", seed, len(kept.Pauses), maxRunPauses)
			}
			want, got := heldBy(a.at), heldBy(a.at)
			want.readPauses(whole.marshal())
			got.readPauses(kept.marshal())
			wd, ws := want.deadline(start)
			gd, gs := got.deadline(start)
			if !gd.Equal(wd) || gs != ws {
				t.Fatalf("seed %d at %v: folded deadline %v stopped %v, want %v stopped %v (record %s)",
					seed, a.at.Sub(base), gd.Sub(base), gs, wd.Sub(base), ws, kept.marshal())
			}
			if wn, gn := want.nextCheck(start, a.at), got.nextCheck(start, a.at); !gn.Equal(wn) {
				t.Fatalf("seed %d at %v: folded next check %v, want %v", seed, a.at.Sub(base), gn.Sub(base), wn.Sub(base))
			}
		}
		if kept.PausesFolded != nil {
			folds++
		}
	}
	if folds < 250 {
		t.Fatalf("only %d of 300 histories folded anything — the property is barely exercised", folds)
	}
}

// The record write folds past the cap: a run paused many times, held for
// review across some of the pauses being folded, keeps a bounded list, and a
// clock re-armed from its row reads the deadline the whole history gives.
func TestRecordRunPause_FoldsPastTheCapAndKeepsTheDeadline(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	ctx := context.Background()
	sess, _ := srv.store.CreateSession(ctx, "", "solver", "alice")
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_folds", UserID: "alice", RunConfig: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AppendEvent(ctx, run.ID, string(providers.EventAwaitingReview), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := srv.store.AppendEvent(ctx, run.ID, "user_input", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	run, _ = srv.store.GetRun(ctx, run.ID)
	held := resumedChildClock{bound: time.Minute}
	if err := held.readEvents(ctx, srv.store, run.ID); err != nil || len(held.holds) != 1 {
		t.Fatalf("holds read = %+v (%v), want one", held.holds, err)
	}
	// Pauses every 8ms from just before the hold began, 5ms each: the first
	// few overlap the hold and are folded.
	from := held.holds[0].from.Add(-4 * time.Millisecond)
	var whole runConfigRecord
	for i := 0; i < 3*maxRunPauses; i++ {
		since := from.Add(time.Duration(8*i) * time.Millisecond)
		until := since.Add(5 * time.Millisecond)
		if err := recordRunPause(ctx, srv.store, run.ID, since, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := recordRunPause(ctx, srv.store, run.ID, since, until); err != nil {
			t.Fatal(err)
		}
		whole.openPause(since)
		_ = whole.endPause(since, until)
	}
	rec := runRecord(t, srv, run.ID)
	if len(rec.Pauses) > maxRunPauses || rec.PausesFolded == nil {
		t.Fatalf("record after %d pauses keeps %d listed, folded %+v: want at most %d and the rest folded",
			3*maxRunPauses, len(rec.Pauses), rec.PausesFolded, maxRunPauses)
	}
	got, want := held, held
	got.readPauses(runRecord(t, srv, run.ID).marshal())
	want.readPauses(whole.marshal())
	gd, _ := got.deadline(run.StartedAt)
	wd, _ := want.deadline(run.StartedAt)
	if !gd.Equal(wd) {
		t.Errorf("deadline from the folded record = %v, want %v from the whole history (off by %v)", gd, wd, gd.Sub(wd))
	}
}
