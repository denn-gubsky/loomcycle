package grpc

import (
	"context"
	"io"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// putConnector records the put it is handed, reading the body as the
// connector's inbox writer would.
type putConnector struct {
	mockConnector
	req  connector.InboxPutRequest
	body []byte
	err  error
}

func (c *putConnector) PutInboxFile(_ context.Context, req connector.InboxPutRequest) (connector.InboxPutResult, error) {
	c.req = req
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return connector.InboxPutResult{}, err
	}
	c.body = b
	if c.err != nil {
		return connector.InboxPutResult{}, c.err
	}
	return connector.InboxPutResult{Ref: "volume:inbox:documents/a.pdf", Name: "a.pdf", Bytes: int64(len(b)), SHA256: "ab", MediaType: "application/pdf"}, nil
}

// fakePutStream is a PutInboxFile server stream over a fixed list of
// messages.
type fakePutStream struct {
	googlegrpc.ServerStream
	msgs []*loomcyclepb.PutInboxFileRequest
	resp *loomcyclepb.PutInboxFileResponse
}

func (f *fakePutStream) Context() context.Context { return context.Background() }

func (f *fakePutStream) Recv() (*loomcyclepb.PutInboxFileRequest, error) {
	if len(f.msgs) == 0 {
		return nil, io.EOF
	}
	m := f.msgs[0]
	f.msgs = f.msgs[1:]
	return m, nil
}

func (f *fakePutStream) SendAndClose(r *loomcyclepb.PutInboxFileResponse) error {
	f.resp = r
	return nil
}

// The stream is one body: the first message names the run, the file and its
// type; the data of every message, in order, is the file.
func TestGrpcPutInboxFile_StreamIsOneBodyNamedByTheFirstMessage(t *testing.T) {
	c := &putConnector{}
	s := New(Config{Connector: c})
	stream := &fakePutStream{msgs: []*loomcyclepb.PutInboxFileRequest{
		{RunId: "r1", Name: "a.pdf", MediaType: "application/pdf", Data: []byte("%PDF")},
		{Data: []byte("-1.7")},
		{RunId: "other", Name: "ignored.txt", Data: []byte(" end")},
	}}
	if err := s.PutInboxFile(stream); err != nil {
		t.Fatal(err)
	}
	if c.req.RunID != "r1" || c.req.Name != "a.pdf" || c.req.MediaType != "application/pdf" || c.req.Size != -1 {
		t.Fatalf("request = %+v", c.req)
	}
	if string(c.body) != "%PDF-1.7 end" {
		t.Fatalf("body = %q", c.body)
	}
	if stream.resp.GetRef() != "volume:inbox:documents/a.pdf" || stream.resp.GetBytes() != int64(len(c.body)) {
		t.Fatalf("response = %v", stream.resp)
	}
}

// A first message without run_id or name, or no message at all, is
// InvalidArgument before the connector is asked.
func TestGrpcPutInboxFile_FirstMessageMustNameRunAndFile(t *testing.T) {
	for _, msgs := range [][]*loomcyclepb.PutInboxFileRequest{
		nil,
		{{Name: "a.pdf", Data: []byte("x")}},
		{{RunId: "r1", Data: []byte("x")}},
	} {
		c := &putConnector{}
		err := New(Config{Connector: c}).PutInboxFile(&fakePutStream{msgs: msgs})
		if status.Code(err) != codes.InvalidArgument || c.req.RunID != "" {
			t.Errorf("%v: err = %v, connector asked = %v", msgs, err, c.req.RunID != "")
		}
	}
}

// A refusal keeps the HTTP route's meaning: the status by kind, the wire
// code as ErrorInfo.reason.
func TestGrpcPutInboxFile_RefusalsMapToCodesWithTheReason(t *testing.T) {
	for _, c := range []struct {
		kind   connector.InboxPutErrorKind
		reason string
		code   codes.Code
	}{
		{connector.InboxPutNotFound, "not_found", codes.NotFound},
		{connector.InboxPutNotLive, "run_not_live", codes.FailedPrecondition},
		{connector.InboxPutElsewhere, "run_elsewhere", codes.Unavailable},
		{connector.InboxPutTooBig, "client_put_max_bytes", codes.ResourceExhausted},
		{connector.InboxPutInboxFull, "inbox_full", codes.ResourceExhausted},
		{connector.InboxPutUnavailable, "audit_unavailable", codes.Unavailable},
		{connector.InboxPutBadRequest, "bad_request", codes.InvalidArgument},
	} {
		pc := &putConnector{err: &connector.InboxPutError{Kind: c.kind, Code: c.reason, Msg: "refused"}}
		err := New(Config{Connector: pc}).PutInboxFile(&fakePutStream{msgs: []*loomcyclepb.PutInboxFileRequest{{RunId: "r1", Name: "a.pdf"}}})
		st, _ := status.FromError(err)
		var reason string
		for _, d := range st.Details() {
			if ei, ok := d.(*errdetails.ErrorInfo); ok {
				reason = ei.Reason
			}
		}
		if st.Code() != c.code || reason != c.reason {
			t.Errorf("%s: %v (reason %q), want %v", c.reason, err, reason, c.code)
		}
	}
}

// A token limited to certain agents reaches PutInboxFile only when it was
// granted the inbox_put surface, and that grant opens no other off-run RPC.
func TestGrpcPutInboxFile_RestrictedTokenNeedsTheInboxPutSurface(t *testing.T) {
	ctxFor := func(surfaces ...string) context.Context {
		p := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
			Restrictions: auth.Restrictions{Agents: []string{"chat"}, Surfaces: surfaces}}
		return auth.WithPrincipal(authz.WithTransport(authz.WithMode(context.Background(), authz.ModeCompat), "grpc"), p)
	}
	put := grpcMethodPrefix + "PutInboxFile"
	if err := enforceScope(ctxFor(), put); status.Code(err) != codes.PermissionDenied {
		t.Errorf("no grant: %v, want PermissionDenied", err)
	}
	if err := enforceScope(ctxFor(auth.SurfaceTranscription), put); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another surface granted: %v, want PermissionDenied", err)
	}
	if err := enforceScope(ctxFor(auth.SurfaceInboxPut), put); err != nil {
		t.Errorf("granted: %v, want nil", err)
	}
	if err := enforceScope(ctxFor(auth.SurfaceInboxPut), grpcMethodPrefix+"Decide"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("granted inbox_put, Decide: %v, want PermissionDenied", err)
	}
}
