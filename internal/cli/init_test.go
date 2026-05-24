package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInit_NonInteractive_WritesBothFiles — the no-wizard path drops
// the bundled example yaml + CONFIGURATION.md into the target dir.
func TestInit_NonInteractive_WritesBothFiles(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runInitWithStdin(
		[]string{"--path", dir, "--no-interactive"},
		&bytes.Buffer{}, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	yamlPath := filepath.Join(dir, "loomcycle.yaml")
	docPath := filepath.Join(dir, "CONFIGURATION.md")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Fatalf("yaml not written: %v", err)
	}
	if _, err := os.Stat(docPath); err != nil {
		t.Fatalf("doc not written: %v", err)
	}
	yamlBytes, _ := os.ReadFile(yamlPath)
	if !bytes.Contains(yamlBytes, []byte("agents:")) {
		t.Errorf("yaml doesn't look like the example (missing 'agents:' marker)")
	}
	docBytes, _ := os.ReadFile(docPath)
	if !bytes.Contains(docBytes, []byte("loomcycle — operator configuration reference")) {
		t.Errorf("doc doesn't look like the bundled CONFIGURATION.md")
	}
	if !strings.Contains(stdout.String(), "Next steps:") {
		t.Errorf("stdout missing the Next-steps block; got:\n%s", stdout.String())
	}
}

// TestInit_RefusesExistingWithoutForce — running init twice in the
// same dir without --force errors with a pointed message.
func TestInit_RefusesExistingWithoutForce(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runInitWithStdin([]string{"--path", dir, "--no-interactive"}, &bytes.Buffer{}, &stdout, &stderr); code != 0 {
		t.Fatalf("first init: exit=%d", code)
	}
	stdout.Reset()
	stderr.Reset()
	code := runInitWithStdin([]string{"--path", dir, "--no-interactive"}, &bytes.Buffer{}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit=1 on existing files without --force; got %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr missing 'already exists' hint; got: %s", stderr.String())
	}
}

// TestInit_ForceOverwrites — --force overwrites a pre-existing yaml.
func TestInit_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "loomcycle.yaml")
	if err := os.WriteFile(yamlPath, []byte("# placeholder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runInitWithStdin([]string{"--path", dir, "--no-interactive", "--force"}, &bytes.Buffer{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("force-overwrite: exit=%d stderr=%s", code, stderr.String())
	}
	yamlBytes, _ := os.ReadFile(yamlPath)
	if !bytes.Contains(yamlBytes, []byte("agents:")) {
		t.Errorf("yaml wasn't replaced with the example")
	}
}

// TestInit_Wizard_PicksProviderAndListenAddr — drive the wizard with
// a canned-stdin buffer; assert the generated yaml carries the
// operator's choices.
func TestInit_Wizard_PicksProviderAndListenAddr(t *testing.T) {
	dir := t.TempDir()
	// Three answers: provider=openai, env var=(blank → default), addr=0.0.0.0:9000
	stdin := bytes.NewBufferString("openai\n\n0.0.0.0:9000\n")
	var stdout, stderr bytes.Buffer
	code := runInitWithStdin(
		[]string{"--path", dir, "--interactive"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("wizard: exit=%d stderr=%s", code, stderr.String())
	}
	yamlBytes, _ := os.ReadFile(filepath.Join(dir, "loomcycle.yaml"))
	yaml := string(yamlBytes)
	if !strings.Contains(yaml, "provider: openai") {
		t.Errorf("yaml missing pinned openai provider; tail:\n%s", tail(yaml, 20))
	}
	if !strings.Contains(yaml, `listen_addr: "0.0.0.0:9000"`) {
		t.Errorf("yaml missing pinned listen addr; tail:\n%s", tail(yaml, 20))
	}
	if !strings.Contains(stdout.String(), "export OPENAI_API_KEY=<your-key-here>") {
		t.Errorf("stdout missing env-var suggestion for OpenAI; got:\n%s", stdout.String())
	}
}

// TestInit_Wizard_Skip — picking "skip" omits the API-key suggestion
// but still writes the yaml + doc.
func TestInit_Wizard_Skip(t *testing.T) {
	dir := t.TempDir()
	stdin := bytes.NewBufferString("skip\n127.0.0.1:8787\n")
	var stdout, stderr bytes.Buffer
	code := runInitWithStdin(
		[]string{"--path", dir, "--interactive"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("wizard skip: exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "API_KEY=<your-key-here>") {
		t.Errorf("skip path shouldn't suggest an API key; got:\n%s", out)
	}
	if !strings.Contains(out, "LOOMCYCLE_AUTH_TOKEN") {
		t.Errorf("skip path should still suggest the auth token; got:\n%s", out)
	}
}

// TestInit_Wizard_InvalidProviderReprompts — typing a bad provider
// then a good one succeeds (the validator re-prompts).
func TestInit_Wizard_InvalidProviderReprompts(t *testing.T) {
	dir := t.TempDir()
	stdin := bytes.NewBufferString("garbage\nanthropic\n\n127.0.0.1:8787\n")
	var stdout, stderr bytes.Buffer
	code := runInitWithStdin([]string{"--path", dir, "--interactive"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("wizard reprompt: exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "must be one of") {
		t.Errorf("stdout missing validator error; got:\n%s", stdout.String())
	}
}

// TestInit_FlagConflict_MutuallyExclusive — --interactive + --no-interactive
// is operator misuse; refuse with a clear error.
func TestInit_FlagConflict_MutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runInitWithStdin(
		[]string{"--path", dir, "--interactive", "--no-interactive"},
		&bytes.Buffer{}, &stdout, &stderr,
	)
	if code != 2 {
		t.Errorf("expected exit=2 (invocation error); got %d", code)
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("stderr missing mutually-exclusive hint; got: %s", stderr.String())
	}
}

func tail(s string, lines int) string {
	parts := strings.Split(s, "\n")
	if len(parts) <= lines {
		return s
	}
	return strings.Join(parts[len(parts)-lines:], "\n")
}
