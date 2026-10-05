package teamgraph

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// localJSON is a two-state team whose one working state runs its own agent.
const localJSON = `{
  "entry": "review",
  "local": {"agents": {"reviewer": {"tier": "middle", "tools": ["Read"], "system_prompt": "You review diffs."}}},
  "states": [
    {"state": "review", "handler": {"kind": "agent", "agent": "./reviewer"}},
    {"state": "done", "handler": {"kind": "terminal"}}
  ],
  "transitions": [{"from": "review", "to": "done", "on": "success"}]
}`

func TestValidate_AcceptsDeclaredLocalAgent(t *testing.T) {
	if err := Validate(mustParse(t, localJSON)); err != nil {
		t.Fatalf("a team running its own declared agent must validate: %v", err)
	}
}

// Every agent-bearing field is held to the rule, not only `agent`.
func TestValidate_RefusesUndeclaredLocalRefInEveryField(t *testing.T) {
	for field, handler := range map[string]string{
		"agent":         `{"kind":"agent","agent":"./ghost"}`,
		"agents":        `{"kind":"parallel","agents":["./reviewer","./ghost"],"consolidator":"./reviewer"}`,
		"consolidator":  `{"kind":"agent","agent":"./reviewer","consolidator":"./ghost"}`,
		"fanout.agent":  `{"kind":"starter","source":{"kind":"input"},"fanout":{"agent":"./ghost","per":"once"}}`,
		"fanout.agents": `{"kind":"starter","source":{"kind":"input"},"fanout":{"agents":["./ghost"],"per":"once"}}`,
	} {
		def := `{"entry":"s","local":{"agents":{"reviewer":{"tier":"middle"}}},
		  "states":[{"state":"s","handler":` + handler + `},{"state":"done","handler":{"kind":"terminal"}}],
		  "transitions":[{"from":"s","to":"done","on":"success"}]}`
		err := Validate(mustParse(t, def))
		if err == nil {
			t.Errorf("%s: \"./ghost\" is not declared and must be refused", field)
			continue
		}
		for _, want := range []string{`"./ghost"`, field, "reviewer"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal %q should name %s", field, err, want)
			}
		}
	}
}

func TestValidate_RefusesLocalRefWhenTeamDeclaresNone(t *testing.T) {
	def := `{"entry":"s","states":[{"state":"s","handler":{"kind":"agent","agent":"./x"}},
	  {"state":"done","handler":{"kind":"terminal"}}],"transitions":[{"from":"s","to":"done","on":"success"}]}`
	err := Validate(mustParse(t, def))
	if err == nil || !strings.Contains(err.Error(), "declares none") {
		t.Fatalf("want a refusal saying the team declares no local agents, got %v", err)
	}
}

// A bare name is a GLOBAL agent even when a local agent has that name: the
// definition's own references never change meaning by a later declaration.
func TestValidate_BareNameIsNotALocalReference(t *testing.T) {
	def := strings.Replace(localJSON, `"./reviewer"`, `"reviewer"`, 1)
	d := mustParse(t, def)
	if err := Validate(d); err != nil {
		t.Fatalf("a bare agent name needs no declaration: %v", err)
	}
	if got := UnreferencedLocalAgents(d); len(got) != 1 || got[0] != "reviewer" {
		t.Errorf("the declared agent is named by no state; UnreferencedLocalAgents = %v", got)
	}
}

func TestValidate_RefusesBadLocalAgentName(t *testing.T) {
	for _, name := range []string{"a/b", "a.b", "a b", "", strings.Repeat("x", MaxLocalNameLen+1), "./a"} {
		def := `{"entry":"done","local":{"agents":{` + quote(name) + `:{"tier":"middle"}}},
		  "states":[{"state":"done","handler":{"kind":"terminal"}}],"transitions":[]}`
		if err := Validate(mustParse(t, def)); err == nil {
			t.Errorf("local agent name %q must be refused", name)
		}
	}
}

func quote(s string) string { return `"` + s + `"` }

// The definition is decoded into a typed struct, which drops a key it does not
// know. Under `local` that would accept a kind this runtime cannot honour and
// run the team without it.
func TestParse_RefusesUnknownLocalKind(t *testing.T) {
	for _, kind := range []string{"channels", "schedules", "webhooks", "agent", "skill"} {
		_, err := Parse([]byte(`{"entry":"s","local":{"` + kind + `":{}}}`))
		if err == nil {
			t.Errorf("local.%s must be refused, not ignored", kind)
			continue
		}
		if !strings.Contains(err.Error(), kind) || strings.Contains(err.Error(), "invalid JSON") {
			t.Errorf("local.%s: refusal %q should name the kind and not call it invalid JSON", kind, err)
		}
	}
}

func TestParse_RefusesLocalAgentBodyThatIsNotAnObject(t *testing.T) {
	for _, body := range []string{`"reviewer"`, `[]`, `null`, `3`} {
		if _, err := Parse([]byte(`{"entry":"s","local":{"agents":{"a":` + body + `}}}`)); err == nil {
			t.Errorf("a local agent body of %s must be refused", body)
		}
	}
}

// recordedNoLocalHash is Sign("sdlc", sdlcJSON) as it was before `local`
// existed. A team that declares no local agents must keep its hash.
const recordedNoLocalHash = "sha256:0fb692c1dd4f8a03795850de178ea4d1547ddea9f0a75380a19896fb5b676e3c"

func TestSign_TeamWithoutLocalKeepsItsRecordedHash(t *testing.T) {
	d := mustParse(t, sdlcJSON)
	if got := Sign("sdlc", d); got != recordedNoLocalHash {
		t.Fatalf("a team with no local block hashes %s, recorded %s", got, recordedNoLocalHash)
	}
	// An empty block declares nothing, so it is the same content.
	d.Local = &Local{}
	if got := Sign("sdlc", d); got != recordedNoLocalHash {
		t.Errorf("an empty local block changed the hash to %s", got)
	}
	d.Local = &Local{Agents: map[string]json.RawMessage{}}
	if got := Sign("sdlc", d); got != recordedNoLocalHash {
		t.Errorf("an empty local.agents changed the hash to %s", got)
	}
}

func TestSign_LocalAgentIsContent(t *testing.T) {
	base := Sign("t", mustParse(t, localJSON))
	changed := Sign("t", mustParse(t, strings.Replace(localJSON, "You review diffs.", "You review code.", 1)))
	if base == changed {
		t.Error("editing a local agent's prompt must change the team's content hash")
	}
	without := mustParse(t, localJSON)
	without.Local = nil
	if Sign("t", without) == base {
		t.Error("removing the local agents must change the team's content hash")
	}
}

// The hash must not depend on how a local agent body was laid out: key order
// and whitespace at any depth are not content.
func TestSign_LocalAgentBodyIsHashedCanonically(t *testing.T) {
	a := `{"entry":"review","local":{"agents":{
	  "reviewer":{"tier":"middle","tools":["Read"],"sampling":{"temperature":0.5,"top_p":0.9}},
	  "writer":{"tier":"low"}}}}`
	b := `{"entry":"review","local":{"agents":{"writer":{ "tier" : "low" },
	  "reviewer":{"sampling":{"top_p":0.9,
	     "temperature":0.5},"tools":["Read"],   "tier":"middle"}}}}`
	if ha, hb := Sign("t", mustParse(t, a)), Sign("t", mustParse(t, b)); ha != hb {
		t.Fatalf("key order / whitespace changed the hash: %s vs %s", ha, hb)
	}
	// Array order IS content, as it is for an agent's own hash.
	c := strings.Replace(a, `["Read"]`, `["Read","Grep"]`, 1)
	d := strings.Replace(a, `["Read"]`, `["Grep","Read"]`, 1)
	if Sign("t", mustParse(t, c)) == Sign("t", mustParse(t, d)) {
		t.Error("reordering a local agent's tools must change the hash")
	}
}

// QualifyLocalRefs walks the same field list AgentRefs does, so after it no
// field may still carry a local reference — and the caller's definition is
// left as it was.
func TestQualifyLocalRefs_RewritesEveryAgentBearingField(t *testing.T) {
	def := Definition{States: []State{
		{ID: "one", Handler: Handler{Kind: HandlerAgent, Agent: "./a1", Consolidator: "./c1"}},
		{ID: "par", Handler: Handler{Kind: HandlerParallel, Agents: []string{"./a2", "global"}}},
		{ID: "wave", Handler: Handler{Kind: HandlerStarter,
			Fanout: &StarterFanout{Agent: "./a4", Agents: []string{"./a5"}}}},
	}}
	got := map[string]bool{}
	for _, r := range AgentRefs(QualifyLocalRefs(def, "sdlc")) {
		got[r.Agent] = true
	}
	for _, want := range []string{"sdlc/a1", "sdlc/c1", "sdlc/a2", "global", "sdlc/a4", "sdlc/a5"} {
		if !got[want] {
			t.Errorf("qualified refs %v are missing %q", got, want)
		}
	}
	for _, r := range AgentRefs(def) {
		if r.Agent != "global" && !strings.HasPrefix(r.Agent, LocalRefPrefix) {
			t.Errorf("QualifyLocalRefs changed the caller's definition: %s is now %q", r.Field, r.Agent)
		}
	}
}

func TestRenderMermaid_ShowsLocalRefsAsWritten(t *testing.T) {
	def := `{"entry":"par","local":{"agents":{"reviewer":{"tier":"middle"},"judge":{"tier":"middle"}}},
	  "states":[{"state":"par","handler":{"kind":"parallel","agents":["./reviewer","sec"],"consolidator":"./judge"}},
	    {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"par","to":"done","on":"success"}]}`
	out := RenderMermaid("t", mustParse(t, def), "")
	for _, want := range []string{"parallel: ./reviewer, sec", "consolidator: ./judge"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagram is missing %q:\n%s", want, out)
		}
	}
}

// A local agent picks its state's colour by name like a global one does.
func TestResolve_LocalAgentNameChoosesTheFill(t *testing.T) {
	local := mustParse(t, `{"entry":"s","states":[{"state":"s","handler":{"kind":"agent","agent":"./reviewer"}}]}`)
	global := mustParse(t, `{"entry":"s","states":[{"state":"s","handler":{"kind":"agent","agent":"reviewer"}}]}`)
	if l, g := Resolve(local).Fill["s"], Resolve(global).Fill["s"]; l == "" || l != g {
		t.Errorf("fill for ./reviewer = %q, for reviewer = %q; want the same keyword colour", l, g)
	}
}

func TestValidate_CapsTheNumberOfLocalAgents(t *testing.T) {
	build := func(n int) Definition {
		d := mustParse(t, localJSON)
		for i := 1; i < n; i++ {
			d.Local.Agents[fmt.Sprintf("a%d", i)] = json.RawMessage(`{"tier":"low"}`)
		}
		return d
	}
	if err := Validate(build(MaxLocalAgents)); err != nil {
		t.Fatalf("%d local agents is the limit and must be accepted: %v", MaxLocalAgents, err)
	}
	err := Validate(build(MaxLocalAgents + 1))
	if err == nil || !strings.Contains(err.Error(), "more than the maximum 64") {
		t.Fatalf("%d local agents must be refused naming the limit, got %v", MaxLocalAgents+1, err)
	}
}

func TestCheckLocalRunNames_RefusesBareReferenceToALocalAgentsRunName(t *testing.T) {
	d := mustParse(t, strings.Replace(localJSON, `"./reviewer"`, `"sdlc/reviewer"`, 1))
	if err := CheckLocalRunNames(d, "sdlc"); err == nil || !strings.Contains(err.Error(), `"./reviewer"`) {
		t.Fatalf("want a refusal pointing at \"./reviewer\", got %v", err)
	}
	for _, team := range []string{"other", "sdl"} {
		if err := CheckLocalRunNames(d, team); err != nil {
			t.Errorf("for team %q the name is an ordinary global one: %v", team, err)
		}
	}
	if err := CheckLocalRunNames(mustParse(t, localJSON), "sdlc"); err != nil {
		t.Errorf("a \"./reviewer\" reference is the team's own and is fine: %v", err)
	}
	if got := LocalRunNames(mustParse(t, localJSON), "sdlc"); len(got) != 1 || got["sdlc/reviewer"] != "./reviewer" {
		t.Errorf("LocalRunNames = %v", got)
	}
}
