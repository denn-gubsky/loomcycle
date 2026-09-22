package http

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// turnStateProvider answers every stateful step by ending the turn, writing the
// turn number into Σ, and records what each request fed the model.
type turnStateProvider struct {
	mu   sync.Mutex
	n    int
	feds []string
}

func (p *turnStateProvider) ID() string                                   { return "stub" }
func (p *turnStateProvider) Probe(context.Context) error                  { return nil }
func (p *turnStateProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *turnStateProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *turnStateProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.n++
	n := p.n
	var b strings.Builder
	for _, m := range req.Messages {
		for _, c := range m.Content {
			b.WriteString(c.Text)
		}
	}
	p.feds = append(p.feds, b.String())
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: fmt.Sprintf("t%d", n),
		Name: "emit_state", Input: json.RawMessage(fmt.Sprintf(`{"patch":{"turn%d":"seen"},"done":true,"final":"answer %d"}`, n, n))}}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

// ⚠️ THE REPORTED SYMPTOM, THROUGH THE REAL SERVER: the embedded terminal
// showed a parked stateful chat as completed after its first answer, because
// a `done` frame arrived at every turn boundary. The terminal then sent the
// next message as a continuation — a new run with an empty Σ. This drives the
// SSE wire the terminal reads: no `done` may appear while the chat is live,
// and turn two must be fed turn one's Σ plus the operator's message.
func TestInteractiveTerminal_AStatefulChatStaysLiveAndKeepsItsState(t *testing.T) {
	mode := config.ContextModeStateful
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"statefulterm": {Model: "stub-model", SystemPrompt: "be brief", Context: &config.Context{Mode: &mode}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &turnStateProvider{}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "stateful-term.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"statefulterm","interactive":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatalf("post run: %v", err)
	}
	defer resp.Body.Close()

	type frame struct{ typ, data string }
	frames := make(chan frame, 128)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		var typ, data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if typ != "" {
					frames <- frame{typ, data}
					typ, data = "", ""
				}
			case strings.HasPrefix(line, "event:"):
				typ = strings.TrimSpace(line[len("event:"):])
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(line[len("data:"):])
			}
		}
	}()
	untilParked := func(turn string) (runID string) {
		t.Helper()
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					t.Fatalf("%s: the stream closed before the chat parked", turn)
				}
				switch f.typ {
				case "agent":
					var a struct {
						RunID string `json:"run_id"`
					}
					_ = json.Unmarshal([]byte(f.data), &a)
					runID = a.RunID
				case "done":
					t.Fatalf("%s: a `done` frame arrived while the chat was live — the terminal "+
						"marks the run completed on it and continues the session as a new run", turn)
				case "error":
					t.Fatalf("%s: error frame: %s", turn, f.data)
				case "awaiting_input":
					return runID
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s: timed out waiting for the chat to park", turn)
			}
		}
	}

	runID := untilParked("turn 1")
	if runID == "" {
		t.Fatal("never captured run_id")
	}
	in, err := http.Post(ts.URL+"/v1/runs/"+runID+"/input", "application/json",
		strings.NewReader(`{"text":"and refresh?"}`))
	if err != nil {
		t.Fatalf("post input: %v", err)
	}
	body, _ := io.ReadAll(in.Body)
	in.Body.Close()
	if in.StatusCode != 200 {
		t.Fatalf("input status = %d: %s", in.StatusCode, body)
	}
	untilParked("turn 2")

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.feds) != 2 {
		t.Fatalf("model calls = %d, want 2 (one per turn, on the same run)", len(prov.feds))
	}
	if !strings.Contains(prov.feds[1], `"turn1":"seen"`) {
		t.Errorf("turn 2 was not fed turn 1's Σ:\n%s", prov.feds[1])
	}
	if !strings.Contains(prov.feds[1], "operator: and refresh?") {
		t.Errorf("turn 2 was not fed the operator's message:\n%s", prov.feds[1])
	}
	if strings.Contains(prov.feds[1], "answer 1") {
		t.Errorf("turn 2 was fed the previous answer — the transcript leaked back in:\n%s", prov.feds[1])
	}
}
