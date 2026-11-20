package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// streamIO is a Stream op's input and output.
type streamIO struct {
	in  io.Reader
	out io.Writer
}

// errStreamBroke is the receive side seeing the op's stream end.
var errStreamBroke = errors.New("the op stream ended")

// Stream runs one op whose bytes are too large to hold (RFC GW P2: get and
// put): in, when not nil, is sent to the worker as the op's input, exactly
// req.InputSize bytes of it, and the worker's output is written to out as
// it arrives. Neither is ever held whole: each moves one chunk at a time,
// and gRPC's flow control holds the sender back when the other side reads
// slowly, so a slow out slows the worker rather than filling memory.
//
// A reader that holds fewer or more than InputSize bytes fails the op with
// size_mismatch, and the stream is ended without InputEnd, so the worker
// keeps nothing of it. A worker that answers before its input is all sent
// (a refusal) stops the sending. req.MaxOutput, when set, bounds the bytes
// written to out (output_too_large past it); zero leaves the bound to out.
// Errors are Do's.
func (d *Dispatcher) Stream(ctx context.Context, workerID string, req Request, in io.Reader, out io.Writer) (res Result, err error) {
	defer func() { d.count(opLabel(req.Op), outcome(err)) }()
	if req.Input != nil {
		return Result{}, &OpError{Class: workspace.ClassInvalid, Detail: "Stream takes its input as in, not Request.Input"}
	}
	if in != nil && req.InputSize < 0 {
		return Result{}, &OpError{Class: workspace.ClassInvalid, Detail: "a negative input size"}
	}
	w, err := d.target(workerID, req)
	if err != nil {
		return Result{}, err
	}
	if out == nil {
		out = io.Discard
	}
	o := &op{ctx: ctx, req: req, max: req.MaxOutput, done: make(chan result, 1), stream: &streamIO{in: in, out: out}}
	return d.runOp(ctx, w, o)
}

// serveStream runs a Stream op on the stream. This goroutine sends (the
// start, then the input); a second one receives and writes the output to
// the caller's writer, under a lock this one takes before it returns, so
// nothing is written to it after Stream has returned.
func (d *Dispatcher) serveStream(ctx context.Context, stream workspacepb.WorkspaceService_OpServer, w *worker, o *op) result {
	req, sio := o.req, o.stream
	if err := stream.Send(&workspacepb.OpToWorker{Msg: &workspacepb.OpToWorker_Start{Start: &workspacepb.OpStart{
		WorkspaceId: req.WorkspaceID, Op: req.Op, Args: req.Args, HasInput: sio.in != nil,
	}}}); err != nil {
		return result{err: ErrWorkerLost}
	}
	var (
		mu     sync.Mutex
		closed bool
	)
	defer func() {
		mu.Lock()
		closed = true
		mu.Unlock()
	}()
	resc := make(chan result, 1)
	safego.Go(ctx, "workspace.dispatch.stream-recv", func() {
		resc <- d.receiveOutput(stream, o, func(data []byte) error {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return errStreamBroke
			}
			_, err := sio.out.Write(data)
			return err
		})
	})
	if sio.in != nil {
		if r, ok := sendSized(stream, o, resc); !ok {
			return d.settle(w, r)
		}
	}
	select {
	case r := <-resc:
		return d.settle(w, r)
	case <-o.ctx.Done():
		return result{err: o.ctx.Err()}
	case <-ctx.Done():
		return d.lostMidOp(w)
	}
}

// settle turns the receive side's view of a broken stream into a lost
// worker; any other result is the op's.
func (d *Dispatcher) settle(w *worker, r result) result {
	if errors.Is(r.err, errStreamBroke) {
		return d.lostMidOp(w)
	}
	return r
}

// receiveOutput reads the worker's frames until OpDone, handing each output
// chunk to write.
func (d *Dispatcher) receiveOutput(stream workspacepb.WorkspaceService_OpServer, o *op, write func([]byte) error) result {
	var n int64
	for {
		f, err := stream.Recv()
		if err != nil {
			return result{err: errStreamBroke}
		}
		switch {
		case f.GetOutput() != nil:
			data := f.GetOutput().GetData()
			if len(data) > workspace.ChunkSize {
				return result{err: &OpError{Class: workspace.ClassProtocol, Detail: "an output chunk over 64 KB"}}
			}
			if o.max > 0 && n+int64(len(data)) > o.max {
				return result{err: &OpError{Class: workspace.ClassOutputTooLarge, Detail: fmt.Sprintf("the worker sent more than %d bytes", o.max)}}
			}
			if err := write(data); err != nil {
				if errors.Is(err, errStreamBroke) {
					return result{err: err}
				}
				return result{err: fmt.Errorf("workspace: writing the op's output: %w", err)}
			}
			n += int64(len(data))
		case f.GetDone() != nil:
			return doneResult(f.GetDone(), nil)
		default:
			return result{err: &OpError{Class: workspace.ClassProtocol, Detail: "an unexpected frame on an op stream"}}
		}
	}
}

// sendSized sends exactly InputSize bytes of the op's input, each chunk in a
// buffer of its own, then InputEnd. It reads one byte past the size to see
// a reader that holds more. ok is false when the op ended here: a mismatch,
// a read error, a cancelled caller, or the worker's answer arriving first
// (a refusal), which is returned.
func sendSized(stream workspacepb.WorkspaceService_OpServer, o *op, resc <-chan result) (result, bool) {
	in, size := o.stream.in, o.req.InputSize
	var sent int64
	for {
		if err := o.ctx.Err(); err != nil {
			return result{err: err}, false
		}
		select {
		case r := <-resc:
			return r, false
		default:
		}
		if sent == size {
			var probe [1]byte
			n, err := io.ReadFull(in, probe[:])
			if n > 0 {
				return mismatch(fmt.Sprintf("the input holds more than its declared %d bytes", size)), false
			}
			if err != io.EOF {
				return result{err: fmt.Errorf("workspace: reading the op's input: %w", err)}, false
			}
			break
		}
		buf := make([]byte, min(int64(workspace.ChunkSize), size-sent))
		n, rerr := io.ReadFull(in, buf)
		if n > 0 {
			if err := stream.Send(&workspacepb.OpToWorker{Msg: &workspacepb.OpToWorker_Input{Input: &workspacepb.Chunk{Data: buf[:n]}}}); err != nil {
				return result{err: errStreamBroke}, false
			}
			sent += int64(n)
		}
		switch {
		case rerr == io.EOF || rerr == io.ErrUnexpectedEOF:
			return mismatch(fmt.Sprintf("the input held %d bytes, not its declared %d", sent, size)), false
		case rerr != nil:
			return result{err: fmt.Errorf("workspace: reading the op's input: %w", rerr)}, false
		}
	}
	if err := stream.Send(&workspacepb.OpToWorker{Msg: &workspacepb.OpToWorker_InputEnd{InputEnd: &workspacepb.InputEnd{}}}); err != nil {
		return result{err: errStreamBroke}, false
	}
	return result{}, true
}

func mismatch(detail string) result {
	return result{err: &OpError{Class: workspace.ClassSizeMismatch, Detail: detail}}
}
