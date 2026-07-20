package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEngine_RunArgs_DurableWorkspaceBindMount(t *testing.T) {
	e := NewEngine(testCfg(), &fakeRunner{})
	args := e.runArgs("n", openOpts{
		Network: "none", TmpfsMB: 512, CPUs: 1, MemMB: 512, Pids: 100, Image: "img",
		WorkspaceHostDir: "/data/ws/op_x/proj",
	})
	// /work is a persistent bind mount, NOT tmpfs.
	if !hasPair(args, "-v", "/data/ws/op_x/proj:/work:rw") {
		t.Errorf("expected -v bind mount for the durable workspace: %v", args)
	}
	for i, a := range args {
		if a == "--tmpfs" && i+1 < len(args) && strings.HasPrefix(args[i+1], workDir+":") {
			t.Errorf("tmpfs /work must NOT be present when a workspace is bound: %v", args)
		}
	}
	// Hardening otherwise unchanged: read-only rootfs + /tmp tmpfs still there.
	if !has(args, "--read-only") {
		t.Errorf("--read-only missing: %v", args)
	}
	if !hasPair(args, "--tmpfs", "/tmp:rw,size=64m,exec") {
		t.Errorf("/tmp tmpfs missing: %v", args)
	}
	// Default (no workspace) stays tmpfs — no bind mount.
	def := e.runArgs("n", openOpts{Network: "none", TmpfsMB: 512, CPUs: 1, MemMB: 512, Pids: 100, Image: "img"})
	if has(def, "-v") {
		t.Errorf("default session must not bind-mount /work: %v", def)
	}
}

// TestEngine_RunArgs_TmpfsWorkWritableByNonRoot is the regression for the /work
// permission bug: the container runs as a NON-ROOT user (--user CtrUser), but a
// tmpfs mounts root-owned — so mode=0700 left /work neither writable nor readable
// by that user (file writes AND `bash -l` reading $HOME/.bash_profile both failed
// with "permission denied"). /work must be mode=0777.
func TestEngine_RunArgs_TmpfsWorkWritableByNonRoot(t *testing.T) {
	e := NewEngine(testCfg(), &fakeRunner{})
	args := e.runArgs("n", openOpts{Network: "none", TmpfsMB: 512, CPUs: 1, MemMB: 512, Pids: 100, Image: "img"})
	var workTmpfs string
	for i, a := range args {
		if a == "--tmpfs" && i+1 < len(args) && strings.HasPrefix(args[i+1], workDir+":") {
			workTmpfs = args[i+1]
		}
	}
	if workTmpfs == "" {
		t.Fatalf("no tmpfs /work mount found: %v", args)
	}
	if !strings.Contains(workTmpfs, "mode=0777") {
		t.Errorf("/work tmpfs must be mode=0777 (writable by the non-root user); got %q", workTmpfs)
	}
	if strings.Contains(workTmpfs, "mode=0700") {
		t.Errorf("/work tmpfs is mode=0700 (root-only) — the non-root user can't write: %q", workTmpfs)
	}
	if !strings.Contains(workTmpfs, "exec") {
		t.Errorf("/work tmpfs lost exec (compiled binaries must run in /work): %q", workTmpfs)
	}
}

func TestValidateWorkspaceName(t *testing.T) {
	for _, n := range []string{"proj", "my-repo", "a1", "build_2", "x"} {
		if err := validateWorkspaceName(n); err != nil {
			t.Errorf("good name %q rejected: %v", n, err)
		}
	}
	for _, n := range []string{"", "../escape", "a/b", ".hidden", "-lead", "_lead", "UP", "x\x00y", strings.Repeat("a", 65)} {
		if err := validateWorkspaceName(n); err == nil {
			t.Errorf("bad name %q should have errored", n)
		}
	}
}

func TestPrincipalSegment(t *testing.T) {
	if got := principalSegment("op:abc123"); got != "op_abc123" {
		t.Errorf("colon not sanitized: %q", got)
	}
	if got := principalSegment(""); got != "_anon" {
		t.Errorf("empty principal = %q, want _anon", got)
	}
	if strings.ContainsAny(principalSegment("a/b\\c:d"), `/\:`) {
		t.Errorf("unsafe chars survived sanitization")
	}
}

func TestResolveWorkspaceDir_GateAndFence(t *testing.T) {
	// Gate: no root configured → refused.
	d0 := NewDispatcher(&Config{}, nil, nil)
	if _, err := d0.resolveWorkspaceDir("op:x", "proj"); err == nil {
		t.Errorf("expected an error when WorkspaceRoot is unset")
	}

	// Enabled: derives + provisions <root>/<principal>/<name>.
	root := t.TempDir()
	cfg := testCfg()
	cfg.WorkspaceRoot = root
	cfg.CtrUser = "1000:1000" // chown is best-effort; will no-op as a non-root test user
	d := NewDispatcher(cfg, nil, nil)

	dir, err := d.resolveWorkspaceDir("op:abc", "proj")
	if err != nil {
		t.Fatalf("resolveWorkspaceDir: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(filepath.Join(root, "op_abc", "proj"))
	if dir != wantResolved {
		t.Errorf("dir = %q, want %q", dir, wantResolved)
	}
	if fi, e := os.Stat(dir); e != nil || !fi.IsDir() {
		t.Errorf("workspace dir not provisioned: %v", e)
	}
	// A hostile name never escapes (charset gate).
	if _, err := d.resolveWorkspaceDir("op:abc", "../escape"); err == nil {
		t.Errorf("`../escape` must be rejected")
	}
	// Two principals get separate subtrees.
	other, _ := d.resolveWorkspaceDir("op:zzz", "proj")
	if other == dir {
		t.Errorf("different principals must not share a workspace dir")
	}
}

func TestDispatch_OpenWithWorkspace(t *testing.T) {
	root := t.TempDir()
	cfg := testCfg()
	cfg.WorkspaceRoot = root
	fr := &fakeRunner{}
	d := NewDispatcher(cfg, NewEngine(cfg, fr), NewStore(cfg.SessionIdleTTL, cfg.SessionMaxTTL))

	args, _ := json.Marshal(map[string]any{"workspace": "proj"})
	text, isErr, err := d.Call(context.Background(), caller{Principal: "op:test"}, "sandbox_open", args)
	if err != nil || isErr {
		t.Fatalf("open failed: isErr=%v err=%v text=%s", isErr, err, text)
	}
	if !strings.Contains(text, `"persistent":true`) || !strings.Contains(text, `"workspace":"proj"`) {
		t.Errorf("open response should mark the durable workspace: %s", text)
	}
	joined := strings.Join(fr.calls[0], " ")
	if !strings.Contains(joined, ":/work:rw") {
		t.Errorf("expected a durable-workspace bind mount in the run argv: %v", fr.calls[0])
	}
	if strings.Contains(joined, "--tmpfs /work") {
		t.Errorf("tmpfs /work must not be used with a workspace: %v", fr.calls[0])
	}

	// Gate: with no WorkspaceRoot, a workspace request is refused (tmpfs-only).
	cfg2 := testCfg()
	d2 := NewDispatcher(cfg2, NewEngine(cfg2, &fakeRunner{}), NewStore(cfg2.SessionIdleTTL, cfg2.SessionMaxTTL))
	text2, isErr2, _ := d2.Call(context.Background(), caller{Principal: "op:test"}, "sandbox_open", args)
	if !isErr2 || !strings.Contains(text2, "not enabled") {
		t.Errorf("workspace without a root should be refused: isErr=%v text=%s", isErr2, text2)
	}
}
