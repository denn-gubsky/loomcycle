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
		{"no Git tool", AgentDef{Tools: []string{"Read"}, GitRemote: []string{"fetch"}}, Env{}, []string{"Git is not in tools"}},
		{"push without hosts", AgentDef{Tools: []string{"Git"}, GitRemote: []string{"fetch", "push"}}, Env{}, []string{"LOOMCYCLE_GIT_PUSH_HOSTS is empty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(gitRemoteWarnings("a", tc.def, tc.env), "\n")
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

// LOOMCYCLE_GIT_PUSH_HOSTS, LOOMCYCLE_GIT_AUTHOR_NAME/EMAIL and
// LOOMCYCLE_GIT_PUSH_MAX_BYTES are read; the cap defaults to 100 MiB, and a
// malformed cap is a load error rather than the default.
func TestLoad_GitEnvKnobs(t *testing.T) {
	cfg, err := loadYAMLString(t, gitRemoteYAMLHead)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env.GitPushMaxBytes != 100<<20 || len(cfg.Env.GitPushHosts) != 0 {
		t.Errorf("defaults: push max %d, hosts %v", cfg.Env.GitPushMaxBytes, cfg.Env.GitPushHosts)
	}

	t.Setenv("LOOMCYCLE_GIT_PUSH_HOSTS", " github.com , git.example.com ,")
	t.Setenv("LOOMCYCLE_GIT_AUTHOR_NAME", "Loom Bot")
	t.Setenv("LOOMCYCLE_GIT_AUTHOR_EMAIL", "bot@example.com")
	t.Setenv("LOOMCYCLE_GIT_PUSH_MAX_BYTES", "1048576")
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
	if cfg.Env.GitPushMaxBytes != 1<<20 {
		t.Errorf("GitPushMaxBytes = %d", cfg.Env.GitPushMaxBytes)
	}

	for _, bad := range []string{"100MiB", "0", "-5"} {
		t.Setenv("LOOMCYCLE_GIT_PUSH_MAX_BYTES", bad)
		if _, err := loadYAMLString(t, gitRemoteYAMLHead); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_GIT_PUSH_MAX_BYTES") {
			t.Errorf("LOOMCYCLE_GIT_PUSH_MAX_BYTES=%q: err = %v, want a load error naming the variable", bad, err)
		}
	}
}
