package grpc

import (
	"errors"
	"io"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// PutInboxFile keeps a file in the inbox of a run the caller owns (RFC HE
// F1), the twin of PUT /v1/runs/{run_id}/inbox/{name}. The connector does the
// work and holds every rule; this reads the stream as one body: the first
// message's run_id, name and media_type, and the data of every message in
// order. The bytes are streamed into the inbox's writer, never gathered here.
func (s *Server) PutInboxFile(stream googlegrpc.ClientStreamingServer[loomcyclepb.PutInboxFileRequest, loomcyclepb.PutInboxFileResponse]) error {
	if s.connector == nil {
		return status.Error(codes.Unavailable, "connector not wired")
	}
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return status.Error(codes.InvalidArgument, "PutInboxFile: the stream carried no message")
	}
	if err != nil {
		return err
	}
	if first.GetRunId() == "" || first.GetName() == "" {
		return status.Error(codes.InvalidArgument, "PutInboxFile: the first message needs run_id and name")
	}
	body := &putInboxReader{stream: stream, buf: first.GetData()}
	res, err := s.connector.PutInboxFile(stream.Context(), connector.InboxPutRequest{
		RunID:     first.GetRunId(),
		Name:      first.GetName(),
		MediaType: first.GetMediaType(),
		Body:      body,
		Size:      -1,
	})
	if err != nil {
		return inboxPutStatus(err)
	}
	return stream.SendAndClose(&loomcyclepb.PutInboxFileResponse{
		Ref: res.Ref, Name: res.Name, Bytes: res.Bytes, Sha256: res.SHA256, MediaType: res.MediaType,
	})
}

// putInboxReader reads a PutInboxFile stream's data as one body.
type putInboxReader struct {
	stream googlegrpc.ClientStreamingServer[loomcyclepb.PutInboxFileRequest, loomcyclepb.PutInboxFileResponse]
	buf    []byte
	done   bool
}

func (r *putInboxReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		msg, err := r.stream.Recv()
		if errors.Is(err, io.EOF) {
			r.done = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		r.buf = msg.GetData()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// inboxPutStatus is a refused put as a gRPC status, the HTTP statuses'
// counterparts, with the wire code as ErrorInfo.reason.
func inboxPutStatus(err error) error {
	var perr *connector.InboxPutError
	if !errors.As(err, &perr) {
		return status.Errorf(codes.Internal, "PutInboxFile: %v", err)
	}
	code := codes.Internal
	switch perr.Kind {
	case connector.InboxPutNotFound:
		code = codes.NotFound
	case connector.InboxPutBadRequest:
		code = codes.InvalidArgument
	case connector.InboxPutNotLive:
		code = codes.FailedPrecondition
	case connector.InboxPutTooBig, connector.InboxPutInboxFull:
		code = codes.ResourceExhausted
	case connector.InboxPutElsewhere, connector.InboxPutUnavailable:
		code = codes.Unavailable
	}
	return statusWithReason(code, perr.Code, perr.Msg)
}
