package config

import (
	"strings"
	"testing"
)

// LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS takes bare credential names and
// "command:NAME" bindings, mixed. A malformed entry, or a binding to a
// command the operator did not allowlist, refuses the boot naming the entry:
// either would otherwise leave a credential silently unused.
func TestBashboxFallbackCreds_RefusesMalformedEntries(t *testing.T) {
	t.Setenv("LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS", "git,gh")
	for _, tc := range []struct {
		value, wantErr string
	}{
		{":GITHUB_TOKEN", "command before ':' is empty"},
		{"git:", "credential name"},
		{"git:GITHUB TOKEN", "credential name"},
		{"git:A:B", "credential name"},
		{"GITHUB$TOKEN", "credential name"},
		{"npm:NPM_TOKEN", "not in LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS", "SHARED,"+tc.value)
			_, err := loadYAMLString(t, "providers: {}\n")
			if err == nil {
				t.Fatalf("%q was accepted", tc.value)
			}
			if !strings.Contains(err.Error(), "LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v; want it to name the variable and say %q", err, tc.wantErr)
			}
		})
	}

	t.Setenv("LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS", "git:GITHUB_TOKEN, gh:GH_TOKEN, SHARED-1")
	if _, err := loadYAMLString(t, "providers: {}\n"); err != nil {
		t.Errorf("a mixed list of bindings and bare names was refused: %v", err)
	}
}

func TestParseBashboxFallbackCred_SplitsBindingFromBareName(t *testing.T) {
	for _, tc := range []struct{ entry, cmd, name string }{
		{"GITHUB_TOKEN", "", "GITHUB_TOKEN"},
		{"git:GITHUB_TOKEN", "git", "GITHUB_TOKEN"},
		{"git-lfs:LFS-TOKEN", "git-lfs", "LFS-TOKEN"},
	} {
		cmd, name, err := ParseBashboxFallbackCred(tc.entry)
		if err != nil || cmd != tc.cmd || name != tc.name {
			t.Errorf("%q → (%q, %q, %v); want (%q, %q, nil)", tc.entry, cmd, name, err, tc.cmd, tc.name)
		}
	}
}
