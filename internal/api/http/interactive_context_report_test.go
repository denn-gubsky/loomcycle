package http

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// windowedStubProvider is stubProvider with a reported context window, which is
// what arms the distillation gate — the plain stub reports none, so every
// existing interactive test runs with the gate permanently shut.
type windowedStubProvider struct {
	maxCtx int
	in     int
}

func (p *windowedStubProvider) ID() string                    { return "stub" }
func (p *windowedStubProvider) Probe(_ context.Context) error { return nil }
func (p *windowedStubProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *windowedStubProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: p.maxCtx}
}
func (p *windowedStubProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn",
		Usage: &providers.Usage{InputTokens: p.in, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

// The incident, on the consumer's own transport.
//
// An operator's terminal reported that every interactive run in v1.85.0 "stopped
// after started" — and the runtime was fine: it emitted three NEW frames
// (two declines and an exhaustion report) between `started` and the first turn,
// then answered and parked exactly as before. The frames were false. The run had
// sent nothing; the 88%-full window they described was the preamble estimate.
//
// This asserts at the SSE seam because that is where the break was seen. The
// loop-level twin (TestRun_NoContextReportBeforeTheFirstMeasuredTurn) pins the
// mechanism; this pins the frame sequence a client actually reads.
func TestInteractiveRun_NoContextReportBeforeTheFirstTurn(t *testing.T) {
	// 16000 preamble tokens against a 20000 window: the system prompt alone is
	// 80% of the window, which is the shape a small local model has.
	big := strings.Repeat("system prompt filler. ", 3200)
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"termagent": {Model: "stub-model", SystemPrompt: big, UnboundedIterations: true},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode

	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "ctxreport.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: &windowedStubProvider{maxCtx: 20000, in: 17000}},
		[]tools.Tool{}, sem, st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"termagent","interactive":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("run status = %d", resp.StatusCode)
	}

	type frame struct{ typ string }
	frames := make(chan frame, 128)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		var typ, data string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if typ != "" {
					frames <- frame{typ}
					typ, data = "", ""
				}
				continue
			}
			if strings.HasPrefix(line, "event:") {
				typ = strings.TrimSpace(line[len("event:"):])
			}
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(line[len("data:"):])
			}
			_ = data
		}
	}()

	var got []string
	seenTurn := false
	deadline := time.After(10 * time.Second)
collect:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				break collect
			}
			got = append(got, f.typ)
			switch f.typ {
			case "text", "usage":
				seenTurn = true
			case "context_distill_declined", "context_exhausted":
				if !seenTurn {
					t.Errorf("%s frame arrived before the agent's first turn; frames=%v", f.typ, got)
				}
			}
			if f.typ == "awaiting_input" || f.typ == "done" || f.typ == "error" {
				break collect
			}
		case <-deadline:
			t.Fatalf("interactive run produced no terminal frame within 10s; frames=%v", got)
		}
	}
	t.Logf("frames: %v", got)
	if !seenTurn {
		t.Errorf("the run never produced a turn at all; frames=%v", got)
	}
}
