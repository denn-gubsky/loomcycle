package agents

import "testing"

// RFC EW: a Markdown agent's frontmatter keeps an absent shell_credentials
// apart from an empty one, as the yaml path does.
func TestParseAgent_ShellCredentialsKeepsAbsentApartFromEmpty(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		wantNil    bool
		wantLen    int
	}{
		{"absent", "", true, 0},
		{"empty", "shell_credentials: []\n", false, 0},
		{"one name", "shell_credentials: [GITHUB_TOKEN]\n", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := parseAgent([]byte("---\nname: coder\ndescription: d\n" + tc.line + "---\nbody\n"))
			if err != nil {
				t.Fatal(err)
			}
			if (a.ShellCredentials == nil) != tc.wantNil {
				t.Fatalf("ShellCredentials = %v, want nil=%v", a.ShellCredentials, tc.wantNil)
			}
			if a.ShellCredentials != nil && len(*a.ShellCredentials) != tc.wantLen {
				t.Errorf("ShellCredentials = %v, want %d names", *a.ShellCredentials, tc.wantLen)
			}
		})
	}
}

// shell_credentials is authority over secrets, not authored behaviour: two
// agents differing only in it hash identically, so a definition that only
// narrows its shell's credentials is not new content.
func TestSign_ShellCredentialsExcludedFromHash(t *testing.T) {
	none := []string{}
	bare := FromYAMLAgent(&Agent{Name: "x", SystemPrompt: "p"})
	with := FromYAMLAgent(&Agent{Name: "x", SystemPrompt: "p", ShellCredentials: &none})
	if Sign(bare) != Sign(with) {
		t.Error("shell_credentials leaked into the yaml-path hash")
	}
}

// The overlay path (a runtime-authored definition) excludes it too.
func TestSign_ShellCredentialsExcludedFromOverlayHash(t *testing.T) {
	a, err := FromOverlay([]byte(`{"system_prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromOverlay([]byte(`{"system_prompt":"p","shell_credentials":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if Sign(a) != Sign(b) {
		t.Error("shell_credentials leaked into the overlay-path hash")
	}
}
