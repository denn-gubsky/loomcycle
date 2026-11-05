package grpc

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// A panic in a unary handler ends that call with Internal, not the process.
func TestRecoverUnaryInterceptor_AHandlerPanicBecomesInternal(t *testing.T) {
	info := &googlegrpc.UnaryServerInfo{FullMethod: "/loomcycle.v1.Loomcycle/GetAgent"}
	resp, err := RecoverUnaryInterceptor()(context.Background(), nil, info, func(context.Context, any) (any, error) {
		var m map[string]int
		m["x"] = 1
		return "unreachable", nil
	})
	if resp != nil || status.Code(err) != codes.Internal || status.Convert(err).Message() != panicMessage {
		t.Errorf("resp = %v, err = %v; want nil and Internal %q", resp, err, panicMessage)
	}
	// A handler that does not panic is untouched.
	resp, err = RecoverUnaryInterceptor()(context.Background(), nil, info, func(context.Context, any) (any, error) { return "ok", nil })
	if resp != "ok" || err != nil {
		t.Errorf("resp = %v, err = %v", resp, err)
	}
}

type fakeStream struct{ googlegrpc.ServerStream }

func (fakeStream) Context() context.Context    { return context.Background() }
func (fakeStream) SetHeader(metadata.MD) error { return nil }

// A panic in a streaming handler (a run's stream) ends that stream with
// Internal, not the process.
func TestRecoverStreamInterceptor_AHandlerPanicBecomesInternal(t *testing.T) {
	info := &googlegrpc.StreamServerInfo{FullMethod: "/loomcycle.v1.Loomcycle/Run"}
	err := RecoverStreamInterceptor()(nil, fakeStream{}, info, func(any, googlegrpc.ServerStream) error {
		panic("a bug")
	})
	if status.Code(err) != codes.Internal {
		t.Errorf("err = %v; want Internal", err)
	}
}

// The panic value may quote the request (a run's prompt, a document), and a
// gRPC handler does not know whether the run keeps any trace: the log names
// the value's type and the stack, never the value.
func TestRecoverUnaryInterceptor_TheLogNeverHoldsThePanicValue(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	info := &googlegrpc.UnaryServerInfo{FullMethod: "/loomcycle.v1.Loomcycle/Run"}
	_, _ = RecoverUnaryInterceptor()(context.Background(), nil, info, func(context.Context, any) (any, error) {
		panic("the user's private prompt text")
	})
	if out := buf.String(); strings.Contains(out, "private prompt") || !strings.Contains(out, "panic_type=string") {
		t.Errorf("log = %q; want the panic's type and not its value", out)
	}
}
