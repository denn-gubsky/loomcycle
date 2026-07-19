package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestClampOpen_ClampsAndDefaults(t *testing.T) {
	cfg := testCfg() // ceilings: tmpfs 2048, cpu 2, mem 2048, pids 512

	// Over-ceiling requests are clamped DOWN.
	o := clampOpen(cfg, "none", 999999, 64, 999999, 999999)
	if o.TmpfsMB != 2048 || o.CPUs != 2 || o.MemMB != 2048 || o.Pids != 512 {
		t.Errorf("over-ceiling not clamped: %+v", o)
	}
	// Zero/omitted → defaults.
	d := clampOpen(cfg, "", 0, 0, 0, 0)
	if d.TmpfsMB != 512 || d.CPUs != 2 || d.MemMB != 2048 || d.Pids != 512 || d.Network != "none" {
		t.Errorf("defaults wrong: %+v", d)
	}
	// A smaller request is honoured.
	sm := clampOpen(cfg, "none", 128, 1, 256, 64)
	if sm.TmpfsMB != 128 || sm.CPUs != 1 || sm.MemMB != 256 || sm.Pids != 64 {
		t.Errorf("smaller request not honoured: %+v", sm)
	}
	if clampOpen(cfg, "egress", 0, 0, 0, 0).Network != "egress" {
		t.Errorf("egress request should set Network=egress (engine still gates it)")
	}
}

func TestSafeRelPath(t *testing.T) {
	good := []string{"main.go", "src/main.go", "a/b/c.txt", ".hidden"}
	for _, p := range good {
		if _, err := safeRelPath(p); err != nil {
			t.Errorf("safeRelPath(%q) unexpected error: %v", p, err)
		}
	}
	bad := []string{"", "/etc/passwd", "../escape", "a/../../b", "a/../b", "x\x00y", "line\nbreak"}
	for _, p := range bad {
		if _, err := safeRelPath(p); err == nil {
			t.Errorf("safeRelPath(%q) should have errored", p)
		}
	}
}

func TestDecodeContent(t *testing.T) {
	if b, err := decodeContent("aGk=", "base64"); err != nil || string(b) != "hi" {
		t.Errorf("base64 decode: %q %v", b, err)
	}
	if _, err := decodeContent("!!!notb64", "base64"); err == nil {
		t.Errorf("expected base64 error")
	}
	if b, _ := decodeContent("plain", "utf8"); string(b) != "plain" {
		t.Errorf("utf8 passthrough failed")
	}
}

// TestDispatch_OpenExecCloseLifecycle drives the tool layer end to end with a
// fake podman, asserting the session lifecycle + exit-code → isError semantics.
func TestDispatch_OpenExecCloseLifecycle(t *testing.T) {
	cfg := testCfg()
	// exec returns exit 1 for a "fail" command, 0 otherwise; run/rm succeed.
	fr := &fakeRunner{fn: func(argv []string, _ []byte) ([]byte, int, error) {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "exec") && strings.Contains(joined, "fail"):
			return []byte("boom"), 1, nil
		case strings.Contains(joined, "exec"):
			return []byte("ok"), 0, nil
		default:
			return nil, 0, nil // run / rm
		}
	}}
	d := NewDispatcher(cfg, NewEngine(cfg, fr), NewStore(cfg.SessionIdleTTL, cfg.SessionMaxTTL))
	ctx := context.Background()
	const principal = "op:test"

	// open
	text, isErr, err := d.Call(ctx, caller{Principal: principal}, "sandbox_open", json.RawMessage(`{}`))
	if err != nil || isErr {
		t.Fatalf("open failed: isErr=%v err=%v text=%s", isErr, err, text)
	}
	var opened struct {
		SessionID string `json:"session_id"`
		Workspace string `json:"workspace_path"`
	}
	if e := json.Unmarshal([]byte(text), &opened); e != nil || opened.SessionID == "" {
		t.Fatalf("open response not parseable: %s (%v)", text, e)
	}
	if opened.Workspace != workDir {
		t.Errorf("workspace_path = %q want %q", opened.Workspace, workDir)
	}

	// exec success → isError false
	args, _ := json.Marshal(map[string]any{"session_id": opened.SessionID, "command": "echo ok"})
	text, isErr, _ = d.Call(ctx, caller{Principal: principal}, "sandbox_exec", args)
	if isErr || !strings.Contains(text, "ok") {
		t.Errorf("exec ok: isErr=%v text=%q", isErr, text)
	}

	// exec failure → isError true + [exit: 1] marker
	args, _ = json.Marshal(map[string]any{"session_id": opened.SessionID, "command": "fail"})
	text, isErr, _ = d.Call(ctx, caller{Principal: principal}, "sandbox_exec", args)
	if !isErr || !strings.Contains(text, "[exit: 1]") {
		t.Errorf("exec fail should be isError with exit marker: isErr=%v text=%q", isErr, text)
	}

	// a foreign principal cannot exec in this session
	_, isErr, _ = d.Call(ctx, caller{Principal: "op:other"}, "sandbox_exec", args)
	if !isErr {
		t.Errorf("foreign principal should get an error (no such session)")
	}

	// close
	closeArgs, _ := json.Marshal(map[string]any{"session_id": opened.SessionID})
	_, isErr, _ = d.Call(ctx, caller{Principal: principal}, "sandbox_close", closeArgs)
	if isErr {
		t.Errorf("close should succeed")
	}
	// second close is idempotent (session gone → not an error)
	_, isErr, _ = d.Call(ctx, caller{Principal: principal}, "sandbox_close", closeArgs)
	if isErr {
		t.Errorf("idempotent close should not error")
	}
}

func TestDispatch_CapacityLimit(t *testing.T) {
	cfg := testCfg()
	cfg.MaxSessions = 1
	fr := &fakeRunner{}
	d := NewDispatcher(cfg, NewEngine(cfg, fr), NewStore(cfg.SessionIdleTTL, cfg.SessionMaxTTL))
	if _, isErr, _ := d.Call(context.Background(), caller{Principal: "p"}, "sandbox_open", json.RawMessage(`{}`)); isErr {
		t.Fatal("first open should succeed")
	}
	text, isErr, _ := d.Call(context.Background(), caller{Principal: "p"}, "sandbox_open", json.RawMessage(`{}`))
	if !isErr || !strings.Contains(text, "capacity") {
		t.Errorf("second open should hit capacity: isErr=%v text=%q", isErr, text)
	}
}
