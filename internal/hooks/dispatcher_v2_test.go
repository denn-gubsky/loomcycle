package hooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every payload places the call in its run: which run, which run spawned it,
// which iteration. Without them a hook cannot correlate one run's calls or
// tell a sub-agent's call from its parent's.
func TestDispatcher_PayloadCarriesTheRun(t *testing.T) {
	pre := newFakeHook(t, `{}`)
	post := newFakeHook(t, `{}`)
	r := NewRegistry()
	mustRegister(t, r, &Hook{Owner: "x", Name: "p", Phase: PhasePre, CallbackURL: pre.srv.URL})
	mustRegister(t, r, &Hook{Owner: "x", Name: "q", Phase: PhasePost, CallbackURL: post.srv.URL})
	d := NewDispatcher(r, nil)
	ident := Identity{Agent: "a", RunID: "r_child", ParentRunID: "r_parent", Iteration: 3}
	tc := ToolCall{ID: "t1", Name: "Read", Input: json.RawMessage(`{}`)}
	d.RunPre(context.Background(), ident, tc)
	d.RunPost(context.Background(), ident, tc, ToolResult{Text: "ok"})
	for name, body := range map[string]string{"pre": pre.bodies[0], "post": post.bodies[0]} {
		for _, want := range []string{`"run_id":"r_child"`, `"parent_run_id":"r_parent"`, `"iteration":3`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s payload lacks %s: %s", name, want, body)
			}
		}
	}
}

// What each hook did is reported, in order — deny, rewrite, context, and a
// hook that failed — so the run can record it. A pass-through reports nothing.
func TestDispatcher_ReportsEachDecision(t *testing.T) {
	passthrough := newFakeHook(t, `{}`)
	rewrite := newFakeHook(t, `{"input":{"path":"/safe"}}`)
	deny := newFakeHook(t, `{"deny":{"text":"not here","is_error":true}}`)
	r := NewRegistry()
	mustRegister(t, r, &Hook{Owner: "x", Name: "pass", Phase: PhasePre, CallbackURL: passthrough.srv.URL})
	mustRegister(t, r, &Hook{Owner: "x", Name: "rw", Phase: PhasePre, CallbackURL: rewrite.srv.URL})
	mustRegister(t, r, &Hook{Owner: "x", Name: "no", Phase: PhasePre, CallbackURL: deny.srv.URL})
	d := NewDispatcher(r, nil)
	out := d.RunPre(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read", Input: json.RawMessage(`{"path":"/etc"}`)})
	if len(out.Decisions) != 2 {
		t.Fatalf("decisions = %+v, want rewrite then deny (the pass-through reports nothing)", out.Decisions)
	}
	if d0 := out.Decisions[0]; d0.Kind != "rewrite_input" || d0.Name != "rw" || string(d0.UpdatedInput) != `{"path":"/safe"}` {
		t.Errorf("first decision = %+v", d0)
	}
	if d1 := out.Decisions[1]; d1.Kind != "deny" || d1.Reason != "not here" {
		t.Errorf("second decision = %+v", d1)
	}
}

// A hook that fails is reported, with what its fail mode made of it.
func TestDispatcher_ReportsAnUnavailableHook(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, &Hook{Owner: "x", Name: "down", Phase: PhasePre, CallbackURL: "http://127.0.0.1:1/nope", FailMode: FailClosed})
	d := NewDispatcher(r, nil)
	out := d.RunPre(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read"})
	if out.Deny == nil || len(out.Decisions) != 1 || out.Decisions[0].Kind != "unavailable" || out.Decisions[0].FailMode != FailClosed {
		t.Errorf("outcome = %+v, want a closed-fail deny reported as unavailable", out)
	}
}

// post_failure fires only when the tool failed, innermost (before the post
// chain), and sees the failure's classification; additional_context is
// returned for the loop to append.
func TestDispatcher_PostFailureSeesOnlyFailuresWithTheirClassification(t *testing.T) {
	onFail := newFakeHook(t, `{"additional_context":"check the credentials"}`)
	post := newFakeHook(t, `{}`)
	r := NewRegistry()
	mustRegister(t, r, &Hook{Owner: "x", Name: "fail", Phase: PhasePostFailure, CallbackURL: onFail.srv.URL})
	mustRegister(t, r, &Hook{Owner: "x", Name: "all", Phase: PhasePost, CallbackURL: post.srv.URL})
	d := NewDispatcher(r, nil)
	tc := ToolCall{ID: "t1", Name: "HTTP"}

	d.RunPost(context.Background(), Identity{Agent: "a"}, tc, ToolResult{Text: "ok"})
	if len(onFail.bodies) != 0 || len(post.bodies) != 1 {
		t.Fatalf("on success: post_failure called %d times, post %d — want 0 and 1", len(onFail.bodies), len(post.bodies))
	}

	out := d.RunPost(context.Background(), Identity{Agent: "a"}, tc, ToolResult{
		Text: "401", IsError: true, Error: &ToolError{Category: "permission", Retryable: false},
	})
	if len(onFail.bodies) != 1 || !strings.Contains(onFail.bodies[0], `"category":"permission"`) || !strings.Contains(onFail.bodies[0], `"phase":"post_failure"`) {
		t.Errorf("post_failure payload = %v, want the classification and its own phase", onFail.bodies)
	}
	if len(out.AdditionalContext) != 1 || out.AdditionalContext[0] != "check the credentials" {
		t.Errorf("additional context = %v", out.AdditionalContext)
	}
	if out.Result.Error == nil || out.Result.Error.Category != "permission" {
		t.Errorf("result error = %+v, want the tool's classification kept", out.Result.Error)
	}
}

// A post hook that rewrites a failure keeps the tool's classification; one that
// turns it into a success drops it.
func TestDispatcher_PostRewriteKeepsTheClassificationOnlyWhileFailing(t *testing.T) {
	for name, tc := range map[string]struct {
		resp     string
		wantCat  bool
		wantKind string
	}{
		"still failing": {`{"result":{"text":"redacted","is_error":true}}`, true, "rewrite_output"},
		"now a success": {`{"result":{"text":"fine","is_error":false}}`, false, "rewrite_output"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHook(t, tc.resp)
			r := NewRegistry()
			mustRegister(t, r, &Hook{Owner: "x", Name: "rw", Phase: PhasePost, CallbackURL: h.srv.URL})
			out := NewDispatcher(r, nil).RunPost(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "HTTP"},
				ToolResult{Text: "secret", IsError: true, Error: &ToolError{Category: "business"}})
			if (out.Result.Error != nil) != tc.wantCat {
				t.Errorf("error = %+v, want present=%v", out.Result.Error, tc.wantCat)
			}
			if len(out.Decisions) != 1 || out.Decisions[0].Kind != tc.wantKind {
				t.Errorf("decisions = %+v", out.Decisions)
			}
		})
	}
}

func TestRegistry_AcceptsThePostFailurePhase(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Register(&Hook{Owner: "x", Name: "f", Phase: PhasePostFailure, CallbackURL: "http://h/x"}); err != nil {
		t.Errorf("post_failure refused: %v", err)
	}
	if _, err := r.Register(&Hook{Owner: "x", Name: "g", Phase: "after", CallbackURL: "http://h/x"}); err == nil {
		t.Error("an unknown phase was accepted")
	}
}

// An unavailable webhook's reason — streamed to the run's viewer and persisted
// — is a short category. It used to be the raw error: the full callback URL
// (a token in its query string) and up to 1 KiB of the callback's error body.
func TestDispatcher_AnUnavailableWebhooksReasonNeverCarriesItsURLOrBody(t *testing.T) {
	const secret = "tok_s3cr3t"
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/500":
			http.Error(w, "internal detail "+secret, http.StatusInternalServerError)
		case "/json":
			_, _ = w.Write([]byte("not json " + secret))
		case "/slow":
			<-release
		}
	}))
	defer srv.Close()
	defer close(release) // before Close, which waits for the slow handler
	for path, want := range map[string]string{
		"/500":  "the hook returned status 500",
		"/json": "the hook's response was not valid JSON",
		"/slow": "the hook timed out",
	} {
		r := NewRegistry()
		mustRegister(t, r, &Hook{Owner: "x", Name: "h", Phase: PhasePre, CallbackURL: srv.URL + path + "?token=" + secret, TimeoutMs: 50})
		out := NewDispatcher(r, nil).RunPre(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read", Input: json.RawMessage(`{}`)})
		if len(out.Decisions) != 1 || out.Decisions[0].Reason != want {
			t.Errorf("%s: decisions = %+v, want the reason %q", path, out.Decisions, want)
		}
	}
	r := NewRegistry()
	mustRegister(t, r, &Hook{Owner: "x", Name: "h", Phase: PhasePre, CallbackURL: "http://127.0.0.1:1/?token=" + secret})
	out := NewDispatcher(r, nil).RunPre(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read", Input: json.RawMessage(`{}`)})
	if len(out.Decisions) != 1 || out.Decisions[0].Reason != "the hook could not be reached" {
		t.Errorf("unreachable: decisions = %+v", out.Decisions)
	}
}
