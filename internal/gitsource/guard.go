package gitsource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"

	"github.com/go-git/go-git/v5/storage/filesystem"
)

// The pieces of a fetch that a repository tool reuses: one network operation
// bound to one host with byte limits, the pack check in front of a store, and
// the closed list of failure classes. Fetch uses the same pieces; a caller of
// these has the same guarantees for clone, fetch, pull and push.

// Guard is what one network operation may do.
type Guard struct {
	// Host is the only host the operation may talk to, as HostKey gives it.
	Host string
	// Transport performs the requests: the operator's network rules.
	Transport http.RoundTripper
	// MaxReadBytes bounds the response bytes of the whole operation.
	MaxReadBytes int64
	// MaxWriteBytes bounds the request-body bytes (a push's pack, a fetch's
	// negotiation). Zero leaves them uncounted.
	MaxWriteBytes int64
}

// ErrUploadTooLarge is a push whose pack passed Guard.MaxWriteBytes.
var ErrUploadTooLarge = errors.New("upload_too_large")

// Session is one bound operation's state, for classifying its failure.
type Session struct {
	st *call
}

// Bind returns ctx carrying the guard: every request the library makes with
// it goes through the dispatcher, to Host only, over https only, counted. A
// request made with a context that was not bound is refused.
func Bind(ctx context.Context, g Guard) (context.Context, *Session, error) {
	if g.Host == "" || g.Transport == nil || g.MaxReadBytes <= 0 {
		return ctx, nil, fmt.Errorf("%w: a host, a transport and a read limit are required", ErrInvalidRequest)
	}
	installTransport()
	st := &call{rt: g.Transport, host: g.Host, max: g.MaxReadBytes, maxWrite: g.MaxWriteBytes, packRefused: new(atomic.Bool)}
	return context.WithValue(ctx, callKey{}, st), &Session{st: st}, nil
}

// WatchPacks points the session at a store's refusal flag, so a pack turned
// away for an object's size is classified as too large.
func (s *Session) WatchPacks(g *GuardedStorage) { s.st.packRefused = &g.refused }

// Classify turns an error of the bound operation into one of the package's
// classes, keeping the original as detail for the operator's log. A nil
// error stays nil.
func (s *Session) Classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errWriteOverLimit) || s.st.writeOver.Load() {
		return fmt.Errorf("%w: the upload passed %d bytes", ErrUploadTooLarge, s.st.maxWrite)
	}
	return classify(ctx, s.st, err)
}

// HostKey is a URL's host as the dispatcher compares it: lower case, and
// without the port when it is https's own.
func HostKey(u *url.URL) string { return hostKey(u) }

// ValidateURL checks a repository URL as Fetch does — https, a host, no user,
// password, query, fragment, encoding, space, control character or
// backslash — and returns its host key.
func ValidateURL(raw string) (host string, err error) {
	bad := func(format string, a ...any) (string, error) {
		return "", fmt.Errorf("%w: "+format, append([]any{ErrInvalidRequest}, a...)...)
	}
	for _, c := range raw {
		if c <= 0x20 || c == 0x7f || c == '\\' || c == '%' {
			return bad("the repository URL has a space, a control character, a backslash or a percent-encoding")
		}
	}
	u, perr := url.Parse(raw)
	if perr != nil || u.Scheme != "https" || u.Host == "" {
		return bad("the repository URL must be an https URL")
	}
	if u.User != nil {
		return bad("the repository URL must not carry a user or a password")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return bad("the repository URL must not carry a query or a fragment")
	}
	return hostKey(u), nil
}

// GuardedStorage is a store that checks every pack before taking it (see
// packguard.go): no object over the size limit, and no more inflated bytes in
// all than the operation's read limit allows.
type GuardedStorage struct {
	guardedStorage
}

// NewGuardedStorage wraps base. spoolDir is a directory of the runtime's own,
// outside anything an agent can reach, where an incoming pack is spooled for
// the check; it must exist. ctx bounds the checks.
func NewGuardedStorage(ctx context.Context, base *filesystem.Storage, spoolDir string, maxObjectBytes, maxReadBytes int64) *GuardedStorage {
	return &GuardedStorage{guardedStorage{
		Storage:   base,
		ctx:       ctx,
		spoolDir:  spoolDir,
		maxObject: maxObjectBytes,
		inflated:  inflatedBudget(maxReadBytes),
	}}
}
