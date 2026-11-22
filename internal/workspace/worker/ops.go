package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// answer is how an op ended: output to send, then OpDone.
type answer struct {
	output []byte
	exit   int32
	class  string
	detail string
}

func refuse(class, format string, args ...any) answer {
	return answer{class: class, detail: fmt.Sprintf(format, args...)}
}

// serveOne opens one Op stream, waits for the core to hand it an op, runs
// it, and ends the stream.
func (w *worker) serveOne(ctx context.Context, client workspacepb.WorkspaceServiceClient) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.Op(sctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Ready{Ready: &workspacepb.Ready{WorkerId: w.opt.ID}}}); err != nil {
		return err
	}
	f, err := stream.Recv()
	if err != nil {
		return err
	}
	start := f.GetStart()
	if start == nil {
		return errors.New("the first op frame was not OpStart")
	}
	if w.opt.Observe != nil {
		w.opt.Observe(start.GetOp(), start.GetArgs())
	}
	if streamOp(start.GetOp()) {
		return w.serveStream(ctx, sctx, cancel, stream, start)
	}
	var (
		a     answer
		input []byte
	)
	if start.GetHasInput() {
		// The input is read to its end whatever its size, so the core's
		// sends finish; an op whose input is over the bound is refused.
		input, err = readInput(stream)
		if err != nil {
			a = refuse(workspace.ClassInvalid, "%v", err)
		}
	}
	done := watchEnd(sctx, cancel, stream)
	if a.class == "" {
		a = w.run(sctx, start, input)
	}
	return finish(ctx, stream, a, done)
}

// watchEnd receives until the core ends the stream, discarding what comes.
// The core sends nothing more once the op's input has ended, until it ends
// the stream, which it does when it has the answer or gives up on the op
// (its caller cancelled, or the run ended). A receive that ends cancels the
// op, so a command the core no longer waits for is killed rather than left
// running. done closes when it returns.
func watchEnd(sctx context.Context, cancel context.CancelFunc, stream workspacepb.WorkspaceService_OpClient) <-chan struct{} {
	done := make(chan struct{})
	safego.Go(sctx, "workspace.worker.stream-end", func() {
		defer close(done)
		defer cancel()
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	})
	return done
}

// finish sends the answer and waits for the core to end the stream, so the
// answer is read before this side's cancel resets it.
func finish(ctx context.Context, stream workspacepb.WorkspaceService_OpClient, a answer, done <-chan struct{}) error {
	if err := sendAnswer(stream, a); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	select {
	case <-done:
	case <-time.After(endWait):
	case <-ctx.Done():
	}
	return nil
}

// readInput reads the op's input up to InputEnd. Past maxInput it keeps
// reading and discards, then reports the overflow.
func readInput(stream workspacepb.WorkspaceService_OpClient) ([]byte, error) {
	var (
		buf  []byte
		over bool
	)
	for {
		f, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		switch {
		case f.GetInput() != nil:
			data := f.GetInput().GetData()
			if over || len(buf)+len(data) > maxInput {
				over, buf = true, nil
				continue
			}
			buf = append(buf, data...)
		case f.GetInputEnd() != nil:
			if over {
				return nil, fmt.Errorf("the op's input is over %d bytes", maxInput)
			}
			return buf, nil
		default:
			return nil, errors.New("an unexpected frame in the op's input")
		}
	}
}

// sendAnswer sends the output in chunks of at most ChunkSize, each a copy of
// its own (a buffer is never shared between two sends), then OpDone.
func sendAnswer(stream workspacepb.WorkspaceService_OpClient, a answer) error {
	for off := 0; off < len(a.output); off += workspace.ChunkSize {
		end := min(off+workspace.ChunkSize, len(a.output))
		chunk := append([]byte(nil), a.output[off:end]...)
		if err := stream.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Output{Output: &workspacepb.Chunk{Data: chunk}}}); err != nil {
			return err
		}
	}
	return stream.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Done{Done: &workspacepb.OpDone{
		ExitCode: a.exit, ErrorClass: a.class, ErrorDetail: a.detail,
	}}})
}

// run runs one op. input is the op's input, nil when it has none.
func (w *worker) run(ctx context.Context, start *workspacepb.OpStart, input []byte) answer {
	id := start.GetWorkspaceId()
	if !workspace.ValidID(id) {
		return refuse(workspace.ClassInvalid, "malformed workspace id")
	}
	// The id holds no separator, so dir is a direct child of the root.
	dir := filepath.Join(w.opt.Root, id)
	switch start.GetOp() {
	case workspace.OpOpen:
		return w.open(id, dir, start.GetArgs())
	case workspace.OpRemove:
		// Its kernels first: a kernel is a process working in the folder.
		w.kernels.Drop(id)
		if err := os.RemoveAll(dir); err != nil {
			return refuse(workspace.ClassFailed, "removing the workspace: %v", err)
		}
		w.dropQuota(id)
		return answer{}
	case workspace.OpStatus:
		st, err := os.Lstat(dir)
		exists := err == nil && st.IsDir()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return refuse(workspace.ClassFailed, "reading the workspace: %v", err)
		}
		out, _ := json.Marshal(workspace.StatusAnswer{Exists: exists})
		return answer{output: out}
	case workspace.OpExec, workspace.OpRead, workspace.OpWrite, workspace.OpEdit, workspace.OpGlob, workspace.OpGrep:
		return w.inWorkspace(ctx, dir, start.GetOp(), start.GetArgs(), input)
	case workspace.OpRunCell, workspace.OpResetKernel:
		dir, real, a := w.workspaceDir(id)
		if a.class == "" {
			raw := start.GetArgs()
			if len(raw) == 0 {
				raw = input
			}
			a = w.kernelOp(ctx, id, real, start.GetOp(), raw)
		}
		// A kernel that failed to start reports its stderr, which can name
		// the drivers' folder in the worker's root.
		return hidePaths(a, false, real, dir, w.opt.Root)
	case workspace.OpMDSplice:
		dir, real, a := w.workspaceDir(id)
		if a.class == "" {
			a = w.mdSpliceOp(ctx, id, real, start.GetArgs(), input)
		}
		return hidePaths(a, false, real, dir)
	case workspace.OpFStat, workspace.OpFList, workspace.OpMkdir, workspace.OpFRemove,
		workspace.OpStage, workspace.OpCommit, workspace.OpDiscard, workspace.OpCopyLocal:
		dir, real, a := w.workspaceDir(id)
		if a.class == "" {
			a = w.fileOp(ctx, id, real, start.GetOp(), start.GetArgs())
		}
		return hidePaths(a, false, real, dir)
	}
	return refuse(workspace.ClassUnsupportedOp, "this worker does not run %q", start.GetOp())
}

// open makes the workspace's directory and records the disk quota its
// arguments name (none: the default). One already there is success, so a
// retried open is harmless; a new one past capacity is refused, and the core
// places it elsewhere.
func (w *worker) open(id, dir string, raw []byte) answer {
	var in workspace.OpenArgs
	if len(raw) > 0 {
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
	}
	if in.DiskQuotaBytes < 0 || in.DiskQuotaBytes > workspace.MaxDiskQuotaBytes {
		return refuse(workspace.ClassInvalid, "disk_quota_bytes must be from 1 to %d", int64(workspace.MaxDiskQuotaBytes))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() {
			return refuse(workspace.ClassFailed, "the workspace's name is taken by something that is not a directory")
		}
		return w.recordQuota(id, in.DiskQuotaBytes)
	}
	held, err := w.held()
	if err != nil {
		return refuse(workspace.ClassFailed, "listing the workspaces: %v", err)
	}
	if len(held) >= w.opt.Capacity {
		return refuse(workspace.ClassAtCapacity, "this worker holds %d workspaces, its capacity", len(held))
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return refuse(workspace.ClassFailed, "making the workspace: %v", err)
	}
	if a := w.recordQuota(id, in.DiskQuotaBytes); a.class != "" {
		_ = os.Remove(dir)
		return a
	}
	return answer{}
}

// recordQuota keeps an open's quota; zero leaves what is recorded (or the
// default) as it is, which is what a core that sends no quota expects.
func (w *worker) recordQuota(id string, quota int64) answer {
	if quota == 0 {
		return answer{}
	}
	if err := w.setLimit(id, quota); err != nil {
		return refuse(workspace.ClassFailed, "recording the workspace's quota: %v", err)
	}
	return answer{}
}
