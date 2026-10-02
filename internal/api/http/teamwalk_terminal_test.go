package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A team with two end states — success publishes, a pushback abandons — could
// not say which one its walk reached: the walk's run result held only the
// final text, and a client had to re-derive the end from the transcript.

// twoEndsWalkTeam's judge selects its edge with a `signal:` line: none takes
// success to `published`, `signal: pushback:stop` takes it to `abandoned`.
const twoEndsWalkTeam = `{"entry":"judge","states":[` +
	`{"state":"judge","handler":{"kind":"consolidator","agent":"judge"}},` +
	`{"state":"published","handler":{"kind":"terminal"}},` +
	`{"state":"abandoned","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"judge","to":"published","on":"success"},` +
	`{"from":"judge","to":"abandoned","on":"pushback:stop"}]}`

// brokenWalkTeam's only member names an agent that does not exist, so its
// walk fails before reaching its end.
const brokenWalkTeam = `{"entry":"judge","states":[` +
	`{"state":"judge","handler":{"kind":"agent","agent":"ghost"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"judge","to":"done","on":"success"}]}`

// replyProvider answers every call with one fixed text.
type replyProvider struct{ reply string }

func (p *replyProvider) ID() string                  { return "stub" }
func (p *replyProvider) Probe(context.Context) error { return nil }
func (p *replyProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *replyProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *replyProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: p.reply}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

// newTerminalHarness is a server whose `judge` always answers reply, with the
// TeamDef tool wired and both teams promoted in tenant acme.
func newTerminalHarness(t *testing.T, reply string) *walkHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"judge": {Model: "stub-model", SystemPrompt: "judge"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "terminal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: &replyProvider{reply: reply}}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	seedTenantTeam(t, st, "acme", "two-ends", twoEndsWalkTeam)
	seedTenantTeam(t, st, "acme", "broken", brokenWalkTeam)
	return &walkHarness{t: t, srv: srv, st: st}
}

// readWalkRun reads GET /v1/runs/{run_id} through the mux as alice.
func readWalkRun(t *testing.T, srv *Server, runID string) agentResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID, nil)
	req = req.WithContext(alicePrincipal(req.Context()))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s = %d: %s", runID, rec.Code, rec.Body)
	}
	var out agentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body)
	}
	return out
}

// resultTerminal reads `terminal` from a run's result, "" when absent.
func resultTerminal(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var r struct {
		Terminal string `json:"terminal"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("result %s: %v", raw, err)
	}
	return r.Terminal
}

// awaitWalkEnd waits for a detached walk's run to leave running.
func awaitWalkEnd(t *testing.T, st store.Store, runID string) {
	t.Helper()
	waitFor(t, "walk "+runID+" to finish", func() bool {
		run, err := st.GetRun(context.Background(), runID)
		return err == nil && run.Status != store.RunRunning
	})
}

func TestTeamWalkRun_ResultNamesTheEndStateReached(t *testing.T) {
	for name, tc := range map[string]struct {
		reply  string
		detach bool
		want   string
	}{
		"success publishes":          {reply: "ship it", want: "published"},
		"pushback abandons":          {reply: "not good enough\nsignal: pushback:stop", want: "abandoned"},
		"detached pushback abandons": {reply: "not good enough\nsignal: pushback:stop", detach: true, want: "abandoned"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTerminalHarness(t, tc.reply)
			body := `{"op":"run","name":"two-ends","input":"draft"}`
			if tc.detach {
				body = `{"op":"run","name":"two-ends","input":"draft","mode":"detach"}`
			}
			walkID := h.postTeamDef(alicePrincipal, body)
			awaitWalkEnd(t, h.st, walkID)

			got := readWalkRun(t, h.srv, walkID)
			if got.Status != store.RunCompleted {
				t.Fatalf("walk status = %q (%s), want completed", got.Status, got.Error)
			}
			if term := resultTerminal(t, got.Result); term != tc.want {
				t.Errorf("result.terminal = %q, want %q (result %s)", term, tc.want, got.Result)
			}
			// The answer is still there beside it.
			answer, _, _ := strings.Cut(tc.reply, "\n")
			if !strings.Contains(string(got.Result), answer) {
				t.Errorf("result = %s, want the walk's final text kept", got.Result)
			}
		})
	}
}

// A walk whose member fails reached no end, so its result names none.
func TestTeamWalkRun_FailedWalkResultHasNoTerminal(t *testing.T) {
	h := newTerminalHarness(t, "ship it")
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"broken","input":"draft","mode":"detach"}`)
	awaitWalkEnd(t, h.st, walkID)

	got := readWalkRun(t, h.srv, walkID)
	if got.Status != store.RunFailed {
		t.Fatalf("walk status = %q, want failed", got.Status)
	}
	if strings.Contains(string(got.Result), `"terminal"`) {
		t.Errorf("a failed walk's result = %s, want no terminal", got.Result)
	}
}

// A cancel that lands after the walk entered its end still records the walk
// as cancelled; the result must not then claim the walk ended somewhere.
func TestTeamWalkRun_CancelledWalkResultHasNoTerminal(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	_, runID, finish, err := srv.openTeamWalkRun(substrateAdminCtx(tenantOperatorCtx("acme")), builtin.WalkRunSpec{Name: "triage", Detach: true})
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	if stopped, _, err := srv.CancelTurn(tenantOperatorCtx("acme"), runID, "operator stop"); err != nil || !stopped {
		t.Fatalf("CancelTurn = (%v, %v), want stopped", stopped, err)
	}
	finish(builtin.WalkEnd{FinalText: "half done", Terminal: "published"})

	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunCancelled {
		t.Fatalf("status = %q, want cancelled", run.Status)
	}
	if got := readResult(t, srv.store, runID); got.Terminal != "" || got.FinalText != "half done" {
		t.Errorf("cancelled walk result = %+v, want its text and no terminal", got)
	}
}

// terminal belongs to a walk: an agent run's result never carries it.
func TestRunResult_AgentRunHasNoTerminal(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	run := seedTenantRun(t, srv.store, "acme", "u1", "a_plain")
	srv.finishRun(context.Background(), run.ID, loop.RunResult{FinalText: "done", State: map[string]any{"k": "v"}}, nil, runStateMeta{})
	got, err := srv.store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(got.Result) == 0 || strings.Contains(string(got.Result), `"terminal"`) {
		t.Errorf("agent run result = %s, want a result without terminal", got.Result)
	}
}
