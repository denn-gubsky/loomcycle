package cli

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// twoConfigFiles writes two config files that each declare one agent, so a
// command that kept only one of them shows one agent and one file.
func twoConfigFiles(t *testing.T) (a, b string) {
	t.Helper()
	_, cwd := isolateLayerEnv(t)
	t.Setenv("LOOMCYCLE_DATA_DIR", filepath.Join(cwd, "data"))
	a = writeYAML(t, filepath.Join(cwd, "a.yaml"),
		"defaults: { provider: anthropic, model: claude-sonnet-4-6 }\nagents:\n  from-a: { system_prompt: \"a\", tools: [] }\n")
	b = writeYAML(t, filepath.Join(cwd, "b.yaml"),
		"agents:\n  from-b: { system_prompt: \"b\", tools: [] }\n")
	return a, b
}

// The server takes --config any number of times, and that is the documented
// way to layer several files. A command that reports on the server's config
// has to take the same files: one that kept only the last would describe a
// server nobody runs.
func TestRunAgentsList_LayersEveryConfigFlag(t *testing.T) {
	a, b := twoConfigFiles(t)
	var stdout, stderr bytes.Buffer
	if rc := RunAgents([]string{"list", "--config", a, "--config", b}, &stdout, &stderr); rc != 0 {
		t.Fatalf("rc=%d, stderr=%q", rc, stderr.String())
	}
	for _, want := range []string{"from-a", "from-b"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("agents list is missing %q, the agent of one of the two --config files:\n%s", want, stdout.String())
		}
	}
}

func TestRunValidate_TakesSeveralPaths(t *testing.T) {
	a, b := twoConfigFiles(t)
	var stdout, stderr bytes.Buffer
	if rc := RunValidate([]string{a, b}, &stdout, &stderr); rc != 0 {
		t.Fatalf("rc=%d, stderr=%q", rc, stderr.String())
	}
	for _, want := range []string{"from-a", "from-b", "Agents           : 2"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("validate is missing %q:\n%s", want, stdout.String())
		}
	}
}

// The rule for a retired variable whose setting moved to the yaml asks which
// keys the loaded yaml names, over all layers. validate reaches the load
// through the one assembly with several files, so a key named in a file that
// is not the last one must count, and a key no file names must not. The
// summary carries both the layer line and the sign-in mode line.
func TestRunValidate_MovedSettingRuleReadsEveryNamedLayer(t *testing.T) {
	a, b := twoConfigFiles(t)
	t.Setenv("LOOMCYCLE_METRICS_ENABLED", "1")
	var stdout, stderr bytes.Buffer
	if rc := RunValidate([]string{a, b}, &stdout, &stderr); rc != 2 || !strings.Contains(stderr.String(), "does not name metrics.enabled") {
		t.Fatalf("no layer names the key: rc=%d, stderr=%q", rc, stderr.String())
	}
	// Named in the first file, which the second is layered over.
	writeYAML(t, a, "defaults: { provider: anthropic, model: claude-sonnet-4-6 }\nmetrics: { enabled: false }\nagents:\n  from-a: { system_prompt: \"a\", tools: [] }\n")
	stdout.Reset()
	stderr.Reset()
	if rc := RunValidate([]string{a, b}, &stdout, &stderr); rc != 0 {
		t.Fatalf("the first layer names the key: rc=%d, stderr=%q", rc, stderr.String())
	}
	for _, want := range []string{"loomcycle config: ", a + " ◁ " + b, "Auth mode        : ", "Agents           : 2"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("validate is missing %q:\n%s", want, stdout.String())
		}
	}
}

// Every command that reports on the server's config, run through its own
// flag parsing with two --config files, names the stack the server assembles
// from the same two files. Comparing the assembly function with a wrapper
// around itself would pass whatever a command did with its flags; this fails
// when one drops a --config, reorders them, or assembles differently.
func TestCLICommands_NameTheServersStackForSeveralConfigFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(a, b string, stdout, stderr io.Writer)
	}{
		{"validate", func(a, b string, stdout, stderr io.Writer) {
			RunValidate([]string{a, b}, stdout, stderr)
		}},
		{"doctor", func(a, b string, stdout, stderr io.Writer) {
			RunDoctor([]string{"--config", a, "--config", b}, stdout, stderr)
		}},
		{"agents list", func(a, b string, stdout, stderr io.Writer) {
			RunAgents([]string{"list", "--config", a, "--config", b}, stdout, stderr)
		}},
		{"migrate status", func(a, b string, stdout, stderr io.Writer) {
			RunMigrate([]string{"status", "--config", a, "--config", b}, stdout, stderr)
		}},
		{"memory-calibrate", func(a, b string, stdout, stderr io.Writer) {
			RunMemoryCalibrate([]string{"--embedder", "stub", "--config", a, "--config", b}, stdout, stderr)
		}},
		{"memory-eval-live", func(a, b string, stdout, stderr io.Writer) {
			RunMemoryEvalLive([]string{"--provider", "mock", "--model", "m", "--config", a, "--config", b}, stdout, stderr)
		}},
		{"memory-eval-judge", func(a, b string, stdout, stderr io.Writer) {
			RunMemoryEvalJudge([]string{"--provider", "mock", "--model", "m", "--config", a, "--config", b}, stdout, stderr)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := twoConfigFiles(t)
			server, err := AssembleConfigStack(nil, []string{a, b}, false)
			if err != nil {
				t.Fatal(err)
			}
			want := describeStack(server)
			if !strings.Contains(want, a+" ◁ "+b) {
				t.Fatalf("the server's stack %q does not layer the two files in order", want)
			}
			var stdout, stderr bytes.Buffer
			tc.run(a, b, &stdout, &stderr)
			if out := stdout.String() + stderr.String(); !strings.Contains(out, want) {
				t.Errorf("%s does not name the server's stack %q:\n%s", tc.name, want, out)
			}
		})
	}
}

// The eval tools score the shipped prompts and embedder. A loomcycle.yaml the
// developer happens to have in the working directory (or under ~/.config)
// must not be layered over them, with its auth.env, without being asked for:
// it could replace the agent being scored or supply the keys, silently.
func TestMemoryTools_DoNotLoadAnAutoDiscoveredConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(stdout, stderr io.Writer, extra ...string) int
	}{
		{"memory-calibrate", func(stdout, stderr io.Writer, extra ...string) int {
			return RunMemoryCalibrate(append([]string{"--embedder", "stub"}, extra...), stdout, stderr)
		}},
		{"memory-eval-live", func(stdout, stderr io.Writer, extra ...string) int {
			return RunMemoryEvalLive(append([]string{"--provider", "mock", "--model", "m"}, extra...), stdout, stderr)
		}},
		{"memory-eval-judge", func(stdout, stderr io.Writer, extra ...string) int {
			return RunMemoryEvalJudge(append([]string{"--provider", "mock", "--model", "m"}, extra...), stdout, stderr)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, cwd := isolateLayerEnv(t)
			t.Setenv("LOOMCYCLE_DATA_DIR", filepath.Join(cwd, "data"))
			own := "agents:\n  mine: { system_prompt: \"mine\", tier: middle }\n"
			writeYAML(t, filepath.Join(cwd, "loomcycle.yaml"), own)
			writeYAML(t, filepath.Join(home, ".config", "loomcycle", "loomcycle.yaml"), own)

			t.Run("presets alone are the stack", func(t *testing.T) {
				t.Setenv("LOOMCYCLE_PRESETS", "base,memory")
				var stdout, stderr bytes.Buffer
				tc.run(&stdout, &stderr)
				want := tc.name + ": config layers: providers.default ◁ base ◁ memory\n"
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr does not say the stack is the presets alone (%q):\n%s", want, stderr.String())
				}
			})
			t.Run("a named file is layered and listed", func(t *testing.T) {
				t.Setenv("LOOMCYCLE_PRESETS", "base,memory")
				extra := writeYAML(t, filepath.Join(cwd, "extra.yaml"), "agents:\n  extra: { system_prompt: \"x\", tier: middle }\n")
				var stdout, stderr bytes.Buffer
				tc.run(&stdout, &stderr, "--config", extra)
				want := tc.name + ": config layers: providers.default ◁ base ◁ memory ◁ " + extra + "\n"
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr does not list the named file (%q):\n%s", want, stderr.String())
				}
			})
			t.Run("nothing selected is an error, not the discovered file", func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				if rc := tc.run(&stdout, &stderr); rc != 2 {
					t.Errorf("rc=%d, want 2", rc)
				}
				if !strings.Contains(stderr.String(), "LOOMCYCLE_PRESETS") || strings.Contains(stderr.String(), "config layers:") {
					t.Errorf("stderr should say how to select a config and load none:\n%s", stderr.String())
				}
			})
		})
	}
}

// agents list reports what the server would run, so it keeps the server's
// auto-discovery, and says on stderr which layers that came to. stdout, the
// part a script reads, is unchanged.
func TestRunAgentsList_SaysWhichLayersItLoaded(t *testing.T) {
	_, cwd := isolateLayerEnv(t)
	writeYAML(t, filepath.Join(cwd, "loomcycle.yaml"),
		"defaults: { provider: anthropic, model: claude-sonnet-4-6 }\nagents:\n  found: { system_prompt: \"f\", tools: [] }\n")
	var stdout, stderr bytes.Buffer
	if rc := RunAgents([]string{"list", "--json"}, &stdout, &stderr); rc != 0 {
		t.Fatalf("rc=%d, stderr=%q", rc, stderr.String())
	}
	if want := "agents list: config layers: providers.default ◁ loomcycle.yaml\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "[") && !strings.HasPrefix(strings.TrimSpace(stdout.String()), "{") {
		t.Errorf("stdout is no longer JSON alone:\n%s", stdout.String())
	}
}
