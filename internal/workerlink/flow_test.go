package workerlink

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// rawCodec carries []byte as it is, so the test needs no generated service.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec: %T", v)
	}
	return *b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec: %T", v)
	}
	*b = append((*b)[:0], data...)
	return nil
}

func (rawCodec) Name() string { return "workerlink-test-raw" }

// The windows are in force on a real connection: a sender whose reader has
// stopped gets one stream window in flight (gRPC's own default would stop it
// near 64 KiB, measured at 192 KiB) and no more, so a stalled reader holds a
// bounded amount of its data.
func TestFlowControl_AStalledReaderHoldsOneStreamWindow(t *testing.T) {
	const chunk = 32 << 10
	var sent atomic.Int64
	desc := grpc.ServiceDesc{
		ServiceName: "workerlink.test.Flow",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{StreamName: "Pour", ServerStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				var in []byte
				if err := stream.RecvMsg(&in); err != nil {
					return err
				}
				for stream.Context().Err() == nil {
					// A fresh buffer per send: gRPC may still hold the last.
					b := make([]byte, chunk)
					if err := stream.SendMsg(&b); err != nil {
						return err
					}
					sent.Add(chunk)
				}
				return nil
			}}},
	}
	srv := grpc.NewServer(append(ServerOptions(), grpc.ForceServerCodec(rawCodec{}))...)
	srv.RegisterService(&desc, struct{}{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(l.Addr().String(), append(DialOptions(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := conn.NewStream(ctx, &desc.Streams[0], "/workerlink.test.Flow/Pour")
	if err != nil {
		t.Fatal(err)
	}
	start := []byte("go")
	if err := stream.SendMsg(&start); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	// Read nothing; wait for the sender to stall.
	deadline := time.Now().Add(10 * time.Second)
	for last := int64(-1); ; {
		time.Sleep(300 * time.Millisecond)
		n := sent.Load()
		if n == last && n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sender never stalled (%d bytes sent)", n)
		}
		last = n
	}
	// The window, gRPC's own per-stream write quota (64 KiB) and a message
	// or two in hand.
	if n, low, high := sent.Load(), int64(StreamWindow*3/4), int64(StreamWindow+256<<10); n < low || n > high {
		t.Fatalf("a stalled reader let the sender put %d bytes in flight, want %d to %d", n, low, high)
	}
}
