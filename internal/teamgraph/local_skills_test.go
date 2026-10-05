package teamgraph

import (
	"fmt"
	"strings"
	"testing"
)

// skillTeamJSON is localJSON with a team skill its reviewer is granted.
const skillTeamJSON = `{
  "entry": "review",
  "local": {
    "agents": {"reviewer": {"tier": "middle", "tools": ["Read", "Skill"], "skills": ["./style"], "system_prompt": "You review diffs."}},
    "skills": {"style": {"body": "Prefer short functions.", "description": "House style", "tools": ["Read"]}}
  },
  "states": [
    {"state": "review", "handler": {"kind": "agent", "agent": "./reviewer"}},
    {"state": "done", "handler": {"kind": "terminal"}}
  ],
  "transitions": [{"from": "review", "to": "done", "on": "success"}]
}`

func TestParse_DecodesLocalSkills(t *testing.T) {
	d := mustParse(t, skillTeamJSON)
	sk, ok := d.LocalSkill("style")
	if !ok || sk.Body != "Prefer short functions." || sk.Description != "House style" || len(sk.Tools) != 1 || sk.Tools[0] != "Read" {
		t.Fatalf("LocalSkill(style) = %+v, %v", sk, ok)
	}
	if err := Validate(d); err != nil {
		t.Fatalf("a team granting its own declared skill must validate: %v", err)
	}
}

// A skill has a fixed shape. A misspelt field must be refused rather than
// dropped, or the team stores a skill without the part its author wrote.
func TestParse_RefusesLocalSkillThatIsNotExactlyASkill(t *testing.T) {
	for _, body := range []string{`{"bdy":"x"}`, `"x"`, `[]`, `null`, `{"body":3}`} {
		_, err := Parse([]byte(`{"entry":"s","local":{"skills":{"a":` + body + `}}}`))
		if err == nil {
			t.Errorf("local skill %s must be refused", body)
			continue
		}
		if !strings.Contains(err.Error(), `local.skills["a"]`) {
			t.Errorf("local skill %s: refusal %q should name the skill", body, err)
		}
	}
}

func TestValidate_RefusesUndeclaredLocalSkillGrant(t *testing.T) {
	noSkills := strings.Replace(skillTeamJSON, `,
    "skills": {"style": {"body": "Prefer short functions.", "description": "House style", "tools": ["Read"]}}`, "", 1)
	for _, tc := range []struct{ what, def string }{
		{"declares other skills", strings.Replace(skillTeamJSON, `"./style"`, `"./ghost"`, 1)},
		{"granted with +", strings.Replace(skillTeamJSON, `"./style"`, `"+./ghost"`, 1)},
		{"declares none", noSkills},
	} {
		if tc.def == skillTeamJSON {
			t.Fatalf("%s: fixture replacement did not apply", tc.what)
		}
		err := Validate(mustParse(t, tc.def))
		if err == nil || !strings.Contains(err.Error(), `local.agents["reviewer"].skills`) || !strings.Contains(err.Error(), "does not declare") {
			t.Errorf("%s: Validate = %v, want a refusal naming the agent and the undeclared skill", tc.what, err)
		}
	}
}

// Only an exact "./<name>" grants a team's own skill, so a pattern written as
// a local grant is refused: it would read as granting skills and grant none.
func TestValidate_RefusesPatternAsLocalSkillGrant(t *testing.T) {
	for _, grant := range []string{"./*", "./**", "./sty*", "./st?le"} {
		err := Validate(mustParse(t, strings.Replace(skillTeamJSON, `"./style"`, quote(grant), 1)))
		if err == nil || !strings.Contains(err.Error(), "exact name") {
			t.Errorf("grant %q: Validate = %v, want it refused as a pattern", grant, err)
		}
	}
}

func TestValidate_RefusesBadLocalSkillName(t *testing.T) {
	for _, name := range []string{"has/slash", "has space", "dot.dot", strings.Repeat("x", MaxLocalNameLen+1)} {
		def := strings.Replace(skillTeamJSON, `"skills": {"style"`, `"skills": {`+quote(name), 1)
		def = strings.Replace(def, `"./style"`, `"./ok"`, 1) // keep the grant out of it
		def = strings.Replace(def, `"skills": ["./ok"]`, `"skills": []`, 1)
		if err := Validate(mustParse(t, def)); err == nil || !strings.Contains(err.Error(), "local.skills") {
			t.Errorf("local skill name %q: Validate = %v, want refused", name, err)
		}
	}
}

func TestValidate_CapsTheNumberOfLocalSkills(t *testing.T) {
	skills := make([]string, 0, MaxLocalSkills+1)
	for i := 0; i <= MaxLocalSkills; i++ {
		skills = append(skills, fmt.Sprintf(`"s%d":{"body":"b"}`, i))
	}
	def := `{"entry":"s","local":{"skills":{` + strings.Join(skills, ",") + `}},
	  "states":[{"state":"s","handler":{"kind":"agent","agent":"x"}},{"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"s","to":"done","on":"success"}]}`
	if err := Validate(mustParse(t, def)); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Errorf("Validate = %v, want a refusal for more than %d local skills", err, MaxLocalSkills)
	}
}

// recordedLocalAgentsHash is Sign("t", localJSON) as it was before local
// skills existed. A team declaring only agents must keep its hash.
const recordedLocalAgentsHash = "sha256:bf5bfac59d3231cf205385fe7267121c52a39e286b49adc5123dc13e4944f304"

func TestSign_TeamWithoutLocalSkillsKeepsItsRecordedHash(t *testing.T) {
	d := mustParse(t, localJSON)
	if got := Sign("t", d); got != recordedLocalAgentsHash {
		t.Fatalf("a team declaring only agents hashes %s, recorded %s", got, recordedLocalAgentsHash)
	}
	d.Local.Skills = map[string]LocalSkill{}
	if got := Sign("t", d); got != recordedLocalAgentsHash {
		t.Errorf("an empty local.skills changed the hash to %s", got)
	}
	// And a team declaring nothing at all keeps the hash it had before `local`.
	n := mustParse(t, sdlcJSON)
	n.Local = &Local{Skills: map[string]LocalSkill{}}
	if got := Sign("sdlc", n); got != recordedNoLocalHash {
		t.Errorf("an empty local.skills on a team without local agents changed the hash to %s", got)
	}
}

func TestSign_LocalSkillIsContent(t *testing.T) {
	base := Sign("t", mustParse(t, skillTeamJSON))
	for what, def := range map[string]string{
		"body":        strings.Replace(skillTeamJSON, "Prefer short functions.", "Prefer long functions.", 1),
		"description": strings.Replace(skillTeamJSON, "House style", "Style", 1),
		"tools":       strings.Replace(skillTeamJSON, `"tools": ["Read"]}}`, `"tools": ["Read","Grep"]}}`, 1),
	} {
		if def == skillTeamJSON {
			t.Fatalf("%s: fixture replacement did not apply", what)
		}
		if Sign("t", mustParse(t, def)) == base {
			t.Errorf("editing a local skill's %s must change the team's content hash", what)
		}
	}
	// Key order and whitespace inside a skill are not content.
	relaid := strings.Replace(skillTeamJSON,
		`{"body": "Prefer short functions.", "description": "House style", "tools": ["Read"]}`,
		`{ "tools":["Read"],"description":"House style",  "body":"Prefer short functions." }`, 1)
	if relaid == skillTeamJSON || Sign("t", mustParse(t, relaid)) != base {
		t.Error("re-laying a local skill's JSON changed the team's content hash")
	}
	without := mustParse(t, skillTeamJSON)
	without.Local.Skills = nil
	if Sign("t", without) == base {
		t.Error("removing the local skills must change the team's content hash")
	}
}

// A glob in an agent's skills list governs global skills only; a team's own
// skill is reached by an exact "./<name>" and by nothing else, and a negative
// entry still denies it.
func TestLocalSkillGranted_OnlyAnExactLocalEntryGrants(t *testing.T) {
	for _, tc := range []struct {
		skills []string
		want   bool
	}{
		{[]string{"./style"}, true},
		{[]string{"+./style"}, true},
		{[]string{" ./style "}, true},
		{[]string{"doc/*", "./style"}, true},
		{nil, false},
		{[]string{"*"}, false},
		{[]string{"**"}, false},
		{[]string{"+*"}, false},
		{[]string{"./*"}, false},
		{[]string{"sdlc/*"}, false},
		{[]string{"sdlc/style"}, false},
		{[]string{"style"}, false},
		{[]string{"./other"}, false},
		{[]string{"./style", "-*"}, false},
		{[]string{"./style", "-./style"}, false},
		{[]string{"-./style"}, false},
		{[]string{"./style", "-doc/*"}, true},
	} {
		if got := LocalSkillGranted(tc.skills, "style"); got != tc.want {
			t.Errorf("LocalSkillGranted(%q, style) = %v, want %v", tc.skills, got, tc.want)
		}
	}
}
