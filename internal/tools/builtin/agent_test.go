package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Happy path: input parses, runner is called with the right args, the
// returned text is wrapped in a successful Result.
func TestAgentTool_HappyPath(t *testing.T) {
	var gotName, gotPrompt string
	a := &AgentTool{Run: func(_ context.Context, name, prompt, _ string) (string, error) {
		gotName, gotPrompt = name, prompt
		return "sub-agent output", nil
	}}

	res, err := a.Execute(context.Background(),
		json.RawMessage(`{"name":"cv-adapter","prompt":"Generate CV"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Errorf("expected success, got IsError=true: %s", res.Text)
	}
	if res.Text != "sub-agent output" {
		t.Errorf("Text = %q", res.Text)
	}
	if gotName != "cv-adapter" || gotPrompt != "Generate CV" {
		t.Errorf("runner got (%q, %q)", gotName, gotPrompt)
	}
}

// Missing required fields surface as IsError tool_results so the model
// can self-correct, NOT as Go errors that tear down the run.
func TestAgentTool_MissingName(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		t.Fatal("runner should not be called when name is missing")
		return "", nil
	}}
	res, err := a.Execute(context.Background(), json.RawMessage(`{"prompt":"X"}`))
	if err != nil {
		t.Fatalf("Execute returned hard error: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError=true on missing name")
	}
	if !strings.Contains(res.Text, "name") {
		t.Errorf("error should name the missing field: %q", res.Text)
	}
}

func TestAgentTool_MissingPrompt(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		t.Fatal("runner should not be called when prompt is missing")
		return "", nil
	}}
	res, _ := a.Execute(context.Background(), json.RawMessage(`{"name":"foo"}`))
	if !res.IsError || !strings.Contains(res.Text, "prompt") {
		t.Errorf("expected missing-prompt IsError, got %+v", res)
	}
}

// Whitespace-only name is treated as missing — guards against the model
// emitting `"name": "  "` and getting a confusing "unknown agent" error
// from the runner instead of a clean "missing field" response.
func TestAgentTool_WhitespaceNameRejected(t *testing.T) {
	called := false
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		called = true
		return "", nil
	}}
	res, _ := a.Execute(context.Background(), json.RawMessage(`{"name":"   ","prompt":"X"}`))
	if called {
		t.Error("runner should not be called with whitespace-only name")
	}
	if !res.IsError {
		t.Error("expected IsError on whitespace name")
	}
}

// Malformed JSON input is also a model-correctable error, not a hard
// crash. The model gets feedback "invalid input JSON" and can retry.
func TestAgentTool_MalformedJSON(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		t.Fatal("runner should not be called on malformed JSON")
		return "", nil
	}}
	res, err := a.Execute(context.Background(), json.RawMessage(`{not json`))
	if err != nil {
		t.Fatalf("Execute returned hard error: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError=true on malformed JSON")
	}
}

// Runner errors propagate to the model as IsError tool_results — the
// parent run continues so the model can decide how to recover (try a
// different sub-agent, fall back, give up gracefully).
func TestAgentTool_RunnerError(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		return "", errors.New("provider returned 500")
	}}
	res, err := a.Execute(context.Background(),
		json.RawMessage(`{"name":"x","prompt":"y"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError=true when runner fails")
	}
	if !strings.Contains(res.Text, "provider returned 500") {
		t.Errorf("error message lost: %q", res.Text)
	}
}

// A sub-agent that ends_turn with no text is rare but possible (made
// only tool calls, then stopped). Surface a hint so the parent's
// model has something concrete to read instead of empty Text.
func TestAgentTool_EmptyOutputHint(t *testing.T) {
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		return "", nil
	}}
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"name":"silent-agent","prompt":"x"}`))
	if res.IsError {
		t.Errorf("empty output is not an error: %+v", res)
	}
	if !strings.Contains(res.Text, "silent-agent") || !strings.Contains(res.Text, "no final text") {
		t.Errorf("hint should name the agent and explain: %q", res.Text)
	}
}

// Recursion guard: a context already at MaxAgentDepth refuses to spawn.
// EMPIRICAL: with the depth check disabled, this test fails because
// the runner gets called instead of the IsError path.
func TestAgentTool_MaxDepthGuard(t *testing.T) {
	called := false
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		called = true
		return "should not run", nil
	}}
	ctx := context.Background()
	for i := 0; i < MaxAgentDepth; i++ {
		ctx = IncrementAgentDepth(ctx)
	}
	res, _ := a.Execute(ctx, json.RawMessage(`{"name":"x","prompt":"y"}`))
	if called {
		t.Error("runner should not have been invoked at max depth")
	}
	if !res.IsError || !strings.Contains(res.Text, "max sub-agent recursion depth") {
		t.Errorf("expected max-depth IsError, got %+v", res)
	}
}

// One depth below the cap is still allowed — the guard fires at >=,
// not >. Worth pinning so a future refactor doesn't shift the
// boundary by one.
func TestAgentTool_DepthBelowCapAllowed(t *testing.T) {
	called := false
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		called = true
		return "ok", nil
	}}
	ctx := context.Background()
	for i := 0; i < MaxAgentDepth-1; i++ {
		ctx = IncrementAgentDepth(ctx)
	}
	res, _ := a.Execute(ctx, json.RawMessage(`{"name":"x","prompt":"y"}`))
	if !called {
		t.Error("runner should run at depth = MaxAgentDepth-1")
	}
	if res.IsError {
		t.Errorf("unexpected error: %s", res.Text)
	}
}

// AgentDepth on a fresh context returns 0 — the top-level run is depth 0.
func TestAgentDepth_DefaultsToZero(t *testing.T) {
	if d := AgentDepth(context.Background()); d != 0 {
		t.Errorf("AgentDepth(empty ctx) = %d, want 0", d)
	}
}

// IncrementAgentDepth chains: each call increments by exactly one.
func TestIncrementAgentDepth_Chains(t *testing.T) {
	ctx := context.Background()
	for want := 1; want <= 5; want++ {
		ctx = IncrementAgentDepth(ctx)
		if got := AgentDepth(ctx); got != want {
			t.Errorf("after %d increments: depth = %d, want %d", want, got, want)
		}
	}
}

// Defensive: tool with nil Run surfaces a clear error so a misconfigured
// server doesn't silently accept Agent calls.
func TestAgentTool_NilRunner(t *testing.T) {
	a := &AgentTool{} // Run is nil
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"name":"x","prompt":"y"}`))
	if !res.IsError || !strings.Contains(res.Text, "not wired") {
		t.Errorf("expected nil-runner IsError, got %+v", res)
	}
}

// v0.8.5 PR 5: optional def_id pins this sub-run to a specific
// agent_defs row. Test only that the tool propagates the field
// through to the runner — actual lookup + overlay is wired in the
// HTTP server's runSubAgent and tested at that layer.
func TestAgentTool_DefIDPassthrough(t *testing.T) {
	var gotDef string
	a := &AgentTool{Run: func(_ context.Context, _, _, defID string) (string, error) {
		gotDef = defID
		return "ok", nil
	}}
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"name":"x","prompt":"y","def_id":"def_abc"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if gotDef != "def_abc" {
		t.Errorf("def_id passthrough lost: %q", gotDef)
	}
}

// Backwards-compat: omitting def_id leaves it empty (zero-value).
func TestAgentTool_DefIDOmittedDefaultsEmpty(t *testing.T) {
	var gotDef string
	a := &AgentTool{Run: func(_ context.Context, _, _, defID string) (string, error) {
		gotDef = defID
		return "ok", nil
	}}
	res, _ := a.Execute(context.Background(),
		json.RawMessage(`{"name":"x","prompt":"y"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if gotDef != "" {
		t.Errorf("def_id should default empty, got %q", gotDef)
	}
}
