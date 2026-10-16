package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// hostProbeProvider asks for one HTTP GET to a host outside the operator's
// list and records what the tool answered. An agent whose prompt says
// "launcher" spawns the definition-locked agent instead.
type hostProbeProvider struct {
	mu      sync.Mutex
	results []string
}

func (p *hostProbeProvider) ID() string                  { return "stub" }
func (p *hostProbeProvider) Probe(context.Context) error { return nil }
func (p *hostProbeProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *hostProbeProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *hostProbeProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	launcher := len(req.System) > 0 && strings.Contains(req.System[0].Text, "launcher")
	var answered string
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.Type == "tool_result" {
				answered = c.Text
			}
		}
	}
	usage := &providers.Usage{InputTokens: 1, OutputTokens: 1}
	var evs []providers.Event
	switch {
	case answered != "":
		if !launcher {
			p.mu.Lock()
			p.results = append(p.results, answered)
			p.mu.Unlock()
		}
		evs = []providers.Event{{Type: providers.EventText, Text: "done"}, {Type: providers.EventDone, StopReason: "end_turn", Usage: usage}}
	case launcher:
		evs = []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "a1", Name: "Agent", Input: json.RawMessage(`{"name":"fixed-net","prompt":"go"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: usage},
		}
	default:
		evs = []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "h1", Name: "HTTP", Input: json.RawMessage(`{"method":"GET","url":"https://widen.invalid/"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: usage},
		}
	}
	ch := make(chan providers.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (p *hostProbeProvider) last(t *testing.T) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.results) == 0 {
		t.Fatal("the HTTP tool was never called")
	}
	return p.results[len(p.results)-1]
}

// In caller-authoritative mode a caller's allowed_hosts replaces the
// operator's list. A caller held to its agent's own settings — by its token's
// override lock, or by the agent's allow_overrides: false — may only narrow
// it: the operator's list stays the floor, here as everywhere else.
func TestOverrideLock_CallerHostsCannotWidenTheOperatorList(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "stub"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Env.HTTPCallerAuthoritative = true
	cfg.Agents = map[string]config.AgentDef{
		"net":       {Model: "stub-model", Tools: []string{"HTTP"}, SystemPrompt: "net"},
		"fixed-net": {Model: "stub-model", Tools: []string{"HTTP"}, SystemPrompt: "fixed", AllowOverrides: new(bool)},
		"launcher":  {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "launcher"},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "hosts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	member := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}
	for _, tok := range []struct{ id, bearer, restrictions string }{
		{"def_free", freeBearer, ""},
		{"def_locked", lockedBearer, `{"overrides":"none"}`},
	} {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: tok.id, Name: "u-" + tok.id, TenantID: "acme", Subject: "dev",
			TokenHash: auth.HashToken(testTokenPepper, tok.bearer), AllowedScopes: member,
			RestrictionsJSON: tok.restrictions,
		}); err != nil {
			t.Fatal(err)
		}
	}
	prov := &hostProbeProvider{}
	httpTool := &builtin.HTTP{HostAllowlist: []string{"operator.example"}}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{httpTool}, concurrency.New(8, 8, 5*time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	run := func(bearer, agent string) string {
		t.Helper()
		body := `{"agent":"` + agent + `","agent_id":"a_` + agent + `","allowed_hosts":["widen.invalid"],"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
		if code, out := bearerCall(t, ts, bearer, "POST", "/v1/runs", body); code != 200 {
			t.Fatalf("%s as %s: status %d: %s", agent, bearer, code, out)
		}
		return prov.last(t)
	}
	refused := func(res string) bool {
		return strings.Contains(res, "not in allowlist") || strings.Contains(res, "empty host allowlist")
	}
	if res := run(lockedBearer, "net"); !refused(res) {
		t.Errorf("a locked token widened the operator's list: the tool answered %q", res)
	}
	if res := run(freeBearer, "fixed-net"); !refused(res) {
		t.Errorf("a run of an allow_overrides:false agent widened the operator's list: %q", res)
	}
	if res := run(freeBearer, "launcher"); !refused(res) {
		t.Errorf("a spawned allow_overrides:false agent widened the operator's list: %q", res)
	}
	// What the child records is what a resume re-applies: the clipped list,
	// not its parent's.
	parent, err := st.GetRunByAgentID(context.Background(), "a_launcher")
	if err != nil {
		t.Fatal(err)
	}
	kids, err := st.ListRunsByParentRunID(context.Background(), parent.ID)
	if err != nil || len(kids) != 1 {
		t.Fatalf("children of the launcher = %+v (err %v), want one", kids, err)
	}
	var rec runConfigRecord
	if err := json.Unmarshal(kids[0].RunConfig, &rec); err != nil {
		t.Fatal(err)
	}
	for _, h := range rec.callerHosts() {
		if h == "widen.invalid" {
			t.Errorf("the locked child recorded its parent's wider list %v: a resume would re-apply it", rec.callerHosts())
		}
	}
	// The mode itself is unchanged for a caller nothing holds.
	if res := run(freeBearer, "net"); refused(res) {
		t.Errorf("an unlocked caller's list no longer replaces the operator's: %q", res)
	}
}
