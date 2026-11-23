package builtin

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW P5 through the Workspace tool, a real worker and real
// interpreters: each test skips without the interpreter it runs.

func needProgram(t *testing.T, program string) {
	t.Helper()
	if _, err := exec.LookPath(program); err != nil {
		t.Skipf("%s is not on PATH", program)
	}
}

func cellOf(t *testing.T, res tools.Result) workspace.RunCellAnswer {
	t.Helper()
	var ca workspace.RunCellAnswer
	if err := json.Unmarshal([]byte(res.Text), &ca); err != nil {
		t.Fatalf("not a cell result: %s", res.Text)
	}
	return ca
}

func cellValue(ca workspace.RunCellAnswer) string {
	if ca.Value == nil {
		return "<none>"
	}
	return *ca.Value
}

func TestWorkspaceRunCell_VariablesSurviveBetweenCallsInPythonAndNode(t *testing.T) {
	needProgram(t, "python3")
	needProgram(t, "node")
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"work"}`)
	f.mustOK(t, ctx, `{"op":"run_cell","name":"work","code":"rows = [1, 2, 3]\nprint('<loaded>')"}`)
	ca := cellOf(t, f.mustOK(t, ctx, `{"op":"run_cell","name":"work","code":"sum(rows)"}`))
	if cellValue(ca) != "6" || ca.ExecutionCount != 2 || ca.Kernel != "python" {
		t.Fatalf("python: %+v", ca)
	}
	f.mustOK(t, ctx, `{"op":"run_cell","name":"work","kernel":"node","code":"const total = await Promise.resolve(40)"}`)
	res := f.mustOK(t, ctx, `{"op":"run_cell","name":"work","kernel":"node","code":"total + 2"}`)
	if ca := cellOf(t, res); cellValue(ca) != "42" || ca.Kernel != "node" {
		t.Fatalf("node: %+v", ca)
	}
	// The kernels are apart: Python's rows is not Node's.
	res = f.call(t, ctx, `{"op":"run_cell","name":"work","kernel":"node","code":"rows"}`)
	if ca := cellOf(t, res); !res.IsError || ca.Error == "" {
		t.Fatalf("node read python's variable: %s", res.Text)
	}
}

func TestWorkspaceRunCell_OutputIsNotHTMLEscaped(t *testing.T) {
	needProgram(t, "python3")
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"work"}`)
	res := f.mustOK(t, ctx, `{"op":"run_cell","name":"work","code":"print('<b>&</b>')"}`)
	if want := `"output":"<b>&</b>\n"`; !strings.Contains(res.Text, want) {
		t.Fatalf("result %s, want %s", res.Text, want)
	}
}

func TestWorkspaceRunCell_NeedsTheExecGrantAndStrictArguments(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	reader := tools.WorkspacePolicyValue{Read: true, Write: true, Open: true}
	ctx := f.runCtx(t, "bob", "", reader)
	for _, in := range []string{`{"op":"run_cell","name":"work","code":"1"}`, `{"op":"reset_kernel","name":"work"}`} {
		if c := refusalClass(f.call(t, ctx, in)); c != "not_granted" {
			t.Errorf("%s without exec = %q, want not_granted", in, c)
		}
	}
	for _, in := range []string{
		`{"op":"run_cell","name":"work","code":"1","command":"ls"}`,
		`{"op":"run_cell","name":"work","code":"1","kernel":"ruby"}`,
		`{"op":"run_cell","name":"work","code":"  "}`,
		`{"op":"run_cell","name":"work","code":"1","timeout_s":100000}`,
		`{"op":"reset_kernel","name":"work","code":"1"}`,
	} {
		if c := refusalClass(f.call(t, owner, in)); c != "invalid_argument" {
			t.Errorf("%s = %q, want invalid_argument", in, c)
		}
	}
}

func TestWorkspaceRunCell_AFullWorkspaceRefusesACell(t *testing.T) {
	needProgram(t, "python3")
	f := newWSFixture(t, lifecycle.Limits{DiskQuotaBytes: 1 << 20})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"work"}`)
	// A cell is a command: it may write past the quota, and the workspace
	// is then marked full.
	f.mustOK(t, ctx, `{"op":"run_cell","name":"work","code":"open('big', 'wb').write(b'0' * 2000000)"}`)
	if c := refusalClass(f.call(t, ctx, `{"op":"run_cell","name":"work","code":"1"}`)); c != workspace.ClassOverQuota {
		t.Fatalf("a cell in a full workspace = %q, want %s", c, workspace.ClassOverQuota)
	}
}

func TestWorkspaceRunCell_ATimeoutInterruptsAndKeepsTheVariables(t *testing.T) {
	needProgram(t, "python3")
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"work"}`)
	f.mustOK(t, ctx, `{"op":"run_cell","name":"work","code":"kept = 7"}`)
	res := f.call(t, ctx, `{"op":"run_cell","name":"work","code":"while True: pass","timeout_s":1}`)
	if ca := cellOf(t, res); !res.IsError || !ca.TimedOut || !ca.Interrupted || ca.KernelKilled {
		t.Fatalf("the endless cell: %s", res.Text)
	}
	if ca := cellOf(t, f.mustOK(t, ctx, `{"op":"run_cell","name":"work","code":"kept"}`)); cellValue(ca) != "7" || ca.KernelRestarted {
		t.Fatalf("after the interrupt: %+v", ca)
	}
}

func TestWorkspaceRunCell_AWorkerRestartIsReportedOnTheNextCell(t *testing.T) {
	needProgram(t, "python3")
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"work"}`)
	f.mustOK(t, ctx, `{"op":"run_cell","name":"work","code":"kept = 7"}`)
	f.h.StopWorker()
	f.h.StartWorker()
	ca := cellOf(t, f.mustOK(t, ctx, `{"op":"run_cell","name":"work","code":"'kept' in globals()"}`))
	if !ca.KernelRestarted || cellValue(ca) != "False" {
		t.Fatalf("after the worker restarted: %+v", ca)
	}
	res := f.mustOK(t, ctx, `{"op":"reset_kernel","name":"work"}`)
	if res.Text != `{"kernel":"python","stopped":true}` {
		t.Fatalf("reset: %s", res.Text)
	}
	if ca := cellOf(t, f.mustOK(t, ctx, `{"op":"run_cell","name":"work","code":"1"}`)); ca.KernelRestarted {
		t.Fatalf("a cell after a reset says restarted: %+v", ca)
	}
}
