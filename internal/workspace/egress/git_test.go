package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// forge is a TLS server standing in for a git forge: it records every request
// it receives (method, path, query, headers, body) and answers with its
// handler, else 200 "ok".
type forge struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []forgeReq
	handler func(w http.ResponseWriter, r *http.Request, body []byte)
}

type forgeReq struct {
	method, path, query string
	header              http.Header
	body                []byte
}

func newForge(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) *forge {
	t.Helper()
	f := &forge{handler: h}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, forgeReq{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		if f.handler != nil {
			f.handler(w, r, body)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *forge) requests() []forgeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forgeReq(nil), f.reqs...)
}

// gitProxyTo is a GitProxy whose every dial lands on f, whatever host the
// grant's URL names.
func gitProxyTo(t *testing.T, f *forge) *GitProxy {
	t.Helper()
	addr := f.Listener.Addr().String()
	p, err := NewGitProxy(GitOptions{
		TLSConfig: &tls.Config{RootCAs: testRoots(t), ServerName: "example.com", MinVersion: tls.VersionTLS12},
		dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const gitToken = "tok-forge-4c19-never-in-the-worker"

func gitGrant() GitGrant {
	return GitGrant{
		WorkspaceID:  "ws1",
		RepoURL:      "https://forge.example/acme/app.git",
		Username:     "x-access-token",
		Token:        gitToken,
		Shapes:       []GitShape{GitAdvertiseUpload, GitUploadPack, GitAdvertiseReceive, GitReceivePack},
		Push:         ownPolicy,
		MaxPushBytes: 1 << 20,
	}
}

func receive(body string) GitRequest {
	return GitRequest{
		Shape:  GitReceivePack,
		Header: http.Header{"Content-Type": {"application/x-git-receive-pack-request"}},
		Body:   strings.NewReader(body), ContentLength: int64(len(body)),
	}
}

func gitDo(t *testing.T, p *GitProxy, g GitGrant, r GitRequest) (*Response, error) {
	t.Helper()
	resp, err := p.Do(context.Background(), g, r)
	if err == nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	return resp, err
}

func receivePackPosts(f *forge) int {
	n := 0
	for _, r := range f.requests() {
		if r.method == http.MethodPost && strings.HasSuffix(r.path, "/git-receive-pack") {
			n++
		}
	}
	return n
}

// A refused push sends nothing to the forge (RFC GW §6.2, §11 "Push policy").
func TestGitProxy_ARefusedPushSendsNothingToTheForge(t *testing.T) {
	f := newForge(t, nil)
	p := gitProxyTo(t, f)
	for name, body := range map[string]string{
		"the default branch": section(cmd(oldID, newID, "refs/heads/main")),
		"another branch":     section(cmd(oldID, newID, "refs/heads/feature")),
		"a deletion":         section(cmd(oldID, zeroID, "refs/heads/loom/fix-1")),
		"a tag":              section(cmd(zeroID, newID, "refs/tags/v1")),
		"mixed":              section(cmd(oldID, newID, "refs/heads/loom/fix-1"), cmd(oldID, newID, "refs/heads/feature")),
		"push options":       pkt(cmd(oldID, newID, "refs/heads/loom/fix-1")+"\x00report-status push-options\n") + "0000",
		"a shallow line":     pkt("shallow "+oldID+"\n") + section(cmd(oldID, newID, "refs/heads/loom/fix-1")),
		"a huge pkt-line":    "ffff" + strings.Repeat("a", 70000),
		"garbage":            "not a pkt-line at all",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := gitDo(t, p, gitGrant(), receive(body+"PACK"))
			wantPushRefusal(t, err, "")
			if n := receivePackPosts(f); n != 0 {
				t.Fatalf("the forge received %d receive-pack requests", n)
			}
		})
	}
}

// An accepted push reaches the forge with its body byte for byte, the
// credential added by the core, and only the request the core built.
func TestGitProxy_AnAcceptedPushReachesTheForgeUnchanged(t *testing.T) {
	f := newForge(t, nil)
	p := gitProxyTo(t, f)
	body := section(cmd(oldID, newID, "refs/heads/loom/fix-1")) + "PACK\x00\x00\x00\x02" + strings.Repeat("p", 100000)
	req := receive(body)
	req.Header.Set("Authorization", "Bearer from-the-worker")
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Cookie", "a=b")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if _, err := gitDo(t, p, gitGrant(), req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("the forge received %d requests", len(reqs))
	}
	got := reqs[0]
	if got.path != "/acme/app.git/git-receive-pack" || got.query != "" || got.method != http.MethodPost {
		t.Fatalf("request %s %s?%s", got.method, got.path, got.query)
	}
	if string(got.body) != body {
		t.Fatalf("the body changed on the way (%d bytes, want %d)", len(got.body), len(body))
	}
	if u, pw, ok := (&http.Request{Header: got.header}).BasicAuth(); !ok || u != "x-access-token" || pw != gitToken {
		t.Fatalf("authorization = %q", got.header.Get("Authorization"))
	}
	for _, h := range []string{"Git-Protocol", "Cookie", "X-Forwarded-For"} {
		if got.header.Get(h) != "" {
			t.Errorf("%s reached the forge", h)
		}
	}
}

// The four shapes build their URLs from the grant's repository; a shape the
// grant does not hold is refused before any network call.
func TestGitProxy_BuildsEachShapeFromTheGrantAndRefusesOneNotGranted(t *testing.T) {
	f := newForge(t, nil)
	p := gitProxyTo(t, f)
	want := map[GitShape]string{
		GitAdvertiseUpload:  "GET /acme/app.git/info/refs?service=git-upload-pack",
		GitAdvertiseReceive: "GET /acme/app.git/info/refs?service=git-receive-pack",
		GitUploadPack:       "POST /acme/app.git/git-upload-pack?",
	}
	for shape, line := range want {
		r := GitRequest{Shape: shape, Header: http.Header{}}
		if shape == GitUploadPack {
			r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
			r.Body, r.ContentLength = strings.NewReader("0000"), 4
		}
		if _, err := gitDo(t, p, gitGrant(), r); err != nil {
			t.Fatalf("%v: %v", shape, err)
		}
		reqs := f.requests()
		last := reqs[len(reqs)-1]
		if got := last.method + " " + last.path + "?" + last.query; strings.TrimSuffix(got, "?") != strings.TrimSuffix(line, "?") {
			t.Errorf("%v: %s, want %s", shape, got, line)
		}
	}
	fetchOnly := gitGrant()
	fetchOnly.Shapes = []GitShape{GitAdvertiseUpload, GitUploadPack}
	before := len(f.requests())
	_, err := gitDo(t, p, fetchOnly, GitRequest{Shape: GitAdvertiseReceive, Header: http.Header{}})
	wantPushRefusal(t, err, "")
	if len(f.requests()) != before {
		t.Fatal("a shape the grant does not hold reached the forge")
	}
	if _, err := gitDo(t, p, gitGrant(), GitRequest{Shape: GitShape(99), Header: http.Header{}}); err == nil {
		t.Fatal("an unknown shape was sent")
	}
}

// A POST must carry its shape's own content type; a receive-pack request in
// any encoding cannot be read and is refused; an upload-pack in gzip passes.
func TestGitProxy_ChecksTheContentTypeAndEncoding(t *testing.T) {
	f := newForge(t, nil)
	p := gitProxyTo(t, f)
	wrongType := receive(section(cmd(oldID, newID, "refs/heads/loom/fix-1")))
	wrongType.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	_, err := gitDo(t, p, gitGrant(), wrongType)
	wantPushRefusal(t, err, "content type")
	gz := receive(section(cmd(oldID, newID, "refs/heads/loom/fix-1")))
	gz.Header.Set("Content-Encoding", "gzip")
	_, err = gitDo(t, p, gitGrant(), gz)
	wantPushRefusal(t, err, "encoding")
	if receivePackPosts(f) != 0 {
		t.Fatal("a refused push reached the forge")
	}
	up := GitRequest{Shape: GitUploadPack, Header: http.Header{"Content-Type": {"application/x-git-upload-pack-request"}, "Content-Encoding": {"gzip"}}, Body: strings.NewReader("x"), ContentLength: 1}
	if _, err := gitDo(t, p, gitGrant(), up); err != nil {
		t.Fatalf("a gzip upload-pack was refused: %v", err)
	}
}

// A push over the size cap is refused: by its length before anything is
// read, and while it streams when its length is unknown, the forge then
// seeing a cut request it cannot apply.
func TestGitProxy_RefusesAPushOverTheSizeCap(t *testing.T) {
	f := newForge(t, nil)
	p := gitProxyTo(t, f)
	g := gitGrant()
	g.MaxPushBytes = 4096
	body := section(cmd(oldID, newID, "refs/heads/loom/fix-1")) + strings.Repeat("p", 10000)
	_, err := gitDo(t, p, g, receive(body))
	wantPushRefusal(t, err, "size")
	if receivePackPosts(f) != 0 {
		t.Fatal("a push declared over the cap reached the forge")
	}
	unknown := receive(body)
	unknown.ContentLength = -1
	if _, err := gitDo(t, p, g, unknown); err == nil {
		t.Fatal("a streamed push over the cap went through")
	}
	for _, r := range f.requests() {
		if len(r.body) >= len(body) {
			t.Fatal("the forge received the whole push")
		}
	}
}

// The forge's redirect, 401 and 404 never reach the worker: each is a 403
// with one line, and the body and Location are dropped.
func TestGitProxy_NeverRelaysARedirectOrAnAuthChallenge(t *testing.T) {
	for status, says := range map[int]string{
		http.StatusFound:        "redirected",
		http.StatusUnauthorized: "refused the credential",
		http.StatusNotFound:     "not found",
	} {
		f := newForge(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			w.Header().Set("Location", "https://evil.example/steal")
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "forge text "+gitToken)
		})
		p := gitProxyTo(t, f)
		_, err := gitDo(t, p, gitGrant(), GitRequest{Shape: GitAdvertiseUpload, Header: http.Header{}})
		wantPushRefusal(t, err, says)
		if strings.Contains(err.Error(), "evil.example") || strings.Contains(err.Error(), gitToken) {
			t.Fatalf("the refusal carries the forge's words: %v", err)
		}
	}
}

// An answer passes with its hop-by-hop, cookie and authentication headers
// removed.
func TestGitProxy_StripsTheAnswersCookiesAndAuthHeaders(t *testing.T) {
	f := newForge(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Set-Cookie", "s=1")
		w.Header().Set("Authentication-Info", "x")
		w.Header().Set("Connection", "close")
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "001e# service=git-upload-pack\n0000")
	})
	p := gitProxyTo(t, f)
	resp, err := gitDo(t, p, gitGrant(), GitRequest{Shape: GitAdvertiseUpload, Header: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Set-Cookie", "Authentication-Info", "Connection"} {
		if resp.Header.Get(h) != "" {
			t.Errorf("%s passed to the worker", h)
		}
	}
	if resp.Header.Get("Content-Type") == "" {
		t.Error("the content type was dropped")
	}
}

// The default branch is the forge's own answer as the core read it: the
// symref in the advertisement's first ref line.
func TestGitProxy_ReadsTheDefaultBranchFromTheAdvertisement(t *testing.T) {
	adv := pkt("# service=git-upload-pack\n") + "0000" +
		pkt(oldID+" HEAD\x00multi_ack side-band-64k symref=HEAD:refs/heads/trunk agent=git/2.47\n") +
		pkt(oldID+" refs/heads/trunk\n") + "0000"
	f := newForge(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) { _, _ = io.WriteString(w, adv) })
	p := gitProxyTo(t, f)
	var got string
	g := gitGrant()
	g.OnDefaultBranch = func(ref string) { got = ref }
	resp, err := gitDo(t, p, g, GitRequest{Shape: GitAdvertiseUpload, Header: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != adv {
		t.Fatal("the advertisement changed on the way")
	}
	if got != "refs/heads/trunk" {
		t.Fatalf("default branch = %q", got)
	}
}

// An answer over the operation's read budget is cut.
func TestGitProxy_CutsAnAnswerOverTheReadBudget(t *testing.T) {
	f := newForge(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		_, _ = io.WriteString(w, strings.Repeat("x", 100000))
	})
	p := gitProxyTo(t, f)
	g := gitGrant()
	g.ReadBudget = NewByteBudget(1000)
	resp, err := gitDo(t, p, g, GitRequest{Shape: GitAdvertiseUpload, Header: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if !errors.Is(err, ErrByteBudget) || n > 1000 {
		t.Fatalf("read %d, err %v; want cut at 1000 with ErrByteBudget", n, err)
	}
}

// The core dials through the network guard: a private address is refused
// unless the operator's allowlist vouches for it.
func TestGitProxy_RefusesAPrivateAddress(t *testing.T) {
	f := newForge(t, nil)
	p, err := NewGitProxy(GitOptions{TLSConfig: &tls.Config{RootCAs: testRoots(t), MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	g := gitGrant()
	g.RepoURL = f.URL + "/acme/app.git"
	if _, err := gitDo(t, p, g, GitRequest{Shape: GitAdvertiseUpload, Header: http.Header{}}); err == nil {
		t.Fatal("the proxy dialled a loopback address no allowlist vouches for")
	}
	if len(f.requests()) != 0 {
		t.Fatal("the request reached the private address")
	}
}

// No error the proxy returns carries the credential.
func TestGitProxy_TheTokenIsInNoError(t *testing.T) {
	f := newForge(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		_ = c.Close()
	})
	p := gitProxyTo(t, f)
	for _, g := range []GitGrant{gitGrant(), func() GitGrant { g := gitGrant(); g.RepoURL = "http://forge.example/x.git"; return g }()} {
		_, err := gitDo(t, p, g, GitRequest{Shape: GitAdvertiseUpload, Header: http.Header{}})
		if err == nil {
			t.Fatal("no error")
		}
		if strings.Contains(err.Error(), gitToken) {
			t.Fatalf("the token is in an error: %v", err)
		}
	}
}

// Whether a branch exists is read from the forge's own advertisement as it
// passes, at any depth of it, and only from the refs: not from a later line
// of the answer.
func TestGitProxy_ReportsAWatchedRefFromTheAdvertisement(t *testing.T) {
	var refs strings.Builder
	for i := range 3000 {
		refs.WriteString(pkt(oldID + " refs/heads/b" + strings.Repeat("x", 20) + fmt.Sprint(i) + "\n"))
	}
	adv := pkt("# service=git-upload-pack\n") + "0000" +
		pkt(oldID+" HEAD\x00symref=HEAD:refs/heads/main\n") + refs.String() +
		pkt(oldID+" refs/heads/feature\n") + "0000" + pkt(oldID+" refs/heads/after-the-refs\n")
	f := newForge(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) { _, _ = io.WriteString(w, adv) })
	p := gitProxyTo(t, f)
	for want, seen := range map[string]bool{"refs/heads/feature": true, "refs/heads/absent": false, "refs/heads/after-the-refs": false, "refs/heads/fea": false} {
		g := gitGrant()
		got := false
		g.WatchRef, g.OnWatchedRef = want, func() { got = true }
		resp, err := gitDo(t, p, g, GitRequest{Shape: GitAdvertiseUpload, Header: http.Header{}})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != adv {
			t.Fatal("the advertisement changed on the way")
		}
		if got != seen {
			t.Errorf("%s: seen = %v, want %v", want, got, seen)
		}
	}
}
