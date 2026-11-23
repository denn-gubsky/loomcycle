package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// egress_client.go — the worker's side of a workspace's way out (RFC GW §6.1,
// §6.3). A session asks for http://egress/<route>/<path>; the handler here
// describes that request to the core on an Egress stream over the worker's
// own connection and passes the core's answer back. The worker holds no URL,
// no route table and no credential: the core decides from the workspace's
// row whether the request goes out, and adds the credential itself.

// egressLinks are the worker's attached connections, any of which may carry
// an Egress stream: every replica it is attached to knows its workspaces.
type egressLinks struct {
	mu   sync.Mutex
	list []workspacepb.WorkspaceServiceClient
}

// add records an attached connection; the returned func drops it when the
// attachment ends.
func (l *egressLinks) add(c workspacepb.WorkspaceServiceClient) func() {
	l.mu.Lock()
	l.list = append(l.list, c)
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, x := range l.list {
			if x == c {
				l.list = append(l.list[:i], l.list[i+1:]...)
				return
			}
		}
	}
}

// pick is the newest attached connection, or nil when there is none.
func (l *egressLinks) pick() workspacepb.WorkspaceServiceClient {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.list) == 0 {
		return nil
	}
	return l.list[len(l.list)-1]
}

// egressHandler is the HTTP handler a session of workspaceID reaches the
// routes through: a request for /<route>/<rest> goes to the core as one
// Egress stream naming that workspace. The workspace is the caller's to know
// (where the request arrived from), never the request's.
func (w *worker) egressHandler(workspaceID string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { w.serveEgress(rw, r, workspaceID) })
}

func (w *worker) serveEgress(rw http.ResponseWriter, r *http.Request, workspaceID string) {
	// The raw request target, not r.URL: the core checks the path's
	// escapes (an encoded slash, a dot segment) as the session sent them,
	// and a decoded path would hide them.
	raw := r.RequestURI
	if r.Method == http.MethodConnect || !strings.HasPrefix(raw, "/") {
		egressText(rw, http.StatusBadRequest, "the egress address takes a request for /<route>/<path>, not a proxy request")
		return
	}
	route, rest, ok := strings.Cut(raw[1:], "/")
	if !ok || !egress.ValidName(route) {
		egressText(rw, http.StatusBadRequest, "the egress address is http://egress/<route>/<path>, with a route name of lower-case letters, digits and '-'")
		return
	}
	client := w.links.pick()
	if client == nil {
		egressText(rw, http.StatusServiceUnavailable, "the workspace worker is not attached to the runtime; try again")
		return
	}
	ctx, cancel := context.WithCancel(w.outgoing(r.Context()))
	defer cancel()
	s, err := client.Egress(ctx)
	if err != nil {
		egressText(rw, http.StatusBadGateway, "the runtime cannot be reached")
		return
	}
	hasBody := r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0
	start := &workspacepb.EgressStart{
		WorkerId: w.opt.ID, WorkspaceId: workspaceID, Route: route, Method: r.Method, Path: rest,
		HasBody: hasBody, ContentLength: r.ContentLength,
	}
	for name, vals := range r.Header {
		for _, v := range vals {
			start.Headers = append(start.Headers, &workspacepb.Header{Name: name, Value: v})
		}
	}
	if err := s.Send(&workspacepb.EgressFromWorker{Msg: &workspacepb.EgressFromWorker_Start{Start: start}}); err != nil && !errors.Is(err, io.EOF) {
		egressText(rw, http.StatusBadGateway, "the runtime cannot be reached")
		return
	}
	rc := http.NewResponseController(rw)
	if hasBody {
		// The body goes up while the answer comes down: a refusal may
		// arrive before the body is all sent. The handler must not return
		// while the body is still read (net/http forbids it), so it ends
		// the upload and waits: the cancel stops a send, the read deadline
		// a read waiting on a slow session.
		_ = rc.EnableFullDuplex()
		uploaded := make(chan struct{})
		safego.Go(ctx, "workspace.worker.egress-body", func() {
			defer close(uploaded)
			sendEgressBody(s, r.Body)
		})
		defer func() {
			cancel()
			_ = rc.SetReadDeadline(time.Now())
			<-uploaded
		}()
	} else if err := s.CloseSend(); err != nil {
		egressText(rw, http.StatusBadGateway, "the runtime cannot be reached")
		return
	}
	w.relayEgressAnswer(rw, rc, s)
}

// sendEgressBody sends the request body in chunks of at most 64 KB, each in a
// buffer of its own (RFC GW §8, spike B), then InputEnd. A body that fails to
// read ends the stream without InputEnd, so the core sends nothing of it.
func sendEgressBody(s workspacepb.WorkspaceService_EgressClient, body io.Reader) {
	for {
		buf := make([]byte, workspace.ChunkSize)
		n, err := io.ReadFull(body, buf)
		if n > 0 {
			if serr := s.Send(&workspacepb.EgressFromWorker{Msg: &workspacepb.EgressFromWorker_Body{Body: &workspacepb.Chunk{Data: buf[:n]}}}); serr != nil {
				return
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			_ = s.CloseSend()
			return
		}
	}
	_ = s.Send(&workspacepb.EgressFromWorker{Msg: &workspacepb.EgressFromWorker_BodyEnd{BodyEnd: &workspacepb.InputEnd{}}})
	_ = s.CloseSend()
}

// relayEgressAnswer writes the core's answer to the session. A stream that
// fails before the head is a 502; one that fails, or ends cut, after it
// ends the session's connection mid-answer, so its client sees a broken
// answer rather than a short one that looks whole.
func (w *worker) relayEgressAnswer(rw http.ResponseWriter, rc *http.ResponseController, s workspacepb.WorkspaceService_EgressClient) {
	m, err := s.Recv()
	if err != nil || m.GetHead() == nil {
		reason := "the runtime ended the request"
		if st, ok := status.FromError(err); ok && err != nil {
			reason = "the runtime refused the request: " + st.Message()
		}
		egressText(rw, http.StatusBadGateway, reason)
		return
	}
	h := rw.Header()
	for _, hd := range m.GetHead().GetHeaders() {
		h.Add(hd.GetName(), hd.GetValue())
	}
	code := int(m.GetHead().GetStatus())
	if code < 100 || code > 599 {
		code = http.StatusBadGateway
	}
	rw.WriteHeader(code)
	for {
		m, err := s.Recv()
		if err != nil {
			abortAnswer(rc)
			return
		}
		switch {
		case m.GetBody() != nil:
			if _, err := rw.Write(m.GetBody().GetData()); err != nil {
				return
			}
			_ = rc.Flush()
		case m.GetDone() != nil:
			if m.GetDone().GetError() != "" {
				w.log.Warn("an egress answer was cut", "reason", m.GetDone().GetError())
				abortAnswer(rc)
			}
			return
		default:
			abortAnswer(rc)
			return
		}
	}
}

// abortAnswer closes the session's connection mid-answer, with no final
// chunk, so the client reads an error and never a shortened answer as whole.
func abortAnswer(rc *http.ResponseController) {
	if conn, _, err := rc.Hijack(); err == nil {
		_ = conn.Close()
	}
}

// egressText answers the session with status and a one-line reason.
func egressText(rw http.ResponseWriter, code int, reason string) {
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.WriteHeader(code)
	_, _ = fmt.Fprintln(rw, reason)
}

// wireEgress points a container worker's session network (RFC GW §6.3) at
// the worker's own egress handler. The engine is built before the worker, so
// the handler reads *self when a session starts, not now; until it is set a
// session gets the engine's placeholder, which answers 503. A copy of the
// egress options is made so the caller's are not changed under it.
func wireEgress(opt *Options, self **worker) {
	eg := opt.Container.Egress
	if eg == nil {
		return
	}
	c := *eg
	c.Handler = func(workspaceID string) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			w := *self
			if w == nil {
				egressText(rw, http.StatusServiceUnavailable, "the workspace worker is not ready to send requests out")
				return
			}
			w.serveEgress(rw, r, workspaceID)
		})
	}
	opt.Container.Egress = &c
}
