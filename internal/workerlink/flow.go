package workerlink

import "google.golang.org/grpc"

// The worker endpoint's HTTP/2 flow-control windows (RFC GW §8), on both
// ends. They bound what one stream, and one worker's connection, can have in
// flight unread: a slow reader on either side holds at most this much of
// the other's data. A static window also turns off gRPC's bandwidth-delay
// estimate, which would otherwise grow a busy connection's window to 16 MiB.
const (
	StreamWindow = 1 << 20
	ConnWindow   = 8 << 20
)

// ServerOptions are the options the worker endpoint's gRPC server is built
// with.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.InitialWindowSize(StreamWindow),
		grpc.InitialConnWindowSize(ConnWindow),
	}
}

// DialOptions are the flow-control options a worker dials the endpoint with;
// the caller adds the transport credentials.
func DialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithInitialWindowSize(StreamWindow),
		grpc.WithInitialConnWindowSize(ConnWindow),
	}
}
