package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// These boot the real binary (RFC ES #8): open mode — no legacy secret and no
// admin token — serves every route to anyone who can reach the port, so it must
// not start on an address beyond loopback unless the operator says so.
//
// Each boot is isolated: an explicit environment (nothing inherited, so no
// ~/.config/loomcycle and no real credentials), a scratch HOME and data dir,
// an explicit --config, and a free port.

const openModeBootYAML = `provider_priority: [mock]
tiers:
  middle:
    - { provider: mock, model: mock-generic }
`

// openModeWarning is a phrase of the boot warning an open-mode server logs.
const openModeWarning = "open mode"

func TestBoot_OpenModeOnNonLoopbackRefused(t *testing.T) {
	b := newOpenModeBoot(t)
	res := b.boot(t, "0.0.0.0", nil)
	if res.served {
		t.Fatalf("open mode on 0.0.0.0 served /healthz; want a refused boot\n%s", res.log)
	}
	if res.exitErr == nil {
		t.Fatalf("open mode on 0.0.0.0 exited cleanly; want a non-zero exit\n%s", res.log)
	}
	for _, want := range []string{"LOOMCYCLE_AUTH_TOKEN", "LOOMCYCLE_ALLOW_OPEN_MODE=1", "LOOMCYCLE_LISTEN_ADDR"} {
		if !strings.Contains(res.log, want) {
			t.Errorf("refusal should name %s\n%s", want, res.log)
		}
	}
}

// A non-loopback gRPC listener is refused the same way, even with the HTTP
// listener on loopback: it serves the same routes under the same open-mode
// decision.
func TestBoot_OpenModeOnNonLoopbackGRPCRefused(t *testing.T) {
	b := newOpenModeBoot(t)
	res := b.boot(t, "127.0.0.1", map[string]string{"LOOMCYCLE_GRPC_ADDR": fmt.Sprintf(":%d", freePort(t))})
	if res.served || res.exitErr == nil {
		t.Fatalf("open mode with gRPC on all interfaces booted; want a refused boot\n%s", res.log)
	}
	if !strings.Contains(res.log, "LOOMCYCLE_GRPC_ADDR") {
		t.Errorf("refusal should name LOOMCYCLE_GRPC_ADDR\n%s", res.log)
	}
}

func TestBoot_OpenModeOnLoopbackAllowed(t *testing.T) {
	b := newOpenModeBoot(t)
	res := b.boot(t, "127.0.0.1", nil)
	if !res.served {
		t.Fatalf("open mode on loopback did not serve /healthz\n%s", res.log)
	}
	if !strings.Contains(res.log, openModeWarning) {
		t.Errorf("open mode on loopback should still log the open-mode warning\n%s", res.log)
	}
}

func TestBoot_OpenModeOptInAllowsNonLoopback(t *testing.T) {
	b := newOpenModeBoot(t)
	res := b.boot(t, "0.0.0.0", map[string]string{"LOOMCYCLE_ALLOW_OPEN_MODE": "1"})
	if !res.served {
		t.Fatalf("open mode on 0.0.0.0 with the opt-in did not serve /healthz\n%s", res.log)
	}
	if !strings.Contains(res.log, openModeWarning) {
		t.Errorf("an opted-in open mode should still log the open-mode warning\n%s", res.log)
	}
}

// An admin operator token in the store turns auth on with no legacy secret:
// such a deployment is not in open mode, so it boots on a public address and
// does not log that it is unauthenticated.
func TestBoot_AdminTokenWithoutLegacySecretIsNotOpenMode(t *testing.T) {
	b := newOpenModeBoot(t)
	st, err := storesqlite.Open(filepath.Join(b.dataDir, "loomcycle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_, err = st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID:         "def_root",
		Name:          "root",
		TenantID:      "default",
		Subject:       "root",
		TokenHash:     "not-a-real-token-hash",
		AllowedScopes: []string{auth.ScopeAdmin},
	})
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("seed admin token: %v", err)
	}

	res := b.boot(t, "0.0.0.0", nil)
	if !res.served {
		t.Fatalf("auth by admin token on 0.0.0.0 did not serve /healthz\n%s", res.log)
	}
	if res.unauthStatus != http.StatusUnauthorized {
		t.Errorf("GET /v1/runs with no bearer = %d, want 401", res.unauthStatus)
	}
	for _, phrase := range []string{openModeWarning, "unauthenticated"} {
		if strings.Contains(res.log, phrase) {
			t.Errorf("an admin-token deployment logged %q as if it were open\n%s", phrase, res.log)
		}
	}
}

// Config principals turn auth on with no legacy secret and no admin token
// (RFC ES F2): the deployment boots on a public address, refuses a request with
// no bearer, and does not log that it is unauthenticated.
func TestBoot_DeclaredPrincipalsWithoutLegacySecretIsNotOpenMode(t *testing.T) {
	b := newOpenModeBoot(t)
	yaml := openModeBootYAML + `principals:
  alice:
    tenant: acme
    subject: alice
    scopes: [substrate:tenant]
    token_env: LOOMCYCLE_ALICE_TOKEN
`
	if err := os.WriteFile(b.cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	res := b.boot(t, "0.0.0.0", map[string]string{"LOOMCYCLE_ALICE_TOKEN": "lct_boot_test_alice"})
	if !res.served {
		t.Fatalf("auth by config principals on 0.0.0.0 did not serve /healthz\n%s", res.log)
	}
	if res.unauthStatus != http.StatusUnauthorized {
		t.Errorf("GET /v1/runs with no bearer = %d, want 401", res.unauthStatus)
	}
	for _, phrase := range []string{openModeWarning, "unauthenticated"} {
		if strings.Contains(res.log, phrase) {
			t.Errorf("a principals-only deployment logged %q as if it were open\n%s", phrase, res.log)
		}
	}
}

// The boot tests share one build of the binary: each build links the whole
// runtime, which takes tens of seconds on a loaded machine.
var bootBinary struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if bootBinary.dir != "" {
		_ = os.RemoveAll(bootBinary.dir)
	}
	os.Exit(code)
}

func builtBootBinary(t *testing.T) string {
	t.Helper()
	bootBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "loomcycle-boot-test-")
		if err != nil {
			bootBinary.err = err
			return
		}
		bootBinary.dir = dir
		bootBinary.path = filepath.Join(dir, "loomcycle-test")
		if out, err := exec.Command("go", "build", "-o", bootBinary.path, ".").CombinedOutput(); err != nil {
			bootBinary.err = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if bootBinary.err != nil {
		t.Fatal(bootBinary.err)
	}
	return bootBinary.path
}

type openModeBoot struct {
	bin, dir, dataDir, cfgPath string
}

func newOpenModeBoot(t *testing.T) *openModeBoot {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and boots the binary; skipped in -short")
	}
	dir := t.TempDir()
	b := &openModeBoot{
		bin:     builtBootBinary(t),
		dir:     dir,
		dataDir: filepath.Join(dir, "data"),
		cfgPath: filepath.Join(dir, "loomcycle.yaml"),
	}
	for _, d := range []string{b.dataDir, filepath.Join(dir, "home")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(b.cfgPath, []byte(openModeBootYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

type bootResult struct {
	served       bool  // /healthz answered 200
	unauthStatus int   // GET /v1/runs with no bearer, when served
	exitErr      error // set when the process exited on its own
	log          string
}

// boot starts the binary listening on host:<free port> and waits until it
// either answers /healthz or exits. A served process is stopped afterwards.
func (b *openModeBoot) boot(t *testing.T, host string, extra map[string]string) bootResult {
	t.Helper()
	port := freePort(t)
	env := map[string]string{
		"HOME":                   filepath.Join(b.dir, "home"),
		"LOOMCYCLE_DATA_DIR":     b.dataDir,
		"LOOMCYCLE_LISTEN_ADDR":  net.JoinHostPort(host, fmt.Sprint(port)),
		"LOOMCYCLE_MOCK_ENABLED": "1",
	}
	for k, v := range extra {
		env[k] = v
	}
	cmd := exec.Command(b.bin, "--config", b.cfgPath)
	cmd.Dir = b.dir
	cmd.Env = []string{}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.After(90 * time.Second)
	for {
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("exit status 0")
			}
			return bootResult{exitErr: err, log: out.String()}
		case <-deadline:
			_ = cmd.Process.Kill()
			<-exited
			t.Fatalf("binary neither served /healthz nor exited within 90s\n%s", out.String())
		case <-time.After(200 * time.Millisecond):
		}
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		res := bootResult{served: true}
		if r2, err := client.Get(base + "/v1/runs?user_id=u"); err == nil {
			res.unauthStatus = r2.StatusCode
			_ = r2.Body.Close()
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		res.log = out.String()
		return res
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// lockedBuffer collects a child's stdout and stderr, written from two copy
// goroutines and read by the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
