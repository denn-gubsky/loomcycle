package agents

import (
	"strings"
	"testing"
)

// RFC FB: a Markdown agent's frontmatter carries git_remote to the agent.
func TestParseAgent_GitRemote(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
	}{
		{"absent", "", ""},
		{"empty", "git_remote: []\n", ""},
		{"fetch and push", "git_remote: [fetch, push]\n", "fetch,push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := parseAgent([]byte("---\nname: coder\ndescription: d\n" + tc.line + "---\nbody\n"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(a.GitRemote, ","); got != tc.want {
				t.Errorf("GitRemote = %q, want %q", got, tc.want)
			}
		})
	}
}

// git_remote is authority, not authored behaviour: two agents differing only
// in it hash identically on the yaml path and the overlay path.
func TestSign_GitRemoteExcludedFromHash(t *testing.T) {
	bare := FromYAMLAgent(&Agent{Name: "x", SystemPrompt: "p"})
	with := FromYAMLAgent(&Agent{Name: "x", SystemPrompt: "p", GitRemote: []string{"fetch", "push"}})
	if Sign(bare) != Sign(with) {
		t.Error("git_remote leaked into the yaml-path hash")
	}
	a, err := FromOverlay([]byte(`{"system_prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromOverlay([]byte(`{"system_prompt":"p","git_remote":["fetch","push"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if Sign(a) != Sign(b) {
		t.Error("git_remote leaked into the overlay-path hash")
	}
}
