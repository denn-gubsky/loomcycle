package config

import (
	"encoding/json"
	"strings"
	"testing"
)

const shellCredsYAMLHead = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  coder:
    model: claude-sonnet-4-6
    tools: [Bash]
`

// RFC EW: absent and [] are different declarations, and both reach the
// agent as declared: absent narrows nothing, [] keeps no credential.
func TestLoad_ShellCredentialsKeepsAbsentApartFromEmpty(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		wantNil     bool
		want        []string
	}{
		{"absent", "", true, nil},
		{"empty", "    shell_credentials: []\n", false, []string{}},
		{"two names", "    shell_credentials: [GITHUB_TOKEN, NPM_TOKEN]\n", false, []string{"GITHUB_TOKEN", "NPM_TOKEN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAMLString(t, shellCredsYAMLHead+tc.field)
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Agents["coder"].ShellCredentials
			if (got == nil) != tc.wantNil {
				t.Fatalf("ShellCredentials = %v, want nil=%v", got, tc.wantNil)
			}
			if got != nil && strings.Join(*got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ShellCredentials = %v, want %v", *got, tc.want)
			}
		})
	}
}

// A name that is not a credential name is refused at load, naming the agent,
// the entry and the pattern.
func TestLoad_RefusesAShellCredentialThatIsNotAName(t *testing.T) {
	_, err := loadYAMLString(t, shellCredsYAMLHead+"    shell_credentials: [GITHUB_TOKEN, \"git:GH_TOKEN\"]\n")
	if err == nil {
		t.Fatal("expected an error for a malformed credential name")
	}
	for _, want := range []string{`"coder"`, "shell_credentials[1]", `"git:GH_TOKEN"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A well-formed name the operator does not list keeps nothing; the field on
// an agent without a shell tool narrows nothing. Both are advised at boot,
// never refused.
func TestShellCredentialWarnings(t *testing.T) {
	env := Env{BashAllowedCreds: []string{"A"}, BashboxFallbackAllowedCreds: []string{"git:B"}}
	list := func(n ...string) *CredentialNames { c := CredentialNames(n); return &c }
	for _, tc := range []struct {
		name string
		def  AgentDef
		want []string // phrases; nil = no advisory
	}{
		{"absent", AgentDef{Tools: []string{"Bash"}}, nil},
		{"names the operator lists", AgentDef{Tools: []string{"Bash"}, ShellCredentials: list("A", "B")}, nil},
		{"empty", AgentDef{Tools: []string{"Bashbox"}, ShellCredentials: list()}, nil},
		{"a name no list has", AgentDef{Tools: []string{"Bash"}, ShellCredentials: list("A", "C")}, []string{"lists C", "keep nothing"}},
		{"no shell tool", AgentDef{Tools: []string{"Read"}, ShellCredentials: list("A")}, []string{"neither Bash nor Bashbox"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(shellCredentialWarnings("a", tc.def, env), "\n")
			if tc.want == nil {
				if got != "" {
					t.Fatalf("unexpected advisory: %s", got)
				}
				return
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("advisory does not say %q: %s", w, got)
				}
			}
		})
	}
}

// A yaml entry that only narrows shell credentials is an overlay on an
// existing agent, like the other grant-only declarations.
func TestLooksLikeAnOverrideOnly_CountsShellCredentials(t *testing.T) {
	none := CredentialNames{}
	if !looksLikeAnOverrideOnly(AgentDef{ShellCredentials: &none}) {
		t.Error("a shell_credentials-only declaration was not recognised as an override")
	}
}

// A yaml overlay over a discovered agent sets the field, empty included, and
// an overlay without it keeps the discovered agent's.
func TestMergeAgentDef_ShellCredentials(t *testing.T) {
	base := CredentialNames{"A"}
	empty := CredentialNames{}
	if got := mergeAgentDef(AgentDef{ShellCredentials: &base}, AgentDef{ShellCredentials: &empty}).ShellCredentials; got == nil || len(*got) != 0 {
		t.Errorf("an overlay of [] gave %v, want []", got)
	}
	if got := mergeAgentDef(AgentDef{ShellCredentials: &base}, AgentDef{}).ShellCredentials; got == nil || strings.Join(*got, ",") != "A" {
		t.Errorf("an overlay without the field gave %v, want the base's [A]", got)
	}
}

// A present-but-empty shell_credentials means none, and JSON null reads back
// as absent (every credential), so a nil list always encodes as [] — even one
// reached through a pointer, the shape a stored definition holds.
func TestCredentialNames_NilEncodesAsAnEmptyList(t *testing.T) {
	var nilList CredentialNames
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"nil list", nilList, `[]`},
		{"pointer to a nil list", &nilList, `[]`},
		{"names", CredentialNames{"A"}, `["A"]`},
		{"nil pointer stays null (absent)", (*CredentialNames)(nil), `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.in)
			if err != nil || string(b) != tc.want {
				t.Errorf("got %s (%v), want %s", b, err, tc.want)
			}
		})
	}
}
