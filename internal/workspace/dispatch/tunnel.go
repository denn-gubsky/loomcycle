package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// Tunnel is one op stream used both ways at once (RFC GW P9 plan §3.2): what
// is written to it is the op's input, sent as it is written, and what is
// read from it is the op's output, as it arrives. It is safe for one reader
// and one writer at once.
type Tunnel struct {
	inW    *io.PipeWriter
	outR   *io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	// err is how the op ended; set before done closes.
	err error
}

// errTunnelClosed is a tunnel's input or output after Close.
var errTunnelClosed = errors.New("workspace: the tunnel is closed")

// errTunnelEnded is the input of a tunnel whose worker has answered: the
// far side closed, and nothing more is sent.
var errTunnelEnded = errors.New("workspace: the tunnel's far side closed")

// Tunnel runs req (an op the worker serves both ways at once, such as
// workspace.OpTunnel) and returns once the worker has admitted it: reached
// what the op names. A refusal before that is returned as Do's would be.
// req.MaxInput bounds the bytes written (size_mismatch past it) and is
// required; req.MaxOutput, when set, bounds the bytes read
// (output_too_large past it). The op ends when the worker ends it (the far
// side closed: reads then end with io.EOF once what was sent is read), when
// ctx ends, or with Close; the caller always calls Close.
func (d *Dispatcher) Tunnel(ctx context.Context, workerID string, req Request) (*Tunnel, error) {
	if req.Input != nil || req.MaxInput <= 0 {
		d.count(opLabel(req.Op), workspace.ClassInvalid)
		return nil, &OpError{Class: workspace.ClassInvalid, Detail: "a tunnel takes its input by Write and needs a maximum"}
	}
	w, err := d.target(workerID, req)
	if err != nil {
		d.count(opLabel(req.Op), outcome(err))
		return nil, err
	}
	tctx, cancel := context.WithCancel(ctx)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	t := &Tunnel{inW: inW, outR: outR, cancel: cancel, done: make(chan struct{})}
	req.InputSize = workspace.PutSizeUnknown
	sio := &streamIO{in: inR, out: outW, admitted: make(chan struct{}), duplex: true,
		stopInput: func() { inR.CloseWithError(errTunnelEnded) }}
	o := &op{ctx: tctx, req: req, max: req.MaxOutput, done: make(chan result, 1), stream: sio}
	// The op's receive side writes to outW under a lock the op's stream
	// waits for as it ends; a caller that stops reading must not hold it,
	// so the end of ctx (Close included) unblocks every pipe.
	stop := context.AfterFunc(tctx, func() {
		outR.CloseWithError(errTunnelClosed)
		inR.CloseWithError(errTunnelClosed)
	})
	safego.Go(ctx, "workspace.dispatch.tunnel", func() {
		defer stop()
		_, err := d.runOp(tctx, w, o)
		d.count(opLabel(req.Op), outcome(err))
		t.err = err
		if err == nil {
			err = io.EOF
		}
		_ = outW.CloseWithError(err)
		inR.CloseWithError(errTunnelEnded)
		close(t.done)
	})
	select {
	case <-sio.admitted:
		return t, nil
	case <-t.done:
		select {
		case <-sio.admitted:
			// Reached, and ended at once: its output is still to read.
			return t, nil
		default:
		}
		cancel()
		if t.err == nil {
			return nil, &OpError{Class: workspace.ClassProtocol, Detail: "the worker ended a tunnel it never admitted"}
		}
		return nil, t.err
	}
}

func (t *Tunnel) Read(b []byte) (int, error)  { return t.outR.Read(b) }
func (t *Tunnel) Write(b []byte) (int, error) { return t.inW.Write(b) }

// CloseWrite ends the input; the output can still be read.
func (t *Tunnel) CloseWrite() error { return t.inW.Close() }

// Close ends the op, if it still runs, and waits for it to end.
func (t *Tunnel) Close() error {
	t.cancel()
	<-t.done
	return nil
}

// Done is closed once the op has ended.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Err is how the op ended, once Close (or a read reaching the end) has
// returned: nil when the worker ended it, the worker's refusal, a lost
// worker, or the context's error.
func (t *Tunnel) Err() error {
	select {
	case <-t.done:
		return t.err
	default:
		return nil
	}
}

// sendDuplex sends a tunnel's input once the worker has admitted it, each
// read as it comes rather than in 64 KB chunks (a tunnel's far side waits
// for its bytes, not for a full chunk), then InputEnd at the reader's end;
// past MaxInput bytes it ends the op with size_mismatch. A read cut short
// because the worker answered returns that answer. ok is as sendSized's.
func sendDuplex(stream workspacepb.WorkspaceService_OpServer, o *op, resc <-chan result) (result, bool) {
	select {
	case <-o.stream.admitted:
	case r := <-resc:
		return r, false
	case <-o.ctx.Done():
		return result{err: o.ctx.Err()}, false
	}
	in, maxIn := o.stream.in, o.req.MaxInput
	var sent int64
	for {
		buf := make([]byte, workspace.ChunkSize)
		n, rerr := in.Read(buf)
		if sent+int64(n) > maxIn {
			return mismatch(fmt.Sprintf("the input holds more than its maximum %d bytes", maxIn)), false
		}
		if n > 0 {
			if err := stream.Send(&workspacepb.OpToWorker{Msg: &workspacepb.OpToWorker_Input{Input: &workspacepb.Chunk{Data: buf[:n]}}}); err != nil {
				return result{err: errStreamBroke}, false
			}
			sent += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			select {
			case r := <-resc:
				return r, false
			default:
			}
			if err := o.ctx.Err(); err != nil {
				return result{err: err}, false
			}
			return result{err: fmt.Errorf("workspace: reading the tunnel's input: %w", rerr)}, false
		}
	}
	if err := stream.Send(&workspacepb.OpToWorker{Msg: &workspacepb.OpToWorker_InputEnd{InputEnd: &workspacepb.InputEnd{}}}); err != nil {
		return result{err: errStreamBroke}, false
	}
	return result{}, true
}
