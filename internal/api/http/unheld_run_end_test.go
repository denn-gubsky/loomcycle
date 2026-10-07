package http

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A run ended with no loop behind it — by a cancel that found nothing holding
// its row, or by a resume pass that would not resume it — still reports its
// end to its run_end hooks: once, as cancelled, with the reason. Its own hooks
// are no longer in memory, and are read back from what its record pinned.

// orphanEnds is a server with a hookless agent (plain) and one whose
// definition carries a run_end hook (audited), plus a fixed chain every run
// with no hooks of its own fires (ops).
type orphanEnds struct {
	srv      *Server
	ops, own *recordingHook
	audited  config.AgentDef
}

func newOrphanEnds(t *testing.T) *orphanEnds {
	t.Helper()
	f := &orphanEnds{ops: newRecordingHook(t, `{}`), own: newRecordingHook(t, `{}`)}
	f.audited = config.AgentDef{Provider: "scripted", Model: "stub-model", SystemPrompt: "work",
		Hooks: hooks.EventHooks{hooks.PhaseRunEnd: {{Inline: &hooks.Inline{Name: "audit", URL: f.own.srv.URL}}}}}
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"plain":   {Provider: "scripted", Model: "stub-model", SystemPrompt: "work"},
			"audited": f.audited,
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Hooks.PrivateHostAllowlist = []string{"127.0.0.1"}
	f.srv, _ = makeServer(t, answeringProvider(), cfg)
	f.srv.unheldRunGrace = time.Nanosecond
	register(t, f.srv, &hooks.Hook{Owner: "ops", Name: "log", Phase: hooks.PhaseRunEnd, CallbackURL: f.ops.srv.URL})
	return f
}

// left files a running row of agent with no loop behind it. An audited run
// records the hooks it started with, as its start would have.
func (f *orphanEnds) left(t *testing.T, agent, agentID, parentRunID string) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := f.srv.store.CreateSession(ctx, "", agent, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var rec runConfigRecord
	if agent == "audited" {
		// The static agent as a resume resolves it: the operator's own yaml.
		rec.PinnedHooks = &pinnedHooks{Agent: agentHooksFingerprint(config.AgentDef{Hooks: f.audited.Hooks, OperatorAuthored: true})}
	}
	run, err := f.srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: "alice", Model: "stub-model",
		ParentRunID: parentRunID, RunConfig: rec.marshal()})
	if err != nil {
		t.Fatal(err)
	}
	return runNow(t, f.srv.store, run.ID)
}

// runEnds counts the run_end payloads rec has for runID.
func runEnds(rec *recordingHook, runID string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, b := range rec.bodies {
		if strings.Contains(b, `"phase":"run_end"`) && strings.Contains(b, `"run_id":"`+runID+`"`) {
			n++
		}
	}
	return n
}

func TestRunEnd_ARunEndedWithNoLoopReportsItsEndOnce(t *testing.T) {
	ctx := context.Background()
	// byCancel ends a row nothing holds; byResume ends a paused child whose
	// parent ended before it was resumed. Each returns the reason it gave.
	byCancel := func(t *testing.T, f *orphanEnds, agent string) (store.Run, string) {
		run := f.left(t, agent, "a_"+agent, "")
		if !f.srv.finishUnheldRun(ctx, run, "stop it") {
			t.Fatal("a row nothing holds was not finished")
		}
		return run, "stop it"
	}
	byResume := func(t *testing.T, f *orphanEnds, agent string) (store.Run, string) {
		parent := f.left(t, "plain", "a_parent", "")
		if err := f.srv.store.FinishRun(ctx, parent.ID, store.RunCancelled, "operator stop", store.Usage{}, ""); err != nil {
			t.Fatal(err)
		}
		run := f.left(t, agent, "a_"+agent, parent.ID)
		pauseMidTurn(t, f.srv, run)
		if n, warns := f.srv.ResumePausedRuns(ctx); n != 0 || len(warns) != 1 {
			t.Fatalf("resumed %d (warnings: %v), want the child refused as orphaned", n, warns)
		}
		return run, "its parent run " + parent.ID + " ended (cancelled) before it was resumed"
	}
	for _, tc := range []struct {
		name  string
		end   func(*testing.T, *orphanEnds, string) (store.Run, string)
		agent string
		// fired is the endpoint the run's end reports to: the agent's own hook
		// for a run that carries one, else the fixed chain.
		fired, silent func(*orphanEnds) *recordingHook
	}{
		{"a cancel, no hooks of its own", byCancel, "plain", (*orphanEnds).opsHook, (*orphanEnds).ownHook},
		{"a cancel, its agent's hook", byCancel, "audited", (*orphanEnds).ownHook, (*orphanEnds).opsHook},
		{"a resume pass, no hooks of its own", byResume, "plain", (*orphanEnds).opsHook, (*orphanEnds).ownHook},
		{"a resume pass, its agent's hook", byResume, "audited", (*orphanEnds).ownHook, (*orphanEnds).opsHook},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrphanEnds(t)
			run, reason := tc.end(t, f, tc.agent)
			if got := runNow(t, f.srv.store, run.ID); got.Status != store.RunCancelled || got.StopReason != reason {
				t.Fatalf("row = %s %q, want cancelled with %q", got.Status, got.StopReason, reason)
			}
			body := hookBody(t, tc.fired(f), hooks.PhaseRunEnd, run.ID)
			for _, want := range []string{`"status":"cancelled"`, `"stop_reason":"` + reason + `"`, `"agent":"` + tc.agent + `"`} {
				if !strings.Contains(body, want) {
					t.Errorf("payload lacks %s: %s", want, body)
				}
			}
			if strings.Contains(body, `"final_text"`) {
				t.Errorf("a run that never answered reported a final text: %s", body)
			}
			if _, kept := f.srv.runHookSets.Load(run.ID); kept {
				t.Error("the ended run's hooks are still held in memory")
			}

			// Ended again — by its row as it was, and as it is now — it reports
			// nothing more. A later run's end arriving bounds the wait for a
			// second report that must not come.
			f.srv.cancelOrphanedRun(run, "again")
			f.srv.finishUnheldRun(ctx, runNow(t, f.srv.store, run.ID), "again")
			later := f.left(t, "plain", "a_later", "")
			f.srv.finishUnheldRun(ctx, later, "stop it")
			hookBody(t, f.ops, hooks.PhaseRunEnd, later.ID)
			if n := runEnds(tc.fired(f), run.ID); n != 1 {
				t.Errorf("the run's end was reported %d times, want once", n)
			}
			if n := runEnds(tc.silent(f), run.ID); n != 0 {
				t.Errorf("the run's end also reached a chain that is not its own (%d)", n)
			}
			if got := runNow(t, f.srv.store, run.ID); got.StopReason != reason {
				t.Errorf("a second cancel rewrote the reason: %q", got.StopReason)
			}
		})
	}
}

func (f *orphanEnds) opsHook() *recordingHook { return f.ops }
func (f *orphanEnds) ownHook() *recordingHook { return f.own }

// readTogether holds each read of one run's row until a second reader has
// read it too, or a bound passes: two cancellers that both read the row
// before either ends it both see it running.
type readTogether struct {
	store.Store
	runID   string
	mu      sync.Mutex
	readers int
}

func (r *readTogether) GetRun(ctx context.Context, id string) (store.Run, error) {
	run, err := r.Store.GetRun(ctx, id)
	if id != r.runID {
		return run, err
	}
	r.mu.Lock()
	r.readers++
	r.mu.Unlock()
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		r.mu.Lock()
		together := r.readers >= 2
		r.mu.Unlock()
		if together {
			break
		}
	}
	return run, err
}

// Two cancellers reaching one unheld row together report its end once: the
// second stands down rather than read a row the first has not yet ended.
func TestRunEnd_ConcurrentCancelsOfAnUnheldRunReportItsEndOnce(t *testing.T) {
	f := newOrphanEnds(t)
	run := f.left(t, "plain", "a_plain", "")
	f.srv.store = &readTogether{Store: f.srv.store, runID: run.ID}
	const cancellers = 4
	start, done := make(chan struct{}), make(chan struct{}, cancellers)
	for range cancellers {
		go func() {
			<-start
			f.srv.cancelOrphanedRun(run, "stop it")
			done <- struct{}{}
		}()
	}
	close(start)
	for range cancellers {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a cancel of an unheld row did not return")
		}
	}
	hookBody(t, f.ops, hooks.PhaseRunEnd, run.ID)
	later := f.left(t, "plain", "a_later", "")
	f.srv.finishUnheldRun(context.Background(), later, "stop it")
	hookBody(t, f.ops, hooks.PhaseRunEnd, later.ID)
	if n := runEnds(f.ops, run.ID); n != 1 {
		t.Errorf("the run's end was reported %d times, want once", n)
	}
	if got := runNow(t, f.srv.store, run.ID); got.Status != store.RunCancelled {
		t.Errorf("row = %s, want cancelled", got.Status)
	}
}
