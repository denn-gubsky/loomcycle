package http

import (
	"context"
	"encoding/json"
	"fmt"
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

// hostProbeProvider makes one HTTP GET to the URL the run was prompted with
// and records what the tool answered. An agent whose prompt is
// "launcher:<name>" spawns <name> with the same URL instead.
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
	var system string
	if len(req.System) > 0 {
		system = req.System[0].Text
	}
	child, launcher := strings.CutPrefix(strings.TrimSpace(system), "launcher:")
	var url, answered string
	for _, m := range req.Messages {
		for _, c := range m.Content {
			switch {
			case c.Type == "tool_result":
				answered = c.Text
			case c.Type == "text" && url == "" && strings.Contains(c.Text, "https://"):
				url = c.Text[strings.Index(c.Text, "https://"):]
				url = strings.Fields(url)[0]
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
		in, _ := json.Marshal(map[string]string{"name": strings.Fields(child)[0], "prompt": "fetch " + url})
		evs = []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "a1", Name: "Agent", Input: in}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: usage},
		}
	default:
		in, _ := json.Marshal(map[string]string{"method": "GET", "url": url})
		evs = []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "h1", Name: "HTTP", Input: in}},
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

// hostRefused reports whether the HTTP tool refused a host by its allowlist,
// rather than trying it (a dial error, since no probed host resolves).
func hostRefused(res string) bool {
	return strings.Contains(res, "not in allowlist") || strings.Contains(res, "empty host allowlist")
}

type hostLockEnv struct {
	t   *testing.T
	ts  *httptest.Server
	st  store.Store
	n   int
	out *hostProbeProvider
}

// hostLockFixture is a caller-authoritative server whose operator HTTP list
// is floor, with a free and an override-locked member token of one tenant.
func hostLockFixture(t *testing.T, floor []string) *hostLockEnv {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "stub"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Env.HTTPCallerAuthoritative = true
	cfg.Agents = map[string]config.AgentDef{
		"net":          {Model: "stub-model", Tools: []string{"HTTP"}, SystemPrompt: "net"},
		"fixed-net":    {Model: "stub-model", Tools: []string{"HTTP"}, SystemPrompt: "fixed", AllowOverrides: new(bool)},
		"launch-fixed": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "launcher:fixed-net"},
		"launch-net":   {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "launcher:net"},
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
	httpTool := &builtin.HTTP{HostAllowlist: floor}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{httpTool}, concurrency.New(8, 8, 5*time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &hostLockEnv{t: t, ts: ts, st: st, out: prov}
}

// run starts agent as bearer with allowed_hosts set to hosts (a JSON array),
// prompted to fetch url, and returns the HTTP tool's answer and the run.
func (e *hostLockEnv) run(bearer, agent, hosts, url string) (string, store.Run) {
	e.t.Helper()
	e.n++
	agentID := fmt.Sprintf("a_hosts_%d", e.n)
	body := `{"agent":"` + agent + `","agent_id":"` + agentID + `","allowed_hosts":` + hosts +
		`,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"fetch ` + url + `"}]}]}`
	if code, out := bearerCall(e.t, e.ts, bearer, "POST", "/v1/runs", body); code != 200 {
		e.t.Fatalf("%s as %s: status %d: %s", agent, bearer, code, out)
	}
	run, err := e.st.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.out.last(e.t), run
}

// In caller-authoritative mode a caller's allowed_hosts replaces the
// operator's list. A caller held to its agent's own settings — by its token's
// override lock, or by the agent's allow_overrides: false — may only narrow
// it: the operator's list stays the floor, here as everywhere else.
func TestOverrideLock_CallerHostsCannotWidenTheOperatorList(t *testing.T) {
	e := hostLockFixture(t, []string{"operator.example"})
	widen := `["widen.invalid"]`
	if res, _ := e.run(lockedBearer, "net", widen, "https://widen.invalid/"); !hostRefused(res) {
		t.Errorf("a locked token widened the operator's list: the tool answered %q", res)
	}
	if res, _ := e.run(freeBearer, "fixed-net", widen, "https://widen.invalid/"); !hostRefused(res) {
		t.Errorf("a run of an allow_overrides:false agent widened the operator's list: %q", res)
	}
	res, parent := e.run(freeBearer, "launch-fixed", widen, "https://widen.invalid/")
	if !hostRefused(res) {
		t.Errorf("a spawned allow_overrides:false agent widened the operator's list: %q", res)
	}
	// What the child records is what a resume re-applies: its list applied
	// as a narrowing, not its parent's list as a replacement.
	kids, err := e.st.ListRunsByParentRunID(context.Background(), parent.ID)
	if err != nil || len(kids) != 1 {
		t.Fatalf("children of the launcher = %+v (err %v), want one", kids, err)
	}
	if !strings.Contains(string(kids[0].RunConfig), `"narrow_only":true`) {
		t.Errorf("the locked child's record does not say its list only narrows: %s", kids[0].RunConfig)
	}
	// The mode itself is unchanged for a caller nothing holds.
	if res, _ := e.run(freeBearer, "net", widen, "https://widen.invalid/"); hostRefused(res) {
		t.Errorf("an unlocked caller's list no longer replaces the operator's: %q", res)
	}
}

// For a locked caller, a list that keeps nothing of the operator's means no
// hosts — never a fall back to the operator's whole list, which would hand a
// caller that asked for one host every host the operator allows. The same
// holds for an empty list, a pattern no host matches literally, the run's own
// sub-agents, and the record a resume reads.
func TestOverrideLock_ALockedListThatKeepsNothingReachesNothing(t *testing.T) {
	e := hostLockFixture(t, []string{"operator.example"})
	floorURL := "https://operator.example/"
	for name, hosts := range map[string]string{
		"a host off the floor": `["c.invalid"]`,
		"an empty list":        `[]`,
		"a wildcard pattern":   `["*.example"]`,
	} {
		res, run := e.run(lockedBearer, "net", hosts, floorURL)
		if !hostRefused(res) {
			t.Errorf("%s: the locked run reached the operator's host: %q", name, res)
		}
		if !strings.Contains(string(run.RunConfig), `"narrow_only":true`) {
			t.Errorf("%s: the record a resume reads does not say the list only narrows: %s", name, run.RunConfig)
		}
	}
	if res, _ := e.run(lockedBearer, "launch-net", `["c.invalid"]`, floorURL); !hostRefused(res) {
		t.Errorf("a sub-agent of the locked run reached the operator's host: %q", res)
	}
	// Narrowing within the floor still reaches what it names.
	if res, _ := e.run(lockedBearer, "net", `["operator.example"]`, floorURL); hostRefused(res) {
		t.Errorf("a locked caller narrowing to a floor host was refused it: %q", res)
	}
}

// The operator's "*" floor and an empty floor are floors too.
func TestOverrideLock_ALockedListUnderAnAllowAllOrEmptyFloor(t *testing.T) {
	all := hostLockFixture(t, []string{"*"})
	if res, _ := all.run(lockedBearer, "net", `["c.invalid"]`, "https://operator.example/"); !hostRefused(res) {
		t.Errorf(`under a "*" floor, a locked caller's list did not narrow: %q`, res)
	}
	none := hostLockFixture(t, nil)
	if res, _ := none.run(lockedBearer, "net", `["operator.example"]`, "https://operator.example/"); !hostRefused(res) {
		t.Errorf("under an empty floor, a locked caller's list opened a host: %q", res)
	}
}
