package gitsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// The library keeps one HTTP client per protocol for the whole process, so
// the client installed here cannot belong to one fetch. It is a dispatcher:
// what does belong to a fetch (its transport, its host, its byte count)
// travels on the request's context, which the library passes through, and a
// request that carries none is refused.

type callKey struct{}

// call is one fetch's state.
type call struct {
	rt   http.RoundTripper
	host string // the only host this fetch may talk to, as hostKey gives it
	max  int64
	read atomic.Int64
	over atomic.Bool
	// packRefused points at the store's flag for a pack turned away over an
	// object's size (packguard.go).
	packRefused *atomic.Bool
}

var errOverLimit = errors.New("gitsource: fetch passed its byte limit")

var installOnce sync.Once

// installTransport makes this package's dispatcher the process's client for
// git over https. Nothing else in loomcycle uses the library's client.
func installTransport() {
	installOnce.Do(func() {
		client.InstallProtocol("https", githttp.NewClient(&http.Client{
			Transport: dispatcher{},
			// No redirect is followed, not even to the same host. A forge
			// redirects a renamed or transferred repository to its new path,
			// and following that would send the token to a repository the
			// caller did not name and its credential may not cover.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("gitsource: the server answered with a redirect, which is not followed")
			},
		}))
	})
}

type dispatcher struct{}

func (dispatcher) RoundTrip(r *http.Request) (*http.Response, error) {
	st, _ := r.Context().Value(callKey{}).(*call)
	if st == nil {
		return nil, errors.New("gitsource: request outside a fetch")
	}
	if r.URL.Scheme != "https" || hostKey(r.URL) != st.host {
		return nil, fmt.Errorf("gitsource: request to %q refused; this fetch talks to %q over https only", r.URL.Host, st.host)
	}
	resp, err := st.rt.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &countedBody{ReadCloser: resp.Body, st: st}
	return resp, nil
}

// countedBody counts a response against the fetch's byte limit and fails the
// read that passes it, which stops the fetch where it is.
type countedBody struct {
	io.ReadCloser
	st *call
}

func (b *countedBody) Read(p []byte) (int, error) {
	if b.st.over.Load() {
		return 0, errOverLimit
	}
	n, err := b.ReadCloser.Read(p)
	if b.st.read.Add(int64(n)) > b.st.max {
		// The bytes of this read are withheld, not handed over with the
		// error: a reader that has what it needs by now would use them and
		// never look at the error. Fetch also checks the flag itself, for
		// the same reason.
		b.st.over.Store(true)
		return 0, errOverLimit
	}
	return n, err
}

// withCall is for tests of the dispatcher alone.
func withCall(ctx context.Context, st *call) context.Context {
	return context.WithValue(ctx, callKey{}, st)
}
