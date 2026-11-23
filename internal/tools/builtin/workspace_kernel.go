package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// maxCellCode bounds a cell's code, as Write bounds a file body.
const maxCellCode = 1 << 20

// kernelOf is the kernel a run_cell or reset_kernel names; python when none.
func kernelOf(in workspaceInput) (string, *tools.Result) {
	switch in.Kernel {
	case "":
		return "python", nil
	case "python", "node":
		return in.Kernel, nil
	}
	r := workspaceRefusal("invalid_argument", fmt.Sprintf("kernel %q: want python or node", in.Kernel))
	return "", &r
}

// runCell runs code in the workspace's kernel (RFC GW §5.7). The worker
// keeps the kernel between calls; this side only bounds the call.
func (w *Workspace) runCell(ctx context.Context, row store.WorkspaceRow, in workspaceInput) tools.Result {
	kind, bad := kernelOf(in)
	if bad != nil {
		return *bad
	}
	timeout, bad := w.timeout(in)
	if bad != nil {
		return *bad
	}
	if strings.TrimSpace(in.Code) == "" {
		return workspaceRefusal("invalid_argument", "run_cell needs code")
	}
	if len(in.Code) > maxCellCode {
		return workspaceRefusal("invalid_argument", fmt.Sprintf("code is %d bytes, over the %d a cell may hold; write it to a file and run that", len(in.Code), maxCellCode))
	}
	grace := w.KernelGrace
	if grace <= 0 {
		grace = config.DefaultWorkspaceKernelGraceS * time.Second
	}
	args, _ := json.Marshal(workspace.RunCellArgs{Code: in.Code, Kernel: kind, TimeoutMS: timeout.Milliseconds(), GraceMS: grace.Milliseconds()})
	// The code travels as the op's input: it can be larger than an
	// OpStart should carry.
	res, err := w.Manager.Do(ctx, row, dispatch.Request{Op: workspace.OpRunCell, Input: bytes.NewReader(args)})
	if err != nil {
		return workspaceError(err, in.Name)
	}
	var ca workspace.RunCellAnswer
	if json.Unmarshal(res.Output, &ca) != nil {
		return workspaceRefusal(workspace.ClassProtocol, "the worker's answer was not a cell result")
	}
	out := cellResult(ca)
	// A cell that failed is still an answer to read, marked so the model
	// sees it failed, as exec's non-zero exit is.
	out.IsError = ca.Error != "" || ca.TimedOut || ca.KernelKilled || ca.KernelDied
	return out
}

// cellResult encodes a cell's answer without HTML escaping: the output is
// text the model reads, and `<` should read as `<`.
func cellResult(ca workspace.RunCellAnswer) tools.Result {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ca); err != nil {
		return workspaceRefusal("failed", "could not encode the result")
	}
	return tools.Result{Text: strings.TrimSuffix(buf.String(), "\n")}
}

func (w *Workspace) resetKernel(ctx context.Context, row store.WorkspaceRow, in workspaceInput) tools.Result {
	kind, bad := kernelOf(in)
	if bad != nil {
		return *bad
	}
	args, _ := json.Marshal(workspace.ResetKernelArgs{Kernel: kind})
	res, err := w.Manager.Do(ctx, row, dispatch.Request{Op: workspace.OpResetKernel, Args: args})
	if err != nil {
		return workspaceError(err, in.Name)
	}
	var ra workspace.ResetKernelAnswer
	if json.Unmarshal(res.Output, &ra) != nil {
		return workspaceRefusal(workspace.ClassProtocol, "the worker's answer was not a reset result")
	}
	return workspaceOK(ra)
}
