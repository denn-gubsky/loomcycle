package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// EgressFunc answers one request a workspace's session sends out (RFC GW
// §6.3). row is the workspace's row as the store holds it, read here and
// never taken from the worker: it says whose the workspace is and which
// routes it may use. A *egress.Refusal is answered to the session as an
// HTTP answer; any other error as a 502.
type EgressFunc func(ctx context.Context, row store.WorkspaceRow, req egress.Request) (*egress.Response, error)

// SetEgress sets what answers the workers' Egress streams. Called once at
// boot; until then, and when never called, every request is refused.
func (d *Dispatcher) SetEgress(f EgressFunc) { d.egress.Store(&f) }

// Bounds on what an EgressStart may carry. The proxy decides what passes;
// these keep a worker from making the core hold an unbounded start.
const (
	maxEgressHeaders     = 256
	maxEgressHeaderBytes = 64 << 10
	maxEgressPath        = 16 << 10
)

// egressBodyEnded is the request body's end when the stream went away
// before InputEnd.
var egressBodyEnded = errors.New("the worker's request stream ended before its body did")

// Egress serves one Egress stream: one HTTP round trip a ready workspace's
// session sent out, through its worker.
//
// The stream is accepted only from the principal that attached the worker
// id it names, exactly as an Op stream is, and only for a workspace whose
// row is on that worker and ready. Everything else about the request, which
// routes it may use above all, is decided from that row.
func (d *Dispatcher) Egress(stream workspacepb.WorkspaceService_EgressServer) (err error) {
	outcome := "refused"
	defer func() { d.count("egress", outcome) }()
	owner, _, _, err := d.authenticate(stream.Context())
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "the first Egress frame must be EgressStart")
	}
	if err := checkEgressStart(start); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	d.mu.Lock()
	// The id is built from what this stream's own token grants: a stream
	// naming a worker another principal attached finds nothing.
	w := d.workers[owner+"/"+start.GetWorkerId()]
	d.mu.Unlock()
	if w == nil {
		return status.Error(codes.PermissionDenied, "no worker this token grants is attached under that id")
	}
	select {
	case <-w.lost:
		return status.Error(codes.PermissionDenied, "the worker is no longer attached")
	default:
	}
	row, err := d.egressRow(ctx, w, start.GetWorkspaceId())
	if err != nil {
		return err
	}

	req := egress.Request{
		Route: start.GetRoute(), Method: start.GetMethod(), Path: start.GetPath(),
		Header: http.Header{}, ContentLength: -1,
	}
	for _, h := range start.GetHeaders() {
		req.Header.Add(h.GetName(), h.GetValue())
	}
	if start.GetHasBody() {
		pr, pw := io.Pipe()
		// The body ends with the stream: the egress function's reads and a
		// receive blocked writing into the pipe both return once ctx does.
		stop := context.AfterFunc(ctx, func() { pr.CloseWithError(egressBodyEnded) })
		defer stop()
		defer pr.CloseWithError(egressBodyEnded)
		safego.Go(ctx, "workspace.dispatch.egress-body", func() { receiveEgressBody(stream, pw) })
		req.Body = pr
		if n := start.GetContentLength(); n >= 0 {
			req.ContentLength = n
		}
	}

	f := d.egress.Load()
	if f == nil {
		return sendEgressText(stream, http.StatusForbidden, "this runtime sends no workspace request out (no egress is configured)")
	}
	resp, err := (*f)(ctx, row, req)
	if err != nil {
		if ctx.Err() != nil {
			outcome = "cancelled"
			return ctx.Err()
		}
		var ref *egress.Refusal
		if errors.As(err, &ref) {
			return sendEgressText(stream, ref.Status, ref.Reason)
		}
		outcome = "failed"
		// The proxy's errors name the route and the transport's failure,
		// never the request's URL, query or a credential; bounded all
		// the same.
		return sendEgressText(stream, http.StatusBadGateway, boundDetail(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	if err := stream.Send(&workspacepb.EgressToWorker{Msg: &workspacepb.EgressToWorker_Head{Head: &workspacepb.EgressHead{
		Status: int32(resp.Status), Headers: protoHeaders(resp.Header),
	}}}); err != nil {
		outcome = "cancelled"
		return err
	}
	done := ""
	for {
		// A buffer of its own for every chunk: gRPC may still hold the
		// previous one when the next read fills (RFC GW §8, spike B).
		buf := make([]byte, workspace.ChunkSize)
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if err := stream.Send(&workspacepb.EgressToWorker{Msg: &workspacepb.EgressToWorker_Body{Body: &workspacepb.Chunk{Data: buf[:n]}}}); err != nil {
				outcome = "cancelled"
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				outcome = "cancelled"
				return ctx.Err()
			}
			done = "the answer was cut: the upstream's body ended early"
			if errors.Is(rerr, egress.ErrByteBudget) {
				done = "the answer was cut: " + egress.ErrByteBudget.Error()
			}
			break
		}
	}
	outcome = "ok"
	return stream.Send(&workspacepb.EgressToWorker{Msg: &workspacepb.EgressToWorker_Done{Done: &workspacepb.EgressDone{Error: done}}})
}

// checkEgressStart bounds what a start carries.
func checkEgressStart(s *workspacepb.EgressStart) error {
	if !workspace.ValidID(s.GetWorkspaceId()) {
		return status.Error(codes.InvalidArgument, "a malformed workspace id")
	}
	if len(s.GetPath()) > maxEgressPath || len(s.GetRoute()) > 64 || len(s.GetMethod()) > 16 {
		return status.Error(codes.InvalidArgument, "the request's route, method or path is too long")
	}
	if len(s.GetHeaders()) > maxEgressHeaders {
		return status.Error(codes.InvalidArgument, "the request has too many headers")
	}
	n := 0
	for _, h := range s.GetHeaders() {
		n += len(h.GetName()) + len(h.GetValue())
	}
	if n > maxEgressHeaderBytes {
		return status.Error(codes.InvalidArgument, "the request's headers are too large")
	}
	return nil
}

// egressRow is the ready row of workspace id on worker w. A workspace on
// another worker, of a tenant w does not serve, not ready, or unknown is
// refused alike, so a worker learns nothing of rows not its own. The worker's
// rows are bounded by its capacity, so reading them all is cheap.
func (d *Dispatcher) egressRow(ctx context.Context, w *worker, id string) (store.WorkspaceRow, error) {
	st := d.store()
	if st == nil {
		return store.WorkspaceRow{}, status.Error(codes.Unavailable, "the runtime is starting")
	}
	rows, err := st.WorkspaceListByWorker(ctx, w.id, store.WorkspaceReady)
	if err != nil {
		return store.WorkspaceRow{}, status.Error(codes.Unavailable, "the runtime could not read the workspace")
	}
	for _, r := range rows {
		// The query asks for both already; checked again so a store that
		// ever answered wider cannot hand one worker another's workspace.
		// A parked durable workspace (RFC GW P12) is ready but held by no
		// run: whatever is left running in it sends nothing out.
		if r.ID == id && r.WorkerID == w.id && r.State == store.WorkspaceReady && r.OwnerKind != store.WorkspaceOwnerParked && w.servesTenant(r.TenantID) {
			return r, nil
		}
	}
	return store.WorkspaceRow{}, status.Error(codes.PermissionDenied, "no ready workspace of that id is on this worker")
}

// receiveEgressBody copies the request body's chunks into pw until InputEnd,
// and ends pw with an error on anything else.
func receiveEgressBody(stream workspacepb.WorkspaceService_EgressServer, pw *io.PipeWriter) {
	for {
		m, err := stream.Recv()
		if err != nil {
			pw.CloseWithError(egressBodyEnded)
			return
		}
		switch {
		case m.GetBody() != nil:
			data := m.GetBody().GetData()
			if len(data) > workspace.ChunkSize {
				pw.CloseWithError(errors.New("a request body chunk over 64 KB"))
				return
			}
			if _, err := pw.Write(data); err != nil {
				return
			}
		case m.GetBodyEnd() != nil:
			_ = pw.Close()
			return
		default:
			pw.CloseWithError(errors.New("an unexpected frame in a request body"))
			return
		}
	}
}

// sendEgressText answers the session with status and a one-line plain-text
// body, then ends the stream.
func sendEgressText(stream workspacepb.WorkspaceService_EgressServer, code int, reason string) error {
	if code < 100 || code > 599 {
		code = http.StatusBadGateway
	}
	body := []byte(reason + "\n")
	for _, m := range []*workspacepb.EgressToWorker{
		{Msg: &workspacepb.EgressToWorker_Head{Head: &workspacepb.EgressHead{Status: int32(code), Headers: []*workspacepb.Header{
			{Name: "Content-Type", Value: "text/plain; charset=utf-8"},
			{Name: "Content-Length", Value: fmt.Sprint(len(body))},
		}}}},
		{Msg: &workspacepb.EgressToWorker_Body{Body: &workspacepb.Chunk{Data: body}}},
		{Msg: &workspacepb.EgressToWorker_Done{Done: &workspacepb.EgressDone{}}},
	} {
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	return nil
}

func protoHeaders(h http.Header) []*workspacepb.Header {
	out := make([]*workspacepb.Header, 0, len(h))
	for name, vals := range h {
		for _, v := range vals {
			out = append(out, &workspacepb.Header{Name: name, Value: v})
		}
	}
	return out
}

// boundDetail keeps a reason to one short line.
func boundDetail(s string) string {
	if len(s) > maxDetail {
		s = s[:maxDetail]
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return s[:i]
		}
	}
	return s
}
