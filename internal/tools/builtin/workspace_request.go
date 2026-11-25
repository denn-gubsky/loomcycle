package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// Bounds on a request op besides the body's (workspace.MaxRequestBody).
const (
	maxRequestPath    = 8 << 10
	maxRequestHeaders = 64
	// requestHeadRoom is what the answer may carry besides its body: the
	// status line and headers. The tunnel's output bound is the body's
	// bound plus this and one chunk (the bound is checked a chunk at a
	// time), so an answer of endless headers ends the op.
	requestHeadRoom = 64 << 10
	// requestEndWait is how long a whole answer waits for the server to
	// close the connection before the tunnel is cut.
	requestEndWait = time.Second
)

// requestOwnHeaders are the headers the runtime writes: the framing and the
// connection's life are the tunnel's, not the caller's.
var requestOwnHeaders = map[string]bool{"Content-Length": true, "Transfer-Encoding": true, "Connection": true, "Upgrade": true, "Te": true, "Trailer": true, "Keep-Alive": true}

// requestAnswer is a request op's result. The body is text when it is
// UTF-8, else base64; it is cut at workspace.MaxRequestBody.
type requestAnswer struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       *string           `json:"body,omitempty"`
	BodyBase64 *string           `json:"body_base64,omitempty"`
	Truncated  bool              `json:"truncated,omitempty"`
}

// request sends one HTTP request to a port of the run's own workspace (RFC
// GW §5.3, D26: single requests, no held socket) over a tunnel the worker
// opens to it, and returns the answer as it came: a redirect is not
// followed. The worker reaches only a port a running server of the
// workspace declared. Nothing is added to the request but Host (localhost
// and the port, unless the caller sets one) and Connection: close; no
// credential of the run or the user is in it. The answer is untrusted tool
// output.
func (w *Workspace) request(ctx context.Context, row store.WorkspaceRow, in workspaceInput) tools.Result {
	req, bad := buildWorkspaceRequest(in)
	if bad != nil {
		return *bad
	}
	var raw bytes.Buffer
	if err := req.Write(&raw); err != nil {
		return workspaceRefusal("invalid_argument", "the request cannot be written: "+err.Error())
	}
	// A server that never answers holds the call no longer than a command.
	timeout, _ := w.timeout(workspaceInput{})
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t, err := w.Manager.Tunnel(ctx, row, in.Port, int64(raw.Len()), workspace.MaxRequestBody+requestHeadRoom+workspace.ChunkSize)
	if err != nil {
		return workspaceRequestError(err, in)
	}
	defer t.Close()
	// The request is written whole and the input left open: some servers
	// read a half-closed connection as the client gone and drop the answer.
	// Connection: close makes the server end it after the answer.
	if _, err := t.Write(raw.Bytes()); err != nil {
		return workspaceRequestFailure(t, err, in)
	}
	resp, err := http.ReadResponse(bufio.NewReader(t), req)
	if err != nil {
		return workspaceRequestFailure(t, err, in)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, workspace.MaxRequestBody+1))
	out := requestAnswer{Status: resp.StatusCode, Headers: map[string]string{}}
	switch {
	case err == nil, errors.Is(err, io.ErrUnexpectedEOF):
	case dispatch.ClassOf(err) == workspace.ClassOutputTooLarge:
		// The tunnel's own bound cut a long body: the head was read, so it
		// is the body that is cut.
		out.Truncated = true
	default:
		return workspaceRequestFailure(t, err, in)
	}
	if len(body) > workspace.MaxRequestBody {
		body, out.Truncated = body[:workspace.MaxRequestBody], true
	}
	if !out.Truncated {
		// The answer is whole: let the op end as the server closes (it was
		// asked to), so it is counted as done rather than cancelled. A
		// server that keeps the connection is cut after a moment.
		_ = t.CloseWrite()
		select {
		case <-t.Done():
		case <-time.After(requestEndWait):
		}
	}
	for name, vs := range resp.Header {
		out.Headers[name] = strings.Join(vs, ", ")
	}
	if utf8.Valid(body) {
		s := string(body)
		out.Body = &s
	} else {
		s := base64.StdEncoding.EncodeToString(body)
		out.BodyBase64 = &s
	}
	return unescapedResult(out)
}

// buildWorkspaceRequest checks the call's request and builds it.
func buildWorkspaceRequest(in workspaceInput) (*http.Request, *tools.Result) {
	bad := func(detail string) (*http.Request, *tools.Result) {
		r := workspaceRefusal("invalid_argument", detail)
		return nil, &r
	}
	if in.Port < 1 || in.Port > 65535 {
		return bad("request needs port, from 1 to 65535: the port a server of the workspace declared at start")
	}
	if !validMethod(in.Method) {
		return bad("request needs method, an HTTP method such as GET or POST")
	}
	if in.Path == "" || in.Path[0] != '/' || len(in.Path) > maxRequestPath {
		return bad(fmt.Sprintf("request needs path: the request target, starting with / (at most %d bytes)", maxRequestPath))
	}
	for i := 0; i < len(in.Path); i++ {
		if c := in.Path[i]; c <= ' ' || c == 0x7f {
			return bad("path holds a space or a control character; escape it (%20)")
		}
	}
	u, err := url.ParseRequestURI(in.Path)
	if err != nil || u.Host != "" || u.Scheme != "" {
		return bad("path is not a request target such as /index.html?q=1")
	}
	if len(in.Body) > workspace.MaxRequestBody {
		return bad(fmt.Sprintf("body is over %d bytes", workspace.MaxRequestBody))
	}
	if len(in.Headers) > maxRequestHeaders {
		return bad(fmt.Sprintf("at most %d headers", maxRequestHeaders))
	}
	req := &http.Request{Method: in.Method, URL: u, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Host: "localhost:" + strconv.Itoa(in.Port), Close: true}
	names := make([]string, 0, len(in.Headers))
	for name := range in.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := in.Headers[name]
		if !validHeaderName(name) {
			return bad(fmt.Sprintf("header %q is not a header name", name))
		}
		canon := http.CanonicalHeaderKey(name)
		if requestOwnHeaders[canon] {
			return bad(fmt.Sprintf("header %s is the runtime's to set", canon))
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return bad(fmt.Sprintf("header %s holds a line break", canon))
		}
		if canon == "Host" {
			req.Host = value
			continue
		}
		req.Header.Set(canon, value)
	}
	if in.Body != "" {
		req.Body = io.NopCloser(strings.NewReader(in.Body))
		req.ContentLength = int64(len(in.Body))
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "loomcycle-workspace")
	}
	return req, nil
}

// validMethod: 1 to 16 upper-case letters.
func validMethod(m string) bool {
	if m == "" || len(m) > 16 {
		return false
	}
	for i := 0; i < len(m); i++ {
		if m[i] < 'A' || m[i] > 'Z' {
			return false
		}
	}
	return true
}

// validHeaderName: an HTTP token of at most 128 bytes.
func validHeaderName(n string) bool {
	if n == "" || len(n) > 128 {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// workspaceRequestFailure is a request that broke after the port was
// reached: the op's own end says why when it has one.
func workspaceRequestFailure(t *dispatch.Tunnel, err error, in workspaceInput) tools.Result {
	_ = t.Close()
	if opErr := t.Err(); opErr != nil && !errors.Is(opErr, context.Canceled) {
		return workspaceRequestError(opErr, in)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return workspaceRefusal("timeout", fmt.Sprintf("port %d did not answer in time", in.Port))
	}
	return workspaceRefusal("bad_response", fmt.Sprintf("port %d did not answer with HTTP: %v", in.Port, err))
}

// workspaceRequestError is workspaceError with the tunnel's classes
// explained.
func workspaceRequestError(err error, in workspaceInput) tools.Result {
	switch dispatch.ClassOf(err) {
	case workspace.ClassPortNotServed:
		return workspaceRefusal(workspace.ClassPortNotServed, fmt.Sprintf("no running server of workspace %q serves port %d; start one with port %d (logs lists them)", in.Name, in.Port, in.Port))
	case workspace.ClassPortUnreachable:
		return workspaceRefusal(workspace.ClassPortUnreachable, fmt.Sprintf("a server of workspace %q declared port %d but nothing answers on it yet; read its logs", in.Name, in.Port))
	case workspace.ClassOutputTooLarge:
		return workspaceRefusal(workspace.ClassOutputTooLarge, fmt.Sprintf("the answer's status line and headers are over %d bytes", requestHeadRoom))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return workspaceRefusal("timeout", fmt.Sprintf("port %d did not answer in time", in.Port))
	}
	return workspaceError(err, in.Name)
}
