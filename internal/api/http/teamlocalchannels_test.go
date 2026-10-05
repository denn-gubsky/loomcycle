package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// These tests run real walks over a real store and read what the store holds:
// under which NAME, in which KEYSPACE, a team's own channel's messages land.

// chanProvider answers "I am <first line of the system prompt>". A system
// prompt line "CHAN:<json>" makes the agent call the Channel tool once with
// that input first; every tool result it is then handed is recorded.
type chanProvider struct {
	mu      sync.Mutex
	results []providers.ContentBlock
}

func (p *chanProvider) ID() string                  { return "stub" }
func (p *chanProvider) Probe(context.Context) error { return nil }
func (p *chanProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *chanProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *chanProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	sys := systemText(req)
	answered := false
	if n := len(req.Messages); n > 0 {
		for _, c := range req.Messages[n-1].Content {
			if c.Type == "tool_result" {
				answered = true
				p.mu.Lock()
				p.results = append(p.results, c)
				p.mu.Unlock()
			}
		}
	}
	directive := ""
	for _, line := range strings.Split(sys, "\n") {
		if in, ok := strings.CutPrefix(line, "CHAN:"); ok {
			directive = in
		}
	}
	ch := make(chan providers.Event, 2)
	if directive != "" && !answered {
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_chan", Name: "Channel", Input: json.RawMessage(directive)}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	} else {
		first, _, _ := strings.Cut(sys, "\n")
		ch <- providers.Event{Type: providers.EventText, Text: "I am " + first}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	}
	close(ch)
	return ch, nil
}

// lastResult is the most recent Channel tool result an agent was handed.
func (p *chanProvider) lastResult(t *testing.T) providers.ContentBlock {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.results) == 0 {
		t.Fatal("no agent was handed a tool result")
	}
	return p.results[len(p.results)-1]
}

type channelHarness struct {
	t    *testing.T
	srv  *Server
	st   store.Store
	prov *chanProvider
	pub  *channels.StorePublisher
}

// newChannelHarness serves the global agents given (plus "reviewer"), with the
// Channel tool, the channel writer (holds resolved by the server, as main.go
// wires it) and the TeamDef tool.
func newChannelHarness(t *testing.T, agents map[string]config.AgentDef) *channelHarness {
	t.Helper()
	all := map[string]config.AgentDef{"reviewer": {Model: "stub-model", SystemPrompt: "GLOBAL reviewer"}}
	for k, v := range agents {
		all[k] = v
	}
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      all,
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
		Env:         config.Env{ChannelsMaxValueBytes: 64 * 1024, ChannelsLongPollCapMS: 200},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "lc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &chanProvider{}
	bus := channels.NewBus()
	chTool := &builtin.Channel{Store: st, Bus: bus, MaxValueBytes: 64 * 1024}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{chTool}, concurrency.New(8, 8, 5*time.Second), st)
	pub := &channels.StorePublisher{Store: st, Bus: bus, Defs: srv.ChannelWriteDef}
	srv.SetSystemPublisher(pub)
	chTool.Writer = pub
	srv.SetChannelBus(bus)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	return &channelHarness{t: t, srv: srv, st: st, prov: prov, pub: pub}
}

// seed writes one promoted version of a team in tenant acme.
func (h *channelHarness) seed(defID, name, defJSON string) store.TeamScope {
	h.t.Helper()
	def, err := teamgraph.Parse([]byte(defJSON))
	if err != nil {
		h.t.Fatalf("parse fixture: %v", err)
	}
	ctx := context.Background()
	row, err := h.st.TeamDefCreate(ctx, store.TeamDefRow{
		DefID: defID, Name: name, TenantID: "acme", Definition: json.RawMessage(defJSON), ContentSHA256: teamgraph.Sign(name, def),
	})
	if err != nil {
		h.t.Fatalf("seed %s: %v", name, err)
	}
	if err := h.st.TeamDefSetActive(ctx, "acme", name, row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
		h.t.Fatalf("promote %s: %v", name, err)
	}
	return store.TeamScope{Tenant: "acme", Team: name, DefID: row.DefID}
}

func acmeUser(subject string) func(context.Context) context.Context {
	return func(ctx context.Context) context.Context {
		return auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: subject, Scopes: []string{auth.ScopeTenant}})
	}
}

// teamDef posts one TeamDef op as `as` and returns the reply.
func (h *channelHarness) teamDef(as func(context.Context) context.Context, body string) (int, map[string]any) {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/_teamdef", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(as(r.Context()))
	rr := httptest.NewRecorder()
	h.srv.handleSubstrateTeamDef(rr, r)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out == nil {
		out = map[string]any{"raw": rr.Body.String()}
	}
	return rr.Code, out
}

func (h *channelHarness) walk(as func(context.Context) context.Context, name, input string) map[string]any {
	h.t.Helper()
	in, _ := json.Marshal(input)
	code, out := h.teamDef(as, `{"op":"run","name":"`+name+`","input":`+string(in)+`}`)
	if code != http.StatusOK || out["status"] != "completed" {
		h.t.Fatalf("walk %s: HTTP %d %v", name, code, out)
	}
	return out
}

// stored peeks a stored channel name in tenant acme.
func (h *channelHarness) stored(name string, scope store.MemoryScope, scopeID string) []store.ChannelMessage {
	h.t.Helper()
	msgs, err := h.st.ChannelPeek(context.Background(), "acme", name, scope, scopeID, "", 100)
	if err != nil {
		h.t.Fatalf("peek %s: %v", name, err)
	}
	return msgs
}

// starterTeam reads its own `events` (tenant) and sinks each run's result to
// its own `verdicts` (user).
const starterTeam = `{"entry":"wave",
  "local":{"channels":{"events":{"scope":"tenant"},"verdicts":{"scope":"user"}}},
  "states":[{"state":"wave","handler":{"kind":"starter","source":{"channel":"./events"},
    "fanout":{"agent":"reviewer","per":"message","max":4},"sink":{"channel":"./verdicts"}}},
    {"state":"done","handler":{"kind":"terminal"}}],
  "transitions":[{"from":"wave","to":"done","on":"success"}]}`

// intakeTeam publishes the walk's input to its own `events`, whose body is given.
func intakeTeam(eventsBody string) string {
	return `{"entry":"form","local":{"channels":{"events":` + eventsBody + `}},
	  "states":[{"state":"form","handler":{"kind":"input","publish":{"channel":"./events"}}},
	    {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"form","to":"done","on":"success"}]}`
}

func TestTeamWalk_StarterReadsAndSinksItsOwnChannels(t *testing.T) {
	h := newChannelHarness(t, nil)
	sc := h.seed("tdf_triage_1", "triage", starterTeam)

	// Fed from outside a walk, the way a schedule or webhook will feed one.
	if _, err := h.srv.publishTeamLocalChannel(context.Background(), "acme", sc, "events", "alice", json.RawMessage(`{"pr":7}`)); err != nil {
		t.Fatalf("publish into the team's own channel: %v", err)
	}
	if got := h.stored("_team/triage/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Fatalf("the event is stored under _team/triage/events: %d message(s)", len(got))
	}
	h.walk(acmeUser("alice"), "triage", "go")

	verdicts := h.stored("_team/triage/verdicts", store.MemoryScopeUser, "alice")
	if len(verdicts) != 1 || !strings.Contains(string(verdicts[0].Payload), "GLOBAL reviewer") {
		t.Fatalf("the sink's result is stored under _team/triage/verdicts for alice: %+v", verdicts)
	}
	// The walk acked what it read: the next walk finds no work.
	cur, err := h.st.ChannelCommittedCursor(context.Background(), "acme", "_team/triage/events", store.MemoryScopeTenant, "")
	if err != nil || cur == "" {
		t.Errorf("the starter's cursor on its own channel was not committed: %q %v", cur, err)
	}
	// Nothing landed under the spelling the definition used.
	for _, name := range []string{"./events", "./verdicts", "events", "verdicts", "triage/verdicts"} {
		if got, _ := h.st.ChannelPeek(context.Background(), "acme", name, store.MemoryScopeUser, "alice", "", 10); len(got) != 0 {
			t.Errorf("a message landed under %q", name)
		}
	}
}

// A tenant-scoped channel of a team is one keyspace for every walk of it in
// the tenant; a user-scoped one is one per user.
func TestTeamWalk_OwnChannelKeyspaceFollowsItsScope(t *testing.T) {
	h := newChannelHarness(t, nil)
	h.seed("tdf_shared_1", "shared", intakeTeam(`{"scope":"tenant"}`))
	h.seed("tdf_mine_1", "mine", intakeTeam(`{"scope":"user"}`))

	for _, who := range []string{"alice", "bob"} {
		h.walk(acmeUser(who), "shared", who)
		h.walk(acmeUser(who), "mine", who)
	}
	if got := h.stored("_team/shared/events", store.MemoryScopeTenant, ""); len(got) != 2 {
		t.Errorf("two walks of a tenant-scoped channel share it: %d message(s), want 2", len(got))
	}
	for _, who := range []string{"alice", "bob"} {
		got := h.stored("_team/mine/events", store.MemoryScopeUser, who)
		if len(got) != 1 || !strings.Contains(string(got[0].Payload), who) {
			t.Errorf("a user-scoped channel keys on %s: %+v", who, got)
		}
	}
}

// A hold on a team's own channel is honoured by the one writer every message
// goes through.
func TestTeamWalk_OwnChannelHoldIsHonoured(t *testing.T) {
	h := newChannelHarness(t, nil)
	h.seed("tdf_held_1", "held", intakeTeam(`{"scope":"tenant","hold":true}`))
	h.walk(acmeUser("alice"), "held", "x")

	if got := h.stored("_team/held/events", store.MemoryScopeTenant, ""); len(got) != 0 {
		t.Errorf("a held message was delivered: %+v", got)
	}
	stats, err := h.st.ChannelStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var held int64
	for _, s := range stats {
		if s.Channel == "_team/held/events" {
			held += s.Held
		}
	}
	if held != 1 {
		t.Errorf("held count on _team/held/events = %d, want 1", held)
	}
}

// A write to a team's own channel is decided from the version the writer acts
// under, and REFUSED — not written as-is, which would deliver what the
// channel's definition holds — whenever that version cannot say.
func TestChannelWriter_TeamChannelWriteFailsClosed(t *testing.T) {
	h := newChannelHarness(t, nil)
	sc := h.seed("tdf_held_1", "held", intakeTeam(`{"scope":"tenant","hold":true}`))
	write := func(ctx context.Context, tenant, name string) error {
		_, err := h.pub.Write(ctx, channels.WriteRequest{
			Channel: name, TenantID: tenant, Scope: store.MemoryScopeTenant, Payload: json.RawMessage(`{}`),
		})
		return err
	}
	in := func(sc store.TeamScope) context.Context { return store.WithTeamScope(context.Background(), sc) }
	gone := sc
	gone.DefID = "tdf_deleted"
	for name, c := range map[string]struct {
		ctx            context.Context
		tenant, stored string
	}{
		"no team scope (and an active version that holds)": {context.Background(), "acme", "_team/held/events"},
		"another team's scope":                             {in(store.TeamScope{Tenant: "acme", Team: "other", DefID: sc.DefID}), "acme", "_team/held/events"},
		"another tenant":                                   {in(sc), "other", "_team/held/events"},
		"the writer's version is gone":                     {in(gone), "acme", "_team/held/events"},
		"the version does not declare the channel":         {in(sc), "acme", "_team/held/nosuch"},
		"a name that is not a team channel's":              {in(sc), "acme", "_team/held"},
	} {
		if err := write(c.ctx, c.tenant, c.stored); err == nil {
			t.Errorf("%s: the write was accepted", name)
		}
	}
	stats, err := h.st.ChannelStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range stats {
		if store.IsTeamChannelName(st.Channel) {
			t.Errorf("a refused write left %d message(s) on %s (tenant %q)", st.MessageCount+st.Held, st.Channel, st.TenantID)
		}
	}
	// Inside the version that declares it, the write holds.
	if err := write(in(sc), "acme", "_team/held/events"); err != nil {
		t.Fatalf("a write from inside the team: %v", err)
	}
}

// A walk of another tenant's team (an admin's, by def_id) runs in the
// caller's tenant; the team's own channels are its tenant's and are refused.
func TestTeamLocalChannelTarget_RefusesAnotherTenantsWalk(t *testing.T) {
	h := newChannelHarness(t, nil)
	sc := h.seed("tdf_triage_1", "triage", starterTeam)
	def, _ := teamgraph.Parse([]byte(starterTeam))
	if _, _, _, _, err := h.srv.teamLocalChannelTarget(def, sc, "admin-tenant", "events", "root"); err == nil {
		t.Fatal("a walk in another tenant reached the team's own channel")
	}
	if _, _, _, _, err := h.srv.teamLocalChannelTarget(def, store.TeamScope{}, "acme", "events", "alice"); err == nil {
		t.Fatal("no team scope reached a team's own channel")
	}
}
