package teamgraph

import (
	"strings"
	"testing"
)

// A valid full workflow: parallel review + consolidator + pushback loops (the
// RFC's SDLC pilot, abbreviated). Must pass.
const sdlcJSON = `{
  "entry": "implementation",
  "max_iterations": 10,
  "states": [
    { "state": "implementation", "handler": { "kind": "agent", "agent": "code-guru" } },
    { "state": "review", "handler": {
        "kind": "parallel", "agents": ["sec-rev", "code-rev"], "wait": "all",
        "consolidator": "sdlc-consolidator" } },
    { "state": "pr", "handler": { "kind": "terminal" } }
  ],
  "transitions": [
    { "from": "implementation", "to": "review", "on": "success" },
    { "from": "review", "to": "pr", "on": "success" },
    { "from": "review", "to": "implementation", "on": "pushback:code-fix" }
  ]
}`

func TestValidate_ValidSDLC(t *testing.T) {
	d, err := Parse([]byte(sdlcJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(d); err != nil {
		t.Fatalf("valid SDLC graph should pass, got: %v", err)
	}
}

func TestValidate_ValidLinear(t *testing.T) {
	d, _ := Parse([]byte(`{
	  "entry": "draft",
	  "states": [
	    {"state":"draft","handler":{"kind":"agent","agent":"writer"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions": [{"from":"draft","to":"done","on":"success"}]
	}`))
	if err := Validate(d); err != nil {
		t.Fatalf("valid linear graph should pass, got: %v", err)
	}
}

func TestValidate_Rejections(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string // substring of the error
	}{
		{"empty entry",
			`{"entry":"","states":[{"state":"a","handler":{"kind":"terminal"}}]}`, "`entry` is required"},
		{"no states",
			`{"entry":"a","states":[]}`, "at least one state"},
		{"entry not a state",
			`{"entry":"x","states":[{"state":"a","handler":{"kind":"terminal"}}]}`, "does not resolve"},
		{"duplicate state",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"terminal"}},{"state":"a","handler":{"kind":"terminal"}}]}`, "duplicate state"},
		{"dangling transition to",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"agent","agent":"w"}}],"transitions":[{"from":"a","to":"ghost","on":"success"}]}`, "does not resolve"},
		{"bad on label",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"agent","agent":"w"}},{"state":"b","handler":{"kind":"terminal"}}],"transitions":[{"from":"a","to":"b","on":"maybe"}]}`, "invalid `on`"},
		{"unreachable state",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"terminal"}},{"state":"island","handler":{"kind":"terminal"}}]}`, "unreachable"},
		{"parallel without consolidator",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"parallel","agents":["x"]}}]}`, "requires a `consolidator`"},
		{"parallel without agents",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"parallel","consolidator":"c"}}]}`, "non-empty `agents`"},
		{"agent without agent name",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"agent"}}]}`, "requires `agent`"},
		{"unknown handler kind",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"delay"}}]}`, "unknown handler kind"},
		{"missing handler kind",
			`{"entry":"a","states":[{"state":"a","handler":{}}]}`, "missing a `kind`"},
		{"duplicate outbound label",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"agent","agent":"w"}},{"state":"b","handler":{"kind":"terminal"}},{"state":"c","handler":{"kind":"terminal"}}],"transitions":[{"from":"a","to":"b","on":"success"},{"from":"a","to":"c","on":"success"}]}`, "duplicate outbound transition label"},
		{"terminal with outbound",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"terminal"}},{"state":"b","handler":{"kind":"terminal"}}],"transitions":[{"from":"a","to":"b","on":"success"}]}`, "terminal state"},
		{"bad wait",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"parallel","agents":["x"],"consolidator":"c","wait":"most"}}]}`, "invalid wait"},
		{"empty pushback reason",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"agent","agent":"w"}},{"state":"b","handler":{"kind":"terminal"}}],"transitions":[{"from":"a","to":"b","on":"pushback:"}]}`, "non-empty reason"},
		{"negative max_iterations",
			`{"entry":"a","max_iterations":-1,"states":[{"state":"a","handler":{"kind":"terminal"}}]}`, "max_iterations"},
		{"max_iterations over ceiling",
			`{"entry":"a","max_iterations":100000,"states":[{"state":"a","handler":{"kind":"terminal"}}]}`, "exceeds the maximum"},
		{"non-terminal dead end",
			`{"entry":"a","states":[{"state":"a","handler":{"kind":"agent","agent":"w"}}],"transitions":[]}`, "no outbound transition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Parse([]byte(tc.json))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			err = Validate(d)
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
