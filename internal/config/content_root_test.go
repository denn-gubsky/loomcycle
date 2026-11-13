package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A content root defaults to the matching subdirectory of the config dir, only
// when that subdirectory exists, and the variable always wins, including when
// it is set to the empty string.
func TestContentRoot_DefaultsUnderTheConfigDirUnlessTheVariableIsSet(t *testing.T) {
	dir := t.TempDir()
	withSub := filepath.Join(dir, "with")
	if err := os.MkdirAll(filepath.Join(withSub, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file named like the subdirectory is not a root.
	fileSub := filepath.Join(dir, "file")
	if err := os.MkdirAll(fileSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fileSub, "skills"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	const name = "LOOMCYCLE_SKILLS_ROOT"
	explicit := "/srv/skills"
	blank := ""
	for _, tc := range []struct {
		desc      string
		configDir string
		value     *string // nil leaves the variable unset
		want      string
	}{
		{"no config dir, variable unset", "", nil, ""},
		{"config dir without the subdirectory", empty, nil, ""},
		{"config dir whose skills entry is a file", fileSub, nil, ""},
		{"config dir with the subdirectory", withSub, nil, filepath.Join(withSub, "skills")},
		{"variable set wins over the subdirectory", withSub, &explicit, explicit},
		{"variable set to empty declines the default", withSub, &blank, ""},
		{"variable set with no config dir", "", &explicit, explicit},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_CONFIG_DIR", tc.configDir)
			// t.Setenv first so the test restores whatever the host had, then
			// drop the variable for the unset cases.
			t.Setenv(name, "")
			if tc.value == nil {
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv(name, *tc.value)
			}
			if got := contentRoot(name, "skills"); got != tc.want {
				t.Errorf("contentRoot = %q, want %q", got, tc.want)
			}
		})
	}
}

// The three roots are wired to their own subdirectories, and an agent file
// under <config dir>/agents is discovered with no agents variable set.
func TestLoad_ContentRootsDefaultToTheConfigDirSubdirectories(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"skills", "agents", "help"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	agent := "---\nname: helper\ntools: []\n---\nYou help.\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "helper.md"), []byte(agent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOMCYCLE_CONFIG_DIR", dir)
	for _, name := range []string{"LOOMCYCLE_SKILLS_ROOT", "LOOMCYCLE_AGENTS_ROOT", "LOOMCYCLE_HELP_ROOT"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := loadYAMLString(t, "defaults: {provider: mock, model: m}\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env.SkillsRoot != filepath.Join(dir, "skills") || cfg.Env.AgentsRoot != filepath.Join(dir, "agents") || cfg.Env.HelpRoot != filepath.Join(dir, "help") {
		t.Errorf("roots = %q, %q, %q; want the three subdirectories of %s", cfg.Env.SkillsRoot, cfg.Env.AgentsRoot, cfg.Env.HelpRoot, dir)
	}
	if _, ok := cfg.Agents["helper"]; !ok {
		t.Errorf("agents = %v, want helper discovered from %s/agents", len(cfg.Agents), dir)
	}
}
