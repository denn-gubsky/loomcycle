package hooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHook stands up a tiny HTTP server that returns the provided
// response body for every POST it receives. Records the bodies it
// received so tests can assert what the dispatcher sent.
type fakeHook struct {
	srv      *httptest.Server
	respBody string
	status   int
	delay    time.Duration
	calls    int32
	bodies   []string
}

func newFakeHook(t *testing.T, respBody string) *fakeHook {
	t.Helper()
	f := &fakeHook{respBody: respBody, status: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		atomic.AddInt32(&f.calls, 1)
		buf := make([]byte, 1<<14)
		n, _ := r.Body.Read(buf)
		f.bodies = append(f.bodies, string(buf[:n]))
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.respBody))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestDispatcher_PreRewriteInput(t *testing.T) {
	hook := newFakeHook(t, `{"input":{"url":"https://safe.example/redacted"}}`)
	r := NewRegistry()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "redact", Phase: PhasePre, CallbackURL: hook.srv.URL,
		Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a", UserID: "u", AgentID: "ag"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://attacker/"}`)},
	)
	if out.Deny != nil {
		t.Fatalf("unexpected deny: %+v", out.Deny)
	}
	if string(out.Input) != `{"url":"https://safe.example/redacted"}` {
		t.Errorf("Input = %s, want rewritten", out.Input)
	}
}

func TestDispatcher_PreDenyShortCircuits(t *testing.T) {
	denyHook := newFakeHook(t, `{"deny":{"is_error":true,"text":"blocked by policy"}}`)
	// A second hook AFTER the deny one. It should NOT be called when a
	// prior hook short-circuits.
	laterHook := newFakeHook(t, `{"input":{"never":"reached"}}`)

	r := NewRegistry()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "deny", Phase: PhasePre, CallbackURL: denyHook.srv.URL,
		Tools: []string{"WebFetch"},
	})
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "later", Phase: PhasePre, CallbackURL: laterHook.srv.URL,
		Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny == nil {
		t.Fatal("expected deny, got pass-through")
	}
	if !out.Deny.IsError || !strings.Contains(out.Deny.Text, "blocked") {
		t.Errorf("deny payload = %+v, want IsError + text~blocked", out.Deny)
	}
	if atomic.LoadInt32(&laterHook.calls) != 0 {
		t.Errorf("later hook was called %d time(s); deny must short-circuit the chain", laterHook.calls)
	}
}

func TestDispatcher_PostLIFORewrite(t *testing.T) {
	// Two post-hooks. Registered A, B. Post chain runs B (innermost)
	// then A (outermost). Each prepends its name so we can read the
	// final order.
	hookA := newFakeHook(t, `{"result":{"text":"A(B(orig))","is_error":false}}`)
	hookB := newFakeHook(t, `{"result":{"text":"B(orig)","is_error":false}}`)
	r := NewRegistry()
	mustRegister(t, r, &Hook{Owner: "x", Name: "A", Phase: PhasePost, CallbackURL: hookA.srv.URL, Tools: []string{"WebFetch"}})
	mustRegister(t, r, &Hook{Owner: "x", Name: "B", Phase: PhasePost, CallbackURL: hookB.srv.URL, Tools: []string{"WebFetch"}})
	d := NewDispatcher(r, nil)

	got := d.RunPost(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
		ToolResult{Text: "orig"},
	)
	if got.Text != "A(B(orig))" {
		t.Errorf("Post chain output = %q, want %q (LIFO: B inner, A outer)", got.Text, "A(B(orig))")
	}
	// Confirm B saw the original, A saw B's rewrite.
	if !strings.Contains(hookB.bodies[0], `"text":"orig"`) {
		t.Errorf("B's payload didn't include original orig: %s", hookB.bodies[0])
	}
	if !strings.Contains(hookA.bodies[0], `"text":"B(orig)"`) {
		t.Errorf("A's payload didn't include B's rewrite: %s", hookA.bodies[0])
	}
}

func TestDispatcher_FailOpenPassesThroughOnTimeout(t *testing.T) {
	hook := newFakeHook(t, ``)
	hook.delay = 200 * time.Millisecond // exceeds our 50 ms timeout

	r := NewRegistry()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "slow", Phase: PhasePre, CallbackURL: hook.srv.URL,
		Tools: []string{"WebFetch"}, FailMode: FailOpen, TimeoutMs: 50,
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"original":true}`)},
	)
	if out.Deny != nil {
		t.Errorf("fail_mode=open: timeout produced deny=%+v, want pass-through", out.Deny)
	}
	if string(out.Input) != `{"original":true}` {
		t.Errorf("Input = %s, want original (fail-open should preserve)", out.Input)
	}
}

func TestDispatcher_FailClosedDeniesOnTimeout(t *testing.T) {
	hook := newFakeHook(t, ``)
	hook.delay = 200 * time.Millisecond

	r := NewRegistry()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "slow", Phase: PhasePre, CallbackURL: hook.srv.URL,
		Tools: []string{"WebFetch"}, FailMode: FailClosed, TimeoutMs: 50,
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny == nil {
		t.Fatal("fail_mode=closed: timeout did NOT produce deny; security-shaped hook must fail closed")
	}
	if !out.Deny.IsError {
		t.Errorf("deny.IsError = false, want true")
	}
}

func TestDispatcher_EmptyResponseBodyIsNoOp(t *testing.T) {
	// 204 No Content (or empty 200 body) → no rewrite. Common case
	// for telemetry-shaped hooks that just want to observe.
	hook := newFakeHook(t, ``)
	hook.status = 204

	r := NewRegistry()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "telem", Phase: PhasePre, CallbackURL: hook.srv.URL,
		Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"x":1}`)},
	)
	if out.Deny != nil {
		t.Errorf("204 produced deny: %+v", out.Deny)
	}
	if string(out.Input) != `{"x":1}` {
		t.Errorf("Input = %s, want unchanged on 204", out.Input)
	}
	if atomic.LoadInt32(&hook.calls) != 1 {
		t.Errorf("hook calls = %d, want 1 (still invoked, just no-op)", hook.calls)
	}
}

// TestDispatcher_NoMatchIsCheap pins the empty-registry fast path:
// when no hooks match the (agent, tool) the dispatcher does no
// network calls and returns the original input unchanged.
func TestDispatcher_NoMatchIsCheap(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "scoped", Phase: PhasePre, CallbackURL: "https://nope/never",
		Agents: []string{"specific-agent"}, Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	// Different agent → no match. URL would fail to dial but we never
	// call it; if we do, the test surfaces a transport error.
	out := d.RunPre(context.Background(),
		Identity{Agent: "other-agent"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny != nil {
		t.Errorf("no-match produced deny: %+v", out.Deny)
	}
	if string(out.Input) != `{}` {
		t.Errorf("no-match Input = %s, want unchanged", out.Input)
	}
}
