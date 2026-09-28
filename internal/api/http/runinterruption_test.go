package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// askingProvider makes the model call Interruption.notify on every turn that
// does not end in a tool result, and records each tool result it is handed —
// the answer the REAL Interruption tool gave under the policy the run had.
// The assertion is on that answer because it is the only place the policy
// becomes observable: everything upstream is a value being copied around.
type askingProvider struct {
	mu      sync.Mutex
	calls   int
	results []string
}

func (p *askingProvider) ID() string                    { return "scripted" }
func (p *askingProvider) Probe(_ context.Context) error { return nil }
func (p *askingProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *askingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *askingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var events []providers.Event
	if res, ok := lastToolResult(req); ok {
		p.results = append(p.results, res)
		events = endTurn()
	} else {
		p.calls++
		events = []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
				ID: fmt.Sprintf("tu_%d", p.calls), Name: "Interruption",
				Input: json.RawMessage(`{"op":"notify","message":"may I?"}`),
			}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func lastToolResult(req providers.Request) (string, bool) {
	if n := len(req.Messages); n > 0 {
		for _, c := range req.Messages[n-1].Content {
			if c.Type == "tool_result" {
				return c.Text, true
			}
		}
	}
	return "", false
}

// waitResult returns the i-th (0-based) tool result the model was handed.
func (p *askingProvider) waitResult(t *testing.T, i int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		if len(p.results) > i {
			r := p.results[i]
			p.mu.Unlock()
			return r
		}
		p.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the model was never handed tool result #%d", i+1)
	return ""
}

const (
	notifyDelivered   = `"status":"delivered"`
	notifyKindRefused = "not in this agent's allowed kinds"
	notifyDisabled    = "not enabled for this agent"
)

// interruptionServer: "asker" holds the Interruption tool and allows two kinds;
// "plain" does not hold it. The Interruption tool is the real one.
func interruptionServer(t *testing.T) (*Server, *httptest.Server, *askingProvider, store.Store) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"asker": {
				Model: "stub-model", SystemPrompt: "ask", Tools: []string{"Interruption"},
				Interruption: config.AgentInterruptionACL{Kinds: []string{"question", "approval"}, MaxPending: 3},
			},
			"plain": {Model: "stub-model", SystemPrompt: "work", Tools: []string{"Read"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "interruption.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &askingProvider{}
	it := &builtin.Interruption{Store: st, Bus: channels.NewBus()}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{it}, concurrency.New(4, 4, time.Second), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, ts, prov, st
}

// postRunToEnd posts a run and reads it to the end.
func postRunToEnd(t *testing.T, ts *httptest.Server, path, body string) string {
	t.Helper()
	resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("post %s = %d: %s", path, resp.StatusCode, b)
	}
	return string(b)
}

func narrowedToApproval() *config.AgentInterruptionACL {
	return &config.AgentInterruptionACL{Enabled: true, Kinds: []string{"approval"}}
}

// POST /v1/runs had no `interruption` field, so the block every client sent was
// dropped by the decoder; and even a block that reached the run was never read.
// Narrowing the kinds must reach the real tool's own refusal.
func TestRunInterruption_PostRunsNarrowedKindsRefuseTheCall(t *testing.T) {
	_, ts, prov, st := interruptionServer(t)

	// The fixture first: without a block the definition allows the call.
	postRunToEnd(t, ts, "/v1/runs", `{"agent":"asker","prompt":"go"}`)
	if got := prov.waitResult(t, 0); !strings.Contains(got, notifyDelivered) {
		t.Fatalf("fixture drifted: the definition's own policy refused the call: %s", got)
	}

	body := postRunToEnd(t, ts, "/v1/runs", `{"agent":"asker","prompt":"go","interruption":{"enabled":true,"kinds":["approval"]}}`)
	if got := prov.waitResult(t, 1); !strings.Contains(got, notifyKindRefused) {
		t.Errorf("a run narrowed to kinds [approval] was allowed a question: %s", got)
	}
	rec, ok := decodeRunConfig(onlyRun(t, st, extractSessionID(body)).RunConfig)
	if !ok || rec.Interruption == nil || !reflect.DeepEqual(rec.Interruption.Kinds, []string{"approval"}) {
		t.Errorf("run_config interruption = %+v, want the run's own block recorded", rec.Interruption)
	}
}

// NARROW ONLY: a run cannot hand itself the Interruption tool its definition
// does not list. The block is inert there, and the run says so — once.
func TestRunInterruption_ARunCannotGrantTheTool(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(t *testing.T, srv *Server, ts *httptest.Server) (sessionID, runID string)
	}{
		{"POST /v1/runs", func(t *testing.T, _ *Server, ts *httptest.Server) (string, string) {
			sid := extractSessionID(postRunToEnd(t, ts, "/v1/runs", `{"agent":"plain","prompt":"go","interruption":{"enabled":true}}`))
			return sid, ""
		}},
		{"RunOnce", func(t *testing.T, srv *Server, _ *httptest.Server) (string, string) {
			var sid, rid string
			err := srv.RunOnce(context.Background(), runner.RunInput{
				Agent:        "plain",
				Segments:     []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
				Interruption: &config.AgentInterruptionACL{Enabled: true},
			}, runner.RunCallbacks{OnRegistered: func(_, runID, sessionID, _ string) { sid, rid = sessionID, runID }})
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			return sid, rid
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts, prov, st := interruptionServer(t)
			sid, rid := tc.start(t, srv, ts)
			if got := prov.waitResult(t, 0); strings.Contains(got, notifyDelivered) {
				t.Fatalf("a per-run block granted the Interruption tool to an agent that does not hold it: %s", got)
			}
			if rid == "" {
				rid = onlyRun(t, st, sid).ID
			}
			transcript := runTranscriptText(t, st, sid, rid)
			if n := strings.Count(transcript, `"gate":"interruption"`); n != 1 {
				t.Errorf("the inert block was reported %d times, want once — unreported, the caller "+
					"learns it did nothing only when the agent never asks.\ntranscript:\n%s", n, transcript)
			}
		})
	}
}

// Every other path that starts a run reaches the same seam, and each carried the
// field up to it and then lost it. One table, so a path added later has an
// obvious row to join.
func TestRunInterruption_EverySpawnPathCarriesIt(t *testing.T) {
	segs := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
	for _, tc := range []struct {
		name  string
		start func(t *testing.T, srv *Server, ts *httptest.Server)
	}{
		{"connector SpawnRun (MCP spawn_run, gRPC batch)", func(t *testing.T, srv *Server, _ *httptest.Server) {
			if _, err := srv.SpawnRun(context.Background(), connector.SpawnRunRequest{
				Agent: "asker", Segments: segs, Interruption: narrowedToApproval(),
			}); err != nil {
				t.Fatalf("SpawnRun: %v", err)
			}
		}},
		{"RunOnce (gRPC run, MCP streaming spawn)", func(t *testing.T, srv *Server, _ *httptest.Server) {
			if err := srv.RunOnce(context.Background(), runner.RunInput{
				Agent: "asker", Segments: segs, Interruption: narrowedToApproval(),
			}, runner.RunCallbacks{}); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
		}},
		{"HTTP draft, then start", func(t *testing.T, _ *Server, ts *httptest.Server) {
			var c createdRun
			body := postRunToEnd(t, ts, "/v1/runs", `{"agent":"asker","start":false,"prompt":"go","interruption":{"enabled":true,"kinds":["approval"]}}`)
			if err := json.Unmarshal([]byte(body), &c); err != nil || c.RunID == "" {
				t.Fatalf("draft create: %v %s", err, body)
			}
			postRunToEnd(t, ts, "/v1/runs/"+c.RunID+"/start", "")
		}},
		{"HTTP continuation", func(t *testing.T, _ *Server, ts *httptest.Server) {
			// The seed run carries no block, so the refusal can only be the continuation's.
			sid := extractSessionID(postRunToEnd(t, ts, "/v1/runs", `{"agent":"asker","prompt":"go"}`))
			postRunToEnd(t, ts, "/v1/sessions/"+sid+"/messages", `{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"again"}]}],"interruption":{"enabled":true,"kinds":["approval"]}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts, prov, _ := interruptionServer(t)
			tc.start(t, srv, ts)
			last := 0
			if tc.name == "HTTP continuation" {
				last = 1
			}
			if got := prov.waitResult(t, last); !strings.Contains(got, notifyKindRefused) {
				t.Errorf("the run's own interruption block did not reach the tool: %s", got)
			}
		})
	}
}

// parkedAskerRun restores an interactive "asker" chat that is parked waiting for
// its operator — the run an operator actually retunes.
func parkedAskerRun(t *testing.T) (*Server, *httptest.Server, *askingProvider, store.Run) {
	t.Helper()
	srv, ts, prov, st := interruptionServer(t)
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "asker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_intr", UserID: "alice", Model: "stub-model", Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
	})
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "hello"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{
		Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{},
	})
	if err := st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	waitFor(t, "the restored chat to park", func() bool {
		return strings.Contains(runTranscriptText(t, st, run.SessionID, run.ID), "awaiting_input")
	})
	return srv, ts, prov, run
}

// A retune of a parked run lands at its NEXT OPERATOR TURN — the boundary
// routing adopts a retune at. The turn before it proves the tool worked, so the
// refusal after it can only be the retune.
func TestRunInterruption_RetuneReachesAParkedRunAtItsNextOperatorTurn(t *testing.T) {
	_, ts, prov, run := parkedAskerRun(t)

	if code, b := postInput(t, ts, run.ID, `{"text":"ask me"}`); code != 200 {
		t.Fatalf("first turn: %d %s", code, b)
	}
	if got := prov.waitResult(t, 0); !strings.Contains(got, notifyDelivered) {
		t.Fatalf("fixture drifted: the parked run could not ask before any retune: %s", got)
	}

	if code, b := postRetune(t, ts, run.ID, `{"interruption":{"enabled":false}}`); code != 200 {
		t.Fatalf("retune: %d %s", code, b)
	}
	if code, b := postInput(t, ts, run.ID, `{"text":"ask me again"}`); code != 200 {
		t.Fatalf("second turn: %d %s", code, b)
	}
	if got := prov.waitResult(t, 1); !strings.Contains(got, notifyDisabled) {
		t.Errorf("the turn after the retune still asked — the live policy kept what the run "+
			"started with: %s", got)
	}
}

// A run that paused keeps its own block when it comes back: resume re-narrows
// from the record rather than reverting to the definition.
func TestRunInterruption_ResumeKeepsTheRunsBlock(t *testing.T) {
	srv, _, prov, st := interruptionServer(t)
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "asker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	rec := runConfigRecord{Interruption: narrowedToApproval()}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_resume", UserID: "alice", Model: "stub-model", RunConfig: rec.marshal(),
	})
	if err != nil {
		t.Fatal(err)
	}
	parkForResume(t, srv, run.ID)
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	if got := prov.waitResult(t, 0); !strings.Contains(got, notifyKindRefused) {
		t.Errorf("the resumed run reverted to its definition's interruption policy: %s", got)
	}
}

// The rule itself, one row per field — the tests above cross the seams, this
// pins what each field may and may not do.
func TestInterruptionPolicyForRun_NarrowsAndNeverGrants(t *testing.T) {
	s := &Server{}
	holder := config.AgentDef{Tools: []string{"Interruption"},
		Interruption: config.AgentInterruptionACL{Kinds: []string{"question", "approval"}, MaxPending: 3}}
	for _, tc := range []struct {
		name  string
		def   config.AgentDef
		holds bool
		run   *config.AgentInterruptionACL
		want  tools.InterruptionPolicyValue
	}{
		{"no block keeps the definition's", holder, true, nil,
			tools.InterruptionPolicyValue{Enabled: true, Kinds: []string{"question", "approval"}, MaxPending: 3}},
		{"enabled:false switches it off", holder, true, &config.AgentInterruptionACL{},
			tools.InterruptionPolicyValue{}},
		{"kinds intersect", holder, true, &config.AgentInterruptionACL{Enabled: true, Kinds: []string{"approval", "wait"}},
			tools.InterruptionPolicyValue{Enabled: true, Kinds: []string{"approval"}, MaxPending: 3}},
		{"no kind in common switches it off", holder, true, &config.AgentInterruptionACL{Enabled: true, Kinds: []string{"wait"}},
			tools.InterruptionPolicyValue{}},
		{"max_pending cannot be raised", holder, true, &config.AgentInterruptionACL{Enabled: true, MaxPending: 10},
			tools.InterruptionPolicyValue{Enabled: true, Kinds: []string{"question", "approval"}, MaxPending: 3}},
		{"max_pending can be lowered", holder, true, &config.AgentInterruptionACL{Enabled: true, MaxPending: 1},
			tools.InterruptionPolicyValue{Enabled: true, Kinds: []string{"question", "approval"}, MaxPending: 1}},
		{"an empty definition kind list means question", config.AgentDef{Tools: []string{"Interruption"}}, true,
			&config.AgentInterruptionACL{Enabled: true, Kinds: []string{"question", "approval"}},
			tools.InterruptionPolicyValue{Enabled: true, Kinds: []string{"question"}}},
		{"a run cannot grant the tool", config.AgentDef{Tools: []string{"Read"}}, false,
			&config.AgentInterruptionACL{Enabled: true},
			tools.InterruptionPolicyValue{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.interruptionPolicyForRun(tc.def, tc.holds, tc.run); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("policy = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A same-definition child inherits the narrowing: a run told not to ask a
// person must not ask one through a copy of itself.
func TestOverrideInheritance_ChildKeepsTheParentsInterruptionNarrowing(t *testing.T) {
	srv := routingServer(t)
	parentRec := runConfigRecord{Interruption: &config.AgentInterruptionACL{}}
	ctx := tools.WithRunOverrides(context.Background(), tools.RunOverridesValue{
		Record: parentRec.marshal(), AgentName: "router",
	})
	_, cfg := srv.inheritOverridesForChild(ctx, config.AgentDef{Tier: "middle"}, runConfigRecord{}, "", "router")
	if cfg.Interruption == nil || cfg.Interruption.Enabled {
		t.Errorf("child interruption = %+v, want the parent's switch-off inherited", cfg.Interruption)
	}
}
