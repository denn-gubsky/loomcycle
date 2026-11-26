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

// This file EXECUTES the dev-workspace bundle's dev/exec code-js body through the
// real loop and code-js provider, against a fake Workspace tool that records each
// call, so the envelope's translation into open → write → exec → read is proved by
// running it. test/runtime/workspace-bundles runs it against a real local worker.

type fakeWorkspace struct {
	calls   []map[string]any
	failCmd string
}

func (w *fakeWorkspace) Name() string                 { return "Workspace" }
func (w *fakeWorkspace) Description() string          { return "workspace test double" }
func (w *fakeWorkspace) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (w *fakeWorkspace) Execute(_ context.Context, raw json.RawMessage) (tools.Result, error) {
	in := map[string]any{}
	_ = json.Unmarshal(raw, &in)
	w.calls = append(w.calls, in)
	switch in["op"] {
	case "open":
		return okResult(map[string]any{"name": in["name"], "state": "ready", "engine": "host"})
	case "write":
		return tools.Result{Text: "wrote"}, nil
	case "exec":
		cmd, _ := in["command"].(string)
		if cmd == w.failCmd {
			// The real tool: a non-zero exit is a failed call carrying the output.
			return tools.Result{IsError: true, Text: `{"exit_code":1,"output":"boom\n"}`}, nil
		}
		return okResult(map[string]any{"exit_code": 0, "output": "ran: " + cmd + "\n"})
	case "read":
		return tools.Result{Text: "file-contents"}, nil
	}
	return tools.Result{IsError: true, Text: "unexpected op"}, nil
}

func (w *fakeWorkspace) ops() []string {
	out := make([]string, 0, len(w.calls))
	for _, c := range w.calls {
		op, _ := c["op"].(string)
		out = append(out, op)
	}
	return out
}

func runDevWorkspace(t *testing.T, w *fakeWorkspace, envelope string) loop.RunResult {
	t.Helper()
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "dev-workspace")...)
	if err != nil {
		t.Fatalf("load base+dev-workspace: %v", err)
	}
	agent, ok := cfg.Agents["dev/exec"]
	if !ok {
		t.Fatalf("dev/exec not registered (agents: %v)", agentNames(cfg))
	}
	set := []tools.Tool{w}
	prov := codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: 30 * time.Second})
	res, err := loop.Run(context.Background(), loop.RunOptions{
		Provider: prov, Model: "code-js", AgentName: "dev/exec", CodeBody: agent.Code,
		Tools: set, Dispatcher: tools.NewDispatcher(set),
		Segments: []loop.PromptSegment{{Role: "user",
			Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: envelope}}}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v\ncalls so far: %v", err, w.calls)
	}
	return res
}

// The envelope runs in one workspace: files written, commands run in order with
// their output in final_text, artifacts read; the result keeps the old shape.
func TestDevWorkspace_RunsTheEnvelopeInAWorkspace(t *testing.T) {
	w := &fakeWorkspace{}
	res := runDevWorkspace(t, w, `{"toolchain":"python","files":[{"path":"m.py","content":"x"}],"commands":["echo hi","python3 m.py"],"read":["out.txt"],"timeout_seconds":30}`)

	if got := strings.Join(w.ops(), ","); got != "open,write,exec,exec,read" {
		t.Fatalf("ops = %s, want open,write,exec,exec,read", got)
	}
	if w.calls[0]["name"] != "exec" || w.calls[0]["toolchain"] != "python" {
		t.Errorf("open = %v, want the default name exec and the toolchain", w.calls[0])
	}
	if w.calls[2]["timeout_s"] != float64(30) {
		t.Errorf("exec = %v, want timeout_s from timeout_seconds", w.calls[2])
	}
	for _, want := range []string{"2 ok, 0 failed", "ran: echo hi", "ran: python3 m.py", "file-contents", "workspace exec"} {
		if !strings.Contains(res.FinalText, want) {
			t.Errorf("final_text lacks %q:\n%s", want, res.FinalText)
		}
	}
}

// A failing command stops the rest unless continue_on_error, and its output is
// reported; old sandbox fields are named as ignored, never dropped silently.
func TestDevWorkspace_AFailureStopsTheRestAndOldFieldsAreReportedIgnored(t *testing.T) {
	w := &fakeWorkspace{failCmd: "make"}
	res := runDevWorkspace(t, w, `{"commands":["make","make test"],"session_id":"s_1","keep_open":true,"network":"none"}`)

	if got := strings.Join(w.ops(), ","); got != "open,exec" {
		t.Errorf("ops = %s, want open,exec (the second command skipped)", got)
	}
	if strings.Contains(res.FinalText, "isError") {
		t.Errorf("final_text carries the tool's error envelope, want the command's output:\n%s", res.FinalText)
	}
	for _, want := range []string{"0 ok, 1 failed, 1 skipped", "[exit: 1]\nboom", "SKIPPED", "ignored (a workspace has no such setting): session_id, keep_open, network"} {
		if !strings.Contains(res.FinalText, want) {
			t.Errorf("final_text lacks %q:\n%s", want, res.FinalText)
		}
	}
}

// The bundle's grant is the narrowest the envelope needs, and its routes are the
// workspace preset's, which the bundle includes so they always resolve.
func TestDevWorkspace_GrantIsNarrowAndItsRoutesResolve(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(bootStackFor(t, "base", "dev-workspace")...)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Agents["dev/exec"].Workspace
	if g == nil || !g.Open || g.Share || g.Expose || g.Templates || g.Connect {
		t.Fatalf("grant = %+v, want open only, no share, expose, templates or connect", g)
	}
	for _, r := range g.Egress {
		if _, ok := cfg.Workspace.Egress.Routes[r]; !ok {
			t.Errorf("grant names route %q, which the loaded config does not declare", r)
		}
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, `"dev/exec"`) {
			t.Errorf("advisory names dev/exec: %s", w)
		}
	}
}
