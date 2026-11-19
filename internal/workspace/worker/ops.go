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
	var a answer
	if start.GetHasInput() {
		// No op of this layer reads input; it is drained, within a bound,
		// so the core's sends end, and the op is refused if it overflows.
		if err := drainInput(stream); err != nil {
			a = refuse(workspace.ClassInvalid, "%v", err)
		}
	}
	if a.class == "" {
		a = w.run(start)
	}
	if err := sendAnswer(stream, a); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	// Wait for the core to end the stream, so the answer is read before
	// this side's cancel resets it.
	done := make(chan struct{})
	safego.Go(sctx, "workspace.worker.stream-end", func() {
		defer close(done)
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	})
	select {
	case <-done:
	case <-time.After(endWait):
	case <-ctx.Done():
	}
	return nil
}

func drainInput(stream workspacepb.WorkspaceService_OpClient) error {
	var n int
	for {
		f, err := stream.Recv()
		if err != nil {
			return err
		}
		switch {
		case f.GetInput() != nil:
			n += len(f.GetInput().GetData())
			if n > maxInput {
				return fmt.Errorf("the op's input is over %d bytes", maxInput)
			}
		case f.GetInputEnd() != nil:
			return nil
		default:
			return errors.New("an unexpected frame in the op's input")
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

// run runs one op of this layer.
func (w *worker) run(start *workspacepb.OpStart) answer {
	id := start.GetWorkspaceId()
	if !workspace.ValidID(id) {
		return refuse(workspace.ClassInvalid, "malformed workspace id")
	}
	// The id holds no separator, so dir is a direct child of the root.
	dir := filepath.Join(w.opt.Root, id)
	switch start.GetOp() {
	case workspace.OpOpen:
		return w.open(dir)
	case workspace.OpRemove:
		if err := os.RemoveAll(dir); err != nil {
			return refuse(workspace.ClassFailed, "removing the workspace: %v", err)
		}
		return answer{}
	case workspace.OpStatus:
		st, err := os.Lstat(dir)
		exists := err == nil && st.IsDir()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return refuse(workspace.ClassFailed, "reading the workspace: %v", err)
		}
		out, _ := json.Marshal(workspace.StatusAnswer{Exists: exists})
		return answer{output: out}
	}
	return refuse(workspace.ClassUnsupportedOp, "this worker does not run %q", start.GetOp())
}

// open makes the workspace's directory. One already there is success, so a
// retried open is harmless; a new one past capacity is refused, and the core
// places it elsewhere.
func (w *worker) open(dir string) answer {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() {
			return refuse(workspace.ClassFailed, "the workspace's name is taken by something that is not a directory")
		}
		return answer{}
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
	return answer{}
}
