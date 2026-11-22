package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/kernel"
)

// driversDir is the folder of the worker's root the host engine's kernel
// drivers are written into. Its dot keeps it from reading as a workspace.
const driversDir = ".loomcycle-drivers"

// maxGrace bounds the grace a run_cell op may carry: the core decides it
// from workspace.kernel.interrupt_grace_seconds (at most 300 s).
const maxGrace = 10 * time.Minute

// kernelOp runs a cell in, or resets, one of workspace id's kernels (RFC GW
// §5.7). real is the workspace's directory, where the kernel starts.
func (w *worker) kernelOp(ctx context.Context, id, real, op string, raw []byte) answer {
	if w.kernels == nil {
		return refuse(workspace.ClassKernelUnavailable, "this worker runs no kernels")
	}
	switch op {
	case workspace.OpResetKernel:
		var in workspace.ResetKernelArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		stopped, err := w.kernels.Reset(ctx, id, in.Kernel)
		if err != nil {
			return kernelRefusal(err)
		}
		out, _ := json.Marshal(workspace.ResetKernelAnswer{Kernel: in.Kernel, Stopped: stopped})
		return answer{output: out}
	}
	var in workspace.RunCellArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	timeout, grace := time.Duration(in.TimeoutMS)*time.Millisecond, time.Duration(in.GraceMS)*time.Millisecond
	if timeout <= 0 || timeout > maxExecTimeout {
		return refuse(workspace.ClassInvalid, "timeout_ms must be from 1 to %d", maxExecTimeout.Milliseconds())
	}
	if grace <= 0 || grace > maxGrace {
		return refuse(workspace.ClassInvalid, "grace_ms must be from 1 to %d", maxGrace.Milliseconds())
	}
	// A cell is a command (RFC GW P2 ruling Q2): refused while the
	// workspace is marked full, and the workspace is measured after it.
	return w.commandInQuota(ctx, id, real, func() answer {
		res, err := w.kernels.Run(ctx, id, kernel.Request{Kind: in.Kernel, Code: in.Code, Dir: real, Timeout: timeout, Grace: grace})
		if err != nil {
			return kernelRefusal(err)
		}
		return cellAnswer(in.Kernel, res)
	})
}

func kernelRefusal(err error) answer {
	if errors.Is(err, kernel.ErrUnavailable) {
		return refuse(workspace.ClassKernelUnavailable, "%v", err)
	}
	return refuse(workspace.ClassFailed, "%v", err)
}

// cellAnswer encodes a cell's result. HTML is not escaped: a cell's output
// is text the model reads, and escaping would make `<` six bytes, where the
// result's 1 MiB bound is counted before encoding.
func cellAnswer(kind string, r kernel.Result) answer {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(workspace.RunCellAnswer{
		Kernel: kind, ExecutionCount: r.ExecutionCount, Output: r.Output, Value: r.Value, Error: r.Error, Pictures: r.Pictures,
		Truncated: r.Truncated, Interrupted: r.Interrupted, TimedOut: r.TimedOut,
		KernelKilled: r.KernelKilled, KernelDied: r.KernelDied, KernelRestarted: r.KernelRestarted,
	}); err != nil {
		return refuse(workspace.ClassFailed, "encoding the cell's result: %v", err)
	}
	return answer{output: bytes.TrimSuffix(buf.Bytes(), []byte("\n"))}
}
