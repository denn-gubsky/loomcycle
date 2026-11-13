package config

import (
	"os"
	"path/filepath"
	"strings"
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
			got, defaulted := contentRoot(name, "skills")
			if got != tc.want {
				t.Errorf("contentRoot = %q, want %q", got, tc.want)
			}
			if want := tc.value == nil && tc.want != ""; defaulted != want {
				t.Errorf("defaulted = %v, want %v", defaulted, want)
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

// In an agents directory that was only defaulted, a .md file with no
// frontmatter (a README, a prompt body) is not an agent: it is left out and
// the boot is told. The same directory named by its variable loads every file,
// as it always has. Each adopted default is reported once.
func TestLoad_ADefaultedAgentsRootSkipsFilesWithoutFrontmatter(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"helper.md": "---\ntools: []\n---\nYou help.\n",
		"README.md": "# Agents\n\nNotes.\n",
	} {
		if err := os.WriteFile(filepath.Join(agentsDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOOMCYCLE_CONFIG_DIR", dir)
	for _, name := range []string{"LOOMCYCLE_SKILLS_ROOT", "LOOMCYCLE_AGENTS_ROOT", "LOOMCYCLE_HELP_ROOT"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	notices := func(cfg *Config, want string) int {
		n := 0
		for _, line := range cfg.Notices {
			if strings.Contains(line, want) {
				n++
			}
		}
		return n
	}

	cfg, err := loadYAMLString(t, "defaults: {provider: mock, model: m}\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Agents["helper"]; !ok {
		t.Error("helper.md was not discovered in the defaulted agents directory")
	}
	if _, ok := cfg.Agents["README"]; ok {
		t.Error("README.md, which has no frontmatter, was loaded as an agent from a defaulted directory")
	}
	if notices(cfg, filepath.Join(agentsDir, "README.md")) != 1 {
		t.Errorf("the skipped file is not reported once: %q", cfg.Notices)
	}
	if notices(cfg, "LOOMCYCLE_AGENTS_ROOT is not set: using "+agentsDir+",") != 1 {
		t.Errorf("the adopted default is not reported once: %q", cfg.Notices)
	}
	if notices(cfg, "LOOMCYCLE_SKILLS_ROOT is not set") != 0 {
		t.Errorf("a root that was not defaulted is reported: %q", cfg.Notices)
	}

	// Named by its variable, the directory is the operator's statement that
	// its files are agents.
	t.Setenv("LOOMCYCLE_AGENTS_ROOT", agentsDir)
	cfg, err = loadYAMLString(t, "defaults: {provider: mock, model: m}\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Agents["README"]; !ok {
		t.Error("README.md was skipped in a directory the operator named")
	}
	if len(cfg.Notices) != 0 {
		t.Errorf("notices for an explicit root: %q", cfg.Notices)
	}
}
