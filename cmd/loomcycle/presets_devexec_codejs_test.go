package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// This file EXECUTES dev/exec's shipped code-js body through the REAL agent loop +
// the real code-js provider, against a fake mcp__sandbox__* toolset that records
// every dispatch. Grepping the source would only prove the text is present; running
// it proves the command envelope is translated into the right open→(write)→exec→
// (read)→close sequence, that a failing command stops the chain, that a reused
// session is never closed, and that the JS actually COMPILES + RUNS. Hermetic:
// the code-js provider is in-process and the tools are local fakes.

type fakeSandbox struct {
	calls   []recordedCall
	openN   int
	openNet []string        // network arg of each sandbox_open
	execErr map[string]bool // commands whose exec should return IsError
}

func newFakeSandbox() *fakeSandbox { return &fakeSandbox{execErr: map[string]bool{}} }

func (s *fakeSandbox) execs() []string {
	var out []string
	for _, c := range s.calls {
		if c.Tool == "mcp__sandbox__sandbox_exec" {
			cmd, _ := c.Input["command"].(string)
			out = append(out, cmd)
		}
	}
	return out
}

func (s *fakeSandbox) hasTool(name string) bool {
	for _, c := range s.calls {
		if c.Tool == name {
			return true
		}
	}
	return false
}

// sbxTool is one fake mcp__sandbox__sandbox_* tool, bound by its exact canonical
// name so code-js reaches it as mcp__sandbox__sandbox_open({...}) etc.
type sbxTool struct {
	s    *fakeSandbox
	name string
}

func (t *sbxTool) Name() string                 { return t.name }
func (t *sbxTool) Description() string          { return "sandbox test double" }
func (t *sbxTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *sbxTool) Execute(_ context.Context, raw json.RawMessage) (tools.Result, error) {
	in := map[string]any{}
	_ = json.Unmarshal(raw, &in)
	t.s.calls = append(t.s.calls, recordedCall{Tool: t.name, Input: in})
	switch t.name {
	case "mcp__sandbox__sandbox_open":
		t.s.openN++
		net, _ := in["network"].(string)
		t.s.openNet = append(t.s.openNet, net)
		return okResult(map[string]any{"session_id": "s_test", "workspace_path": "/work", "network": net})
	case "mcp__sandbox__sandbox_exec":
		cmd, _ := in["command"].(string)
		if t.s.execErr[cmd] {
			// Mirror the real tool: a non-zero exit is IsError with the output.
			return tools.Result{IsError: true, Text: "boom\n[exit: 1]"}, nil
		}
		return tools.Result{Text: "ok: " + cmd}, nil // raw command output (not JSON)
	case "mcp__sandbox__sandbox_write":
		return tools.Result{Text: "wrote"}, nil
	case "mcp__sandbox__sandbox_read":
		return tools.Result{Text: "file-contents"}, nil
	case "mcp__sandbox__sandbox_close":
		return tools.Result{Text: "closed " + firstString(in["session_id"])}, nil
	}
	return tools.Result{IsError: true, Text: "unexpected tool " + t.name}, nil
}

func firstString(v any) string { s, _ := v.(string); return s }

func sandboxToolset(s *fakeSandbox) []tools.Tool {
	names := []string{
		"mcp__sandbox__sandbox_open", "mcp__sandbox__sandbox_exec",
		"mcp__sandbox__sandbox_write", "mcp__sandbox__sandbox_read",
		"mcp__sandbox__sandbox_close",
	}
	set := make([]tools.Tool, 0, len(names))
	for _, n := range names {
		set = append(set, &sbxTool{s: s, name: n})
	}
	return set
}

// runDevExec drives the SHIPPED dev/exec code-js body through the real loop against
// the fake sandbox toolset with the given JSON envelope as the prompt.
func runDevExec(t *testing.T, s *fakeSandbox, envelope string) loop.RunResult {
	t.Helper()
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "sandbox", "dev-exec")...)
	if err != nil {
		t.Fatalf("load base+sandbox+dev-exec: %v", err)
	}
	agent, ok := cfg.Agents["dev/exec"]
	if !ok {
		t.Fatalf("dev/exec not registered (agents: %v)", agentNames(cfg))
	}
	set := sandboxToolset(s)
	prov := codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: 30 * time.Second})
	res, err := loop.Run(context.Background(), loop.RunOptions{
		Provider:   prov,
		Model:      "code-js",
		AgentName:  "dev/exec",
		CodeBody:   agent.Code,
		Tools:      set,
		Dispatcher: tools.NewDispatcher(set),
		Segments: []loop.PromptSegment{{
			Role:    "user",
			Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: envelope}},
		}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v\ncalls so far: %v", err, s.calls)
	}
	return res
}

// TestDevExec_RunsEnvelopeOpenWriteExecReadClose is the happy path: one session,
// files written, commands run in order (network defaults to egress), artifacts
// read, session closed.
func TestDevExec_RunsEnvelopeOpenWriteExecReadClose(t *testing.T) {
	s := newFakeSandbox()
	env := `{"files":[{"path":"main.go","content":"package main"}],` +
		`"commands":["echo hi","go build ./..."],"read":["bin/app"]}`

	res := runDevExec(t, s, env)

	if s.openN != 1 {
		t.Fatalf("want exactly 1 sandbox_open, got %d", s.openN)
	}
	if len(s.openNet) == 0 || s.openNet[0] != "egress" {
		t.Errorf("open network = %v, want egress (the dev default)", s.openNet)
	}
	if !s.hasTool("mcp__sandbox__sandbox_write") {
		t.Errorf("files were not written; calls %v", s.execs())
	}
	if !s.hasTool("mcp__sandbox__sandbox_read") {
		t.Errorf("read artifacts were not fetched")
	}
	if !s.hasTool("mcp__sandbox__sandbox_close") {
		t.Errorf("a freshly-opened session must be closed when keep_open is unset")
	}
	// The two user commands ran, in order (a best-effort `gh auth setup-git` exec
	// may precede them — assert order among the user commands, not exact count).
	execs := s.execs()
	if !containsInOrder(execs, "echo hi", "go build ./...") {
		t.Errorf("user commands not run in order; execs=%v", execs)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop reason = %q, want end_turn", res.StopReason)
	}
	// The structured result reports both commands succeeded.
	if !strings.Contains(res.FinalText, "2 ok, 0 failed") {
		t.Errorf("final_text = %q, want it to report 2 ok / 0 failed", res.FinalText)
	}
}

// TestDevExec_StopsAtFirstFailingCommand: a non-zero exit halts the chain (the
// later command never runs), the session still closes, and ok is false.
func TestDevExec_StopsAtFirstFailingCommand(t *testing.T) {
	s := newFakeSandbox()
	s.execErr["boom"] = true
	env := `{"commands":["ok-1","boom","never"]}`

	res := runDevExec(t, s, env)

	execs := s.execs()
	if containsStr(execs, "never") {
		t.Errorf("a command ran after a failure — the chain must stop at the first failure; execs=%v", execs)
	}
	if !containsStr(execs, "boom") {
		t.Errorf("the failing command was never attempted; execs=%v", execs)
	}
	if !s.hasTool("mcp__sandbox__sandbox_close") {
		t.Errorf("the session must still close after a failing command")
	}
	if !strings.Contains(res.FinalText, "1 failed") || !strings.Contains(res.FinalText, "first failure: boom") {
		t.Errorf("final_text = %q, want it to report the failure + which command", res.FinalText)
	}
}

// TestDevExec_ReusedSessionIsNotOpenedOrClosed: passing session_id means the caller
// owns the container's lifecycle — dev/exec neither opens a new one nor closes it.
func TestDevExec_ReusedSessionIsNotOpenedOrClosed(t *testing.T) {
	s := newFakeSandbox()
	env := `{"session_id":"s_external","commands":["echo hi"]}`

	runDevExec(t, s, env)

	if s.openN != 0 {
		t.Errorf("a reused session must NOT open a new one (opened %d)", s.openN)
	}
	if s.hasTool("mcp__sandbox__sandbox_close") {
		t.Errorf("a reused (caller-owned) session must NOT be closed unless close=true")
	}
	// The command ran against the passed-in session id.
	for _, c := range s.calls {
		if c.Tool == "mcp__sandbox__sandbox_exec" {
			if c.Input["session_id"] != "s_external" {
				t.Errorf("exec ran against %v, want the reused s_external", c.Input["session_id"])
			}
		}
	}
}

// TestDevExec_EmptyEnvelopeDoesNothing: no commands/files/read/browser → a usage
// message and NO container is opened (nothing to run).
func TestDevExec_EmptyEnvelopeDoesNothing(t *testing.T) {
	s := newFakeSandbox()

	res := runDevExec(t, s, `{}`)

	if s.openN != 0 || len(s.calls) != 0 {
		t.Errorf("an empty envelope must not touch the sandbox; calls=%v", s.calls)
	}
	if !strings.Contains(res.FinalText, "nothing to do") {
		t.Errorf("final_text = %q, want a usage/nothing-to-do message", res.FinalText)
	}
}

func containsStr(hay []string, s string) bool {
	for _, h := range hay {
		if h == s {
			return true
		}
	}
	return false
}

// containsInOrder reports whether want appears as an ordered (not necessarily
// contiguous) subsequence of got.
func containsInOrder(got []string, want ...string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
