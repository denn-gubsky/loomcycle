package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// gatedStatefulProvider ends every stateful step with `final: "answer N"`.
// Its first step stops before its terminal event until release is closed, and
// reports that moment on streaming, so a test can send the run a message
// while its final step is being produced.
type gatedStatefulProvider struct {
	mu        sync.Mutex
	prompts   []string
	streaming chan struct{}
	release   chan struct{}
}

func (p *gatedStatefulProvider) ID() string                                   { return "stub" }
func (p *gatedStatefulProvider) Probe(context.Context) error                  { return nil }
func (p *gatedStatefulProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *gatedStatefulProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *gatedStatefulProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	last := ""
	if n := len(req.Messages); n > 0 {
		for _, c := range req.Messages[n-1].Content {
			last += c.Text
		}
	}
	p.mu.Lock()
	p.prompts = append(p.prompts, last)
	n := len(p.prompts)
	p.mu.Unlock()
	out := make(chan providers.Event)
	go func() {
		defer close(out)
		out <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: fmt.Sprintf("t%d", n), Name: "emit_state",
			Input: json.RawMessage(fmt.Sprintf(`{"patch":{"step":%d},"done":true,"final":"answer %d"}`, n, n))}}
		if n == 1 {
			close(p.streaming)
			<-p.release
		}
		out <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	}()
	return out, nil
}

func (p *gatedStatefulProvider) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

// A non-interactive stateful run that is sent a message while its final step
// is being produced answers it: the API said delivered, so the run takes
// another step with the message as its task and the state it had built, the
// message is recorded on its transcript, and that second answer is the run's.
func TestRunOnce_AStatefulRunSentAMessageDuringItsFinalStepAnswersIt(t *testing.T) {
	mode := config.ContextModeStateful
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"writer": {Model: "stub-model", SystemPrompt: "write", Context: &config.Context{Mode: &mode}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "steer-finish-stateful.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &gatedStatefulProvider{streaming: make(chan struct{}), release: make(chan struct{})}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	var mu sync.Mutex
	var runID, sessionID string
	var texts []string
	done := make(chan error, 1)
	go func() {
		done <- srv.RunOnce(context.Background(), runner.RunInput{
			Agent: "writer", UserID: "u1",
			Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		}, runner.RunCallbacks{
			OnRegistered: func(_, rid, sid, _ string) {
				mu.Lock()
				runID, sessionID = rid, sid
				mu.Unlock()
			},
			OnEvent: func(ev providers.Event) {
				if ev.Type == providers.EventText {
					mu.Lock()
					texts = append(texts, ev.Text)
					mu.Unlock()
				}
			},
		})
	}()

	select {
	case <-prov.streaming:
	case <-time.After(3 * time.Second):
		close(prov.release)
		t.Fatal("the run never started its final step")
	}
	mu.Lock()
	rid, sid := runID, sessionID
	mu.Unlock()
	code, body := postInput(t, ts, rid, `{"text":"Answer in one sentence."}`)
	close(prov.release)
	if code != http.StatusOK {
		t.Fatalf("POST input during the final step = %d %s, want 200", code, body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run never ended")
	}

	seen := prov.seen()
	if len(seen) != 2 {
		t.Fatalf("the provider was called %d time(s), want 2 — the delivered message was never answered: %q", len(seen), seen)
	}
	if !strings.Contains(seen[1], `"step":1`) || !strings.HasSuffix(seen[1], "Latest observation:\nTask: Answer in one sentence.") {
		t.Errorf("second step's prompt = %q, want the first step's state and the message as its observation", seen[1])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 2 || texts[1] != "answer 2" {
		t.Errorf("answers = %q, want the answer to the message last", texts)
	}
	events, err := st.GetTranscript(context.Background(), sid)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	recorded := false
	for _, ev := range events {
		if ev.Type == "user_input" && strings.Contains(string(ev.Payload), "Answer in one sentence.") {
			recorded = true
		}
	}
	if !recorded {
		t.Error("the message is not on the run's transcript as a user_input")
	}
	if code, body := postInput(t, ts, rid, `{"text":"too late"}`); code != http.StatusNotFound {
		t.Errorf("POST input after the run finished = %d %s, want 404", code, body)
	}
}
