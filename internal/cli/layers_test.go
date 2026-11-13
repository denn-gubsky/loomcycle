package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// isolateLayerEnv clears every variable the assembly reads and points the
// auto-discovery at an empty home and working directory.
func isolateLayerEnv(t *testing.T) (home, cwd string) {
	t.Helper()
	for _, k := range []string{"LOOMCYCLE_PRESETS", "LOOMCYCLE_CONFIG_DIR", "LOOMCYCLE_NO_DEFAULT_PROVIDERS", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
	home, cwd = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	return home, cwd
}

func writeYAML(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func layerNames(layers []config.Layer) []string {
	out := make([]string, len(layers))
	for i, l := range layers {
		out[i] = l.Name
	}
	return out
}

// The server (boot, reload, tokens, audit) calls AssembleConfigStack; the
// introspection commands call it through assembleForCLI. For the same inputs
// the two give the same layers, in the same order, with the same contents:
// two assemblies that drifted once had `validate` refuse a config the server
// ran.
func TestAssembleConfigStack_ServerAndCLIPathsGiveTheSameLayers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home, cwd string) (explicit string)
		want  func(home, cwd, explicit string) []string // layer names
	}{
		{
			name: "presets only",
			setup: func(t *testing.T, _, _ string) string {
				t.Setenv("LOOMCYCLE_PRESETS", "base, chat")
				return ""
			},
			want: func(_, _, _ string) []string { return []string{"providers.default", "base", "chat"} },
		},
		{
			// A preset that includes others is expanded; chat, selected again
			// after it, is already in the stack and is not layered twice.
			name: "a preset that includes others, and one of them again",
			setup: func(t *testing.T, _, _ string) string {
				t.Setenv("LOOMCYCLE_PRESETS", "personal, chat")
				return ""
			},
			want: func(_, _, _ string) []string {
				return []string{"providers.default", "base", "local", "chat", "document-agent", "personal"}
			},
		},
		{
			name: "the server preset, and one of its bundles again",
			setup: func(t *testing.T, _, _ string) string {
				t.Setenv("LOOMCYCLE_PRESETS", "server,chat")
				return ""
			},
			want: func(_, _, _ string) []string {
				return []string{"providers.default", "base", "chat", "document-agent", "memory", "server"}
			},
		},
		{
			name: "presets and a config dir",
			setup: func(t *testing.T, _, cwd string) string {
				t.Setenv("LOOMCYCLE_PRESETS", "base")
				dir := filepath.Join(cwd, "conf")
				writeYAML(t, filepath.Join(dir, "20-b.yaml"), "provider_priority: [mock]\n")
				writeYAML(t, filepath.Join(dir, "10-a.yml"), "provider_priority: [mock]\n")
				t.Setenv("LOOMCYCLE_CONFIG_DIR", dir)
				return ""
			},
			want: func(_, cwd, _ string) []string {
				return []string{"providers.default", "base", filepath.Join(cwd, "conf", "10-a.yml"), filepath.Join(cwd, "conf", "20-b.yaml")}
			},
		},
		{
			name: "a named loomcycle.yaml brings its section files",
			setup: func(t *testing.T, _, cwd string) string {
				writeYAML(t, filepath.Join(cwd, "site", "loomcycle.memory.yaml"), "provider_priority: [mock]\n")
				writeYAML(t, filepath.Join(cwd, "site", "loomcycle.example.yaml"), "provider_priority: [mock]\n")
				return writeYAML(t, filepath.Join(cwd, "site", "loomcycle.yaml"), "provider_priority: [mock]\n")
			},
			want: func(_, cwd, explicit string) []string {
				return []string{"providers.default", explicit, filepath.Join(cwd, "site", "loomcycle.memory.yaml")}
			},
		},
		{
			name: "a config dir and a named file: the file is layered last",
			setup: func(t *testing.T, _, cwd string) string {
				dir := filepath.Join(cwd, "conf")
				writeYAML(t, filepath.Join(dir, "a.yaml"), "provider_priority: [mock]\n")
				t.Setenv("LOOMCYCLE_CONFIG_DIR", dir)
				return writeYAML(t, filepath.Join(cwd, "top.yaml"), "provider_priority: [mock]\n")
			},
			want: func(_, cwd, explicit string) []string {
				return []string{"providers.default", filepath.Join(cwd, "conf", "a.yaml"), explicit}
			},
		},
		{
			name: "nothing named: the working directory's loomcycle.yaml",
			setup: func(t *testing.T, _, cwd string) string {
				writeYAML(t, filepath.Join(cwd, "loomcycle.yaml"), "provider_priority: [mock]\n")
				return ""
			},
			want: func(_, _, _ string) []string { return []string{"providers.default", "loomcycle.yaml"} },
		},
		{
			name: "nothing named: the home config, without the default providers",
			setup: func(t *testing.T, home, _ string) string {
				t.Setenv("LOOMCYCLE_NO_DEFAULT_PROVIDERS", "1")
				writeYAML(t, filepath.Join(home, ".config", "loomcycle", "loomcycle.yaml"), "provider_priority: [mock]\n")
				return ""
			},
			want: func(home, _, _ string) []string {
				return []string{filepath.Join(home, ".config", "loomcycle", "loomcycle.yaml")}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, cwd := isolateLayerEnv(t)
			explicit := tc.setup(t, home, cwd)
			var paths []string
			if explicit != "" {
				paths = []string{explicit}
			}
			server, err := AssembleConfigStack(nil, paths, false)
			if err != nil {
				t.Fatalf("server path: %v", err)
			}
			cli, err := assembleForCLI(explicit)
			if err != nil {
				t.Fatalf("CLI path: %v", err)
			}
			want := tc.want(home, cwd, explicit)
			if got := layerNames(server.Layers); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("server layers = %v, want %v", got, want)
			}
			if got := layerNames(cli.Layers); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("CLI layers = %v, want the server's %v", got, want)
			}
			for i := range server.Layers {
				if string(server.Layers[i].Data) != string(cli.Layers[i].Data) {
					t.Errorf("layer %s: the two paths carry different contents", server.Layers[i].Name)
				}
			}
		})
	}
}

// Where the two assemblies used to differ, the server's rule holds for the
// CLI too.
func TestAssembleForCLI_FollowsTheServersRules(t *testing.T) {
	t.Run("a named file that does not exist is an error, presets or not", func(t *testing.T) {
		_, cwd := isolateLayerEnv(t)
		t.Setenv("LOOMCYCLE_PRESETS", "base")
		_, err := assembleForCLI(filepath.Join(cwd, "nope.yaml"))
		if err == nil || !strings.Contains(err.Error(), "config file not found") {
			t.Fatalf("err = %v, want the server's missing-file error", err)
		}
	})
	t.Run("nothing named and nothing to discover is the server's error", func(t *testing.T) {
		isolateLayerEnv(t)
		_, err := assembleForCLI("")
		if err == nil || !strings.Contains(err.Error(), "no config found") || !strings.Contains(err.Error(), "loomcycle init") {
			t.Fatalf("err = %v, want the server's no-config error", err)
		}
	})
	t.Run("auth.env beside the last file is loaded, set-if-unset", func(t *testing.T) {
		_, cwd := isolateLayerEnv(t)
		t.Setenv("LOOMCYCLE_LAYERS_TEST_FROM_FILE", "")
		os.Unsetenv("LOOMCYCLE_LAYERS_TEST_FROM_FILE")
		t.Setenv("LOOMCYCLE_LAYERS_TEST_FROM_SHELL", "shell")
		path := writeYAML(t, filepath.Join(cwd, "site", "loomcycle.yaml"), "provider_priority: [mock]\n")
		writeYAML(t, filepath.Join(cwd, "site", "auth.env"),
			"LOOMCYCLE_LAYERS_TEST_FROM_FILE=file\nLOOMCYCLE_LAYERS_TEST_FROM_SHELL=file\n")
		stack, err := assembleForCLI(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := os.Getenv("LOOMCYCLE_LAYERS_TEST_FROM_FILE"); got != "file" {
			t.Errorf("auth.env variable = %q, want it loaded", got)
		}
		if got := os.Getenv("LOOMCYCLE_LAYERS_TEST_FROM_SHELL"); got != "shell" {
			t.Errorf("a variable the environment already had = %q, want it kept", got)
		}
		if stack.AuthEnvVars != 1 {
			t.Errorf("AuthEnvVars = %d, want 1", stack.AuthEnvVars)
		}
	})
}

// TestConfigDirLayers: LOOMCYCLE_CONFIG_DIR enumerates *.yaml/*.yml directly
// under the dir, sorted lexically; ignores non-YAML files and subdirectories.
func TestConfigDirLayers(t *testing.T) {
	dir := t.TempDir()
	// Create out of order to prove the lexical sort.
	for _, n := range []string{"30-z.yaml", "10-a.yaml", "20-b.yml"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("provider_priority: [mock]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Noise that must be ignored: a non-YAML file and a subdirectory.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "ignored.yaml"), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := configDirLayers(dir)
	if err != nil {
		t.Fatalf("configDirLayers: %v", err)
	}
	want := []string{
		filepath.Join(dir, "10-a.yaml"),
		filepath.Join(dir, "20-b.yml"),
		filepath.Join(dir, "30-z.yaml"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d files %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %s, want %s (lexical order)", i, got[i], want[i])
		}
	}
}

// TestConfigDirLayers_EmptyAndMissing: an empty dir → nil (no error); a missing
// dir → error (the operator's typo is surfaced, not silently skipped).
func TestConfigDirLayers_EmptyAndMissing(t *testing.T) {
	empty := t.TempDir()
	got, err := configDirLayers(empty)
	if err != nil {
		t.Fatalf("empty dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty dir should yield no files, got %v", got)
	}

	if _, err := configDirLayers(filepath.Join(empty, "does-not-exist")); err == nil {
		t.Errorf("a missing LOOMCYCLE_CONFIG_DIR should error (caller exits)")
	}
}

// TestSelectPresetNames: --preset flags override LOOMCYCLE_PRESETS; an
// unset/empty env yields no presets (the opt-in default); order is preserved.
func TestSelectPresetNames(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
		env   string
		want  []string
	}{
		{"unset is opt-in none", nil, "", nil},
		{"env comma-split ordered", nil, "base, document-agent", []string{"base", "document-agent"}},
		{"env trims blanks", nil, " base , , local ", []string{"base", "local"}},
		{"flags override env", []string{"local"}, "base,document-agent", []string{"local"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectPresetNames(tc.flags, tc.env)
			if len(got) != len(tc.want) {
				t.Fatalf("SelectPresetNames(%v, %q) = %v, want %v", tc.flags, tc.env, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The retired variable that named config files stops the assembly, naming
// itself and what to do, before a "no config found" could hide the cause.
func TestAssembleConfigStack_RefusesTheRetiredConfigFilesVariable(t *testing.T) {
	_, cwd := isolateLayerEnv(t)
	t.Setenv("LOOMCYCLE_CONFIG_FILES", filepath.Join(cwd, "zz-layer.yaml"))
	_, err := AssembleConfigStack(nil, nil, false)
	if err == nil {
		t.Fatal("assembled with the retired variable naming a file")
	}
	for _, want := range []string{"LOOMCYCLE_CONFIG_FILES", "--config", "LOOMCYCLE_CONFIG_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "zz-layer") {
		t.Errorf("error carries the variable's value: %v", err)
	}
	// Set and empty, it named nothing: only advised, by the load.
	t.Setenv("LOOMCYCLE_CONFIG_FILES", "")
	t.Setenv("LOOMCYCLE_PRESETS", "base")
	if _, err := AssembleConfigStack(nil, nil, false); err != nil {
		t.Fatalf("the empty retired variable refused: %v", err)
	}
}

// The personal profile loads from the preset alone: no file, no key, no
// variable but the selection. It brings the agents of its bundles and leaves
// the store, the listen address and auth at their defaults.
func TestAssembleConfigStack_PersonalProfileLoadsWithNothingElseSet(t *testing.T) {
	isolateLayerEnv(t)
	t.Setenv("LOOMCYCLE_PRESETS", "personal")
	t.Setenv("LOOMCYCLE_PG_DSN", "") // a developer's own DSN would select Postgres
	stack, err := AssembleConfigStack(nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadLayers(stack.Layers...)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	for _, agent := range []string{"chat/medium", "chat/local", "chat/local-small", "doc/manager"} {
		if _, ok := cfg.Agents[agent]; !ok {
			t.Errorf("agent %q missing from the personal profile", agent)
		}
	}
	if got := cfg.ProviderPriority; len(got) == 0 || got[0] != "ollama-local" {
		t.Errorf("provider_priority = %v, want ollama-local first (local models before cloud)", got)
	}
	if cfg.Storage.Backend != "sqlite" {
		t.Errorf("storage backend = %q, want sqlite", cfg.Storage.Backend)
	}
	if cfg.Env.MetricsEnabled {
		t.Error("the personal profile turned the metrics sampler on")
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "config layer override") {
			t.Errorf("the profile's own layers conflict: %s", w)
		}
	}
}

// The server profile turns on what a server is watched by and nothing that
// decides who gets in, where data lives or when it is deleted: those are the
// operator's, and a profile is selected by one word.
func TestAssembleConfigStack_ServerProfileSetsMetricsAndLeavesTheRestToTheOperator(t *testing.T) {
	isolateLayerEnv(t)
	t.Setenv("LOOMCYCLE_PRESETS", "server")
	for _, k := range []string{"LOOMCYCLE_PG_DSN", "LOOMCYCLE_LISTEN_ADDR", "LOOMCYCLE_AUTH_TOKEN", "LOOMCYCLE_AUTH_MODE"} {
		t.Setenv(k, "") // a developer's own environment must not decide this
	}
	stack, err := AssembleConfigStack(nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadLayers(stack.Layers...)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if !cfg.Env.MetricsEnabled || !cfg.Env.MetricsCollectSystem {
		t.Errorf("metrics enabled=%v collect_system=%v, want both on", cfg.Env.MetricsEnabled, cfg.Env.MetricsCollectSystem)
	}
	for _, agent := range []string{"chat/medium", "doc/manager", "memory/consolidator", "memory/extractor"} {
		if _, ok := cfg.Agents[agent]; !ok {
			t.Errorf("agent %q missing from the server profile", agent)
		}
	}

	if cfg.Retention.Enabled {
		t.Error("the server profile turned retention on: it deletes data, and stays the operator's explicit choice")
	}
	if sr, ok := cfg.ScheduledRuns["memory-consolidation"]; !ok {
		t.Error("the memory bundle's schedule is missing")
	} else if sr.Enabled {
		t.Error("the server profile enabled the consolidation schedule: it spends tokens hourly, and ships off")
	}
	if len(cfg.Volumes) != 0 {
		t.Errorf("the server profile declares volumes %v: a host path is the operator's", cfg.Volumes)
	}
	if len(cfg.Auth.Gates) != 0 || cfg.Auth.Mode != "" || len(cfg.Principals) != 0 {
		t.Errorf("the server profile configures sign-in (mode %q, %d gates, %d principals)", cfg.Auth.Mode, len(cfg.Auth.Gates), len(cfg.Principals))
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "config layer override") {
			t.Errorf("the profile's own layers conflict: %s", w)
		}
	}
}
