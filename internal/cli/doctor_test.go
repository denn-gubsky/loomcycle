package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubConfig implements configForDoctor with whatever fields the test
// wants. Lets us drive every check without a real yaml.
type stubConfig struct {
	providers      []string
	agentProviders []string
	tierProviders  []string
	backend        string
	pgDSN          string
	dataDir        string
	listen         string
}

func (s *stubConfig) ProviderPriorityList() []string  { return s.providers }
func (s *stubConfig) AgentProviderHints() []string    { return s.agentProviders }
func (s *stubConfig) UserTierProviderHints() []string { return s.tierProviders }
func (s *stubConfig) StorageBackend() string          { return s.backend }
func (s *stubConfig) StoragePgDSN() string            { return s.pgDSN }
func (s *stubConfig) StorageDataDir() string          { return s.dataDir }
func (s *stubConfig) ListenAddrValue() string         { return s.listen }

// withStubLoader swaps loadConfigForDoctor for the duration of one
// test. Restored on cleanup so other tests see the real loader.
func withStubLoader(t *testing.T, stub configForDoctor) {
	orig := loadConfigForDoctor
	loadConfigForDoctor = func(path string) (configForDoctor, error) {
		return stub, nil
	}
	t.Cleanup(func() { loadConfigForDoctor = orig })
}

// withStubLoaderErr installs a loader that always returns err.
func withStubLoaderErr(t *testing.T, err error) {
	orig := loadConfigForDoctor
	loadConfigForDoctor = func(path string) (configForDoctor, error) {
		return nil, err
	}
	t.Cleanup(func() { loadConfigForDoctor = orig })
}

// withTempHome points $HOME at a tempdir so the doctor's auto-
// discovery for ~/.config/loomcycle/loomcycle.yaml can be controlled
// without touching the real $HOME.
func withTempHome(t *testing.T) string {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", "") // force the ~/.config fallback
	return tmp
}

// withEnv sets one env var for the test's duration, restoring after.
func withEnv(t *testing.T, key, val string) {
	t.Setenv(key, val)
}

// freePort picks an OS-assigned listenable port we can pass to doctor's
// "is the listen address bindable?" check. Released immediately.
func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// TestDoctor_ConfigNotFound_ReportsFAIL_AndSuggestsInit
func TestDoctor_ConfigNotFound_ReportsFAIL_AndSuggestsInit(t *testing.T) {
	tmp := withTempHome(t)
	// Run from an empty tempdir so ./loomcycle.yaml isn't found
	// either.
	oldWD, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit=%d; want 1", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "[FAIL]  Config found") {
		t.Errorf("missing FAIL for Config found:\n%s", out)
	}
	if !strings.Contains(out, "loomcycle init") {
		t.Errorf("missing init suggestion:\n%s", out)
	}
}

// TestDoctor_AllPass — happy path with everything green.
func TestDoctor_AllPass(t *testing.T) {
	tmp := t.TempDir()
	withTempHome(t)
	withStubLoader(t, &stubConfig{
		providers: []string{"anthropic"},
		backend:   "sqlite",
		dataDir:   tmp,
		listen:    freePort(t),
	})
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "secret-token-value")
	withEnv(t, "ANTHROPIC_API_KEY", "sk-ant-test")

	// Drop a fake yaml so the auto-discovery step finds it.
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit=%d; want 0 (all pass)\n%s", code, stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"[PASS]  Config found",
		"[PASS]  Config parses",
		"[PASS]  LOOMCYCLE_AUTH_TOKEN set",
		"[PASS]  Provider anthropic",
		"[PASS]  Storage backend",
		"[PASS]  Listen address",
		"0 warnings, 0 failures.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestDoctor_AuthTokenAbsent_WARNs — empty LOOMCYCLE_AUTH_TOKEN
// downgrades to WARN, doesn't FAIL.
func TestDoctor_AuthTokenAbsent_WARNs(t *testing.T) {
	tmp := t.TempDir()
	withTempHome(t)
	withStubLoader(t, &stubConfig{
		providers: []string{},
		backend:   "sqlite",
		dataDir:   tmp,
		listen:    freePort(t),
	})
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("WARN shouldn't fail the run; exit=%d", code)
	}
	if !strings.Contains(stdout.String(), "[WARN]  LOOMCYCLE_AUTH_TOKEN set") {
		t.Errorf("expected WARN for missing auth token:\n%s", stdout.String())
	}
}

// TestDoctor_ProviderKeyMissing_WARNs — provider declared in config
// but its API-key env var is empty: WARN, not FAIL.
func TestDoctor_ProviderKeyMissing_WARNs(t *testing.T) {
	tmp := t.TempDir()
	withTempHome(t)
	withStubLoader(t, &stubConfig{
		providers: []string{"openai"},
		backend:   "sqlite",
		dataDir:   tmp,
		listen:    freePort(t),
	})
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "secret")
	withEnv(t, "OPENAI_API_KEY", "")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("WARN shouldn't fail the run; exit=%d\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "[WARN]  Provider openai") {
		t.Errorf("expected WARN for missing OPENAI_API_KEY:\n%s", stdout.String())
	}
}

// TestDoctor_StorageDataDirNotWritable_FAILs — point sqlite at a
// path we can't create + assert FAIL.
func TestDoctor_StorageDataDirNotWritable_FAILs(t *testing.T) {
	withTempHome(t)
	withStubLoader(t, &stubConfig{
		providers: []string{},
		backend:   "sqlite",
		// /dev/null/something — can't be created as a directory
		dataDir: "/dev/null/loomcycle-doctor-cant-create",
		listen:  freePort(t),
	})
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "x")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit=1 on unwritable sqlite dir; got %d\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "[FAIL]  Storage backend") {
		t.Errorf("expected FAIL for storage:\n%s", stdout.String())
	}
}

// TestDoctor_PortBound_FAILs — bind the port ourselves and assert the
// doctor reports it as not bindable.
func TestDoctor_PortBound_FAILs(t *testing.T) {
	tmp := t.TempDir()
	withTempHome(t)
	// Bind a port and hold it for the test duration.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	addr := l.Addr().String()

	withStubLoader(t, &stubConfig{
		providers: []string{},
		backend:   "sqlite",
		dataDir:   tmp,
		listen:    addr,
	})
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "x")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit=1 when port is taken; got %d\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "[FAIL]  Listen address") {
		t.Errorf("expected FAIL for listen address:\n%s", stdout.String())
	}
}

// TestDoctor_ConfigParseError_FAILs — loader returns an error; FAIL.
func TestDoctor_ConfigParseError_FAILs(t *testing.T) {
	withTempHome(t)
	withStubLoaderErr(t, errors.New("yaml: bad indent line 7"))
	// Need a config file present so the find step succeeds and we
	// reach the parse step.
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("bad"), 0o644)

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit=1 on parse error; got %d\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "[FAIL]  Config parses") {
		t.Errorf("expected FAIL for parse:\n%s", stdout.String())
	}
}

// TestDoctor_LocalProviderNoKey — ollama-local is recognized as
// needing no API key; PASS regardless of env.
func TestDoctor_LocalProviderNoKey(t *testing.T) {
	tmp := t.TempDir()
	withTempHome(t)
	withStubLoader(t, &stubConfig{
		providers: []string{"ollama-local"},
		backend:   "sqlite",
		dataDir:   tmp,
		listen:    freePort(t),
	})
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "x")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit=%d on local-provider happy path:\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "[PASS]  Provider ollama-local") {
		t.Errorf("missing PASS for local provider:\n%s", stdout.String())
	}
}

// TestDoctor_PostgresBackendEmptyDSN_FAILs — postgres backend with
// no DSN is a clear operator misconfig.
func TestDoctor_PostgresBackendEmptyDSN_FAILs(t *testing.T) {
	withTempHome(t)
	withStubLoader(t, &stubConfig{
		providers: []string{},
		backend:   "postgres",
		pgDSN:     "",
		listen:    freePort(t),
	})
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "x")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit=1 on missing pg DSN; got %d\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "DSN empty") {
		t.Errorf("expected DSN-empty hint:\n%s", stdout.String())
	}
}

// TestDoctor_AggregatesPerAgentAndPerTierProviders — regression for
// the v0.11.1 review finding: providers declared per-agent or via
// user_tiers overlay must be visible to doctor, not just the
// top-level provider_priority list. An operator running entirely off
// per-agent pins (empty provider_priority) was previously invisible.
func TestDoctor_AggregatesPerAgentAndPerTierProviders(t *testing.T) {
	tmp := t.TempDir()
	withTempHome(t)
	withStubLoader(t, &stubConfig{
		providers:      []string{},           // top-level empty
		agentProviders: []string{"deepseek"}, // per-agent pin
		tierProviders:  []string{"gemini"},   // per-user-tier overlay
		backend:        "sqlite",
		dataDir:        tmp,
		listen:         freePort(t),
	})
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "x")
	withEnv(t, "DEEPSEEK_API_KEY", "")
	withEnv(t, "GEMINI_API_KEY", "")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)

	var stdout, stderr bytes.Buffer
	_ = RunDoctor(nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "Provider deepseek") {
		t.Errorf("expected per-agent provider deepseek in output:\n%s", out)
	}
	if !strings.Contains(out, "Provider gemini") {
		t.Errorf("expected per-tier provider gemini in output:\n%s", out)
	}
	// Both should WARN (keys not set) not silently skipped.
	if !strings.Contains(out, "[WARN]  Provider deepseek") {
		t.Errorf("expected WARN for missing DEEPSEEK_API_KEY:\n%s", out)
	}
	if !strings.Contains(out, "[WARN]  Provider gemini") {
		t.Errorf("expected WARN for missing GEMINI_API_KEY:\n%s", out)
	}
}

// TestDoctor_PostgresBackendWithDSN_PASS — non-empty DSN passes
// (real connectivity check is deferred to v0.11.2).
func TestDoctor_PostgresBackendWithDSN_PASS(t *testing.T) {
	withTempHome(t)
	withStubLoader(t, &stubConfig{
		providers: []string{},
		backend:   "postgres",
		pgDSN:     "postgres://user:secret@localhost/db",
		listen:    freePort(t),
	})
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "x")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit=%d; want 0:\n%s", code, stdout.String())
	}
	// DSN password masked
	if !strings.Contains(stdout.String(), "user:***@localhost") {
		t.Errorf("DSN password not masked in output:\n%s", stdout.String())
	}
	// Demonstrate the unused fmt import isn't really unused — silence linter
	_ = fmt.Sprintf("")
}
