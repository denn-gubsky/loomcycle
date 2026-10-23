package gitsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing/transport"
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
	// maxWrite bounds the request-body bytes of the whole call (a push's
	// pack, a fetch's wants and haves). Zero leaves them uncounted, as a
	// one-commit Fetch has always run.
	maxWrite int64
	wrote    atomic.Int64
	// writeOver is set when a request body passed maxWrite.
	writeOver atomic.Bool
	// status is the last error status (400 and up) a response of the fetch
	// had, for the detail of a refusal (refusalDetail).
	status atomic.Int32
	// packRefused points at the store's flag for a pack turned away over an
	// object's size (packguard.go).
	packRefused *atomic.Bool
}

var (
	errOverLimit      = errors.New("gitsource: fetch passed its byte limit")
	errWriteOverLimit = errors.New("gitsource: the request passed its upload byte limit")
)

var installOnce sync.Once

// InstallTransport makes this package's dispatcher the process's client for
// git over https, and replaces the library's clients for every other scheme
// (http, file, git, ssh) with one that refuses. The library installs those
// process-wide by default; a repository's own config names its remotes, so
// a URL from it must not be able to reach a local path, a plain-text
// protocol or an ssh agent. Nothing else in loomcycle uses the library's
// clients. Idempotent.
func InstallTransport() { installTransport() }

func installTransport() {
	installOnce.Do(func() {
		for _, scheme := range []string{"http", "file", "git", "ssh"} {
			client.InstallProtocol(scheme, refusingTransport{scheme: scheme})
		}
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
	if st.maxWrite > 0 && r.Body != nil && r.Body != http.NoBody {
		r = r.Clone(r.Context())
		r.Body = &countedUpload{ReadCloser: r.Body, st: st}
	}
	resp, err := st.rt.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		st.status.Store(int32(resp.StatusCode))
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

// countedUpload counts a request body against the call's upload limit and
// fails the read that passes it, which stops the request.
type countedUpload struct {
	io.ReadCloser
	st *call
}

func (b *countedUpload) Read(p []byte) (int, error) {
	if b.st.writeOver.Load() {
		return 0, errWriteOverLimit
	}
	n, err := b.ReadCloser.Read(p)
	if b.st.wrote.Add(int64(n)) > b.st.maxWrite {
		b.st.writeOver.Store(true)
		return 0, errWriteOverLimit
	}
	return n, err
}

// refusingTransport is the client for a scheme loomcycle does not speak.
type refusingTransport struct{ scheme string }

func (t refusingTransport) NewUploadPackSession(*transport.Endpoint, transport.AuthMethod) (transport.UploadPackSession, error) {
	return nil, fmt.Errorf("gitsource: %s:// is not allowed; only https is", t.scheme)
}

func (t refusingTransport) NewReceivePackSession(*transport.Endpoint, transport.AuthMethod) (transport.ReceivePackSession, error) {
	return nil, fmt.Errorf("gitsource: %s:// is not allowed; only https is", t.scheme)
}

// withCall is for tests of the dispatcher alone.
func withCall(ctx context.Context, st *call) context.Context {
	return context.WithValue(ctx, callKey{}, st)
}
