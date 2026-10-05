package http

import (
	"context"
	"encoding/json"
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
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// These tests run real walks whose agents call the Skill tool, and read what
// the tool answered off the next request each agent sent its model.

// skillCaller is whoProvider plus one directive: a system-prompt line
// SKILL:<json> makes the agent call the Skill tool with that input, once per
// user turn, and then answer "GOT: <the tool's result>".
type skillCaller struct{ whoProvider }

func (p *skillCaller) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	input := ""
	for _, line := range strings.Split(systemText(req), "\n") {
		if rest, ok := strings.CutPrefix(line, "SKILL:"); ok {
			input = rest
		}
	}
	if input == "" {
		return p.whoProvider.Call(ctx, req)
	}
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	if result, answered := lastSkillResult(req); answered {
		ch <- providers.Event{Type: providers.EventText, Text: "GOT: " + result}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	} else {
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_skill", Name: "Skill", Input: json.RawMessage(input)}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	}
	close(ch)
	return ch, nil
}

func lastSkillResult(req providers.Request) (string, bool) {
	n := len(req.Messages)
	if n == 0 {
		return "", false
	}
	for _, c := range req.Messages[n-1].Content {
		if c.Type == "tool_result" {
			return c.Text, true
		}
	}
	return "", false
}

// toolResults returns every Skill result the agents were handed.
func (p *skillCaller) toolResults() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	for _, r := range p.reqs {
		if res, ok := lastSkillResult(r); ok {
			b.WriteString(res + "\n")
		}
	}
	return b.String()
}

type skillHarness struct {
	*localHarness
	calls *skillCaller
}

// newSkillHarness is newLocalHarness with the Skill tool wired, and one more
// GLOBAL agent, "outsider", whose skills name the team skill "./style" and
// who tries to load it.
func newSkillHarness(t *testing.T) *skillHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"helper": {Model: "stub-model", SystemPrompt: "GLOBAL helper"},
			"outsider": {Model: "stub-model", SystemPrompt: "GLOBAL outsider\nSKILL:" + `{"name":"./style"}`,
				Tools: []string{"Skill"}, Skills: []string{"./style"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "skills.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	calls := &skillCaller{}
	h := &skillHarness{
		localHarness: &localHarness{t: t, st: &teamFaultStore{Store: st}, prov: &calls.whoProvider, cfg: cfg},
		calls:        calls,
	}
	h.srv = h.newSkillServer()
	return h
}

// newSkillServer is one instance over the harness's store; a second is
// another instance that knows a run only from what the store holds.
func (h *skillHarness) newSkillServer() *Server {
	srv := New(h.cfg, &stubResolver{p: h.calls}, []tools.Tool{&builtin.SkillTool{Store: h.st}}, concurrency.New(8, 8, 5*time.Second), h.st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	srv.SetTeamDefTool(&builtin.TeamDef{Store: h.st})
	return srv
}

// skillTeam is a one-state team running `agent`, whose own agents are given as
// name → {system prompt, skills}, and which declares the skill "style" with
// the given body.
func skillTeam(agent string, locals map[string][2]any, styleBody string) string {
	bodies := map[string]any{}
	for name, spec := range locals {
		bodies[name] = map[string]any{"model": "stub-model", "system_prompt": spec[0], "skills": spec[1], "tools": []string{"Skill", "Agent"}}
	}
	def := map[string]any{
		"entry": "work",
		"local": map[string]any{
			"agents": bodies,
			"skills": map[string]any{"style": map[string]any{"body": styleBody, "description": "House style"}},
		},
		"states": []any{
			map[string]any{"state": "work", "handler": map[string]any{"kind": "agent", "agent": agent}},
			map[string]any{"state": "done", "handler": map[string]any{"kind": "terminal"}},
		},
		"transitions": []any{map[string]any{"from": "work", "to": "done", "on": "success"}},
	}
	b, _ := json.Marshal(def)
	return string(b)
}

func writerTeam(styleBody string, skills []string, call string) string {
	return skillTeam("./writer", map[string][2]any{"writer": {"LOCAL writer\nSKILL:" + call, skills}}, styleBody)
}

func TestTeamWalk_TeamAgentLoadsItsTeamSkill(t *testing.T) {
	h := newSkillHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", writerTeam("Prefer short functions.", []string{"./style"}, `{"name":"./style"}`), true)

	out := h.walk("sdlc")

	if out["final_output"] != "GOT: Prefer short functions." {
		t.Errorf("final_output = %v, want the team skill's body", out["final_output"])
	}
	// Run start tells the agent which skills it may load, by the name it
	// loads them under.
	if !h.prov.sawSystem("LOCAL writer") || !strings.Contains(h.systemOf("LOCAL writer"), "matching: ./style") {
		t.Errorf("the writer's system prompt does not offer ./style: %q", h.systemOf("LOCAL writer"))
	}
}

// systemOf is the system prompt of the first call starting with first.
func (h *skillHarness) systemOf(first string) string {
	h.calls.mu.Lock()
	defer h.calls.mu.Unlock()
	for _, r := range h.calls.reqs {
		if s := systemText(r); strings.HasPrefix(s, first) {
			return s
		}
	}
	return ""
}

func TestTeamWalk_TeamAgentListsItsTeamSkillUnderTheNameInvokeTakes(t *testing.T) {
	h := newSkillHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", writerTeam("b", []string{"./style"}, `{"op":"list"}`), true)

	out, _ := h.walk("sdlc")["final_output"].(string)
	var listed struct {
		Skills []struct{ Name, Description string } `json:"skills"`
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(out, "GOT: ")), &listed)
	if len(listed.Skills) != 1 || listed.Skills[0].Name != "./style" || listed.Skills[0].Description != "House style" {
		t.Errorf("list = %s, want ./style with its description", out)
	}
}

// Only an exact "./style" grants it: a glob in the agent's skills does not.
func TestTeamWalk_GlobInATeamAgentsSkillsDoesNotGrantATeamSkill(t *testing.T) {
	h := newSkillHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", writerTeam("SECRET BODY", []string{"*"}, `{"name":"./style"}`), true)

	out := fmtAny(h.walk("sdlc")["final_output"])

	if strings.Contains(out, "SECRET BODY") || !strings.Contains(out, "not granted") {
		t.Errorf("a team agent with skills [*] loaded ./style: %s", out)
	}
}

// A GLOBAL agent running inside the team — here as the team's own state — is
// not the team's own agent, so even naming "./style" in its skills reaches
// nothing.
func TestTeamWalk_GlobalAgentInsideTheTeamReachesNoTeamSkill(t *testing.T) {
	h := newSkillHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", skillTeam("outsider", nil, "SECRET BODY"), true)

	out := fmtAny(h.walk("sdlc")["final_output"])

	if strings.Contains(out, "SECRET BODY") || !strings.Contains(out, "only that team's own agents") {
		t.Errorf("a global agent inside the team was answered %s, want the team skill refused", out)
	}
}

// Outside a walk no spelling reaches a team's skill.
func TestRun_TeamSkillIsUnreachableOutsideItsTeam(t *testing.T) {
	h := newSkillHarness(t)
	h.seedTeamVersion("tdf_sdlc_1", "sdlc", writerTeam("SECRET BODY", []string{"./style"}, `{"name":"./style"}`), true)

	if err := h.runOnce("outsider", ""); err != nil {
		t.Fatalf("run outsider: %v", err)
	}
	if got := h.calls.toolResults(); got == "" || strings.Contains(got, "SECRET BODY") {
		t.Errorf("a run outside the team was answered %q, want the team skill refused", got)
	}
}

// A paused run of a team's own agent resumes on the team version it started
// under, and so loads that version's skill — not the one a fork has since
// written and promoted.
func TestResumedRun_LoadsTheTeamSkillOfTheRecordedTeamVersion(t *testing.T) {
	for _, where := range []string{"same instance", "another instance, from the store"} {
		t.Run(where, func(t *testing.T) {
			h := newSkillHarness(t)
			h.seedTeamVersion("tdf_sdlc_1", "sdlc", writerTeam("v1 rules", []string{"./style"}, `{"name":"./style"}`), true)
			h.walk("sdlc")
			writer := h.runsByAgent()["sdlc/writer"]
			if writer.ID == "" || !strings.Contains(h.calls.toolResults(), "v1 rules") {
				t.Fatalf("fixture drifted: the live walk did not load v1's skill")
			}

			h.seedTeamVersion("tdf_sdlc_2", "sdlc", writerTeam("v2 rules", []string{"./style"}, `{"name":"./style"}`), true)
			h.prov.forget()
			srv := h.srv
			if where != "same instance" {
				srv = h.newSkillServer()
			}
			resumeAndFinish(t, srv, writer)

			got := h.calls.toolResults()
			if !strings.Contains(got, "v1 rules") || strings.Contains(got, "v2 rules") {
				t.Errorf("the resumed run loaded %q, want the skill of the version it started under (v1)", got)
			}
		})
	}
}
