package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/embedded"
)

// TestInit_NonInteractive_WritesBothFiles — the no-wizard path drops
// the personal profile's yaml + README.md into the target dir. The yaml
// names its preset, so the next steps ask for no variable to select one.
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
	want, err := embedded.ProfileYAML("personal")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(yamlBytes, want) {
		t.Errorf("yaml is not the personal profile's config; got:\n%s", tail(string(yamlBytes), 10))
	}
	if !bytes.Contains(yamlBytes, []byte("\npresets: [personal]\n")) {
		t.Errorf("the written yaml does not name its preset; got:\n%s", tail(string(yamlBytes), 10))
	}
	if strings.Contains(stdout.String(), "LOOMCYCLE_PRESETS") {
		t.Errorf("the next steps still ask for LOOMCYCLE_PRESETS; got:\n%s", stdout.String())
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
	if bytes.Contains(yamlBytes, []byte("# placeholder")) || !bytes.Contains(yamlBytes, []byte("personal profile")) {
		t.Errorf("yaml wasn't replaced with the profile's config")
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
		[]string{"--path", dir, "--interactive", "--full"},
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
// carrying a 64-hex LOOMCYCLE_AUTH_TOKEN, prints the bare UI URL, and (the
// security-load-bearing part) NEVER echoes the token to stdout nor embeds it
// in a URL.
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
	out := stdout.String()
	// The token must NEVER appear in stdout — not raw, not in a URL. It lives
	// only in the 0600 file. (Guards against a regression that re-embeds it
	// in a `/ui?token=…` link, which leaks into history / proxy logs.)
	if strings.Contains(out, token) {
		t.Error("stdout contains the raw token — it must stay in the 0600 file, never echoed")
	}
	if strings.Contains(out, "?token=") {
		t.Errorf("stdout embeds a ?token= URL; the token must not travel in a URL. got:\n%s", out)
	}
	// The bare UI URL + a pointer to the auth.env file must be present.
	if !strings.Contains(out, "/ui\n") && !strings.Contains(out, "/ui ") {
		t.Errorf("stdout missing the bare /ui URL; got:\n%s", out)
	}
	if !strings.Contains(out, authEnv) {
		t.Errorf("stdout missing the auth.env path to paste from; got:\n%s", out)
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

// --full writes the long reference config, as init did before profiles: it
// carries its own agents and routing and names no preset.
func TestInit_Full_WritesTheReferenceConfig(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runInitWithStdin([]string{"--path", dir, "--no-interactive", "--full"}, &bytes.Buffer{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	yamlBytes, _ := os.ReadFile(filepath.Join(dir, "loomcycle.yaml"))
	if !bytes.Equal(yamlBytes, embedded.ExampleYAML()) {
		t.Error("--full did not write the reference config")
	}
	if strings.Contains(stdout.String(), "LOOMCYCLE_PRESETS") {
		t.Errorf("--full needs no preset, but the next steps name one:\n%s", stdout.String())
	}
}

// The wizard on a profile prints the key to export, and neither a preset
// variable (the yaml names it) nor advice to pin an agent the profile's yaml
// does not have.
func TestInit_Wizard_OnAProfilePrintsNoPresetVariableAndNoPin(t *testing.T) {
	dir := t.TempDir()
	stdin := bytes.NewBufferString("openai\n\n127.0.0.1:8787\n")
	var stdout, stderr bytes.Buffer
	if code := runInitWithStdin([]string{"--path", dir, "--interactive"}, stdin, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "export OPENAI_API_KEY=<your-key-here>") {
		t.Errorf("stdout missing the key to export; got:\n%s", out)
	}
	if strings.Contains(out, "LOOMCYCLE_PRESETS") {
		t.Errorf("stdout still asks for LOOMCYCLE_PRESETS:\n%s", out)
	}
	if strings.Contains(out, "agents.default") {
		t.Errorf("stdout advises pinning agents.default, which a profile's yaml does not have:\n%s", out)
	}
}

func TestInit_ProfileServer_WritesTheServerConfigAndSaysWhatItNeeds(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runInitWithStdin([]string{"--path", dir, "--no-interactive", "--profile", "server"}, &bytes.Buffer{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	yamlBytes, _ := os.ReadFile(filepath.Join(dir, "loomcycle.yaml"))
	want, err := embedded.ProfileYAML("server")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(yamlBytes, want) {
		t.Error("--profile server did not write the server profile's config")
	}
	if !bytes.Contains(yamlBytes, []byte("\npresets: [server]\n")) {
		t.Error("the written yaml does not name the server preset")
	}
	out := stdout.String()
	// A server does not run in open mode: a non-loopback address with no
	// token is refused at boot.
	if strings.Contains(out, "open mode") {
		t.Errorf("the server profile's next steps mention open mode:\n%s", out)
	}
	for _, want := range []string{"LOOMCYCLE_PUBLIC_URL", "LOOMCYCLE_SQLMEM_PG_DSN", "examples/profiles/server"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, out)
		}
	}
}

// A mistyped profile and --full together with --profile are refused before
// anything is written.
func TestInit_BadProfileSelectionWritesNothing(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown profile":    {"--profile", "nope"},
		"full and a profile": {"--full", "--profile", "server"},
	} {
		dir := filepath.Join(t.TempDir(), "conf")
		var stdout, stderr bytes.Buffer
		code := runInitWithStdin(append([]string{"--path", dir, "--no-interactive"}, args...), &bytes.Buffer{}, &stdout, &stderr)
		if code == 0 {
			t.Errorf("%s: exit=0, want a refusal", name)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%s: the directory was created although init refused", name)
		}
		if name == "unknown profile" && !strings.Contains(stderr.String(), "personal, server") {
			t.Errorf("%s: stderr does not list the profiles: %s", name, stderr.String())
		}
	}
}

// What init writes for the default profile loads and passes doctor as it is:
// no variable selecting a preset, no key, no token, no other file. `init`
// then `loomcycle` used to start a server with no agents.
func TestInit_DefaultProfileLoadsAndPassesDoctorWithNothingElseSet(t *testing.T) {
	home, _ := isolateLayerEnv(t)
	for _, k := range []string{"LOOMCYCLE_PG_DSN", "LOOMCYCLE_AUTH_TOKEN", "LOOMCYCLE_LISTEN_ADDR", "LOOMCYCLE_DATA_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("LOOMCYCLE_DATA_DIR", filepath.Join(home, "data"))
	// A port nothing listens on, so doctor's bind check is not about this machine.
	t.Setenv("LOOMCYCLE_LISTEN_ADDR", "127.0.0.1:0")
	var stdout, stderr bytes.Buffer
	// No --path: the default location, where `loomcycle` discovers it.
	if code := runInitWithStdin([]string{"--no-interactive"}, &bytes.Buffer{}, &stdout, &stderr); code != 0 {
		t.Fatalf("init: exit=%d stderr=%s", code, stderr.String())
	}

	stack, err := AssembleConfigStack(nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(stack.Files) != 1 || !strings.HasPrefix(stack.Files[0], home) {
		t.Fatalf("discovered files %v, want the one init wrote under %s", stack.Files, home)
	}
	cfg, err := config.LoadLayers(stack.Layers...)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if _, ok := cfg.Agents["chat/medium"]; !ok {
		t.Error("chat/medium missing: the preset the written yaml names was not layered under it")
	}
	if got := strings.Join(stack.Presets, ","); got != "base,local,chat,document-agent,personal" {
		t.Errorf("presets layered = %s, want the personal stack", got)
	}

	stdout.Reset()
	if code := RunDoctor(nil, &stdout, &stderr); code != 0 {
		t.Errorf("doctor exit=%d on what init wrote:\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "0 failures") {
		t.Errorf("doctor reported failures:\n%s", stdout.String())
	}
}

// A config with no routing and no preset selected: doctor says a preset is
// missing and where to name one, instead of passing an empty config in
// silence.
func TestDoctor_NoPresetSelectedSaysWhereToNameOne(t *testing.T) {
	home, _ := isolateLayerEnv(t)
	for _, k := range []string{"LOOMCYCLE_PG_DSN", "LOOMCYCLE_AUTH_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("LOOMCYCLE_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("LOOMCYCLE_LISTEN_ADDR", "127.0.0.1:0")
	writeYAML(t, filepath.Join(home, ".config", "loomcycle", "loomcycle.yaml"), "# nothing set\n")
	var stdout, stderr bytes.Buffer
	RunDoctor(nil, &stdout, &stderr)
	for _, want := range []string{"no preset is selected", "presets: [personal]", "LOOMCYCLE_PRESETS"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("doctor output lacks %q:\n%s", want, stdout.String())
		}
	}
}
