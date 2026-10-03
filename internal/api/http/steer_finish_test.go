package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// gatedAnswerProvider answers "answer N". Its first answer stops mid-stream —
// text sent, the turn not yet done — until release is closed, and reports
// that moment on streaming, so a test can send the run a message while its
// final answer is being written.
type gatedAnswerProvider struct {
	numberedProvider
	once      sync.Once
	streaming chan struct{}
	release   chan struct{}
}

func (p *gatedAnswerProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	in, err := p.numberedProvider.Call(ctx, req)
	if err != nil {
		return nil, err
	}
	first := false
	p.once.Do(func() { first = true })
	if !first {
		return in, nil
	}
	out := make(chan providers.Event)
	go func() {
		defer close(out)
		for ev := range in {
			if ev.Type == providers.EventDone {
				close(p.streaming)
				<-p.release
			}
			out <- ev
		}
	}()
	return out, nil
}

// newSteerFinishHarness is newReviewHarness on a provider of the test's own.
func newSteerFinishHarness(t *testing.T, prov providers.Provider) *reviewHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"writer": {Model: "stub-model", SystemPrompt: "write"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "steer-finish.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &reviewHarness{t: t, srv: srv, ts: ts, st: st}
}

// A walk member that is sent "Answer in one sentence." while its final answer
// is streaming answers it, and the walk's sink carries that second answer —
// the message was acknowledged, so it is not dropped when the member ends.
func TestTeamWalk_AMemberSentAMessageDuringItsFinalAnswerAnswersIt(t *testing.T) {
	prov := &gatedAnswerProvider{streaming: make(chan struct{}), release: make(chan struct{})}
	h := newSteerFinishHarness(t, prov)

	ch := &sinkCapture{}
	spawn := func(ctx context.Context, name string, p teamrun.Prompt, defID string) (teamrun.SpawnResult, error) {
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1"})
		return h.srv.runTeamMember(ctx, name, p, defID)
	}
	r := teamrun.NewAgentRunner(spawn, teamrun.WithChannels(ch))
	st := teamgraph.State{ID: "wave", Handler: teamgraph.Handler{
		Kind:   teamgraph.HandlerStarter,
		Source: &teamgraph.StarterSource{Channel: "in"},
		Fanout: &teamgraph.StarterFanout{Agent: "writer", Per: teamgraph.FanoutPerMessage, Max: 1},
		Prompt: &teamgraph.StarterPrompt{Input: teamrun.StarterMessageSlot},
		Sink:   &teamgraph.StarterSink{Channel: "out"},
	}}
	done := make(chan error, 1)
	go func() {
		_, err := r.RunHandler(context.Background(), st, &teamrun.Task{Input: "go", WalkID: "wlk_late_steer"})
		done <- err
	}()

	select {
	case <-prov.streaming:
	case <-time.After(3 * time.Second):
		close(prov.release)
		t.Fatal("the member never started its answer")
	}
	runs, err := h.st.ListActiveRunsByUser(context.Background(), "", "u1", store.RunRunning)
	if err != nil || len(runs) != 1 {
		close(prov.release)
		t.Fatalf("active member runs = %d (%v), want 1", len(runs), err)
	}
	code, body := postInput(t, h.ts, runs[0].ID, `{"text":"Answer in one sentence."}`)
	close(prov.release)
	if code != http.StatusOK {
		t.Fatalf("POST input during the final answer = %d %s, want 200", code, body)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("starter: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the starter never returned")
	}
	seen := prov.seen()
	if len(seen) != 2 || seen[1] != "Answer in one sentence." {
		t.Fatalf("the provider was sent %q, want a second call answering the message", seen)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.published) != 1 {
		t.Fatalf("published %d sink messages, want 1", len(ch.published))
	}
	var msg teamrun.SinkMessage
	if err := json.Unmarshal(ch.published[0], &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Status != teamrun.SinkOK || msg.Output != "answer 2" {
		t.Errorf("sink message = %s, want status ok and the answer to the message", ch.published[0])
	}
}

// A message sent once the run has finished — after its last read of the queue,
// in the moment before it deregisters — is refused 404 like one sent to an
// ended run, instead of answered {"delivered": true} and never read.
func TestRunOnce_AMessageSentAfterTheRunFinishedIsNotInFlight(t *testing.T) {
	h := newReviewHarness(t)
	var runID string
	var once sync.Once
	var code int
	var body string
	var registered, closed bool
	err := h.srv.RunOnce(context.Background(), runner.RunInput{
		Agent: "writer", UserID: "u1",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	}, runner.RunCallbacks{
		OnRegistered: func(_, rid, _, _ string) { runID = rid },
		OnEvent: func(ev providers.Event) {
			if ev.Type != providers.EventDone {
				return
			}
			// The run's terminal event: emitted after its last read of the
			// queue and before RunOnce returns and deregisters it.
			once.Do(func() {
				e, ok := h.srv.steerReg.Get(runID)
				registered, closed = ok, e.Closed()
				code, body = postInput(t, h.ts, runID, `{"text":"too late"}`)
			})
		},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !registered {
		t.Fatal("the run was deregistered before its terminal event — the probe ran too late to test anything")
	}
	if !closed {
		t.Error("the finishing run's queue was still open at its terminal event")
	}
	if code != http.StatusNotFound {
		t.Errorf("POST input after the run finished = %d %s, want 404 not in flight", code, body)
	}
	if seen := h.prov.seen(); len(seen) != 1 {
		t.Errorf("the provider was called %d times, want 1: %s", len(seen), fmt.Sprint(seen))
	}
}
