package grpc

import (
	"context"
	"log/slog"
	"runtime/debug"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpc-go, unlike net/http, does not recover a panic in a handler: it ends
// the process, and with it every run on the instance. A gRPC handler runs
// more than its own code — a run started over gRPC renders its prompt's tool
// injections (a page fetched, a document read) and runs its loop on the
// handler's goroutine — so a bug any of those meets would take the instance
// down. These interceptors, chained outside authentication, answer such a
// call with Internal, as net/http answers with a closed connection, and log
// the panic for the operator. A tool call's own panic is recovered before it
// gets here, by the dispatcher, into a failed call the run goes on from.

const panicMessage = "internal error"

// RecoverUnaryInterceptor recovers a panic in a unary handler into an
// Internal status.
func RecoverUnaryInterceptor() googlegrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *googlegrpc.UnaryServerInfo, handler googlegrpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if p := recover(); p != nil {
				logHandlerPanic(ctx, info.FullMethod, p)
				resp, err = nil, status.Error(codes.Internal, panicMessage)
			}
		}()
		return handler(ctx, req)
	}
}

// RecoverStreamInterceptor recovers a panic in a streaming handler into an
// Internal status that ends the stream.
func RecoverStreamInterceptor() googlegrpc.StreamServerInterceptor {
	return func(srv any, ss googlegrpc.ServerStream, info *googlegrpc.StreamServerInfo, handler googlegrpc.StreamHandler) (err error) {
		defer func() {
			if p := recover(); p != nil {
				logHandlerPanic(ss.Context(), info.FullMethod, p)
				err = status.Error(codes.Internal, panicMessage)
			}
		}()
		return handler(srv, ss)
	}
}

func logHandlerPanic(ctx context.Context, method string, p any) {
	slog.ErrorContext(ctx, "grpc: handler panicked", "method", method, "panic", p, "stack", string(debug.Stack()))
}
