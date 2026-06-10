package http

import (
	"bufio"
	"encoding/json"
	"io"
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

// TestInteractiveTerminal_EndToEnd is the codified "smoke": it drives the full
// interactive-terminal flow through the real HTTP server + SSE + loop —
//
//	start interactive run → loop parks at end_turn (`awaiting_input`)
//	→ POST /v1/runs/{run_id}/input → `steer` frame + the run RESUMES
//	→ parks again → client disconnect ends the persistent run.
//
// Everything below the SSE wire is real (RunOnce → loop → steer registry →
// the /input handler → drain → resume); only the provider is stubbed (always
// end_turn, so each turn parks). This is the chain the unit/integration tests
// only covered piecewise.
func TestInteractiveTerminal_EndToEnd(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			// interactive runs pair with unbounded_iterations so parks don't
			// count toward the 16-cap (a persistent terminal agent).
			"termagent": {Model: "stub-model", SystemPrompt: "be brief", UnboundedIterations: true},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode

	// Always end_turn → an interactive run parks after every turn.
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	// A real store so openOrCreateSessionAndRun mints a run_id (the steer
	// registry keys on it — without a store, steering can't be wired).
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "interactive.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, st)
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

	type frame struct{ typ, data string }
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
					frames <- frame{typ, data}
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
		}
	}()
	next := func() frame {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("SSE stream closed unexpectedly")
			}
			return f
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for an SSE frame")
			return frame{}
		}
	}

	// Phase 1: drain until the run parks, capturing run_id from the agent frame.
	var runID string
	for {
		f := next()
		if f.typ == "agent" {
			var a struct {
				RunID string `json:"run_id"`
			}
			_ = json.Unmarshal([]byte(f.data), &a)
			runID = a.RunID
		}
		if f.typ == "awaiting_input" {
			break
		}
		if f.typ == "done" || f.typ == "error" {
			t.Fatalf("run ended (%s) before parking — interactive park not engaged", f.typ)
		}
	}
	if runID == "" {
		t.Fatal("never captured run_id from the agent frame")
	}

	// Phase 2: steer the parked run.
	in, err := http.Post(ts.URL+"/v1/runs/"+runID+"/input", "application/json",
		strings.NewReader(`{"text":"keep going"}`))
	if err != nil {
		t.Fatalf("post input: %v", err)
	}
	body, _ := io.ReadAll(in.Body)
	in.Body.Close()
	if in.StatusCode != 200 {
		t.Fatalf("input status = %d: %s", in.StatusCode, body)
	}

	// Phase 3: the steer surfaces (`steer` frame) and the run RESUMES (another
	// `text` turn) before parking again.
	sawSteer, resumed := false, false
	for {
		f := next()
		switch f.typ {
		case "steer":
			sawSteer = true
		case "text":
			if sawSteer {
				resumed = true
			}
		case "awaiting_input":
			if sawSteer && resumed {
				goto done
			}
		case "done", "error":
			goto done
		}
	}
done:
	if !sawSteer {
		t.Error("no `steer` frame after POST /input — steering didn't reach the live stream")
	}
	if !resumed {
		t.Error("run did not resume (no `text` after the steer) — park didn't wake")
	}

	// End the persistent run: client disconnect cancels the run ctx, which
	// unblocks the park and terminates the loop.
	resp.Body.Close()
}
