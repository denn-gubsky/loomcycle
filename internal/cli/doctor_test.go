package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// stubConfig implements configForDoctor with whatever fields the test
// wants. Lets us drive every check without a real yaml.
type stubConfig struct {
	providers      []string
	agentProviders []string
	tierProviders  []string
	providerKeyEnv map[string]string // id → api_key_env (RFC BF P2a)
	backend        string
	pgDSN          string
	dataDir        string
	listen         string
	configWarnings []string
	awsProviders   map[string]AWSAuthProvider
	hostShell      bool
	tokenEnv       config.Env
	grpc           string
	allowOpen      bool
	principals     int
}

func (s *stubConfig) TokenHasher() (auth.TokenHasher, error) { return s.tokenEnv.TokenHasher() }

func (s *stubConfig) NonLoopbackListeners() []string {
	return (&config.Env{ListenAddr: configListenAddr(s), GrpcAddr: s.grpc}).NonLoopbackListeners()
}
func (s *stubConfig) AllowOpenMode() bool     { return s.allowOpen }
func (s *stubConfig) DeclaredPrincipals() int { return s.principals }

func (s *stubConfig) AWSAuthProviders() map[string]AWSAuthProvider { return s.awsProviders }
func (s *stubConfig) HostShellEnabled() bool                       { return s.hostShell }

func (s *stubConfig) ProviderPriorityList() []string  { return s.providers }
func (s *stubConfig) ConfigWarnings() []string        { return s.configWarnings }
func (s *stubConfig) AgentProviderHints() []string    { return s.agentProviders }
func (s *stubConfig) UserTierProviderHints() []string { return s.tierProviders }
func (s *stubConfig) ProviderAPIKeyEnv(id string) string {
	return s.providerKeyEnv[id]
}
func (s *stubConfig) StorageBackend() string  { return s.backend }
func (s *stubConfig) StoragePgDSN() string    { return s.pgDSN }
func (s *stubConfig) StorageDataDir() string  { return s.dataDir }
func (s *stubConfig) ListenAddrValue() string { return s.listen }

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
		// With auth on, an all-pass setup has a token pepper.
		tokenEnv: config.Env{OperatorTokenPepper: "pep"},
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

// runDoctorOpenMode runs doctor with no LOOMCYCLE_AUTH_TOKEN and the given
// listeners, returning its output.
func runDoctorOpenMode(t *testing.T, stub *stubConfig) string {
	t.Helper()
	withTempHome(t)
	stub.backend, stub.dataDir = "sqlite", t.TempDir()
	withStubLoader(t, stub)
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)
	var stdout, stderr bytes.Buffer
	if code := RunDoctor(nil, &stdout, &stderr); code != 0 {
		t.Errorf("a WARN shouldn't fail the run; exit=%d\n%s", code, stdout.String())
	}
	return stdout.String()
}

// TestDoctor_OpenModeOnPublicListener_WARNsOfBootRefusal: with no auth token
// and a listener beyond loopback, doctor names the boot refusal and its fixes.
func TestDoctor_OpenModeOnPublicListener_WARNsOfBootRefusal(t *testing.T) {
	_, port, _ := net.SplitHostPort(freePort(t))
	out := runDoctorOpenMode(t, &stubConfig{listen: "127.0.0.1:" + port, grpc: ":8788"})
	if !strings.Contains(out, "[WARN]  Open mode") ||
		!strings.Contains(out, "LOOMCYCLE_GRPC_ADDR=:8788") ||
		!strings.Contains(out, "refuses to start") ||
		!strings.Contains(out, "LOOMCYCLE_ALLOW_OPEN_MODE=1") {
		t.Errorf("want an open-mode WARN naming the gRPC listener and the refusal:\n%s", out)
	}
}

// TestDoctor_OpenModeOptIn_WARNsServedUnauthenticated: with the opt-in the
// boot is not refused, but doctor still says the routes are open.
func TestDoctor_OpenModeOptIn_WARNsServedUnauthenticated(t *testing.T) {
	_, port, _ := net.SplitHostPort(freePort(t))
	out := runDoctorOpenMode(t, &stubConfig{listen: "0.0.0.0:" + port, allowOpen: true})
	if !strings.Contains(out, "[WARN]  Open mode") || !strings.Contains(out, "unauthenticated") ||
		strings.Contains(out, "refuses to start") {
		t.Errorf("want an opted-in open-mode WARN, not a refusal:\n%s", out)
	}
}

// TestDoctor_OpenModeOnLoopback_NoOpenModeWarning: open mode on loopback is
// allowed, so there is no open-mode line beyond the auth-token WARN.
func TestDoctor_OpenModeOnLoopback_NoOpenModeWarning(t *testing.T) {
	out := runDoctorOpenMode(t, &stubConfig{listen: freePort(t)})
	if strings.Contains(out, "Open mode") {
		t.Errorf("open mode on loopback should not WARN of a refusal:\n%s", out)
	}
}

// TestDoctor_DeclaredPrincipalsAreAuth: config principals turn auth on (RFC ES
// F2), so a deployment with no LOOMCYCLE_AUTH_TOKEN but declared principals is
// not open — no open-mode WARN on a public listener, and doctor says auth is on.
func TestDoctor_DeclaredPrincipalsAreAuth(t *testing.T) {
	_, port, _ := net.SplitHostPort(freePort(t))
	out := runDoctorOpenMode(t, &stubConfig{listen: "0.0.0.0:" + port, principals: 2})
	if strings.Contains(out, "Open mode") || strings.Contains(out, "unauthenticated") {
		t.Errorf("principals declared: doctor still reports open mode:\n%s", out)
	}
	if !strings.Contains(out, "[PASS]  Authentication") || !strings.Contains(out, "2 config principal") {
		t.Errorf("principals declared: want a PASS saying auth is on through them:\n%s", out)
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

// A taken listen address is NOT a config FAIL — it almost always means a
// runtime already owns this state. doctor WARNs (exit 0) and points at the
// single-runtime invariant + the thin-client (`--upstream`) path (RFC R),
// rather than failing the operator who ran doctor while their server is up.
func TestDoctor_PortBound_WarnsSingleRuntime(t *testing.T) {
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
	if code != 0 {
		t.Errorf("expected exit=0 (WARN, not FAIL) when port is taken; got %d\n%s", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "[WARN]  Listen address") {
		t.Errorf("expected WARN for listen address:\n%s", out)
	}
	if !strings.Contains(out, "--upstream") {
		t.Errorf("expected single-runtime / --upstream guidance:\n%s", out)
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

// An AWS-authenticated provider is checked by whose credentials it would use,
// not as a key-less "local provider"; a provider whose chain finds nothing is
// a WARN with the reason, and a host shell beside AWS credentials is a WARN.
func TestDoctor_AWSProvidersReportTheirIdentity(t *testing.T) {
	tmp := t.TempDir()
	withTempHome(t)
	auth := config.ProviderAuth{Type: config.ProviderAuthSigV4, Region: "eu-central-1"}
	withStubLoader(t, &stubConfig{
		agentProviders: []string{"bedrock-claude"},
		backend:        "sqlite",
		dataDir:        tmp,
		listen:         freePort(t),
		awsProviders: map[string]AWSAuthProvider{
			"bedrock-claude": {BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic", Auth: auth},
			"bedrock-mantle": {BaseURL: "https://bedrock-mantle.eu-central-1.api.aws/v1", Auth: auth},
		},
		hostShell: true,
	})
	orig := awsIdentityForDoctor
	t.Cleanup(func() { awsIdentityForDoctor = orig })
	awsIdentityForDoctor = func(_ context.Context, p AWSAuthProvider) (string, string, string, error) {
		if strings.Contains(p.BaseURL, "mantle") {
			return "", "", "", errors.New("aws credentials unavailable: no EC2 IMDS role found")
		}
		return "123456789012", "arn:aws:sts::123456789012:assumed-role/loomcycle-bedrock/i-0abc", "EC2RoleProvider", nil
	}
	withEnv(t, "LOOMCYCLE_AUTH_TOKEN", "x")
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "loomcycle")
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "loomcycle.yaml"), []byte("p"), 0o644)

	var stdout, stderr bytes.Buffer
	if code := RunDoctor(nil, &stdout, &stderr); code != 0 {
		t.Errorf("exit=%d; an AWS warning must not fail doctor:\n%s", code, stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"[PASS]  Provider bedrock-claude",
		"assumed-role/loomcycle-bedrock/i-0abc in account 123456789012, from EC2RoleProvider",
		"[WARN]  Provider bedrock-mantle",
		"no usable AWS credentials",
		"[WARN]  AWS credentials and host shell",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "local provider") {
		t.Errorf("an AWS provider was reported as a key-less local provider:\n%s", out)
	}
}

func TestCheckTokenPepper_ReportsTheSourceAndWarnsOnNoneWithAuth(t *testing.T) {
	key := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" // 32 bytes of 0x01
	for _, tc := range []struct {
		name     string
		env      config.Env
		authSet  bool
		wantLine string
	}{
		{"explicit", config.Env{OperatorTokenPepper: "p"}, true, "PASS"},
		{"derived", config.Env{SecretKey: key}, true, "move to it at their next login"},
		{"derived, fallback off", config.Env{SecretKey: key, TokenLegacyHashOff: true}, true, "unpeppered token hashes refused"},
		{"none with auth", config.Env{}, true, "WARN"},
		{"none without auth", config.Env{}, false, "no auth configured"},
		{"malformed key", config.Env{SecretKey: "short"}, true, "FAIL"},
	} {
		var out bytes.Buffer
		r := &doctorRun{stdout: &out}
		checkTokenPepper(r, &stubConfig{tokenEnv: tc.env}, tc.authSet)
		if !strings.Contains(out.String(), tc.wantLine) {
			t.Errorf("%s: output %q lacks %q", tc.name, out.String(), tc.wantLine)
		}
	}
}
