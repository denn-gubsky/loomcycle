package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInit_NonInteractive_WritesBothFiles — the no-wizard path drops
// the bundled example yaml + README.md into the target dir.
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
	docPath := filepath.Join(dir, "README.md")
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
	if !bytes.Contains(docBytes, []byte("loomcycle — local config quickstart")) {
		t.Errorf("doc doesn't look like the bundled README.md")
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
// a canned-stdin buffer; assert the choices surface as next-steps
// guidance (env-var suggestion + pasteable yaml block). The yaml
// itself is written verbatim from the example — the wizard never
// rewrites it (prior drafts did, but yaml.v3 last-wins on duplicate
// top-level keys silently dropped every example agent).
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

	// Yaml is the verbatim example — no append, no rewrite.
	yamlBytes, _ := os.ReadFile(filepath.Join(dir, "loomcycle.yaml"))
	if !bytes.Equal(yamlBytes, []byte("")) && !bytes.Contains(yamlBytes, []byte("agents:")) {
		t.Errorf("yaml should be the verbatim example; got tail:\n%s", tail(string(yamlBytes), 10))
	}
	// Two `agents:` occurrences would mean the wizard re-introduced
	// the destructive append. Counting `\nagents:\n` (the top-level
	// key form) gives a sharp regression signal.
	if n := bytes.Count(yamlBytes, []byte("\nagents:\n")); n != 1 {
		t.Errorf("yaml has %d top-level `agents:` keys; want exactly 1 (regression: wizard re-introduced destructive append)", n)
	}

	out := stdout.String()
	if !strings.Contains(out, "export OPENAI_API_KEY=<your-key-here>") {
		t.Errorf("stdout missing env-var suggestion for OpenAI; got:\n%s", out)
	}
	if !strings.Contains(out, "export LOOMCYCLE_LISTEN_ADDR=0.0.0.0:9000") {
		t.Errorf("stdout missing LOOMCYCLE_LISTEN_ADDR suggestion for non-default addr; got:\n%s", out)
	}
	if !strings.Contains(out, "provider: openai") {
		t.Errorf("stdout missing pasteable provider-pin block; got:\n%s", out)
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

// TestInit_WithToken_MintsAuthEnv — --with-token persists a 0600 auth.env
// carrying a 64-hex LOOMCYCLE_AUTH_TOKEN and prints the one-time UI URL.
func TestInit_WithToken_MintsAuthEnv(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runInitWithStdin(
		[]string{"--path", dir, "--no-interactive", "--with-token"},
		&bytes.Buffer{}, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("init --with-token exit=%d stderr=%s", code, stderr.String())
	}

	authEnv := filepath.Join(dir, "auth.env")
	info, err := os.Stat(authEnv)
	if err != nil {
		t.Fatalf("auth.env not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("auth.env mode = %o, want 600 (it holds a secret)", perm)
	}

	body, _ := os.ReadFile(authEnv)
	var token string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "LOOMCYCLE_AUTH_TOKEN=") {
			token = strings.TrimPrefix(line, "LOOMCYCLE_AUTH_TOKEN=")
		}
	}
	if len(token) != 64 {
		t.Errorf("minted token len = %d, want 64 hex chars", len(token))
	}
	for _, c := range token {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("token has non-hex char %q", c)
		}
	}
	// The UI URL must embed the freshly minted token so the operator can
	// click straight through to an authenticated UI.
	if !strings.Contains(stdout.String(), "/ui?token="+token) {
		t.Errorf("stdout missing the ready-to-open UI URL with the minted token; got:\n%s", stdout.String())
	}
}

// TestInit_WithToken_NoClobber — a non-force re-run must never clobber a live
// token. init refuses on the pre-existing yaml before reaching the token
// block, so the secret is preserved; --force is the explicit "regenerate
// everything" signal.
func TestInit_WithToken_NoClobber(t *testing.T) {
	dir := t.TempDir()
	var o1, e1 bytes.Buffer
	if code := runInitWithStdin([]string{"--path", dir, "--no-interactive", "--with-token"},
		&bytes.Buffer{}, &o1, &e1); code != 0 {
		t.Fatalf("first init exit=%d stderr=%s", code, e1.String())
	}
	before, _ := os.ReadFile(filepath.Join(dir, "auth.env"))

	var o2, e2 bytes.Buffer
	code := runInitWithStdin([]string{"--path", dir, "--no-interactive", "--with-token"},
		&bytes.Buffer{}, &o2, &e2)
	if code == 0 {
		t.Errorf("non-force re-run should refuse (yaml already exists); got exit 0")
	}
	if !strings.Contains(e2.String(), "already exists") {
		t.Errorf("expected an 'already exists' refusal; got stderr:\n%s", e2.String())
	}
	after, _ := os.ReadFile(filepath.Join(dir, "auth.env"))
	if string(before) != string(after) {
		t.Error("auth.env changed on a non-force re-run — a live token must never be clobbered")
	}
}

func TestMintAuthToken_UniqueAndHex(t *testing.T) {
	a, err := mintAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := mintAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("token lengths = %d,%d, want 64", len(a), len(b))
	}
	if a == b {
		t.Error("two mints returned the same token — not random")
	}
}
