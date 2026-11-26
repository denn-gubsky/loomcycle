package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// git.go — the core's side of a git workspace's traffic (RFC GW §6.1, P11).
// The worker names one of four request shapes, never a URL or a path; the
// core builds the request from the repository URL it holds for the
// workspace, passes a short fixed list of headers, adds the credential
// itself, follows and relays no redirect, and never hands the worker the
// forge's 401, 404 or redirect. A push passes the push policy (gitpush.go)
// before anything is sent.
//
// This file is the decision and the request; it knows nothing of the worker
// protocol, the store or tickets. The caller hands it the grant: the
// repository, the resolved credential and what the operation may do.

// GitShape is one of the four requests a git client makes over smart HTTP.
type GitShape int

const (
	GitAdvertiseUpload  GitShape = iota + 1 // GET info/refs?service=git-upload-pack
	GitAdvertiseReceive                     // GET info/refs?service=git-receive-pack
	GitUploadPack                           // POST git-upload-pack
	GitReceivePack                          // POST git-receive-pack
)

func (s GitShape) String() string {
	switch s {
	case GitAdvertiseUpload:
		return "advertise_upload"
	case GitAdvertiseReceive:
		return "advertise_receive"
	case GitUploadPack:
		return "upload_pack"
	case GitReceivePack:
		return "receive_pack"
	}
	return "unknown"
}

// The defaults for a grant that leaves a bound unset.
const (
	// DefaultMaxPushBytes is workspace.push.max_bytes' default (RFC GW §6.2):
	// go-git holds a whole push in the worker's memory.
	DefaultMaxPushBytes = 64 << 20
	// maxUploadPackBody bounds a fetch's request (its wants and haves).
	maxUploadPackBody = 16 << 20
	// maxAdvertisementSniff bounds what the core reads of an advertisement to
	// find the default branch.
	maxAdvertisementSniff = 64 << 10
)

// ByteBudget is what an operation's answers may still bring in, shared by
// its requests. Safe for concurrent use.
type ByteBudget struct{ left atomic.Int64 }

// NewByteBudget is a budget of n bytes.
func NewByteBudget(n int64) *ByteBudget {
	b := &ByteBudget{}
	b.left.Store(n)
	return b
}

// take spends n and reports whether the budget held it.
func (b *ByteBudget) take(n int) bool { return b.left.Add(-int64(n)) >= 0 }

// GitGrant is what one operation of one workspace may do.
type GitGrant struct {
	WorkspaceID string
	// RepoURL is the repository as the core holds it: https, a host, no user
	// information, query or fragment.
	RepoURL string
	// Username and Token are the resolved credential, sent as Basic auth;
	// an empty Token sends none.
	Username, Token string
	// Shapes are the requests the operation may make.
	Shapes []GitShape
	// Push is the policy a receive-pack request is held to.
	Push PushPolicy
	// MaxPushBytes bounds a push's body; 0 is DefaultMaxPushBytes.
	MaxPushBytes int64
	// ReadBudget bounds the answers' bytes, across the operation; nil for
	// none.
	ReadBudget *ByteBudget
	// OnDefaultBranch is told the forge's default branch (a full ref) when
	// an advertisement names it.
	OnDefaultBranch func(ref string)
	// WatchRef, when set, is a full ref the core looks for in the forge's
	// advertisement as it passes; OnWatchedRef is told when it is there.
	// The forge's own answer decides whether a branch exists, never the
	// worker's word (the owner's Q2).
	WatchRef     string
	OnWatchedRef func()
}

// GitRequest is what the worker described: a shape, its headers and body.
type GitRequest struct {
	Shape         GitShape
	Header        http.Header
	Body          io.Reader
	ContentLength int64
}

// GitOptions configures a GitProxy.
type GitOptions struct {
	// PrivateHostAllowlist is the operator's vouch list for private
	// addresses; every other private, loopback, link-local or metadata
	// address is refused at dial.
	PrivateHostAllowlist []string
	// TLSConfig is the client's TLS configuration; nil for the system roots.
	TLSConfig *tls.Config
	// ResponseHeaderTimeout bounds the wait for the forge's answer. Default
	// 60 s; the caller's context bounds the rest.
	ResponseHeaderTimeout time.Duration

	// dial replaces the guarded dialer, in this package's tests only.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// GitProxy sends git workspaces' requests to their forge. Safe for
// concurrent use.
type GitProxy struct {
	client *http.Client
}

// NewGitProxy builds a GitProxy.
func NewGitProxy(o GitOptions) (*GitProxy, error) {
	dial := o.dial
	if dial == nil {
		dial = netguard.GuardedDialContext(false, o.PrivateHostAllowlist)
	}
	rht := o.ResponseHeaderTimeout
	if rht <= 0 {
		rht = 60 * time.Second
	}
	var tlsConf *tls.Config
	if o.TLSConfig != nil {
		tlsConf = o.TLSConfig.Clone()
	}
	tr := &http.Transport{
		// No environment proxy: a forge credential must not travel through
		// a host nobody vouched for.
		Proxy:                 nil,
		DialContext:           dial,
		TLSClientConfig:       tlsConf,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: rht,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	return &GitProxy{client: &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func gitRefusal(format string, a ...any) *Refusal {
	return &Refusal{Status: http.StatusForbidden, Reason: fmt.Sprintf(format, a...)}
}

// Do sends one request of an operation, or refuses it with a *Refusal before
// any network call (or, for the forge's redirect, 401 and 404, after it).
// Any other error is the transport's and carries no credential.
func (p *GitProxy) Do(ctx context.Context, g GitGrant, r GitRequest) (*Response, error) {
	if !slices.Contains(g.Shapes, r.Shape) {
		return nil, gitRefusal("the git request %s is not one this operation may make", r.Shape)
	}
	target, err := gitTarget(g.RepoURL, r.Shape)
	if err != nil {
		return nil, err
	}
	method, body, length, err := gitBody(g, r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(netguard.WithPinnable(ctx), method, target, body)
	if err != nil {
		return nil, gitRefusal("the git request cannot be built")
	}
	if body != nil && length > 0 {
		req.ContentLength = length
	}
	if req.Header, err = gitRequestHeader(r); err != nil {
		return nil, err
	}
	if g.Token != "" {
		req.SetBasicAuth(g.Username, g.Token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("egress: git %s: the forge could not be reached: %s", r.Shape, msg)
	}
	if ref := gitAnswerRefusal(resp.StatusCode); ref != nil {
		_ = resp.Body.Close()
		return nil, ref
	}
	var out io.ReadCloser = resp.Body
	if g.ReadBudget != nil {
		out = &budgetBody{rc: out, b: g.ReadBudget}
	}
	if g.OnDefaultBranch != nil && (r.Shape == GitAdvertiseUpload || r.Shape == GitAdvertiseReceive) {
		out = &advertSniffer{rc: out, report: g.OnDefaultBranch}
	}
	if g.WatchRef != "" && g.OnWatchedRef != nil && (r.Shape == GitAdvertiseUpload || r.Shape == GitAdvertiseReceive) {
		out = &refWatcher{rc: out, want: g.WatchRef, seen: g.OnWatchedRef}
	}
	return &Response{Status: resp.StatusCode, Header: gitResponseHeader(resp.Header), Body: out}, nil
}

// gitTarget is the URL of shape under the repository.
func gitTarget(repo string, shape GitShape) (string, error) {
	u, err := url.Parse(repo)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", gitRefusal("the workspace's repository URL is not an https URL with a host and no user, query or fragment")
	}
	base := strings.TrimSuffix(u.String(), "/")
	switch shape {
	case GitAdvertiseUpload:
		return base + "/info/refs?service=git-upload-pack", nil
	case GitAdvertiseReceive:
		return base + "/info/refs?service=git-receive-pack", nil
	case GitUploadPack:
		return base + "/git-upload-pack", nil
	case GitReceivePack:
		return base + "/git-receive-pack", nil
	}
	return "", gitRefusal("an unknown git request")
}

// gitBody is the request's method and body. A receive-pack body passes the
// push policy before anything is sent, and is bounded in size; its command
// section is replayed to the forge unchanged ahead of the rest.
func gitBody(g GitGrant, r GitRequest) (string, io.Reader, int64, error) {
	switch r.Shape {
	case GitAdvertiseUpload, GitAdvertiseReceive:
		return http.MethodGet, nil, 0, nil
	case GitUploadPack:
		if r.Body == nil {
			return "", nil, 0, gitRefusal("an upload-pack request with no body")
		}
		if r.ContentLength > maxUploadPackBody {
			return "", nil, 0, gitRefusal("an upload-pack request over %d MiB", maxUploadPackBody>>20)
		}
		return http.MethodPost, &capReader{r: r.Body, left: maxUploadPackBody}, r.ContentLength, nil
	}
	maxBytes := g.MaxPushBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPushBytes
	}
	if r.Body == nil {
		return "", nil, 0, pushRefusal("a receive-pack request with no body")
	}
	if r.ContentLength > maxBytes {
		return "", nil, 0, pushRefusal("the push's size (%d bytes) is over the operator's limit of %d (workspace.push.max_bytes)", r.ContentLength, maxBytes)
	}
	s, err := ReadPushSection(r.Body)
	if err != nil {
		return "", nil, 0, err
	}
	if err := g.Push.Check(s); err != nil {
		return "", nil, 0, err
	}
	body := &capReader{r: io.MultiReader(bytes.NewReader(s.Raw), r.Body), left: maxBytes}
	return http.MethodPost, body, r.ContentLength, nil
}

// errOverPushLimit cuts a body whose length was not declared at the limit:
// the forge then gets an incomplete pack, which it cannot apply.
var errOverPushLimit = errors.New("egress: git: the request body passed its size limit")

type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errOverPushLimit
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return 0, errOverPushLimit
	}
	return n, err
}

// The content type each POST must carry, and what the core passes of the
// worker's headers: these alone.
var gitContentType = map[GitShape]string{
	GitUploadPack:  "application/x-git-upload-pack-request",
	GitReceivePack: "application/x-git-receive-pack-request",
}

// gitRequestHeader is the request's header: the shape's content type, the
// worker's Accept, a gzip encoding on a fetch's request, and the core's own
// User-Agent. Nothing else of the worker's passes: no Git-Protocol (the
// forge then speaks v0, the protocol the push reader reads), no
// Authorization, Cookie or forwarding header.
func gitRequestHeader(r GitRequest) (http.Header, error) {
	h := http.Header{}
	in := r.Header
	if in == nil {
		in = http.Header{}
	}
	if want, ok := gitContentType[r.Shape]; ok {
		if in.Get("Content-Type") != want {
			return nil, gitRefusal("the git request's content type is not %s", want)
		}
		h.Set("Content-Type", want)
	}
	switch enc := in.Get("Content-Encoding"); {
	case enc == "":
	case r.Shape == GitUploadPack && enc == "gzip":
		h.Set("Content-Encoding", "gzip")
	case r.Shape == GitReceivePack:
		return nil, pushRefusal("a push in an encoding (%q): the core reads its ref updates as they are", truncateName(enc))
	default:
		return nil, gitRefusal("the git request's encoding %q is not one the core passes", truncateName(enc))
	}
	if a := in.Get("Accept"); a != "" && len(a) <= 256 && isPrintableASCII(a) {
		h.Set("Accept", a)
	}
	h.Set("User-Agent", "loomcycle-workspace")
	return h, nil
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// gitAnswerRefusal is what the worker is told in place of the forge's own
// answer: a redirect, an authentication challenge, a refusal or a 404 is a
// 403 with one line, its body and headers dropped. go-git keeps a response
// body only on 401, 403 and 404, so a 403 carries the line to the agent.
func gitAnswerRefusal(status int) *Refusal {
	switch {
	case status >= 300 && status < 400:
		return gitRefusal("the forge redirected the request; the core follows no redirect for a workspace")
	case status == http.StatusUnauthorized || status == http.StatusProxyAuthRequired:
		return gitRefusal("the forge refused the credential")
	case status == http.StatusForbidden:
		return gitRefusal("the forge refused access with this credential")
	case status == http.StatusNotFound:
		return gitRefusal("the repository was not found, or is not readable with this credential")
	}
	return nil
}

// gitResponseHeader is the forge's header without hop-by-hop, cookie and
// authentication headers.
func gitResponseHeader(in http.Header) http.Header {
	h := in.Clone()
	stripHopByHop(h)
	for _, k := range responseDeny {
		h.Del(k)
	}
	h.Del("Proxy-Authenticate")
	return h
}

// budgetBody ends an answer at the operation's read budget.
type budgetBody struct {
	rc io.ReadCloser
	b  *ByteBudget
}

func (b *budgetBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && !b.b.take(n) {
		return 0, ErrByteBudget
	}
	return n, err
}

func (b *budgetBody) Close() error { return b.rc.Close() }

// advertSniffer reads the default branch from an advertisement as it passes,
// from the symref capability of its first ref line; it reads at most
// maxAdvertisementSniff bytes for it and changes nothing.
type advertSniffer struct {
	rc     io.ReadCloser
	report func(string)
	buf    []byte
	done   bool
}

func (a *advertSniffer) Read(p []byte) (int, error) {
	n, err := a.rc.Read(p)
	if !a.done && n > 0 {
		a.buf = append(a.buf, p[:n]...)
		if ref, ok, finished := defaultBranchOf(a.buf); finished || len(a.buf) >= maxAdvertisementSniff {
			a.done = true
			a.buf = nil
			if ok {
				a.report(ref)
			}
		}
	}
	return n, err
}

func (a *advertSniffer) Close() error { return a.rc.Close() }

// defaultBranchOf reads an advertisement's head: an optional "# service=…"
// line and its flush, then the first ref line, whose capabilities follow a
// NUL. finished is false until that line is whole.
func defaultBranchOf(b []byte) (ref string, ok, finished bool) {
	for {
		if len(b) < 4 {
			return "", false, false
		}
		if !isLowerHex(string(b[:4])) {
			return "", false, true
		}
		var n int
		_, _ = fmt.Sscanf(string(b[:4]), "%04x", &n)
		if n == 0 {
			b = b[4:]
			continue
		}
		if n < 4 || len(b) < n {
			return "", false, n < 4
		}
		line := string(b[4:n])
		if strings.HasPrefix(line, "# service=") {
			b = b[n:]
			continue
		}
		_, capText, has := strings.Cut(strings.TrimSuffix(line, "\n"), "\x00")
		if !has {
			return "", false, true
		}
		for _, c := range strings.Fields(capText) {
			if target, found := strings.CutPrefix(c, "symref=HEAD:"); found && checkRefName(target) == nil && strings.HasPrefix(target, "refs/heads/") {
				return target, true, true
			}
		}
		return "", false, true
	}
}

// refWatcher reads an advertisement's pkt-lines as they pass and reports
// when one advertises the watched ref. It holds at most one line, and stops
// at the flush that ends the refs. It changes nothing.
type refWatcher struct {
	rc   io.ReadCloser
	want string
	seen func()
	buf  []byte
	// flushes counts the flushes passed: the service line's comes first.
	flushes int
	done    bool
}

func (w *refWatcher) Read(p []byte) (int, error) {
	n, err := w.rc.Read(p)
	if !w.done && n > 0 {
		w.buf = append(w.buf, p[:n]...)
		w.scan()
	}
	return n, err
}

func (w *refWatcher) scan() {
	for !w.done && len(w.buf) >= 4 {
		if !isLowerHex(string(w.buf[:4])) {
			w.done = true
			return
		}
		var n int
		_, _ = fmt.Sscanf(string(w.buf[:4]), "%04x", &n)
		if n == 0 {
			w.buf = w.buf[4:]
			w.flushes++
			// A smart-HTTP advertisement is "# service" and a flush, then
			// the refs and a flush; without the service line, one flush.
			if w.flushes >= 2 {
				w.done = true
			}
			continue
		}
		if n < 4 || n > maxPktLen {
			w.done = true
			return
		}
		if len(w.buf) < n {
			return
		}
		line := string(w.buf[4:n])
		w.buf = w.buf[n:]
		if strings.HasPrefix(line, "# service=") {
			continue
		}
		if w.flushes == 0 {
			// No service line: this advertisement's refs end at the first
			// flush.
			w.flushes = 1
		}
		line, _, _ = strings.Cut(strings.TrimSuffix(line, "\n"), "\x00")
		if _, name, ok := strings.Cut(line, " "); ok && name == w.want {
			w.done = true
			w.buf = nil
			w.seen()
		}
	}
}

func (w *refWatcher) Close() error { return w.rc.Close() }
