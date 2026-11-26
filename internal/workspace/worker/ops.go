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
		// A git op reaches the forge on the connection it came on: its
		// ticket lives on that core replica (RFC GW P11 plan §3.3).
		a = w.run(withOpClient(sctx, client), start, input)
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
	// The id holds no separator, so dir is a direct child of the root, or
	// the root it is bound to on a personal worker (RFC GW §4.5).
	dir := w.wsDir(id)
	switch start.GetOp() {
	case workspace.OpOpen:
		return hidePaths(w.open(id, dir, start.GetArgs()), false, dir, w.opt.Root)
	case workspace.OpGitFetch:
		return w.gitFetch(ctx, id, start.GetArgs())
	case workspace.OpGitOpen:
		return hidePaths(w.gitOpen(ctx, id, dir, start.GetArgs()), false, dir, w.opt.Root)
	case workspace.OpRemove:
		if _, ok := w.boundRoot(id); ok {
			return w.removeBound(ctx, id)
		}
		// Its kernels and servers first: each is a process working in the
		// folder.
		w.kernels.Drop(id)
		w.servers.Drop(id)
		if err := w.engine.Forget(ctx, id); err != nil {
			return refuse(workspace.ClassFailed, "ending the workspace's session: %v", err)
		}
		if err := w.removeOwnDir(id); err != nil {
			return refuse(workspace.ClassFailed, "removing the workspace: %v", err)
		}
		// A git workspace's pins go with it, so its mirror keeps nothing
		// for it (RFC GW §4.2).
		if err := w.dropGit(id); err != nil {
			return refuse(workspace.ClassFailed, "removing the workspace's pins: %v", err)
		}
		// Its checkpoints go with it (RFC GW §5.4).
		if err := w.dropCheckpoints(id); err != nil {
			return refuse(workspace.ClassFailed, "removing the workspace's checkpoints: %v", err)
		}
		w.dropQuota(id)
		return answer{}
	case workspace.OpPark:
		// A durable workspace no run holds (RFC GW P12 plan §3.3): what runs
		// in it ends, as at a removal, and the directory stays. The update
		// address its servers posted to goes with them (servers.Drop).
		if _, _, a := w.workspaceDir(id); a.class != "" {
			return a
		}
		w.kernels.Drop(id)
		w.servers.Drop(id)
		if err := w.engine.Forget(ctx, id); err != nil {
			return refuse(workspace.ClassFailed, "ending the workspace's session: %v", err)
		}
		return answer{}
	case workspace.OpFork:
		if bad := w.onRoot(id, start.GetOp()); bad != nil {
			return *bad
		}
		own := filepath.Join(w.opt.Root, id)
		return hidePaths(w.forkOp(ctx, id, own, start.GetArgs()), false, own, w.opt.Root)
	case workspace.OpTemplateOpen:
		if bad := w.onRoot(id, start.GetOp()); bad != nil {
			return *bad
		}
		own := filepath.Join(w.opt.Root, id)
		return hidePaths(w.templateOpenOp(ctx, id, own, start.GetArgs()), false, own, w.opt.Root)
	case workspace.OpCheckpoint, workspace.OpCheckpoints, workspace.OpDeleteCheckpoint, workspace.OpRestore:
		if bad := w.onRoot(id, start.GetOp()); bad != nil {
			return *bad
		}
		// The id's own folder, never wsDir: a bind landing after onRoot
		// must not have a restore swap out the user's root.
		dir, real, a := w.workspaceDirAt(id, filepath.Join(w.opt.Root, id))
		if a.class == "" {
			a = w.checkpointOps(ctx, id, dir, real, start.GetOp(), start.GetArgs())
		}
		return hidePaths(a, false, real, dir, w.opt.Root)
	case workspace.OpStatus:
		st, err := os.Lstat(dir)
		exists := err == nil && st.IsDir()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return refuse(workspace.ClassFailed, "reading the workspace: %v", err)
		}
		out, _ := json.Marshal(workspace.StatusAnswer{Exists: exists})
		return answer{output: out}
	case workspace.OpExec, workspace.OpRead, workspace.OpWrite, workspace.OpEdit, workspace.OpGlob, workspace.OpGrep:
		return w.inWorkspaceOf(ctx, id, dir, start.GetOp(), start.GetArgs(), input)
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
	case workspace.OpServerStart, workspace.OpServerStop, workspace.OpServerLogs, workspace.OpAwait:
		dir, real, a := w.workspaceDir(id)
		if a.class == "" {
			a = w.serverOp(ctx, id, real, start.GetOp(), start.GetArgs())
		}
		// A server's output is returned as it is; only a refusal's detail
		// is the runtime's text.
		return hidePaths(a, false, real, dir, w.opt.Root)
	case workspace.OpScreenshot:
		dir, real, a := w.workspaceDir(id)
		if a.class == "" {
			a = w.screenshotOp(ctx, id, real, start.GetArgs())
		}
		// A failed load's detail is the browser's log, which may name the
		// session's folders; the picture itself is returned as it is.
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
	if in.Root != "" {
		return w.openRoot(id, in.Root)
	}
	if name, ok := w.boundRoot(id); ok {
		// Its directory is the root's: an open of the id's own must not
		// find it there and set a quota on the user's folder.
		return refuse(workspace.ClassExists, "the workspace is bound to root %q", name)
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

// openRoot binds workspace id to the user's root name (RFC GW §4.5): the
// workspace is that directory. Nothing is made, and no quota is recorded: a
// root has none. The capacity holds as for any open.
func (w *worker) openRoot(id, name string) answer {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.boundRoot(id); !ok {
		if st, err := os.Lstat(filepath.Join(w.opt.Root, id)); err == nil && st.IsDir() {
			return refuse(workspace.ClassExists, "the workspace already has a directory of its own")
		}
		held, err := w.held()
		if err != nil {
			return refuse(workspace.ClassFailed, "listing the workspaces: %v", err)
		}
		if len(held) >= w.opt.Capacity {
			return refuse(workspace.ClassAtCapacity, "this worker holds %d workspaces, its capacity", len(held))
		}
	}
	return w.bindRoot(id, name)
}

// removeBound ends a root workspace: what runs in it stops, and the binding
// goes. The directory is the user's and is never removed.
func (w *worker) removeBound(ctx context.Context, id string) answer {
	w.kernels.Drop(id)
	w.servers.Drop(id)
	if err := w.engine.Forget(ctx, id); err != nil {
		return refuse(workspace.ClassFailed, "ending the workspace's session: %v", err)
	}
	if err := w.unbind(id); err != nil {
		return refuse(workspace.ClassFailed, "forgetting the root's binding: %v", err)
	}
	w.dropQuota(id)
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
