package teamgraph

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func keyIssuePaths(is []*Issue) map[string]string {
	out := map[string]string{}
	for _, i := range is {
		out[i.Path] = i.Msg
	}
	return out
}

const strictGraph = `"entry":"work","states":[{"state":"work","handler":{"kind":"agent","agent":"a"}},{"state":"done","handler":{"kind":"terminal"}}],"transitions":[{"from":"work","to":"done","on":"success"}]`

// A definition written with exactly the keys the runtime knows has nothing to
// report, in every part of it: typed objects, free-form maps, and the values
// other readers decode.
func TestKeyIssues_ADefinitionReadAsWrittenHasNone(t *testing.T) {
	raw := `{` + strictGraph + `,"max_iterations":5,
	  "vars":{"Tone":"plain","tone":"warm"},
	  "colors":{"states":{"work":"#fff","Work":"#000"}},
	  "layout":{"nodes":{"work":{"x":1,"y":2,"w":3,"h":4}}},
	  "channels":{"publish":["a"],"subscribe":["b"]},
	  "hooks":{"run_end":["audit",{"name":"n","url":"https://h.example","fail_mode":"open","timeout_ms":5,"headers":{"X-A":"1","x-a":"2"}}]},
	  "local":{"agents":{"helper":{"model":"m","Anything":1}},"skills":{"s":{"body":"b","description":"d","tools":["Read"]}},
	           "schedules":{"tick":{"schedule":"@every 1m","channel":"./c","payload":{"K":1,"k":2}}}}}`
	var probe any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	if is := KeyIssues([]byte(raw), nil, nil); len(is) != 0 {
		t.Errorf("issues for a definition with only known keys: %v", keyIssuePaths(is))
	}
}

// Each way the decoder reads a key differently from its text is reported at
// the key's own path, at any depth.
func TestKeyIssues_ReportsEveryKeyTheDecoderWouldReadDifferently(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		path string
		want string
	}{
		"a field matched only by case, at the top": {
			`{"Hooks":{},` + strictGraph + `}`, "Hooks", `read as "hooks" only by ignoring its case`},
		"upper case": {
			`{"ENTRY":"work","states":[],"transitions":[]}`, "ENTRY", `Write "entry"`},
		"a fold only Unicode makes (long s)": {
			`{"hookſ":{},` + strictGraph + `}`, `["hookſ"]`, `read as "hooks"`},
		"two spellings of one field in a handler": {
			`{"entry":"w","states":[{"state":"w","handler":{"kind":"agent","agent":"harmless","Agent":"privileged"}}],"transitions":[]}`,
			"states[0].handler.Agent", `keys "agent" and "Agent" are both read as "agent"`},
		"the same, the other way round": {
			`{"entry":"w","states":[{"state":"w","handler":{"kind":"agent","Agent":"privileged","agent":"harmless"}}],"transitions":[]}`,
			"states[0].handler.agent", `keys "Agent" and "agent" are both read as "agent"`},
		"an exact repeat": {
			`{"entry":"a","entry":"b","states":[],"transitions":[]}`, "entry", `key "entry" is written twice`},
		"an unknown key at the top": {
			`{"extras":1,` + strictGraph + `}`, "extras", `the definition: unknown key "extras"`},
		"a misspelt key, with the one it is closest to": {
			`{"hoks":{},` + strictGraph + `}`, "hoks", `unknown key "hoks" — did you mean "hooks"?`},
		"a misspelt cap": {
			`{"max_iteration":5,` + strictGraph + `}`, "max_iteration", `did you mean "max_iterations"?`},
		"an unknown key in a transition": {
			`{"entry":"w","states":[],"transitions":[{"from":"a","to":"b","on":"success","when":"x"}]}`, "transitions[0].when", `transitions[0]: unknown key "when"`},
		"an unknown key in a starter's source": {
			`{"entry":"w","states":[{"state":"w","handler":{"kind":"starter","source":{"channel":"c","chanel":"d"}}}],"transitions":[]}`,
			"states[0].handler.source.chanel", `did you mean "channel"?`},
		"a field matched by case in a layout node": {
			`{"entry":"w","states":[],"transitions":[],"layout":{"nodes":{"w":{"X":1,"y":2}}}}`, "layout.nodes.w.X", `read as "x"`},
		"an unknown key in an inline hook": {
			`{"entry":"w","states":[],"transitions":[],"hooks":{"run_end":[{"name":"n","url":"u","secret":"s"}]}}`,
			"hooks.run_end[0].secret", `unknown key "secret"`},
		"a field matched by case in a local skill": {
			`{"entry":"w","states":[],"transitions":[],"local":{"skills":{"s":{"Body":"b"}}}}`, "local.skills.s.Body", `read as "body"`},
		"an exact repeat of a variable name": {
			`{"entry":"w","states":[],"transitions":[],"vars":{"tone":"a","tone":"b"}}`, "vars.tone", `key "tone" is written twice`},
		"an exact repeat inside a value this package does not type": {
			`{"entry":"w","states":[{"state":"w","handler":{"kind":"input","schema":{"type":"object","type":"string"}}}],"transitions":[]}`,
			"states[0].handler.schema.type", `written twice`},
		"a key that belongs beside the definition": {
			`{"description":"why",` + strictGraph + `}`, "description", "given beside the definition, not inside it"},
	} {
		t.Run(name, func(t *testing.T) {
			got := keyIssuePaths(KeyIssues([]byte(tc.raw), nil, nil))
			msg, ok := got[tc.path]
			if !ok || !strings.Contains(msg, tc.want) {
				t.Errorf("issues = %v\nwant one at %q containing %q", got, tc.path, tc.want)
			}
			wantN := 1
			if name == "the same, the other way round" {
				wantN = 2 // "Agent", read first, is a case-only match too
			}
			if len(got) != wantN {
				t.Errorf("want %d issue(s), got %d: %v", wantN, len(got), got)
			}
		})
	}
}

// Every offending key is reported, not just the first, and an issue inside a
// state names the state and the field as the graph's own issues do.
func TestKeyIssues_ReportsAllAndNamesTheState(t *testing.T) {
	raw := `{"Entry":"review","extras":1,"states":[{"state":"review","handler":{"kind":"agent","agent":"a","Agent":"b","nope":1}}],"transitions":[]}`
	def, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	is := KeyIssues([]byte(raw), &def, nil)
	if len(is) != 4 {
		t.Fatalf("got %d issues, want 4: %v", len(is), keyIssuePaths(is))
	}
	for _, i := range is {
		if i.Kind != IssueKeyInvalid {
			t.Errorf("%s: kind = %q, want %q", i.Path, i.Kind, IssueKeyInvalid)
		}
		if strings.HasPrefix(i.Path, "states[0].handler.") && (i.State != "review" || i.Field != strings.TrimPrefix(i.Path, "states[0].handler.")) {
			t.Errorf("%s: state=%q field=%q, want the state id and the handler-relative key", i.Path, i.State, i.Field)
		}
	}
}

type bodyForTest struct {
	Scope string `json:"scope"`
	TTL   int    `json:"default_ttl,omitempty"`
}

// A body under `local` is typed by the caller that knows its type. With a
// type, its field names are checked like any other; a kind whose decoder
// already refuses unknown keys keeps that refusal to itself.
func TestKeyIssues_TypesTheLocalBodiesItIsGivenATypeFor(t *testing.T) {
	raw := `{"entry":"w","states":[],"transitions":[],"local":{
	  "agents":{"helper":{"Scope":"x","stray":1}},
	  "channels":{"c":{"Scope":"x","stray":1}}}}`
	bodies := LocalBodyKeys{
		"local.agents":   {Type: reflect.TypeOf(bodyForTest{})},
		"local.channels": {Type: reflect.TypeOf(bodyForTest{}), UnknownRefusedElsewhere: true},
	}
	got := keyIssuePaths(KeyIssues([]byte(raw), nil, bodies))
	for _, path := range []string{"local.agents.helper.Scope", "local.agents.helper.stray", "local.channels.c.Scope"} {
		if _, ok := got[path]; !ok {
			t.Errorf("no issue at %s: %v", path, got)
		}
	}
	if _, ok := got["local.channels.c.stray"]; ok {
		t.Errorf("an unknown key was reported for a kind whose own decoder refuses it: %v", got)
	}
	if len(got) != 3 {
		t.Errorf("want 3 issues, got %v", got)
	}
	// With no type, the same bodies are judged for repeats only.
	if is := KeyIssues([]byte(raw), nil, nil); len(is) != 0 {
		t.Errorf("untyped bodies reported: %v", keyIssuePaths(is))
	}
}

// What the runtime itself stores has nothing to report: only an author's text
// is judged, and a stored definition must stay loadable.
func TestKeyIssues_AStoredDefinitionRoundTripsClean(t *testing.T) {
	def, err := Parse([]byte(`{` + strictGraph + `,"vars":{"tone":"plain"},"hooks":{"run_end":["audit"]},"local":{"skills":{"s":{"body":"b"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if is := KeyIssues(stored, &def, nil); len(is) != 0 {
		t.Errorf("the runtime's own encoding of a definition has key issues: %v", keyIssuePaths(is))
	}
}

func TestKeyIssues_TextThatIsNotJSONIsLeftToParse(t *testing.T) {
	for _, raw := range []string{``, `{`, `{"entry":`, `[1,2`, `nonsense`} {
		if is := KeyIssues([]byte(raw), nil, nil); len(is) != 0 {
			t.Errorf("%q: issues %v, want none (Parse reports invalid JSON)", raw, keyIssuePaths(is))
		}
	}
}
