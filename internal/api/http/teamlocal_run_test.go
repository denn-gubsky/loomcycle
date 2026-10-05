package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// These tests run real walks and read what happened off the run rows and the
// provider's requests: which DEFINITION answered under which NAME.

// whoProvider answers "I am <first line of the system prompt>", so a test can
// tell which definition ran. A system prompt carrying a directive line makes
// the agent call the Agent tool once first:
//
//	SPAWN:<name>            op=spawn
//	PIN:<name>:<def_id>     op=spawn with a def_id
//	PSPAWN:<a>,<b>,...      op=parallel_spawn
//
// It does so once per user turn, so a resumed or continued agent spawns again.
type whoProvider struct {
	mu   sync.Mutex
	reqs []providers.Request
}

func (p *whoProvider) ID() string                  { return "stub" }
func (p *whoProvider) Probe(context.Context) error { return nil }
func (p *whoProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *whoProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *whoProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	sys := systemText(req)
	// "Answered" is about the LAST message: an agent resumed or continued
	// with a new user turn follows its directive again.
	answered := false
	if n := len(req.Messages); n > 0 {
		for _, c := range req.Messages[n-1].Content {
			answered = answered || c.Type == "tool_result"
		}
	}
	ch := make(chan providers.Event, 2)
	if input := spawnDirective(sys); input != "" && !answered {
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_spawn", Name: "Agent", Input: json.RawMessage(input)}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	} else {
		first, _, _ := strings.Cut(sys, "\n")
		ch <- providers.Event{Type: providers.EventText, Text: "I am " + first}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	}
	close(ch)
	return ch, nil
}

func spawnDirective(sys string) string {
	for _, line := range strings.Split(sys, "\n") {
		if name, ok := strings.CutPrefix(line, "SPAWN:"); ok {
			b, _ := json.Marshal(map[string]any{"name": name, "prompt": "go"})
			return string(b)
		}
		if rest, ok := strings.CutPrefix(line, "PIN:"); ok {
			name, defID, _ := strings.Cut(rest, ":")
			b, _ := json.Marshal(map[string]any{"name": name, "prompt": "go", "def_id": defID})
			return string(b)
		}
		if names, ok := strings.CutPrefix(line, "PSPAWN:"); ok {
			var spawns []map[string]any
			for _, n := range strings.Split(names, ",") {
				spawns = append(spawns, map[string]any{"name": n, "prompt": "go"})
			}
			b, _ := json.Marshal(map[string]any{"op": "parallel_spawn", "spawns": spawns})
			return string(b)
		}
	}
	return ""
}

// forget drops the calls seen so far, so a test can ask what ran AFTER a point.
func (p *whoProvider) forget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = nil
}

// offeredTo returns the tools offered on the first call whose system prompt
// starts with first.
func (p *whoProvider) offeredTo(first string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.reqs {
		if strings.HasPrefix(systemText(r), first) {
			return offered(r)
		}
	}
	return nil
}

// sawSystem reports whether any model call carried a system prompt starting
// with first.
func (p *whoProvider) sawSystem(first string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.reqs {
		if strings.HasPrefix(systemText(r), first) {
			return true
		}
	}
	return false
}

type localHarness struct {
	t    *testing.T
	srv  *Server
	st   *teamFaultStore
	prov *whoProvider
	cfg  *config.Config
}

// newLocalHarness serves three GLOBAL agents — reviewer, helper, and scout,
// which spawns "reviewer" by its bare name — with the TeamDef tool wired.
func newLocalHarness(t *testing.T) *localHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"reviewer": {Model: "stub-model", SystemPrompt: "GLOBAL reviewer"},
			"helper":   {Model: "stub-model", SystemPrompt: "GLOBAL helper"},
			"scout":    {Model: "stub-model", SystemPrompt: "GLOBAL scout\nSPAWN:reviewer", Tools: []string{"Agent"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &whoProvider{}
	h := &localHarness{t: t, st: &teamFaultStore{Store: st}, prov: prov, cfg: cfg}
	h.srv = h.newServer()
	return h
}

// teamFaultStore is a store whose reads of team versions can be made to fail.
type teamFaultStore struct {
	store.Store
	faulty atomic.Bool
}

func (f *teamFaultStore) TeamDefGet(ctx context.Context, defID string) (store.TeamDefRow, error) {
	if f.faulty.Load() {
		return store.TeamDefRow{}, errors.New("database unavailable")
	}
	return f.Store.TeamDefGet(ctx, defID)
}

// newServer is one instance over the harness's store. A second one is another
// instance that knows a run only from what the store holds.
func (h *localHarness) newServer() *Server {
	srv := New(h.cfg, &stubResolver{p: h.prov}, []tools.Tool{}, concurrency.New(8, 8, 5*time.Second), h.st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	srv.SetTeamDefTool(&builtin.TeamDef{Store: h.st})
	return srv
}

// teamWith is a one-state team whose state runs `agent`, declaring the given
// local agents (name → system prompt; every one may use the Agent tool).
func teamWith(agent string, locals map[string]string) string {
	bodies := map[string]any{}
	for name, prompt := range locals {
		bodies[name] = map[string]any{"model": "stub-model", "system_prompt": prompt, "tools": []string{"Agent"}}
	}
	def := map[string]any{
		"entry": "work",
		"states": []any{
			map[string]any{"state": "work", "handler": map[string]any{"kind": "agent", "agent": agent}},
			map[string]any{"state": "done", "handler": map[string]any{"kind": "terminal"}},
		},
		"transitions": []any{map[string]any{"from": "work", "to": "done", "on": "success"}},
	}
	if locals != nil {
		def["local"] = map[string]any{"agents": bodies}
	}
	b, _ := json.Marshal(def)
	return string(b)
}

// seedTeamVersion writes one version of a team in tenant acme and returns its
// def id; promote makes it the active one.
func (h *localHarness) seedTeamVersion(defID, name, defJSON string, promote bool) string {
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
	if promote {
		if err := h.st.TeamDefSetActive(ctx, "acme", name, row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
			h.t.Fatalf("promote %s: %v", name, err)
		}
	}
	return row.DefID
}

// teamDef posts one TeamDef op as alice (tenant acme) and returns the reply.
func (h *localHarness) teamDef(body string) (int, map[string]any) {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/_teamdef", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(alicePrincipal(r.Context()))
	rr := httptest.NewRecorder()
	h.srv.handleSubstrateTeamDef(rr, r)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out == nil {
		out = map[string]any{"raw": rr.Body.String()}
	}
	return rr.Code, out
}

// walk runs a team to its end and returns the reply.
func (h *localHarness) walk(name string) map[string]any {
	h.t.Helper()
	code, out := h.teamDef(`{"op":"run","name":"` + name + `","input":"go"}`)
	if code != http.StatusOK || out["status"] != "completed" {
		h.t.Fatalf("walk %s: HTTP %d %v", name, code, out)
	}
	return out
}

// runsByAgent returns alice's runs keyed by the agent name on the row.
func (h *localHarness) runsByAgent() map[string]store.Run {
	h.t.Helper()
	runs, err := h.st.ListActiveRunsByUser(context.Background(), "", "alice", "")
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]store.Run{}
	for _, r := range runs {
		out[r.Agent] = r
	}
	return out
}

func teamScopeOf(t *testing.T, run store.Run) *teamScopeRecord {
	t.Helper()
	rec, _ := decodeRunConfig(run.RunConfig)
	return rec.TeamScope
}

func TestTeamWalk_MemberRunsTheTeamsOwnAgentUnderItsFullName(t *testing.T) {
	h := newLocalHarness(t)
	defID := h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./reviewer", map[string]string{"reviewer": "LOCAL reviewer"}), true)

	out := h.walk("sdlc")

	run, ok := h.runsByAgent()["sdlc/reviewer"]
	if !ok {
		t.Fatalf("no run row for sdlc/reviewer; rows: %v", h.runsByAgent())
	}
	if !h.prov.sawSystem("LOCAL reviewer") || h.prov.sawSystem("GLOBAL reviewer") {
		t.Error("the member must run the team's own reviewer, not the global agent of that name")
	}
	sess, err := h.st.GetSession(context.Background(), run.SessionID)
	if err != nil || sess.Agent != "sdlc/reviewer" {
		t.Errorf("session agent = %q (err %v), want sdlc/reviewer", sess.Agent, err)
	}
	if sc := teamScopeOf(t, run); sc == nil || sc.Team != "sdlc" || sc.DefID != defID || sc.DefTenant != "acme" {
		t.Errorf("the member's recorded team scope = %+v, want team sdlc version %s in acme", sc, defID)
	}
	rec, _ := decodeRunConfig(run.RunConfig)
	if rec.AgentVersion == nil || rec.AgentVersion.TeamDefID != defID || rec.AgentVersion.DefID != "" {
		t.Errorf("recorded agent version = %+v, want the team version and no AgentDef version", rec.AgentVersion)
	}
	steps, _ := out["steps"].([]any)
	if len(steps) == 0 || steps[0].(map[string]any)["agent"] != "sdlc/reviewer" {
		t.Errorf("the walk's step names the member %v, want sdlc/reviewer", steps)
	}
	if out["final_output"] != "I am LOCAL reviewer" {
		t.Errorf("final_output = %v", out["final_output"])
	}
	// It exists nowhere but in the team.
	ctx := context.Background()
	for _, name := range []string{"reviewer", "sdlc/reviewer"} {
		if _, err := h.st.AgentDefGetActive(ctx, "acme", name); err == nil {
			t.Errorf("running a team's own agent wrote an AgentDef named %q", name)
		}
		if _, err := h.st.DynamicAgentGet(ctx, "acme", name); err == nil {
			t.Errorf("running a team's own agent registered an agent named %q", name)
		}
	}
}

// A local agent starts another local agent ("./helper"), a global one
// ("scout"), and — by a bare name the team also declares — a local one that
// shadows the global agent of that name. scout, a GLOBAL agent now running
// inside the team, spawns "reviewer" by its bare name and gets the team's.
func TestTeamWalk_SpawnTreeResolvesNamesInsideTheTeam(t *testing.T) {
	h := newLocalHarness(t)
	defID := h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./lead", map[string]string{
		"lead":     "LOCAL lead\nPSPAWN:./helper,scout,sdlc/helper",
		"helper":   "LOCAL helper",
		"reviewer": "LOCAL reviewer",
	}), true)

	h.walk("sdlc")

	runs := h.runsByAgent()
	for _, agent := range []string{"sdlc/lead", "sdlc/helper", "scout", "sdlc/reviewer"} {
		run, ok := runs[agent]
		if !ok {
			t.Errorf("no run row for %s", agent)
			continue
		}
		// The scope reaches every run below the walk, the global one included.
		if sc := teamScopeOf(t, run); sc == nil || sc.DefID != defID {
			t.Errorf("%s: recorded team scope = %+v, want version %s", agent, sc, defID)
		}
	}
	for _, wrong := range []string{"helper", "reviewer", "./helper"} {
		if _, ok := runs[wrong]; ok {
			t.Errorf("a run row is named %q; a team's own agent runs as <team>/<name>", wrong)
		}
	}
	if h.prov.sawSystem("GLOBAL helper") || h.prov.sawSystem("GLOBAL reviewer") {
		t.Error("inside the team a name the team declares must run the team's agent, never the global one")
	}
	if !h.prov.sawSystem("LOCAL helper") || !h.prov.sawSystem("LOCAL reviewer") || !h.prov.sawSystem("GLOBAL scout") {
		t.Error("expected the team's helper and reviewer and the global scout to have run")
	}
	// A team's own agent is offered what a global agent with the same `tools`
	// is: its definition's tools, decided when it was authored, and neither
	// widened nor narrowed by who runs the team.
	if local, global := h.prov.offeredTo("LOCAL lead"), h.prov.offeredTo("GLOBAL scout"); len(local) == 0 || strings.Join(local, ",") != strings.Join(global, ",") {
		t.Errorf("tools offered to the team's lead %v differ from those offered to the global scout %v; both declare tools: [Agent]", local, global)
	}
	// What the Agent tool hands back names each child the way its row does.
	lead := runs["sdlc/lead"]
	envelope := runTranscriptText(t, h.st, lead.SessionID, lead.ID)
	for _, want := range []string{`sdlc/helper`, `scout`} {
		if !strings.Contains(envelope, `\"agent\":\"`+want+`\"`) {
			t.Errorf("the Agent tool's result does not name %q:\n%s", want, envelope)
		}
	}
	if strings.Contains(envelope, `\"agent\":\"./helper\"`) {
		t.Errorf("the Agent tool's result names a child by the spelling it was called with:\n%s", envelope)
	}
}

// The same global agent, run OUTSIDE the team, gets the global reviewer: the
// team's own agents shadow nothing out there.
func TestRun_OutsideATeamBareNameIsTheGlobalAgent(t *testing.T) {
	h := newLocalHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./reviewer", map[string]string{"reviewer": "LOCAL reviewer"}), true)

	if err := h.runOnce("scout", ""); err != nil {
		t.Fatalf("run scout: %v", err)
	}
	if !h.prov.sawSystem("GLOBAL reviewer") || h.prov.sawSystem("LOCAL reviewer") {
		t.Error("outside a team, scout's \"reviewer\" must be the global agent")
	}
	runs := h.runsByAgent()
	if sc := teamScopeOf(t, runs["scout"]); sc != nil {
		t.Errorf("a top-level run recorded a team scope: %+v", sc)
	}
	if sc := teamScopeOf(t, runs["reviewer"]); sc != nil {
		t.Errorf("its child recorded a team scope: %+v", sc)
	}
}

// runOnce starts a top-level run of agent as alice, or continues sessionID.
func (h *localHarness) runOnce(agent, sessionID string) error {
	return h.srv.RunOnce(alicePrincipal(context.Background()), runner.RunInput{
		Agent: agent, SessionID: sessionID,
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	}, runner.RunCallbacks{})
}

// No spelling reaches a team's own agent from outside a walk of that team.
func TestRun_TeamsOwnAgentIsUnknownOutsideItsTeam(t *testing.T) {
	h := newLocalHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./solo", map[string]string{"solo": "LOCAL solo"}), true)

	for _, name := range []string{"sdlc/solo", "./solo", "solo"} {
		err := h.runOnce(name, "")
		if !errors.Is(err, runner.ErrUnknownAgent) {
			t.Errorf("RunOnce(%q) = %v, want unknown agent", name, err)
		}
		body := `{"agent":"` + name + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
		r = r.WithContext(alicePrincipal(r.Context()))
		rr := httptest.NewRecorder()
		h.srv.handleRuns(rr, r)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown agent") {
			t.Errorf("POST /v1/runs agent=%q = %d %s, want 400 unknown agent", name, rr.Code, rr.Body.String())
		}
	}
	if h.prov.sawSystem("LOCAL solo") {
		t.Error("the team's own agent ran outside its team")
	}
	if _, ok := h.srv.lookupAgent(alicePrincipal(context.Background()), "acme", "sdlc/solo"); ok {
		t.Error("the name every trigger resolves through found a team's own agent with no team scope")
	}
}

func TestListSurfaces_NeverShowATeamsOwnAgent(t *testing.T) {
	h := newLocalHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./solo", map[string]string{"solo": "LOCAL solo"}), true)
	h.walk("sdlc") // it has run, so anything derived from runs has seen it

	ctx := alicePrincipal(context.Background())
	agents, err := h.srv.ListAgents(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if strings.Contains(a.Name, "solo") {
			t.Errorf("list_agents shows %q", a.Name)
		}
	}
	for path, handler := range map[string]http.HandlerFunc{
		"/v1/_runnable-agents": h.srv.handleRunnableAgents,
		"/v1/_agentdef/names":  h.srv.handleListAgentDefNames,
		"/v1/_library/agents":  h.srv.handleListLibraryAgents,
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		handler(rr, r)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d %s", path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "solo") {
			t.Errorf("GET %s shows a team's own agent: %s", path, rr.Body.String())
		}
	}
}

func TestTeamWalk_DefIDPinIsRefusedForATeamsOwnAgent(t *testing.T) {
	h := newLocalHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./lead", map[string]string{
		"lead": "LOCAL lead\nPIN:./helper:def_anything", "helper": "LOCAL helper",
	}), true)

	h.walk("sdlc")

	if h.prov.sawSystem("LOCAL helper") {
		t.Error("a team's own agent ran under a def_id pin")
	}
	lead := h.runsByAgent()["sdlc/lead"]
	if text := runTranscriptText(t, h.st, lead.SessionID, lead.ID); !strings.Contains(text, "def_id does not apply") {
		t.Errorf("the Agent tool should have refused the pin, saying why:\n%s", text)
	}
}

func TestTeamWalk_UndeclaredLocalNameIsRefusedByTheAgentTool(t *testing.T) {
	h := newLocalHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./lead", map[string]string{"lead": "LOCAL lead\nSPAWN:./ghost"}), true)
	h.walk("sdlc")
	lead := h.runsByAgent()["sdlc/lead"]
	if text := runTranscriptText(t, h.st, lead.SessionID, lead.ID); !strings.Contains(text, "declares no agent of its own named") {
		t.Errorf("want the Agent tool to say the team declares no such agent:\n%s", text)
	}
}

// A stored body that never went through create/fork may name an agent it does
// not declare. Qualified, that would be an ordinary name and run a global
// agent called "<team>/x" — so the walk refuses before anything starts.
func TestTeamWalk_RefusesStoredDefinitionWithUndeclaredLocalRef(t *testing.T) {
	h := newLocalHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./ghost", nil), true)
	if err := h.st.DynamicAgentUpsert(context.Background(), store.DynamicAgent{
		Name: "sdlc/ghost", TenantID: "acme", Definition: json.RawMessage(`{"Model":"stub-model","SystemPrompt":"GLOBAL ghost"}`),
	}); err != nil {
		t.Fatal(err)
	}
	_, out := h.teamDef(`{"op":"run","name":"sdlc","input":"go"}`)
	if !strings.Contains(fmtAny(out), "does not declare") {
		t.Errorf("want a refusal naming the undeclared agent, got %v", out)
	}
	if h.prov.sawSystem("GLOBAL ghost") {
		t.Error("an undeclared \"./ghost\" ran the global agent sdlc/ghost")
	}
}

func fmtAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The authoring checks are made once and cannot see everything (a static agent
// added later, restored rows, two writers). If a global agent of the same full
// name exists when the team's own agent is about to run, it does not run: the
// two would share agent-scoped memory and channel cursors.
func TestTeamWalk_RefusesItsOwnAgentWhileAGlobalAgentHasItsName(t *testing.T) {
	h := newLocalHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./solo", map[string]string{"solo": "LOCAL solo"}), true)
	if err := h.st.DynamicAgentUpsert(context.Background(), store.DynamicAgent{
		Name: "sdlc/solo", TenantID: "acme", Definition: json.RawMessage(`{"Model":"stub-model","SystemPrompt":"GLOBAL solo"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if n := h.srv.LogTeamLocalNameClashes(context.Background()); n != 1 {
		t.Errorf("the boot report found %d clashes, want 1", n)
	}
	_, out := h.teamDef(`{"op":"run","name":"sdlc","input":"go"}`)
	if !strings.Contains(fmtAny(out), "would share") {
		t.Errorf("want the walk to fail saying the two agents would share state, got %v", out)
	}
	if h.prov.sawSystem("LOCAL solo") || h.prov.sawSystem("GLOBAL solo") {
		t.Error("an agent ran under a name two definitions hold")
	}
}

// ---- pause / resume / continuation ----

// leadTeam is version N of a team whose lead starts "./helper"; each version's
// agents say which version they are.
func leadTeam(version string) string {
	return teamWith("./lead", map[string]string{
		"lead":   "LOCAL lead " + version + "\nSPAWN:./helper",
		"helper": "LOCAL helper " + version,
	})
}

// A run of a team's own agent resumes on the team VERSION it started under:
// the agent itself, and the team's agents it goes on to start. What the team
// has been forked or promoted to since does not reach it — including a version
// that no longer declares those agents at all.
func TestResumedRun_ReadsTheTeamsOwnAgentsFromTheRecordedTeamVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		v2   string
	}{
		{"a version that changes the agents was promoted", leadTeam("v2")},
		{"a version that removes the agents was promoted", teamWith("scout", nil)},
	} {
		for _, where := range []string{"same instance", "another instance, from the store"} {
			t.Run(tc.name+"/"+where, func(t *testing.T) {
				h := newLocalHarness(t)
				h.seedTeamVersion("tdf_sdlc_1", "sdlc", leadTeam("v1"), true)
				h.walk("sdlc")
				lead := h.runsByAgent()["sdlc/lead"]
				if lead.ID == "" || !h.prov.sawSystem("LOCAL helper v1") {
					t.Fatalf("fixture drifted: the live walk did not run lead and helper on v1")
				}

				h.seedTeamVersion("tdf_sdlc_2", "sdlc", tc.v2, true)
				h.prov.forget()
				srv := h.srv
				if where != "same instance" {
					srv = h.newServer()
				}
				resumeAndFinish(t, srv, lead)

				if !h.prov.sawSystem("LOCAL lead v1") || h.prov.sawSystem("LOCAL lead v2") {
					t.Error("the resumed run must continue as the lead of the version it started under")
				}
				if !h.prov.sawSystem("LOCAL helper v1") || h.prov.sawSystem("LOCAL helper v2") {
					t.Error("the agent it started after resuming must be the helper of that same version")
				}
				if h.prov.sawSystem("GLOBAL helper") {
					t.Error("\"./helper\" fell through to the global agent after resume")
				}
			})
		}
	}
}

// Deleted, not superseded: there is no definition to resume on, and neither
// the team's current version nor a global agent that has taken the name since
// is a substitute. The run fails, saying why, and never runs.
func TestResumedRun_WhoseTeamVersionIsGoneFailsWithoutRunning(t *testing.T) {
	h := newLocalHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./solo", map[string]string{"solo": "LOCAL solo"}), true)
	h.walk("sdlc")
	run := h.runsByAgent()["sdlc/solo"]
	ctx := context.Background()
	if deleted, err := h.st.TeamDefDelete(ctx, "acme", "sdlc"); err != nil || !deleted {
		t.Fatalf("delete team: %v %v", deleted, err)
	}
	if err := h.st.DynamicAgentUpsert(ctx, store.DynamicAgent{
		Name: "sdlc/solo", TenantID: "acme", Definition: json.RawMessage(`{"Model":"stub-model","SystemPrompt":"GLOBAL solo"}`),
	}); err != nil {
		t.Fatal(err)
	}
	h.prov.forget()
	parkForResume(t, h.srv, run.ID)
	// The row here is a finished member's, re-parked, so the verdict is read
	// from the resume itself rather than from a status the row already had.
	n, warnings := h.srv.ResumePausedRuns(ctx)
	if n != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "no longer exists") {
		t.Fatalf("ResumePausedRuns = %d, %v; want nothing re-dispatched and one warning that the team version no longer exists", n, warnings)
	}
	if h.prov.sawSystem("GLOBAL solo") || h.prov.sawSystem("LOCAL solo") {
		t.Error("the run resumed on some other definition")
	}
}

// A session that began inside a team is continued inside it, on the version
// it started under. That is the one way a team's own agent is reached after
// its walk is over, and it reaches only the session that was already its own.
func TestContinuation_OfATeamsOwnAgentStaysInTheTeamVersionItStartedUnder(t *testing.T) {
	h := newLocalHarness(t)
	defID := h.seedTeamVersion("tdf_sdlc_1", "sdlc", leadTeam("v1"), true)
	h.walk("sdlc")
	lead := h.runsByAgent()["sdlc/lead"]
	h.seedTeamVersion("tdf_sdlc_2", "sdlc", leadTeam("v2"), true)
	h.prov.forget()

	if err := h.runOnce("", lead.SessionID); err != nil {
		t.Fatalf("continue the lead's session: %v", err)
	}
	if !h.prov.sawSystem("LOCAL lead v1") || !h.prov.sawSystem("LOCAL helper v1") {
		t.Error("the continuation must run the lead, and the helper it starts, of the version the session began under")
	}
	if h.prov.sawSystem("LOCAL lead v2") || h.prov.sawSystem("LOCAL helper v2") || h.prov.sawSystem("GLOBAL helper") {
		t.Error("the continuation read an agent from somewhere other than its team version")
	}
	runs, err := h.st.RunsForSession(context.Background(), lead.SessionID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("session runs = %d (err %v), want the original and the continuation", len(runs), err)
	}
	if sc := teamScopeOf(t, runs[1]); sc == nil || sc.DefID != defID {
		t.Errorf("the continuation recorded team scope %+v, want version %s — a second continuation would lose its team", sc, defID)
	}

	// Once the team is deleted there is nothing to continue on.
	if _, err := h.st.TeamDefDelete(context.Background(), "acme", "sdlc"); err != nil {
		t.Fatal(err)
	}
	if err := h.runOnce("", lead.SessionID); !errors.Is(err, runner.ErrUnknownAgent) {
		t.Errorf("continuing after the team was deleted = %v, want unknown agent", err)
	}
}

// pausedLocalRun is the row a run of sdlc's own agent `local`, paused on team
// version defID, leaves: still running, its record naming the team version.
func (h *localHarness) pausedLocalRun(defID, local string) store.Run {
	h.t.Helper()
	ctx := context.Background()
	agent := "sdlc/" + local
	sess, err := h.st.CreateSession(ctx, "acme", agent, "alice")
	if err != nil {
		h.t.Fatal(err)
	}
	rec := runConfigRecord{
		AgentVersion: &agentVersionRecord{TeamDefID: defID},
		TeamScope:    &teamScopeRecord{Team: "sdlc", DefID: defID, DefTenant: "acme"},
	}
	run, err := h.st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_paused_local", UserID: "alice", TenantID: "acme", Model: "stub-model", RunConfig: rec.marshal(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	parkForResume(h.t, h.srv, run.ID)
	return run
}

// A store fault reading the team version is not its deletion: the run neither
// runs nor is failed, and stays paused for the next resume to try again.
func TestResumedRun_WhoseTeamVersionCannotBeReadStaysPaused(t *testing.T) {
	h := newLocalHarness(t)
	defID := h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./solo", map[string]string{"solo": "LOCAL solo"}), true)
	run := h.pausedLocalRun(defID, "solo")
	ctx := context.Background()

	h.st.faulty.Store(true)
	if n, warnings := h.srv.ResumePausedRuns(ctx); n != 0 || len(warnings) != 1 {
		t.Fatalf("ResumePausedRuns = %d, %v; want 0 re-dispatched and one warning", n, warnings)
	}
	got, err := h.st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunRunning || got.PauseState != store.PauseStatePaused {
		t.Fatalf("run status %q pause_state %q (%s); want still paused", got.Status, got.PauseState, got.ErrorMsg)
	}
	if h.prov.sawSystem("LOCAL solo") {
		t.Error("the run ran although its definition could not be read")
	}

	// The fault clears, and the same run resumes on its team's agent.
	h.st.faulty.Store(false)
	resumeOne(t, h.srv)
	waitFor(t, "the resumed run to finish", func() bool { _, live := h.srv.cancelReg.Get(run.AgentID); return !live })
	if !h.prov.sawSystem("LOCAL solo") {
		t.Error("the run did not resume on its team's own agent once the store answered")
	}
}

// And a deleted version fails the run, on a row that was really paused.
func TestResumedRun_WhoseTeamVersionWasDeletedIsFailed(t *testing.T) {
	h := newLocalHarness(t)
	defID := h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./solo", map[string]string{"solo": "LOCAL solo"}), true)
	run := h.pausedLocalRun(defID, "solo")
	ctx := context.Background()
	if _, err := h.st.TeamDefDelete(ctx, "acme", "sdlc"); err != nil {
		t.Fatal(err)
	}
	if n, warnings := h.srv.ResumePausedRuns(ctx); n != 0 || len(warnings) != 1 {
		t.Fatalf("ResumePausedRuns = %d, %v; want 0 re-dispatched and one warning", n, warnings)
	}
	got, err := h.st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunFailed || !strings.Contains(got.ErrorMsg, "no longer exists") {
		t.Errorf("run status %q, error %q; want failed because its team version no longer exists", got.Status, got.ErrorMsg)
	}
}

// A provider fallback re-resolves the run's agent BY NAME, on the run's own
// context. For a team's own agent that only works because the context carries
// the team scope: with it the name resolves, without it there is no such agent.
func TestResolveAgent_FallbackReResolveFindsATeamsOwnAgentOnlyInItsScope(t *testing.T) {
	h := newLocalHarness(t)
	defID := h.seedTeamVersion("tdf_sdlc_1", "sdlc", teamWith("./solo", map[string]string{"solo": "LOCAL solo"}), true)
	inTeam := store.WithTeamScope(context.Background(), store.TeamScope{Tenant: "acme", Team: "sdlc", DefID: defID})

	providerID, model, _, err := h.srv.resolveAgent(inTeam, "acme", "alice", "sdlc/solo", "", false, nil)
	if err != nil || providerID != "stub" || model != "stub-model" {
		t.Errorf("inside the team: resolveAgent = %q %q %v, want the local agent's stub-model", providerID, model, err)
	}
	if _, _, _, err := h.srv.resolveAgent(context.Background(), "acme", "alice", "sdlc/solo", "", false, nil); !errors.Is(err, runner.ErrUnknownAgent) {
		t.Errorf("outside the team: resolveAgent = %v, want unknown agent", err)
	}
}
