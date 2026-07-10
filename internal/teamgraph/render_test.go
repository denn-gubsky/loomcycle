package teamgraph

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) Definition {
	t.Helper()
	d, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

func TestResolve_KeywordHeuristic(t *testing.T) {
	d := mustParse(t, `{
	  "entry":"rfc_review",
	  "states":[
	    {"state":"rfc_review","handler":{"kind":"agent","agent":"rfc-reviewer"}},
	    {"state":"architecture","handler":{"kind":"agent","agent":"architect"}},
	    {"state":"planning","handler":{"kind":"agent","agent":"planner"}},
	    {"state":"implementation","handler":{"kind":"agent","agent":"code-guru"}},
	    {"state":"peer_review","handler":{"kind":"agent","agent":"reviewer"}},
	    {"state":"qa_verification","handler":{"kind":"agent","agent":"qa"}},
	    {"state":"pr","handler":{"kind":"terminal"}}
	  ]}`)
	sc := Resolve(d)
	want := map[string]string{
		"rfc_review":      namedHues["cyan"].fill,
		"architecture":    namedHues["yellow"].fill,
		"planning":        namedHues["orange"].fill,
		"implementation":  namedHues["blue"].fill,
		"peer_review":     namedHues["pink"].fill, // token "review" prefixes → pink
		"qa_verification": namedHues["red"].fill,
		"pr":              namedHues["green"].fill,
	}
	for id, exp := range want {
		if sc.Fill[id] != exp {
			t.Errorf("state %q fill = %q, want %q", id, sc.Fill[id], exp)
		}
	}
	// "approved" must NOT match the "pr" keyword (substring-vs-prefix guard).
	d2 := mustParse(t, `{"entry":"approved","states":[{"state":"approved","handler":{"kind":"terminal"}}]}`)
	if Resolve(d2).Fill["approved"] == namedHues["green"].fill {
		// green is the rotation[7] fallback too, so only fail if it matched via keyword —
		// approve is the only state so rotation[0]=cyan; green here would mean a bug.
		t.Errorf("'approved' wrongly matched the 'pr' keyword")
	}
}

func TestResolve_ExplicitOverrideAndRotation(t *testing.T) {
	d := mustParse(t, `{
	  "entry":"a",
	  "colors":{"states":{"a":"violet","b":"#123456"}},
	  "states":[
	    {"state":"a","handler":{"kind":"agent","agent":"x"}},
	    {"state":"b","handler":{"kind":"agent","agent":"y"}},
	    {"state":"c","handler":{"kind":"terminal"}}
	  ]}`)
	sc := Resolve(d)
	if sc.Fill["a"] != namedHues["violet"].fill {
		t.Errorf("named override: a fill = %q, want violet", sc.Fill["a"])
	}
	if sc.Fill["b"] != "#123456" {
		t.Errorf("hex override: b fill = %q, want #123456", sc.Fill["b"])
	}
	if sc.Fill["c"] == "" {
		t.Errorf("c should get a rotation fill")
	}
}

func TestEdgeColor(t *testing.T) {
	d := Definition{}
	cases := map[string]string{
		"success":              namedHues["green"].accent,
		"conditional:$.x=='p'": namedHues["blue"].accent,
		"pushback:revise":      namedHues["orange"].accent,
		"pushback:qa-failure":  namedHues["red"].accent, // *qa* → red
	}
	for on, want := range cases {
		if got := EdgeColor(d, on); got != want {
			t.Errorf("EdgeColor(%q) = %q, want %q", on, got, want)
		}
	}
	// per-def override wins (exact label, then kind).
	d2 := Definition{Colors: &Colors{Transitions: map[string]string{"success": "#abcdef", "pushback": "yellow"}}}
	if EdgeColor(d2, "success") != "#abcdef" {
		t.Errorf("exact override failed")
	}
	if EdgeColor(d2, "pushback:revise") != namedHues["yellow"].accent {
		t.Errorf("kind override (named) failed")
	}
}

func TestRenderMermaid_Structure(t *testing.T) {
	d := mustParse(t, sdlcJSON)
	out := RenderMermaid("sdlc", d, "review")
	for _, want := range []string{
		"stateDiagram-v2",
		"[*] --> implementation",
		"implementation --> review: success",
		"review --> implementation: pushback:code-fix",
		"pr --> [*]",
		"note right of review",
		"parallel: sec-rev, code-rev (wait: all)",
		"consolidator: sdlc-consolidator",
		"classDef c", // at least one fill class emitted
		"_hl ",       // highlight class for the highlighted state
		"class review c",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Mermaid missing %q\n---\n%s", want, out)
		}
	}
	// Deterministic.
	if RenderMermaid("sdlc", d, "review") != out {
		t.Errorf("RenderMermaid is not deterministic")
	}
}
