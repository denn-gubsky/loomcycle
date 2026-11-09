package grpc

import (
	"context"
	"io"
	"net"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	inboundTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	inboundSpanID  = "00f067aa0ba902b7"
)

// spanRunner opens the run span on the ctx the RPC hands RunOnce, as the
// real runner does.
type spanRunner struct{}

func (spanRunner) RunOnce(ctx context.Context, _ runner.RunInput, cb runner.RunCallbacks) error {
	_, span := lcotel.RecordRunStart(ctx, lcotel.RunStartAttrs{RunID: "r"})
	span.End()
	if cb.OnRegistered != nil {
		cb.OnRegistered("a", "r", "s", "")
	}
	return nil
}

func runOverGRPC(t *testing.T, authToken, bearer string) (*tracetest.InMemoryExporter, error) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restoreTP := lcotel.SetTracerProviderForTest(tp)
	restoreIn := lcotel.SetInboundForTest(lcotel.InboundParent, propagation.TraceContext{})
	t.Cleanup(func() { restoreIn(); restoreTP(); _ = tp.Shutdown(context.Background()) })

	adapter := New(Config{Store: newTestStore(t), CancelReg: cancel.NewRegistry(), Runner: spanRunner{}, AuthToken: authToken})
	srv := googlegrpc.NewServer(
		googlegrpc.UnaryInterceptor(adapter.UnaryAuthInterceptor()),
		googlegrpc.StreamInterceptor(adapter.StreamAuthInterceptor()),
	)
	loomcyclepb.RegisterLoomcycleServer(srv, adapter)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := googlegrpc.NewClient(lis.Addr().String(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	md := []string{"traceparent", "00-" + inboundTraceID + "-" + inboundSpanID + "-01"}
	if bearer != "" {
		md = append(md, "authorization", "Bearer "+bearer)
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), md...)
	stream, err := loomcyclepb.NewLoomcycleClient(conn).Run(ctx, &loomcyclepb.RunRequest{Agent: "default"})
	if err != nil {
		return exp, err
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if err == io.EOF {
				return exp, nil
			}
			return exp, err
		}
	}
}

// TestRunRPC_AuthenticatedTraceparentParentsTheRunSpan: the gRPC Run of an
// authenticated caller joins the trace its metadata names.
func TestRunRPC_AuthenticatedTraceparentParentsTheRunSpan(t *testing.T) {
	exp, err := runOverGRPC(t, "tok", "tok")
	if err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].SpanContext.TraceID().String() != inboundTraceID || spans[0].Parent.SpanID().String() != inboundSpanID {
		t.Errorf("run span trace=%s parent=%s, want the caller's %s/%s",
			spans[0].SpanContext.TraceID(), spans[0].Parent.SpanID(), inboundTraceID, inboundSpanID)
	}
}

// TestRunRPC_OpenModeIgnoresTraceparent: with no credential configured
// nobody is authenticated, so the caller's metadata is not read.
func TestRunRPC_OpenModeIgnoresTraceparent(t *testing.T) {
	exp, err := runOverGRPC(t, "", "")
	if err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].Parent.IsValid() || spans[0].SpanContext.TraceID().String() == inboundTraceID {
		t.Errorf("open mode joined the caller's trace: parent=%s", spans[0].Parent.SpanID())
	}
}

// TestRunToProto_CarriesTraceID: GetRun / GetAgent and the started frame map
// the run's trace id onto the wire.
func TestRunToProto_CarriesTraceID(t *testing.T) {
	if got := runToProto(store.Run{ID: "r", TraceID: inboundTraceID}, false).GetTraceId(); got != inboundTraceID {
		t.Errorf("Agent.trace_id = %q, want %q", got, inboundTraceID)
	}
	if got := eventToProto(providers.Event{Type: providers.EventStarted, TraceID: inboundTraceID}).GetTraceId(); got != inboundTraceID {
		t.Errorf("Event.trace_id = %q, want %q", got, inboundTraceID)
	}
}
