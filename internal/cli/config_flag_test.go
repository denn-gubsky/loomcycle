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
