package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// dispatcherWithFake builds a Dispatcher over a fake runner (podman/docker calls
// are recorded, not executed).
func dispatcherWithFake() (*Dispatcher, *fakeRunner) {
	cfg := testCfg()
	fr := &fakeRunner{}
	return NewDispatcher(cfg, NewEngine(cfg, fr), NewStore(cfg.SessionIdleTTL, cfg.SessionMaxTTL)), fr
}

func openSession(t *testing.T, d *Dispatcher, c caller) string {
	t.Helper()
	text, isErr, err := d.Call(context.Background(), c, "sandbox_open", json.RawMessage(`{}`))
	if err != nil || isErr {
		t.Fatalf("open failed: isErr=%v err=%v text=%s", isErr, err, text)
	}
	var o struct {
		SessionID string `json:"session_id"`
	}
	if e := json.Unmarshal([]byte(text), &o); e != nil || o.SessionID == "" {
		t.Fatalf("open response not parseable: %s", text)
	}
	return o.SessionID
}

func TestDispatch_Touch(t *testing.T) {
	d, _ := dispatcherWithFake()
	ctx := context.Background()
	const p = "op:test"
	id := openSession(t, d, caller{Principal: p})

	// touch resets the idle clock (Get's side effect) — assert LastUsed advances.
	before, _ := d.store.Get(id, p, time.Unix(0, 0))
	oldUsed := before.LastUsed
	targs, _ := json.Marshal(map[string]any{"session_id": id})
	res, isErr, _ := d.Call(ctx, caller{Principal: p}, "sandbox_touch", targs)
	if isErr || !strings.Contains(res, "idle timer reset") {
		t.Errorf("touch: isErr=%v res=%q", isErr, res)
	}
	after, _ := d.store.Get(id, p, time.Now())
	if !after.LastUsed.After(oldUsed) {
		t.Errorf("touch should advance LastUsed (%v !> %v)", after.LastUsed, oldUsed)
	}
	// unknown session → isError
	bad, _ := json.Marshal(map[string]any{"session_id": "nope"})
	if _, isErr, _ := d.Call(ctx, caller{Principal: p}, "sandbox_touch", bad); !isErr {
		t.Errorf("touch on unknown session should error")
	}
	// a foreign principal can't touch it
	if _, isErr, _ := d.Call(ctx, caller{Principal: "op:other"}, "sandbox_touch", targs); !isErr {
		t.Errorf("foreign principal must not touch another's session")
	}
}

func TestDispatch_CloseRun(t *testing.T) {
	d, _ := dispatcherWithFake()
	ctx := context.Background()
	const p = "op:test"

	a1 := openSession(t, d, caller{Principal: p, RootRun: "run-A"})
	a2 := openSession(t, d, caller{Principal: p, RootRun: "run-A"})
	b1 := openSession(t, d, caller{Principal: p, RootRun: "run-B"})
	openSession(t, d, caller{Principal: p}) // untagged
	if d.store.Count() != 4 {
		t.Fatalf("expected 4 sessions, got %d", d.store.Count())
	}

	// close_run(run-A) closes exactly a1+a2.
	args, _ := json.Marshal(map[string]any{"root_run_id": "run-A"})
	res, isErr, _ := d.Call(ctx, caller{Principal: p}, "sandbox_close_run", args)
	if isErr || !strings.Contains(res, "closed 2 session") {
		t.Errorf("close_run(run-A): isErr=%v res=%q", isErr, res)
	}
	if _, ok := d.store.Get(a1, p, time.Now()); ok {
		t.Errorf("a1 should be gone")
	}
	if _, ok := d.store.Get(a2, p, time.Now()); ok {
		t.Errorf("a2 should be gone")
	}
	if _, ok := d.store.Get(b1, p, time.Now()); !ok {
		t.Errorf("b1 (run-B) must survive")
	}
	if d.store.Count() != 2 {
		t.Errorf("run-B + untagged should remain; count=%d", d.store.Count())
	}

	// empty root_run_id is refused (never sweeps untagged sessions).
	empty, _ := json.Marshal(map[string]any{"root_run_id": ""})
	if _, isErr, _ := d.Call(ctx, caller{Principal: p}, "sandbox_close_run", empty); !isErr {
		t.Errorf("close_run with empty root_run_id should error")
	}
	// a foreign principal closing run-B closes nothing (principal-scoped).
	argsB, _ := json.Marshal(map[string]any{"root_run_id": "run-B"})
	res, _, _ = d.Call(ctx, caller{Principal: "op:other"}, "sandbox_close_run", argsB)
	if !strings.Contains(res, "closed 0 session") {
		t.Errorf("foreign close_run should close 0, got %q", res)
	}
	if _, ok := d.store.Get(b1, p, time.Now()); !ok {
		t.Errorf("b1 must survive a foreign close_run")
	}
}

// TestMCP_TagsSessionFromAttestedHeader: the X-Loom-Root-Run header (set by the
// handler from the request, NOT a tool arg) tags the session, so close_run finds
// it. Drives it through the real HTTP handler.
func TestMCP_TagsSessionFromAttestedHeader(t *testing.T) {
	h := newTestHandler(t, &fakeRunner{})
	call := func(rootRun, body string) string {
		r, _ := http.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer secret-token")
		if rootRun != "" {
			r.Header.Set("X-Loom-Root-Run", rootRun)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Body.String()
	}
	openText := call("run-Z", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sandbox_open","arguments":{}}}`)
	if !strings.Contains(openText, "session_id") {
		t.Fatalf("open failed: %s", openText)
	}
	// The session is tagged run-Z from the header; close_run(run-Z) closes it.
	closeText := call("run-Z", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sandbox_close_run","arguments":{"root_run_id":"run-Z"}}}`)
	if !strings.Contains(closeText, "closed 1 session") {
		t.Errorf("close_run should have found the header-tagged session; got %s", closeText)
	}
}
