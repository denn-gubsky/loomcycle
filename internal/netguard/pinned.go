package netguard

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
)

// A request can carry a credential that may be sent only to the URLs it is
// restricted to. The request itself was checked against those URLs; a
// redirect is the server choosing another destination, and Go forwards most
// headers to it (Authorization only within the same domain, a custom header
// such as X-API-Key anywhere). So a request whose context is pinned follows no
// redirect at all.
//
// The caller pins the context it will send the request with, before it
// resolves the credentials for that request; whatever hands out a restricted
// credential marks it (Pin); the client's redirect policy reads it.

type pinKey struct{}

// WithPinnable returns a context on which Pin can record that the request it
// carries must not be redirected. Credentials resolved with this context and
// the request sent with it see the same record.
func WithPinnable(ctx context.Context) context.Context {
	if _, ok := ctx.Value(pinKey{}).(*atomic.Bool); ok {
		return ctx
	}
	return context.WithValue(ctx, pinKey{}, new(atomic.Bool))
}

// Pin records that the request sent with ctx carries a credential restricted
// to its destination. It reports false when ctx was not made by WithPinnable:
// the caller then has no way to keep the request from being redirected.
func Pin(ctx context.Context) bool {
	b, ok := ctx.Value(pinKey{}).(*atomic.Bool)
	if ok {
		b.Store(true)
	}
	return ok
}

// Pinned reports whether Pin was called on ctx.
func Pinned(ctx context.Context) bool {
	b, ok := ctx.Value(pinKey{}).(*atomic.Bool)
	return ok && b.Load()
}

// ErrPinnedRedirect is the error a pinned request's redirect stops with.
var ErrPinnedRedirect = errors.New("not following a redirect: the request carries a credential restricted to the URL it was sent to")

// RedirectPolicy is an http.Client CheckRedirect that stops after max hops
// and refuses any hop of a pinned request.
func RedirectPolicy(max int) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if Pinned(req.Context()) {
			return ErrPinnedRedirect
		}
		if len(via) >= max {
			return errors.New("too many redirects")
		}
		return nil
	}
}

// HonorPins returns a shallow copy of c whose redirect policy refuses every
// hop of a pinned request and otherwise does what c's own policy did (Go's
// default of ten hops when c had none). For a client the caller did not build
// with RedirectPolicy: one injected by a test or an embedder.
func HonorPins(c *http.Client) *http.Client {
	if c == nil {
		c = &http.Client{}
	}
	out := *c
	inner := c.CheckRedirect
	out.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if Pinned(req.Context()) {
			return ErrPinnedRedirect
		}
		if inner != nil {
			return inner(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &out
}
