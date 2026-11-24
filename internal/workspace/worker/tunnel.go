package worker

import (
	"context"
	"errors"
	"io"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// serves reports whether a running server of workspace id declared port.
// With alone, no running server of another workspace may declare it too:
// on the host engine every workspace shares one loopback, so a port two
// workspaces declare could be the other's server.
func (s *servers) serves(id string, port int, alone bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	own := false
	for wid, m := range s.by {
		for _, o := range m {
			if o.info.Port != port || o.isEnded() {
				continue
			}
			if wid == id {
				own = true
			} else if alone {
				return false
			}
		}
	}
	return own
}

// tunnelOp joins the op's stream to a port of workspace id (RFC GW P9 plan
// §3.2), both ways at once: input frames are written to the port as they
// come, InputEnd ends what is sent, and what the port answers is sent as
// output until the port closes. Only a port a running server of this
// workspace declared is reached, through the engine (the host's loopback,
// or a relay inside the container session).
//
// done closes when the core has ended the stream; the caller waits on it as
// finish does. A refusal before the port is reached leaves the input to
// watchEnd.
func (w *worker) tunnelOp(sctx context.Context, cancel context.CancelFunc, id, real string, start *workspacepb.OpStart, stream workspacepb.WorkspaceService_OpClient) (answer, <-chan struct{}) {
	refused := func(a answer) (answer, <-chan struct{}) { return a, watchEnd(sctx, cancel, stream) }
	if !start.GetHasInput() {
		return refused(refuse(workspace.ClassInvalid, "a tunnel takes its bytes as the op's input"))
	}
	var in workspace.TunnelArgs
	if a := decodeArgs(start.GetArgs(), &in); a.class != "" {
		return refused(a)
	}
	if in.Port < 1 || in.Port > 65535 {
		return refused(refuse(workspace.ClassInvalid, "port must be from 1 to 65535"))
	}
	if !w.servers.serves(id, in.Port, w.opt.Engine == workspace.EngineHost) {
		return refused(refuse(workspace.ClassPortNotServed, "no running server of this workspace serves port %d alone; start one with that port (logs lists them)", in.Port))
	}
	conn, err := w.engine.Dial(sctx, engine.DialRequest{Workspace: id, Root: real, Port: in.Port})
	if err != nil {
		if errors.Is(err, engine.ErrUnreachable) {
			return refused(refuse(workspace.ClassPortUnreachable, "nothing answers on port %d in the workspace yet: %v", in.Port, err))
		}
		return refused(refuse(workspace.ClassFailed, "reaching port %d: %v", in.Port, err))
	}
	defer conn.Close()
	// Admitted: the core sends the input from here on.
	if err := stream.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Output{Output: &workspacepb.Chunk{}}}); err != nil {
		cancel()
		return refuse(workspace.ClassFailed, "%v", err), closedChan()
	}
	done := make(chan struct{})
	safego.Go(sctx, "workspace.worker.tunnel-in", func() {
		defer close(done)
		// The core ending the stream, or the op ending here, closes the
		// port; Close is safe twice.
		defer conn.Close()
		defer cancel()
		ended := false
		for {
			f, err := stream.Recv()
			if err != nil {
				return
			}
			switch {
			case ended:
				// Nothing follows InputEnd but the stream's end.
			case f.GetInput() != nil:
				if _, err := conn.Write(f.GetInput().GetData()); err != nil {
					// The port stopped reading; its answer still comes.
					ended = true
				}
			case f.GetInputEnd() != nil:
				ended = true
				_ = conn.CloseWrite()
			default:
				return
			}
		}
	})
	for {
		buf := make([]byte, workspace.ChunkSize)
		n, rerr := conn.Read(buf)
		if n > 0 {
			if err := stream.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Output{Output: &workspacepb.Chunk{Data: buf[:n]}}}); err != nil {
				return refuse(workspace.ClassFailed, "%v", err), done
			}
		}
		if rerr == io.EOF {
			return answer{}, done
		}
		if rerr != nil {
			if sctx.Err() != nil {
				return refuse(workspace.ClassFailed, "the tunnel was closed"), done
			}
			return refuse(workspace.ClassFailed, "reading port %d: %v", in.Port, rerr), done
		}
	}
}

func closedChan() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}
