package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// indexOfPair returns the index of flag when immediately followed by value, or -1.
func indexOfPair(args []string, flag, value string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return i
		}
	}
	return -1
}

// argvContains reports whether any recorded call contains the exact arg v.
func argvContains(calls [][]string, v string) bool {
	for _, c := range calls {
		for _, a := range c {
			if a == v {
				return true
			}
		}
	}
	return false
}

func TestEngine_RunArgs_InjectsEnvBeforeSessionEnv(t *testing.T) {
	e := NewEngine(testCfg(), &fakeRunner{})
	args := e.runArgs("n", openOpts{
		Network: "egress", TmpfsMB: 512, CPUs: 1, MemMB: 512, Pids: 100, Image: "img:latest",
		Env: map[string]string{"GH_TOKEN": "ghp_secret", "GIT_TERMINAL_PROMPT": "0"},
	})
	if !hasPair(args, "--env", "GH_TOKEN=ghp_secret") {
		t.Errorf("injected env missing: %v", args)
	}
	// Injected env must precede sessionEnv so HOME/cache always win on any collision.
	iTok := indexOfPair(args, "--env", "GH_TOKEN=ghp_secret")
	iHome := indexOfPair(args, "--env", "HOME="+workDir)
	if iTok < 0 || iHome < 0 || iTok > iHome {
		t.Errorf("injected env must precede sessionEnv (token@%d home@%d): %v", iTok, iHome, args)
	}
}

func TestCollectEnvHeaders_MapsNames(t *testing.T) {
	h := http.Header{}
	h.Set("X-Loom-Sandbox-Env-Gh-Token", "ghp_x")
	h.Set("X-Loom-Sandbox-Env-My-Var", "v")
	h.Set("Authorization", "Bearer y")
	h.Set("X-Loom-Root-Run", "r")
	env := collectEnvHeaders(h)
	if env["GH_TOKEN"] != "ghp_x" || env["MY_VAR"] != "v" {
		t.Errorf("name mapping wrong: %v", env)
	}
	if len(env) != 2 {
		t.Errorf("should ignore non-env headers, got %v", env)
	}
	if collectEnvHeaders(http.Header{}) != nil {
		t.Errorf("no env headers should return nil")
	}
}

func TestValidateInjectedEnv_AcceptsAndRejects(t *testing.T) {
	got, err := validateInjectedEnv(map[string]string{"GH_TOKEN": "ghp_x", "MY_VAR2": "v"}, 32, 1024)
	if err != nil || got["GH_TOKEN"] != "ghp_x" || len(got) != 2 {
		t.Fatalf("valid env should pass: got %v err %v", got, err)
	}
	// An empty value (an unresolved $cred:) is dropped, not an error.
	if got, err := validateInjectedEnv(map[string]string{"GH_TOKEN": ""}, 32, 1024); err != nil || len(got) != 0 {
		t.Errorf("empty value should drop silently: got %v err %v", got, err)
	}
	rejects := map[string]map[string]string{
		"lowercase+dash":  {"gh-token": "x"},
		"leading digit":   {"1FOO": "x"},
		"reserved HOME":   {"HOME": "/x"},
		"reserved PATH":   {"PATH": "/x"},
		"reserved cache":  {"GOCACHE": "/x"},
		"loomcycle infra": {"LOOMCYCLE_AUTH_TOKEN": "x"},
		"pg dsn":          {"PG_DSN": "postgres://x"},
		"oversize value":  {"BIG": strings.Repeat("a", 2000)},
		"newline value":   {"NL": "a\nb"},
	}
	for name, env := range rejects {
		if _, err := validateInjectedEnv(env, 32, 1024); err == nil {
			t.Errorf("%s: expected rejection, got none", name)
		}
	}
	// Over the per-open var cap.
	many := make(map[string]string, 40)
	for i := 0; i < 40; i++ {
		many[fmt.Sprintf("V%d", i)] = "x"
	}
	if _, err := validateInjectedEnv(many, 32, 1024); err == nil {
		t.Errorf("over-cap var count should be rejected")
	}
}

func TestOpen_EnvInjectionGate(t *testing.T) {
	mk := func(allow bool) (*Dispatcher, *fakeRunner) {
		fr := &fakeRunner{}
		cfg := testCfg()
		cfg.AllowEnvInjection = allow
		return NewDispatcher(cfg, NewEngine(cfg, fr), NewStore(cfg.SessionIdleTTL, cfg.SessionMaxTTL)), fr
	}
	env := map[string]string{"GH_TOKEN": "ghp_secret"}

	// Gate OFF → the token is dropped, but the open still succeeds.
	d, fr := mk(false)
	if _, isErr, err := d.Call(context.Background(), caller{Principal: "p", Env: env}, "sandbox_open", json.RawMessage(`{}`)); err != nil || isErr {
		t.Fatalf("gate-off open failed: err=%v isErr=%v", err, isErr)
	}
	if argvContains(fr.calls, "GH_TOKEN=ghp_secret") {
		t.Errorf("gate off: token must NOT be injected: %v", fr.calls)
	}

	// Gate ON → the token is injected as --env.
	d, fr = mk(true)
	if _, isErr, err := d.Call(context.Background(), caller{Principal: "p", Env: env}, "sandbox_open", json.RawMessage(`{}`)); err != nil || isErr {
		t.Fatalf("gate-on open failed: err=%v isErr=%v", err, isErr)
	}
	if !argvContains(fr.calls, "GH_TOKEN=ghp_secret") {
		t.Errorf("gate on: token should be injected: %v", fr.calls)
	}
}

// TestMCP_EnvHeader_NoLeakInResponse is the secret-never-in-model regression: a
// token delivered via the X-Loom-Sandbox-Env-* header reaches the container env
// but must never appear in the model-facing MCP response body.
func TestMCP_EnvHeader_NoLeakInResponse(t *testing.T) {
	fr := &fakeRunner{}
	cfg := testCfg()
	cfg.AuthToken = "secret-token"
	cfg.AllowEnvInjection = true
	d := NewDispatcher(cfg, NewEngine(cfg, fr), NewStore(cfg.SessionIdleTTL, cfg.SessionMaxTTL))
	h := NewMCPHandler(cfg, d, "test")

	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"sandbox_open","arguments":{}}}`))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("X-Loom-Sandbox-Env-Gh-Token", "ghp_supersecret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	// The token reaches the container env...
	if !argvContains(fr.calls, "GH_TOKEN=ghp_supersecret") {
		t.Errorf("token should reach the container env: %v", fr.calls)
	}
	// ...but must NOT appear in the model-facing response body.
	if strings.Contains(rr.Body.String(), "ghp_supersecret") {
		t.Errorf("secret leaked into the MCP response body: %s", rr.Body.String())
	}
}
