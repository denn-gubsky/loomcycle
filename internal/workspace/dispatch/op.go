package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// OpError is an op that did not succeed: a class (workspace.Class*) and a
// reason. A worker's own answer carries one of workspace.KnownWorkerClasses;
// the core adds worker_lost, no_worker, busy, output_too_large and protocol.
type OpError struct {
	Class  string
	Detail string
}

func (e *OpError) Error() string {
	if e.Detail == "" {
		return "workspace: " + e.Class
	}
	return "workspace: " + e.Class + ": " + e.Detail
}

// Is matches any OpError of the same class, so errors.Is(err, ErrWorkerLost)
// holds for every worker_lost.
func (e *OpError) Is(target error) bool {
	t, ok := target.(*OpError)
	return ok && t.Class == e.Class
}

// ErrWorkerLost is an op whose worker went away before it finished, or that
// named a worker not attached to this replica. Whether the op took effect is
// unknown.
var ErrWorkerLost = &OpError{Class: workspace.ClassWorkerLost, Detail: "the worker went away before the op finished"}

// ErrWorkerRevoked is an op whose worker was detached because its token no
// longer attaches it (revoked, rotated, re-bound to other tenants). Whether
// the op took effect is unknown.
var ErrWorkerRevoked = &OpError{Class: workspace.ClassWorkerRevoked, Detail: "the worker's token no longer attaches it"}

// ClassOf is err's class: an OpError's own, "" for nil, and failed for any
// other error.
func ClassOf(err error) string {
	if err == nil {
		return ""
	}
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Class
	}
	return workspace.ClassFailed
}

// Request is one op for a worker.
type Request struct {
	// TenantID is the tenant the op is for. Do refuses a worker that does
	// not serve it, whatever the caller read from a row.
	TenantID string
	// UserID is the user of the workspace's row. A personal worker (RFC GW
	// P13) takes an op only for its own user; a request without one is
	// refused there (D-P13-12).
	UserID      string
	WorkspaceID string
	Op          string
	// Args is the op's JSON object; nil for none.
	Args []byte
	// Input, when set, is streamed to the worker in chunks after OpStart.
	Input io.Reader
	// MaxOutput bounds the output kept; zero is Options.MaxOutput. A worker
	// that sends more fails the op with output_too_large. For Stream, zero
	// is no bound but the caller's writer.
	MaxOutput int64
	// InputSize is, for Stream, the exact number of bytes its reader
	// carries. Stream sends no more, and ends the op without InputEnd when
	// the reader holds fewer or more, so the worker keeps nothing of it.
	// workspace.PutSizeUnknown is a reader of unknown length, bounded by
	// MaxInput.
	InputSize int64
	// MaxInput bounds a Stream input of unknown length, and is required
	// with it: Stream reads nothing of the reader until the worker has
	// admitted the op (an empty output chunk; a refusal ends it first),
	// then sends to the reader's end, and fails the op with size_mismatch,
	// ending it without InputEnd, past MaxInput bytes.
	MaxInput int64
}

// Result is a finished op's answer.
type Result struct {
	ExitCode int32
	Output   []byte
}

// op is one Request handed to a stream.
type op struct {
	ctx  context.Context
	req  Request
	max  int64
	done chan result // buffered 1
	// stream is set for an op Stream runs: its input and output are the
	// caller's reader and writer, never held whole here.
	stream *streamIO
}

type result struct {
	res Result
	err error
	// retry: nothing of the op reached the worker; it may run on another
	// stream at no cost.
	retry bool
}

// Do runs one op on the worker workerID and returns its answer. It waits up
// to StreamWait for an idle stream. A worker not attached to this replica,
// or one that goes away mid-op, fails it with ErrWorkerLost. A worker's own
// refusal comes back as an *OpError with the worker's output, if any, in the
// Result.
func (d *Dispatcher) Do(ctx context.Context, workerID string, req Request) (res Result, err error) {
	defer func() { d.count(opLabel(req.Op), outcome(err)) }()
	w, err := d.target(workerID, req)
	if err != nil {
		return Result{}, err
	}
	o := &op{ctx: ctx, req: req, max: req.MaxOutput, done: make(chan result, 1)}
	if o.max <= 0 {
		o.max = d.opt.MaxOutput
	}
	return d.runOp(ctx, w, o)
}

// target is the attached worker an op for req may run on.
func (d *Dispatcher) target(workerID string, req Request) (*worker, error) {
	if !workspace.ValidID(req.WorkspaceID) {
		return nil, &OpError{Class: workspace.ClassInvalid, Detail: "malformed workspace id"}
	}
	d.mu.Lock()
	w := d.workers[workerID]
	d.mu.Unlock()
	if w == nil {
		return nil, ErrWorkerLost
	}
	if !w.servesUser(req.TenantID, req.UserID) {
		// Placement never puts a workspace here; a row naming this worker
		// for another tenant, or another user on a personal worker, is a bug
		// or a forgery, and nothing is sent.
		return nil, &OpError{Class: workspace.ClassNoWorker, Detail: "the worker does not serve this tenant or user"}
	}
	return w, nil
}

// runOp hands o to one of w's streams and waits for its answer, running it
// again on another stream when nothing of it reached the worker.
func (d *Dispatcher) runOp(ctx context.Context, w *worker, o *op) (Result, error) {
	for range maxFreeRetries + 1 {
		s, err := d.acquire(ctx, w)
		if err != nil {
			return Result{}, err
		}
		d.mu.Lock()
		w.inFlight++
		d.mu.Unlock()
		s.ops <- o
		var r result
		select {
		case r = <-o.done:
		case <-w.lost:
			d.mu.Lock()
			r = result{err: w.gone}
			d.mu.Unlock()
		case <-ctx.Done():
			r = result{err: ctx.Err()}
		}
		d.mu.Lock()
		w.inFlight--
		d.mu.Unlock()
		if !r.retry {
			return r.res, r.err
		}
		o.done = make(chan result, 1)
	}
	return Result{}, &OpError{Class: workspace.ClassBusy, Detail: "the worker's streams kept ending"}
}

// opLabel is an op's name as a metric label: the name when it is a short
// lower-case word, else "other", so no caller can grow the label set.
func opLabel(name string) string {
	if name == "" || len(name) > 32 {
		return "other"
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; (c < 'a' || c > 'z') && c != '_' {
			return "other"
		}
	}
	return name
}

func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return ClassOf(err)
}

// serve runs o on the stream. Every send happens on this goroutine; a second
// one only receives.
func (d *Dispatcher) serve(ctx context.Context, stream workspacepb.WorkspaceService_OpServer, w *worker, o *op) result {
	if o.stream != nil {
		return d.serveStream(ctx, stream, w, o)
	}
	req := o.req
	if err := stream.Send(&workspacepb.OpToWorker{Msg: &workspacepb.OpToWorker_Start{Start: &workspacepb.OpStart{
		WorkspaceId: req.WorkspaceID, Op: req.Op, Args: req.Args, HasInput: req.Input != nil,
	}}}); err != nil {
		// The stream is broken; whether the start reached the worker is not
		// known, so the op is not run again.
		return result{err: ErrWorkerLost}
	}
	frames := make(chan *workspacepb.OpFromWorker)
	recvErr := make(chan error, 1)
	safego.Go(ctx, "workspace.dispatch.recv", func() {
		for {
			f, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	})
	if req.Input != nil {
		if r, ok := d.sendInput(stream, o); !ok {
			return r
		}
	}
	var out []byte
	for {
		select {
		case f := <-frames:
			switch {
			case f.GetOutput() != nil:
				data := f.GetOutput().GetData()
				if len(data) > workspace.ChunkSize {
					return result{err: &OpError{Class: workspace.ClassProtocol, Detail: "an output chunk over 64 KB"}}
				}
				if int64(len(out)+len(data)) > o.max {
					return result{err: &OpError{Class: workspace.ClassOutputTooLarge, Detail: fmt.Sprintf("the worker sent more than %d bytes", o.max)}}
				}
				out = append(out, data...)
				d.countOutput(req.Op, len(data))
			case f.GetDone() != nil:
				return doneResult(f.GetDone(), out)
			default:
				return result{err: &OpError{Class: workspace.ClassProtocol, Detail: "an unexpected frame on an op stream"}}
			}
		case <-recvErr:
			return d.lostMidOp(w)
		case <-o.ctx.Done():
			return result{err: o.ctx.Err()}
		case <-ctx.Done():
			// The connection dropped, or a detach ended the stream.
			return d.lostMidOp(w)
		}
	}
}

// lostMidOp records that an op's stream broke before its answer: the worker
// died, whether or not its attachment has ended yet, and its detach counts
// the death once.
func (d *Dispatcher) lostMidOp(w *worker) result {
	d.mu.Lock()
	w.diedMidOp = true
	d.mu.Unlock()
	return result{err: ErrWorkerLost}
}

// sendInput streams the op's input in chunks of at most ChunkSize, each in a
// buffer of its own: a buffer reused across sends corrupted a stream in the
// RFC GW spike. ok is false when the op ended here.
func (d *Dispatcher) sendInput(stream workspacepb.WorkspaceService_OpServer, o *op) (result, bool) {
	for {
		if err := o.ctx.Err(); err != nil {
			return result{err: err}, false
		}
		buf := make([]byte, workspace.ChunkSize)
		n, rerr := io.ReadFull(o.req.Input, buf)
		if n > 0 {
			if err := stream.Send(&workspacepb.OpToWorker{Msg: &workspacepb.OpToWorker_Input{Input: &workspacepb.Chunk{Data: buf[:n]}}}); err != nil {
				return result{err: ErrWorkerLost}, false
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return result{err: fmt.Errorf("workspace: reading the op's input: %w", rerr)}, false
		}
	}
	if err := stream.Send(&workspacepb.OpToWorker{Msg: &workspacepb.OpToWorker_InputEnd{InputEnd: &workspacepb.InputEnd{}}}); err != nil {
		return result{err: ErrWorkerLost}, false
	}
	return result{}, true
}

// doneResult reads a worker's OpDone. Its class and detail are the worker's
// words: a class the core does not know is reported as failed, and the
// detail is bounded.
func doneResult(dn *workspacepb.OpDone, out []byte) result {
	res := Result{ExitCode: dn.GetExitCode(), Output: out}
	class := dn.GetErrorClass()
	if class == "" {
		return result{res: res}
	}
	if !workspace.KnownWorkerClasses[class] {
		class = workspace.ClassFailed
	}
	detail := strings.ToValidUTF8(dn.GetErrorDetail(), "?")
	if len(detail) > maxDetail {
		detail = strings.ToValidUTF8(detail[:maxDetail], "")
	}
	return result{res: res, err: &OpError{Class: class, Detail: detail}}
}

// ---- the ops of this layer ----

// Open makes workspace id's directory on the worker. An at_capacity answer
// means the caller places it on the next worker.
func (d *Dispatcher) Open(ctx context.Context, tenantID, userID, workerID, id string) error {
	return d.OpenWith(ctx, tenantID, userID, workerID, id, workspace.OpenArgs{})
}

// OpenWith is Open with the workspace's settings, its disk quota (RFC GW
// P2); zero values are the worker's defaults.
func (d *Dispatcher) OpenWith(ctx context.Context, tenantID, userID, workerID, id string, args workspace.OpenArgs) error {
	req := Request{TenantID: tenantID, UserID: userID, WorkspaceID: id, Op: workspace.OpOpen}
	if args != (workspace.OpenArgs{}) {
		raw, err := json.Marshal(args)
		if err != nil {
			return err
		}
		req.Args = raw
	}
	_, err := d.Do(ctx, workerID, req)
	return err
}

// Remove deletes workspace id's directory on the worker; one already gone is
// success.
func (d *Dispatcher) Remove(ctx context.Context, tenantID, userID, workerID, id string) error {
	_, err := d.Do(ctx, workerID, Request{TenantID: tenantID, UserID: userID, WorkspaceID: id, Op: workspace.OpRemove})
	return err
}

// Status reports whether workspace id's directory exists on the worker.
func (d *Dispatcher) Status(ctx context.Context, tenantID, userID, workerID, id string) (exists bool, err error) {
	res, err := d.Do(ctx, workerID, Request{TenantID: tenantID, UserID: userID, WorkspaceID: id, Op: workspace.OpStatus, MaxOutput: 4 << 10})
	if err != nil {
		return false, err
	}
	var st workspace.StatusAnswer
	if err := json.Unmarshal(res.Output, &st); err != nil {
		return false, &OpError{Class: workspace.ClassProtocol, Detail: "a status answer that is not JSON"}
	}
	return st.Exists, nil
}
