package teamgraph

import (
	"fmt"
	"strings"
	"testing"
)

func declaring(vars map[string]string) Definition {
	d := defWith(State{ID: "work", Handler: Handler{Kind: HandlerAgent, Agent: "a"}}, terminal("done"))
	d.Vars = vars
	return d
}

func TestValidate_VarsBlockRefusesWhatAVariableMayNotBe(t *testing.T) {
	tooMany := map[string]string{}
	for i := 0; i <= MaxVars; i++ {
		tooMany[fmt.Sprintf("v%d", i)] = ""
	}
	atTheCap := map[string]string{}
	for i := 0; i < MaxVars; i++ {
		atTheCap[fmt.Sprintf("v%d", i)] = ""
	}
	for name, tc := range map[string]struct {
		vars    map[string]string
		wantErr string // "" = accepted
	}{
		"a default":                     {map[string]string{"tone": "formal"}, ""},
		"an empty default":              {map[string]string{"tone": ""}, ""},
		"every name character":          {map[string]string{"a-Z_09": "x"}, ""},
		"a ${…} is literal text":        {map[string]string{"x": "${var.other} on ${now.date}"}, ""},
		"a single brace":                {map[string]string{"x": "{not a placeholder}"}, ""},
		"the most variables":            {atTheCap, ""},
		"the longest value":             {map[string]string{"x": strings.Repeat("a", MaxVarValueBytes)}, ""},
		"a name with a dot":             {map[string]string{"a.b": "x"}, `vars key "a.b" must match`},
		"a name with a space":           {map[string]string{"a b": "x"}, `vars key "a b" must match`},
		"an empty name":                 {map[string]string{"": "x"}, `vars key "" must match`},
		"a name of 65 characters":       {map[string]string{strings.Repeat("n", 65): "x"}, "must match"},
		"an opening placeholder":        {map[string]string{"x": "{{document:/secret"}, `vars "x": the value contains {{ or }}`},
		"a closing placeholder":         {map[string]string{"x": "a }} b"}, `vars "x": the value contains {{ or }}`},
		"a credential":                  {map[string]string{"x": "${run.credentials.GITHUB_TOKEN}"}, "credentials namespace"},
		"the user bearer":               {map[string]string{"x": "Bearer ${run.user_bearer}"}, "credentials namespace"},
		"one variable over the cap":     {tooMany, "more than the maximum 64"},
		"one byte over the value cap":   {map[string]string{"x": strings.Repeat("a", MaxVarValueBytes+1)}, "more than the maximum 4096"},
		"the bad entry among good ones": {map[string]string{"a": "ok", "b": "{{x}}", "c": "ok"}, `vars "b"`},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate(declaring(tc.vars))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("accepted, want a refusal containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// The hash below was computed by the code as it was BEFORE `vars` existed, so
// this fails if adding the field moved the bytes a definition without one
// marshals to — which would orphan every content_sha256 already recorded.
func TestSign_DefinitionWithoutVarsKeepsItsRecordedHash(t *testing.T) {
	const recorded = "sha256:4bf8e3695555e0a584bf3c0b2d4ff82808564ff36a767515d069597eb00e3a09"
	d, err := Parse([]byte(`{"entry":"review","max_iterations":3,"states":[{"state":"review","handler":{"kind":"agent","agent":"reviewer","input_template":"Tone: ${var.tone}"}},{"state":"done","handler":{"kind":"terminal"}}],"transitions":[{"from":"review","to":"done","on":"success"}],"channels":{"publish":["out"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Sign("vars-pin", d); got != recorded {
		t.Errorf("hash = %s, want the recorded %s", got, recorded)
	}
	// Declaring none is the same definition as never having the block.
	d.Vars = map[string]string{}
	if got := Sign("vars-pin", d); got != recorded {
		t.Errorf("an empty vars block hashes %s, want the recorded %s", got, recorded)
	}
}

func TestSign_VarsAreContent(t *testing.T) {
	base := declaring(nil)
	none := Sign("t", base)
	formal := Sign("t", declaring(map[string]string{"tone": "formal"}))
	if formal == none {
		t.Error("declaring a variable did not change the hash")
	}
	if Sign("t", declaring(map[string]string{"tone": "casual"})) == formal {
		t.Error("changing a default did not change the hash")
	}
	if Sign("t", declaring(map[string]string{"tone": ""})) == none {
		t.Error("a variable declared with an empty default hashes like no variable — the declaration is content")
	}
	if Sign("t", declaring(map[string]string{"voice": "formal"})) == formal {
		t.Error("renaming a variable did not change the hash")
	}
	if Sign("t", declaring(map[string]string{"tone": "formal"})) != formal {
		t.Error("the same declaration hashed differently twice")
	}
}

func TestCheckStartVars_AcceptsOnlyDeclaredNamesAndWritableValues(t *testing.T) {
	d := declaring(map[string]string{"tone": "formal", "lang": ""})
	for name, tc := range map[string]struct {
		def      Definition
		supplied map[string]string
		wantErr  string
	}{
		"nothing supplied":          {d, nil, ""},
		"a declared name":           {d, map[string]string{"tone": "casual"}, ""},
		"a declared name, blanked":  {d, map[string]string{"tone": ""}, ""},
		"an undeclared name":        {d, map[string]string{"tone": "x", "tome": "y"}, `"tome" is not a variable of this team (declared: lang, tone)`},
		"a team that declares none": {declaring(nil), map[string]string{"tone": "x"}, `"tone" is not a variable of this team — it declares none`},
		"a placeholder":             {d, map[string]string{"tone": "{{document:/secret}}"}, `vars "tone": the value contains {{ or }}`},
		"a credential":              {d, map[string]string{"tone": "${run.credentials.K}"}, "credentials namespace"},
		"a value over the cap":      {d, map[string]string{"tone": strings.Repeat("a", MaxVarValueBytes+1)}, "more than the maximum"},
		"a ${…} is literal text":    {d, map[string]string{"tone": "${var.lang}"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckStartVars(tc.def, tc.supplied)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("accepted, want a refusal containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
