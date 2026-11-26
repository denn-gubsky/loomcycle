package config

import (
	"strings"
	"testing"
)

const gitRemoteYAMLHead = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  coder:
    model: claude-sonnet-4-6
    tools: [Git]
`

// RFC FB: git_remote reaches the agent as declared; absent and [] both mean
// no network, so either may load as nil or empty.
func TestLoad_GitRemoteReachesTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        string
	}{
		{"absent", "", ""},
		{"empty", "    git_remote: []\n", ""},
		{"fetch", "    git_remote: [fetch]\n", "fetch"},
		{"fetch and push", "    git_remote: [fetch, push]\n", "fetch,push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAMLString(t, gitRemoteYAMLHead+tc.field)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(cfg.Agents["coder"].GitRemote, ","); got != tc.want {
				t.Errorf("GitRemote = %q, want %q", got, tc.want)
			}
		})
	}
}

// A list holding an unknown value, a duplicate, or push without fetch is
// refused at load, naming the agent and what is wrong.
func TestLoad_RefusesAnInvalidGitRemote(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        []string
	}{
		{"push alone", "    git_remote: [push]\n", []string{`"coder"`, "push needs fetch"}},
		{"unknown value", "    git_remote: [fetch, clone]\n", []string{`"coder"`, "git_remote[1]", `"clone"`}},
		{"duplicate", "    git_remote: [fetch, fetch]\n", []string{`"coder"`, "git_remote[1]", "twice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAMLString(t, gitRemoteYAMLHead+tc.field)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q: %v", w, err)
				}
			}
		})
	}
}

func TestValidateGitRemote(t *testing.T) {
	for _, tc := range []struct {
		in []string
		ok bool
	}{
		{nil, true},
		{[]string{}, true},
		{[]string{"fetch"}, true},
		{[]string{"fetch", "push"}, true},
		{[]string{"push", "fetch"}, true},
		{[]string{"push"}, false},
		{[]string{"Fetch"}, false},
		{[]string{""}, false},
		{[]string{"push", "push", "fetch"}, false},
	} {
		if err := ValidateGitRemote(tc.in); (err == nil) != tc.ok {
			t.Errorf("ValidateGitRemote(%q) = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

// The field on an agent without the Git tool allows nothing, and a push with
// no LOOMCYCLE_GIT_PUSH_HOSTS is always refused: both advised at boot, never
// refused.
func TestGitRemoteWarnings(t *testing.T) {
	hosts := Env{GitPushHosts: []string{"github.com"}}
	for _, tc := range []struct {
		name string
		def  AgentDef
		env  Env
		want []string // phrases; nil = no advisory
	}{
		{"absent", AgentDef{Tools: []string{"Git"}}, Env{}, nil},
		{"fetch with Git", AgentDef{Tools: []string{"Git"}, GitRemote: []string{"fetch"}}, Env{}, nil},
		{"push with a host list", AgentDef{Tools: []string{"Git"}, GitRemote: []string{"fetch", "push"}}, hosts, nil},
		{"no Git tool", AgentDef{Tools: []string{"Read"}, GitRemote: []string{"fetch"}}, Env{}, []string{"neither Git nor Workspace is in tools"}},
		// RFC GW P11: git_remote also governs a git workspace's network ops.
		{"fetch with Workspace", AgentDef{Tools: []string{"Workspace"}, GitRemote: []string{"fetch"}}, Env{}, nil},
		{"push without hosts", AgentDef{Tools: []string{"Git"}, GitRemote: []string{"fetch", "push"}}, Env{}, []string{"LOOMCYCLE_GIT_PUSH_HOSTS is empty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(gitRemoteWarnings("a", tc.def, tc.env, false), "\n")
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

// A yaml entry that only grants git_remote is an overlay on an existing
// agent, like the other grant-only declarations, and a yaml overlay without
// the field keeps the discovered agent's.
func TestMergeAgentDef_GitRemote(t *testing.T) {
	if !looksLikeAnOverrideOnly(AgentDef{GitRemote: []string{"fetch"}}) {
		t.Error("a git_remote-only declaration was not recognised as an override")
	}
	base := AgentDef{GitRemote: []string{"fetch", "push"}}
	if got := mergeAgentDef(base, AgentDef{GitRemote: []string{"fetch"}}).GitRemote; strings.Join(got, ",") != "fetch" {
		t.Errorf("an overlay of [fetch] gave %v", got)
	}
	if got := mergeAgentDef(base, AgentDef{}).GitRemote; strings.Join(got, ",") != "fetch,push" {
		t.Errorf("an overlay without the field gave %v, want the base's", got)
	}
}

// LOOMCYCLE_GIT_PUSH_HOSTS and LOOMCYCLE_GIT_AUTHOR_NAME/EMAIL are read.
func TestLoad_GitEnvKnobs(t *testing.T) {
	cfg, err := loadYAMLString(t, gitRemoteYAMLHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Env.GitPushHosts) != 0 {
		t.Errorf("defaults: hosts %v", cfg.Env.GitPushHosts)
	}

	t.Setenv("LOOMCYCLE_GIT_PUSH_HOSTS", " github.com , git.example.com ,")
	t.Setenv("LOOMCYCLE_GIT_AUTHOR_NAME", "Loom Bot")
	t.Setenv("LOOMCYCLE_GIT_AUTHOR_EMAIL", "bot@example.com")
	cfg, err = loadYAMLString(t, gitRemoteYAMLHead)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Env.GitPushHosts, ","); got != "github.com,git.example.com" {
		t.Errorf("GitPushHosts = %q", got)
	}
	if cfg.Env.GitAuthorName != "Loom Bot" || cfg.Env.GitAuthorEmail != "bot@example.com" {
		t.Errorf("author = %q <%q>", cfg.Env.GitAuthorName, cfg.Env.GitAuthorEmail)
	}
}

// RFC GW P14: a bundle's own git_remote (team/orchestrator's push for the
// team/repo skill) is not advised on while LOOMCYCLE_GIT_PUSH_HOSTS is
// empty, as a bundle's own workspace grant is not called inert (D-P14-18):
// a default install must load without an advisory. An operator's change to
// it is the operator's, and is still advised on.
func TestGitRemoteWarnings_ABundlesOwnPushIsNotAdvisedWithoutPushHosts(t *testing.T) {
	bundle := Layer{Name: "agent-teams", Embedded: true, Data: []byte(`agents:
  team/orchestrator:
    description: a team lead
    tools: [Workspace, Bashbox]
    workspace: {ops: [read, write, exec], open: true}
    git_remote: [fetch, push]
`)}
	pushAdvisories := func(cfg *Config) []string {
		var out []string
		for _, w := range cfg.Warnings {
			if strings.Contains(w, "LOOMCYCLE_GIT_PUSH_HOSTS is empty") {
				out = append(out, w)
			}
		}
		return out
	}
	cfg, err := LoadLayers(bundle, Layer{Name: "t.yaml", Data: []byte(plainYAML)})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if got := pushAdvisories(cfg); len(got) != 0 {
		t.Errorf("a bundle's own git_remote was advised on: %q", got)
	}
	// An operator's own agent with push is still advised on.
	own := Layer{Name: "t.yaml", Data: []byte(plainYAML + `agents:
  my/pusher:
    description: an operator's agent
    tools: [Workspace]
    workspace: {ops: [read], open: true}
    git_remote: [fetch, push]
`)}
	cfg, err = LoadLayers(bundle, own)
	if err != nil {
		t.Fatalf("LoadLayers with an operator agent: %v", err)
	}
	if got := pushAdvisories(cfg); len(got) != 1 || !strings.Contains(got[0], "my/pusher") {
		t.Errorf("an operator's own push: advisories %q, want one naming my/pusher", got)
	}
}
