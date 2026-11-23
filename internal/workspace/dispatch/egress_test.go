package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// egressCall records what the dispatcher handed its egress function.
type egressCall struct {
	row  store.WorkspaceRow
	req  egress.Request
	body []byte
}

// recordingEgress answers every request with answer, after reading the
// request body whole, and records each call.
type recordingEgress struct {
	mu     sync.Mutex
	calls  []egressCall
	answer func(egress.Request) (*egress.Response, error)
}

func (r *recordingEgress) fn(_ context.Context, row store.WorkspaceRow, req egress.Request) (*egress.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
	}
	r.mu.Lock()
	r.calls = append(r.calls, egressCall{row: row, req: req, body: body})
	r.mu.Unlock()
	if r.answer != nil {
		return r.answer(req)
	}
	return &egress.Response{Status: http.StatusOK, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader("hello"))}, nil
}

func (r *recordingEgress) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingEgress) last() egressCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

// egressAnswer is what came back on an Egress stream.
type egressAnswer struct {
	status int
	header http.Header
	body   []byte
	done   string
	chunks int
}

// egress opens one Egress stream as worker id, sends start and body, and
// reads the answer to its end. err is the stream's status when the core
// refused the stream itself.
func (f *fakeWorker) egress(start *workspacepb.EgressStart, body []byte) (egressAnswer, error) {
	f.t.Helper()
	s, err := f.client.Egress(f.ctx)
	if err != nil {
		return egressAnswer{}, err
	}
	start.HasBody = body != nil
	if body == nil {
		start.ContentLength = 0
	} else {
		start.ContentLength = int64(len(body))
	}
	if err := s.Send(&workspacepb.EgressFromWorker{Msg: &workspacepb.EgressFromWorker_Start{Start: start}}); err != nil && !errors.Is(err, io.EOF) {
		return egressAnswer{}, err
	}
	if body != nil {
		for off := 0; off < len(body); off += workspace.ChunkSize {
			chunk := append([]byte(nil), body[off:min(off+workspace.ChunkSize, len(body))]...)
			if err := s.Send(&workspacepb.EgressFromWorker{Msg: &workspacepb.EgressFromWorker_Body{Body: &workspacepb.Chunk{Data: chunk}}}); err != nil {
				break
			}
		}
		_ = s.Send(&workspacepb.EgressFromWorker{Msg: &workspacepb.EgressFromWorker_BodyEnd{BodyEnd: &workspacepb.InputEnd{}}})
	}
	_ = s.CloseSend()
	var a egressAnswer
	for {
		m, err := s.Recv()
		if err != nil {
			return a, err
		}
		switch {
		case m.GetHead() != nil:
			a.status = int(m.GetHead().GetStatus())
			a.header = http.Header{}
			for _, h := range m.GetHead().GetHeaders() {
				a.header.Add(h.GetName(), h.GetValue())
			}
		case m.GetBody() != nil:
			a.body = append(a.body, m.GetBody().GetData()...)
			a.chunks++
		case m.GetDone() != nil:
			a.done = m.GetDone().GetError()
			return a, nil
		}
	}
}

func readyRow(id, tenant, worker string, routes ...string) store.WorkspaceRow {
	return store.WorkspaceRow{ID: id, TenantID: tenant, UserID: "alice", Name: id, WorkerID: worker, OwnerRunID: "run-" + id,
		State: store.WorkspaceReady, Egress: routes}
}

func egressStart(workerID, wsID string) *workspacepb.EgressStart {
	return &workspacepb.EgressStart{WorkerId: workerID, WorkspaceId: wsID, Route: "pypi", Method: http.MethodGet, Path: "simple/numpy/?format=json"}
}

// An Egress stream is accepted only from the principal that attached the
// worker it names, as an Op stream is: another tenant's token naming the
// same declared id finds no worker, and nothing is sent out.
func TestEgress_StreamForAWorkerAttachedByAnotherPrincipalIsRefused(t *testing.T) {
	st := newMemStore(readyRow("ws_a", "acme", acmeID("w1"), "pypi"))
	d, target := core(t, st, dispatch.Options{})
	rec := &recordingEgress{}
	d.SetEgress(rec.fn)
	acme := dialFake(t, target, tokAcme)
	acme.attach("w1")
	acme.heartbeat(time.Second)

	other := dialFake(t, target, tokOther)
	_, err := other.egress(egressStart("w1", "ws_a"), nil)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("another tenant's stream for acme's worker: %v, want PermissionDenied", err)
	}
	if rec.n() != 0 {
		t.Fatalf("the egress function was called %d times for a refused stream", rec.n())
	}

	// The attaching principal's own stream is served.
	a, err := acme.egress(egressStart("w1", "ws_a"), nil)
	if err != nil || a.status != http.StatusOK || string(a.body) != "hello" {
		t.Fatalf("the attaching worker's own stream = %+v, %v", a, err)
	}
}

// A stream that names a workspace that is not a ready row on that very
// worker is refused before anything is sent out: one on another of the
// tenant's workers, one still opening, one releasing, one unknown.
func TestEgress_WorkspaceNotReadyOnThatWorkerIsRefused(t *testing.T) {
	opening := readyRow("ws_open", "acme", acmeID("w1"), "pypi")
	opening.State = store.WorkspaceOpening
	releasing := readyRow("ws_rel", "acme", acmeID("w1"), "pypi")
	releasing.State = store.WorkspaceReleasing
	st := newMemStore(
		readyRow("ws_mine", "acme", acmeID("w1"), "pypi"),
		readyRow("ws_w2", "acme", acmeID("w2"), "pypi"),
		opening, releasing,
	)
	d, target := core(t, st, dispatch.Options{})
	rec := &recordingEgress{}
	d.SetEgress(rec.fn)
	w1 := dialFake(t, target, tokAcme)
	w1.attach("w1")
	w1.heartbeat(time.Second)
	w2 := dialFake(t, target, tokAcme)
	w2.attach("w2")
	w2.heartbeat(time.Second)

	for _, id := range []string{"ws_w2", "ws_open", "ws_rel", "ws_unknown"} {
		_, err := w1.egress(egressStart("w1", id), nil)
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s from w1: %v, want PermissionDenied", id, err)
		}
	}
	if _, err := w1.egress(egressStart("w1", "../ws_mine"), nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed workspace id: %v, want InvalidArgument", err)
	}
	if rec.n() != 0 {
		t.Fatalf("the egress function was called %d times for refused streams", rec.n())
	}
	if a, err := w1.egress(egressStart("w1", "ws_mine"), nil); err != nil || a.status != http.StatusOK {
		t.Fatalf("w1's own ready workspace = %+v, %v", a, err)
	}
	if got := rec.last().row.ID; got != "ws_mine" {
		t.Fatalf("the egress function was handed row %q, want ws_mine", got)
	}
}

// What the worker described reaches the egress function as it was sent (the
// row read from the store, the route, the method, the escaped path with its
// query, the headers and the body), and the answer comes back whole, its
// body in chunks of at most 64 KB.
func TestEgress_CarriesTheRequestAndStreamsTheAnswer(t *testing.T) {
	st := newMemStore(readyRow("ws_a", "acme", acmeID("w1"), "pypi", "npm"))
	d, target := core(t, st, dispatch.Options{})
	big := bytes.Repeat([]byte("0123456789abcdef"), 20000) // 320000 bytes
	rec := &recordingEgress{answer: func(egress.Request) (*egress.Response, error) {
		return &egress.Response{Status: http.StatusCreated, Header: http.Header{"X-Answer": {"yes"}}, Body: io.NopCloser(bytes.NewReader(big))}, nil
	}}
	d.SetEgress(rec.fn)
	w := dialFake(t, target, tokAcme)
	w.attach("w1")
	w.heartbeat(time.Second)

	start := egressStart("w1", "ws_a")
	start.Method, start.Route, start.Path = http.MethodPost, "npm", "%40scope%2fpkg/-/x?a=1&b=%2F"
	start.Headers = []*workspacepb.Header{{Name: "Content-Type", Value: "application/json"}, {Name: "X-Multi", Value: "1"}, {Name: "X-Multi", Value: "2"}}
	reqBody := bytes.Repeat([]byte("q"), 3*workspace.ChunkSize+7)
	a, err := w.egress(start, reqBody)
	if err != nil {
		t.Fatal(err)
	}
	if a.status != http.StatusCreated || a.header.Get("X-Answer") != "yes" || !bytes.Equal(a.body, big) || a.done != "" {
		t.Fatalf("answer = status %d header %v body %d bytes done %q", a.status, a.header, len(a.body), a.done)
	}
	if a.chunks < len(big)/workspace.ChunkSize {
		t.Errorf("the answer came in %d chunks; a 64 KB bound means at least %d", a.chunks, len(big)/workspace.ChunkSize)
	}
	c := rec.last()
	if c.row.ID != "ws_a" || c.row.TenantID != "acme" || len(c.row.Egress) != 2 {
		t.Errorf("row = %+v", c.row)
	}
	if c.req.Route != "npm" || c.req.Method != http.MethodPost || c.req.Path != "%40scope%2fpkg/-/x?a=1&b=%2F" {
		t.Errorf("request = route %q method %q path %q", c.req.Route, c.req.Method, c.req.Path)
	}
	if got := c.req.Header.Values("X-Multi"); len(got) != 2 || c.req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", c.req.Header)
	}
	if !bytes.Equal(c.body, reqBody) || c.req.ContentLength != int64(len(reqBody)) {
		t.Errorf("request body = %d bytes (content length %d), want %d", len(c.body), c.req.ContentLength, len(reqBody))
	}
}

// A request the core refuses comes back as an HTTP answer the session's
// client shows: the refusal's status and its one-line reason as plain text.
// An upstream failure comes back as 502 with a short reason that carries no
// query of the request.
func TestEgress_RefusalsAndFailuresAreHTTPAnswers(t *testing.T) {
	st := newMemStore(readyRow("ws_a", "acme", acmeID("w1"), "pypi"))
	d, target := core(t, st, dispatch.Options{})
	w := dialFake(t, target, tokAcme)
	w.attach("w1")
	w.heartbeat(time.Second)

	d.SetEgress((&recordingEgress{answer: func(egress.Request) (*egress.Response, error) {
		return nil, &egress.Refusal{Status: http.StatusForbidden, Reason: `route "x" is not one this workspace holds`}
	}}).fn)
	a, err := w.egress(egressStart("w1", "ws_a"), nil)
	if err != nil || a.status != http.StatusForbidden || !strings.HasPrefix(a.header.Get("Content-Type"), "text/plain") ||
		strings.TrimSpace(string(a.body)) != `route "x" is not one this workspace holds` {
		t.Fatalf("refusal = %+v (%q), %v", a, a.body, err)
	}

	d.SetEgress((&recordingEgress{answer: func(egress.Request) (*egress.Response, error) {
		return nil, errors.New(`egress: route "pypi": upstream request failed: connection refused`)
	}}).fn)
	start := egressStart("w1", "ws_a")
	start.Path = "simple/?token=sekrit-query"
	a, err = w.egress(start, nil)
	if err != nil || a.status != http.StatusBadGateway || strings.Contains(string(a.body), "sekrit-query") {
		t.Fatalf("upstream failure = %+v (%q), %v", a, a.body, err)
	}
}

// With no egress configured every request is refused as an HTTP 403: a
// session has no way out at all.
func TestEgress_WithNoEgressConfiguredEveryRequestIsRefused(t *testing.T) {
	st := newMemStore(readyRow("ws_a", "acme", acmeID("w1"), "pypi"))
	_, target := core(t, st, dispatch.Options{})
	w := dialFake(t, target, tokAcme)
	w.attach("w1")
	w.heartbeat(time.Second)
	a, err := w.egress(egressStart("w1", "ws_a"), nil)
	if err != nil || a.status != http.StatusForbidden {
		t.Fatalf("answer = %+v, %v; want a 403", a, err)
	}
}

// A worker that goes away mid-request ends the request body the egress
// function reads and the answer it copies, so neither is left running.
func TestEgress_AStreamThatEndsEndsTheRequestAndTheAnswer(t *testing.T) {
	st := newMemStore(readyRow("ws_a", "acme", acmeID("w1"), "pypi"))
	d, target := core(t, st, dispatch.Options{})
	var ended atomic.Int32
	reading := make(chan struct{})
	d.SetEgress(func(ctx context.Context, _ store.WorkspaceRow, req egress.Request) (*egress.Response, error) {
		close(reading)
		_, err := io.ReadAll(req.Body)
		if err != nil {
			ended.Add(1)
		}
		return nil, err
	})
	w := dialFake(t, target, tokAcme)
	w.attach("w1")
	w.heartbeat(time.Second)

	sctx, cancel := context.WithCancel(w.ctx)
	s, err := w.client.Egress(sctx)
	if err != nil {
		t.Fatal(err)
	}
	start := egressStart("w1", "ws_a")
	start.HasBody, start.ContentLength = true, -1
	if err := s.Send(&workspacepb.EgressFromWorker{Msg: &workspacepb.EgressFromWorker_Start{Start: start}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(&workspacepb.EgressFromWorker{Msg: &workspacepb.EgressFromWorker_Body{Body: &workspacepb.Chunk{Data: []byte("part")}}}); err != nil {
		t.Fatal(err)
	}
	<-reading
	cancel()
	waitFor(t, "the request body to end", func() bool { return ended.Load() == 1 })
}
