package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// Each use of what a future release removes is its own WARN under
// "Deprecated", and is not repeated among the config advisories.
func TestDoctor_DeprecatedListsEachUseOnce(t *testing.T) {
	withTempHome(t)
	uses := []string{
		"LOOMCYCLE_BASH_ENABLED is set, which will be removed in a future release: x (see docs/MIGRATING-FROM-BASH.md#environment-variables)",
		`agent "coder" names Bash, which will be removed in a future release: y (see docs/MIGRATING-FROM-BASH.md#the-bash-tool)`,
	}
	withStubLoader(t, &stubConfig{
		providers: []string{"anthropic"}, backend: "sqlite", dataDir: t.TempDir(), listen: freePort(t),
		configWarnings: []string{"an unrelated advisory"},
		deprecations:   uses,
	})
	// A file for the auto-discovery step to find, as TestDoctor_AllPass.
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	RunDoctor(nil, &stdout, &stderr)
	out := stdout.String()
	for _, u := range uses {
		if !strings.Contains(out, "[WARN]  Deprecated") || strings.Count(out, u) != 1 {
			t.Errorf("want %q once under Deprecated in:\n%s", u, out)
		}
	}
	if !strings.Contains(out, "an unrelated advisory") {
		t.Errorf("the other advisories went missing:\n%s", out)
	}
	if strings.Contains(out, "[PASS]  Deprecated") {
		t.Errorf("Deprecated passes with uses:\n%s", out)
	}
}

// The real configuration keeps the two apart: the load's deprecation lines
// are doctor's "Deprecated", and not also among its advisories.
func TestRealConfig_DeprecationsAreNotRepeatedAsAdvisories(t *testing.T) {
	t.Setenv("LOOMCYCLE_BASH_ALLOWED_CREDS", "x")
	cfg, err := config.LoadLayers(config.Layer{Name: "t.yaml", Data: []byte("defaults:\n  provider: mock\n  model: mock\nagents:\n  coder:\n    tools: [Read, Bash]\n")})
	if err != nil {
		t.Fatal(err)
	}
	r := &realConfig{cfg: cfg}
	if got := r.DeprecationWarnings(); len(got) != 2 {
		t.Fatalf("deprecations = %q, want the variable and the agent", got)
	}
	for _, w := range r.ConfigWarnings() {
		if strings.Contains(w, "will be removed in a future release") {
			t.Errorf("a deprecation is repeated as an advisory: %q", w)
		}
	}
}
