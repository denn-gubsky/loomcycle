package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC HI P3: a page's frame capability (§1, D1–D3), behind the real auth
// middleware on the exposure routes' own mux. G35's acceptance list is the
// test list.

const framePrefix = "/v1/workspaces/exposure-frames/"

// frameServer is the lease fixture (alice's pages A and B, bob's C, leases
// up to 8 h) with the frame mint on the same mux.
func frameServer(t *testing.T) (*ticketFixture, *httptest.Server) {
	t.Helper()
	return leaseServer(t)
}

type frameMinted struct {
	FramePath string    `json:"frame_path"`
	ExpiresAt time.Time `json:"expires_at"`
}

func frameMint(t *testing.T, ts *httptest.Server, bearer, id string) (int, frameMinted) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+framePrefix+id, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out frameMinted
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func mustFrame(t *testing.T, ts *httptest.Server, bearer, id string) frameMinted {
	t.Helper()
	code, m := frameMint(t, ts, bearer, id)
	if code != http.StatusOK || m.FramePath == "" {
		t.Fatalf("frame mint for %s = %d", id, code)
	}
	return m
}

// capOf is the capability in a frame path.
func capOf(t *testing.T, framePath string) string {
	t.Helper()
	_, rest, ok := strings.Cut(framePath, "/~f/")
	frameCap, _, _ := strings.Cut(rest, "/")
	if !ok || frameCap == "" {
		t.Fatalf("frame path %q has no capability", framePath)
	}
	return frameCap
}

// frameGet asks path with no credential at all, as a frame's document does,
// and never follows a redirect.
func frameGet(t *testing.T, ts *httptest.Server, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// reframe re-signs cap's claims after change, as only a holder of the
// server's key could: the way to make a capability of another age.
func reframe(t *testing.T, tf *ticketFixture, frameCap string, change func(c *ticketClaims)) string {
	t.Helper()
	key, err := tf.s.clientToolsTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	c, reason := verifyTicket(key, exposureFrameFormat, frameCap)
	if reason != "" {
		t.Fatalf("the minted capability does not verify: %s", reason)
	}
	change(&c)
	out, err := signTicket(key, exposureFrameFormat, c)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func framePathFor(id, frameCap string) string { return exposurePrefix + id + "/~f/" + frameCap + "/" }

// wantNoReferrer asserts the referrer policy RFC HI §1 puts on every answer
// of the route.
func wantNoReferrer(t *testing.T, what string, h http.Header) {
	t.Helper()
	if got := h.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("%s: Referrer-Policy = %q, want no-referrer", what, got)
	}
}

// G35: the frame path serves the page with no header at all; the
// application sees its own path, never the capability or a credential; a
// redirect stays under the capability prefix; every answer is sandboxed and
// carries no referrer.
func TestExposureFrame_ServesThePageWithoutAHeader(t *testing.T) {
	tf, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	frameCap := capOf(t, m.FramePath)
	if want := framePathFor(testExposureID, frameCap); m.FramePath != want {
		t.Errorf("frame_path = %q, want %q", m.FramePath, want)
	}
	resp, body := frameGet(t, ts, m.FramePath+"index.html?q=1", nil)
	if resp.StatusCode != http.StatusOK || body != "hello from the workspace" {
		t.Fatalf("frame GET = %d %q", resp.StatusCode, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != exposurePolicy {
		t.Errorf("Content-Security-Policy = %q, want the sandbox policy", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff on a frame answer")
	}
	wantNoReferrer(t, "frame answer", resp.Header)
	tf.f.mu.Lock()
	got := tf.f.seen[len(tf.f.seen)-1]
	tf.f.mu.Unlock()
	if got.Path != "/index.html?q=1" || got.Auth != "" || got.Cookie != "" {
		t.Errorf("the application saw path %q auth %q cookie %q, want /index.html?q=1 and no credential", got.Path, got.Auth, got.Cookie)
	}
	resp, _ = frameGet(t, ts, m.FramePath+"redirect", map[string]string{"Referer": "https://board.example" + m.FramePath})
	if loc := resp.Header.Get("Location"); loc != strings.TrimSuffix(m.FramePath, "/")+"/there?x=1" {
		t.Errorf("Location = %q, want it under the capability prefix", loc)
	}
	tf.f.mu.Lock()
	got = tf.f.seen[len(tf.f.seen)-1]
	tf.f.mu.Unlock()
	if strings.Contains(got.Header.Get("Referer"), frameCap) {
		t.Errorf("the application received the capability in Referer: %q", got.Header.Get("Referer"))
	}
	// The bare frame prefix is the application's root.
	if resp, body := frameGet(t, ts, strings.TrimSuffix(m.FramePath, "/"), nil); resp.StatusCode != http.StatusOK || body != "hello from the workspace" {
		t.Errorf("frame root without its slash = %d %q", resp.StatusCode, body)
	}
}

// G35: a WebSocket on the frame path is admitted by the capability, with no
// bearer subprotocol and no ticket, and the application never sees it.
func TestExposureFrame_OpensTheSocketWithoutACredential(t *testing.T) {
	tf, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+m.FramePath+"ws", &websocket.DialOptions{
		Subprotocols: []string{exposureSocketMarker}})
	if err != nil {
		t.Fatalf("frame socket: %v", err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := c.Read(ctx); err != nil || string(data) != "hi" {
		t.Fatalf("echo = %q %v", data, err)
	}
	tf.ws.mu.Lock()
	protos, authz := tf.ws.protos, tf.ws.authz
	tf.ws.mu.Unlock()
	if strings.Contains(protos, "lxf1") || authz != "" {
		t.Errorf("the application's handshake carried protocols %q authorization %q", protos, authz)
	}
	tf.f.mu.Lock()
	path := tf.f.seen[len(tf.f.seen)-1].Path
	tf.f.mu.Unlock()
	if path != "/ws" {
		t.Errorf("the application's socket path = %q, want /ws", path)
	}
}

// Only the page's own user mints, by the Authorization header; every other
// page is the unknown id's 404, and a closed page is too.
func TestExposureFrame_MintIsThePagesOwnUsersByHeader(t *testing.T) {
	tf, ts := frameServer(t)
	if code, _ := frameMint(t, ts, "", testExposureID); code != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", code)
	}
	for _, c := range []struct{ who, bearer, id string }{
		{"another user's page", "bob-token", testExposureID},
		{"an unknown id", "alice-token", "ffffffffffffffffffffffff"},
	} {
		if code, m := frameMint(t, ts, c.bearer, c.id); code != http.StatusNotFound || m.FramePath != "" {
			t.Errorf("%s = %d %+v, want 404", c.who, code, m)
		}
	}
	if _, err := tf.f.st.WorkspaceExposureClose(context.Background(), "acme", exposureB, lifecycle.SurfaceUnexposed, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code, _ := frameMint(t, ts, "alice-token", exposureB); code != http.StatusNotFound {
		t.Errorf("a closed page = %d, want 404", code)
	}
}

// A capability for page A is refused on page B, alice's own other page.
func TestExposureFrame_RefusesACapabilityForAnotherPage(t *testing.T) {
	tf, ts := frameServer(t)
	frameCap := capOf(t, mustFrame(t, ts, "alice-token", testExposureID).FramePath)
	before := tf.f.seenCount()
	if resp, _ := frameGet(t, ts, framePathFor(exposureB, frameCap)+"index.html", nil); resp.StatusCode == http.StatusOK {
		t.Errorf("page A's capability on page B = %d, want a refusal", resp.StatusCode)
	}
	if tf.f.seenCount() != before {
		t.Error("the application was reached")
	}
}

// A capability changed in any byte, a socket ticket in its place, and the
// frame segment with no capability are all refused.
func TestExposureFrame_RefusesATamperedCapability(t *testing.T) {
	tf, ts := frameServer(t)
	frameCap := capOf(t, mustFrame(t, ts, "alice-token", testExposureID).FramePath)
	ticket := tf.mustMint(t, "alice-token", testExposureID)
	parts := strings.Split(frameCap, ".")
	flip := func(s string) string {
		b := []byte(s)
		if b[len(b)/2] == 'A' {
			b[len(b)/2] = 'B'
		} else {
			b[len(b)/2] = 'A'
		}
		return string(b)
	}
	before := tf.f.seenCount()
	for what, path := range map[string]string{
		"payload changed":   framePathFor(testExposureID, parts[0]+"."+flip(parts[1])+"."+parts[2]),
		"signature changed": framePathFor(testExposureID, parts[0]+"."+parts[1]+"."+flip(parts[2])),
		"a socket ticket":   framePathFor(testExposureID, ticket),
		"lxp1 relabelled":   framePathFor(testExposureID, "lxp1."+parts[1]+"."+parts[2]),
		"no capability":     exposurePrefix + testExposureID + "/~f/",
		"empty capability":  exposurePrefix + testExposureID + "/~f//index.html",
	} {
		if resp, _ := frameGet(t, ts, path, nil); resp.StatusCode == http.StatusOK {
			t.Errorf("%s = %d, want a refusal", what, resp.StatusCode)
		}
	}
	if tf.f.seenCount() != before {
		t.Error("the application was reached")
	}
}

// D2: ten minutes on its own; past that only while a lease is live; never
// past the hard end, nor past mint + max_lease_s, even with a lease.
func TestExposureFrame_ValidityFollowsTheLease(t *testing.T) {
	tf, ts := frameServer(t)
	frameCap := capOf(t, mustFrame(t, ts, "alice-token", testExposureID).FramePath)
	now := time.Now()
	aged := reframe(t, tf, frameCap, func(c *ticketClaims) { c.Iat = now.Add(-11 * time.Minute).UnixMilli() })
	get := func(frameCap string) int {
		resp, _ := frameGet(t, ts, framePathFor(testExposureID, frameCap)+"index.html", nil)
		return resp.StatusCode
	}
	if code := get(aged); code == http.StatusOK {
		t.Errorf("an 11-minute-old capability with no lease = %d, want a refusal", code)
	}
	if code, body := leaseCall(t, ts, "POST", "alice-token", testExposureID, `{"ttl_s": 3600}`); code != http.StatusOK {
		t.Fatalf("lease = %d %s", code, body)
	}
	if code := get(aged); code != http.StatusOK {
		t.Errorf("an 11-minute-old capability under a live lease = %d, want 200", code)
	}
	expired := reframe(t, tf, frameCap, func(c *ticketClaims) {
		c.Iat = now.Add(-2 * time.Hour).UnixMilli()
		c.Exp = now.Add(-time.Minute).UnixMilli()
	})
	if code := get(expired); code == http.StatusOK {
		t.Errorf("a capability past its hard end = %d, want a refusal under a live lease too", code)
	}
	pastMax := reframe(t, tf, frameCap, func(c *ticketClaims) {
		c.Iat = now.Add(-9 * time.Hour).UnixMilli()
		c.Exp = now.Add(time.Hour).UnixMilli()
	})
	if code := get(pastMax); code == http.StatusOK {
		t.Errorf("a capability past mint + max_lease_s = %d, want a refusal under a live lease too", code)
	}
	if code, _ := leaseCall(t, ts, "DELETE", "alice-token", testExposureID, ""); code != http.StatusNoContent {
		t.Fatalf("release = %d", code)
	}
	if code := get(aged); code == http.StatusOK {
		t.Errorf("an 11-minute-old capability once the lease ended = %d, want a refusal", code)
	}
	if code := get(frameCap); code != http.StatusOK {
		t.Errorf("a fresh capability with no lease = %d, want 200", code)
	}
}

// With leases off the capability lives its ten minutes and no longer, and
// expires_at says so; with a lease, expires_at is the lease's end.
func TestExposureFrame_ExpiresAtIsItsLifeAsOfTheMint(t *testing.T) {
	tf, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	if d := time.Until(m.ExpiresAt); d < 9*time.Minute || d > 10*time.Minute+time.Second {
		t.Errorf("no lease: expires_at in %v, want 10 minutes", d)
	}
	code, body := leaseCall(t, ts, "POST", "alice-token", testExposureID, `{"ttl_s": 7200}`)
	if code != http.StatusOK {
		t.Fatalf("lease = %d", code)
	}
	if m := mustFrame(t, ts, "alice-token", testExposureID); !m.ExpiresAt.Equal(leaseEndOf(t, body)) {
		t.Errorf("under a 2h lease expires_at = %v, want the lease's end %v", m.ExpiresAt, leaseEndOf(t, body))
	}

	tf.f.m.SetExpose(lifecycle.ExposeLimits{Enabled: true})
	m = mustFrame(t, ts, "alice-token", testExposureID)
	if d := time.Until(m.ExpiresAt); d < 9*time.Minute || d > 10*time.Minute+time.Second {
		t.Errorf("leases off: expires_at in %v, want 10 minutes", d)
	}
	key, _ := tf.s.clientToolsTicketKey()
	c, reason := verifyTicket(key, exposureFrameFormat, capOf(t, m.FramePath))
	if reason != "" || time.UnixMilli(c.Exp).Sub(time.UnixMilli(c.Iat)) != exposureFrameOwnLife {
		t.Errorf("leases off: the capability's hard life = %v (%s), want 10 minutes", time.UnixMilli(c.Exp).Sub(time.UnixMilli(c.Iat)), reason)
	}
	aged := reframe(t, tf, capOf(t, m.FramePath), func(c *ticketClaims) {
		c.Iat = time.Now().Add(-11 * time.Minute).UnixMilli()
		c.Exp = time.Now().Add(time.Hour).UnixMilli()
	})
	if resp, _ := frameGet(t, ts, framePathFor(testExposureID, aged), nil); resp.StatusCode == http.StatusOK {
		t.Errorf("leases off, an 11-minute-old capability = %d, want a refusal", resp.StatusCode)
	}
}

// G35: a retired minting token stops the frame once the recheck cache's
// window has passed, on the page and on an open socket.
func TestExposureFrame_StopsWhenTheMintingTokenIsRetired(t *testing.T) {
	tf, ts := frameServer(t)
	tf.s.exposures.socketRecheck = 100 * time.Millisecond
	tf.s.frameCreds.setWindow(300 * time.Millisecond)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	if resp, _ := frameGet(t, ts, m.FramePath, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("before the retirement = %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+m.FramePath+"ws", &websocket.DialOptions{
		Subprotocols: []string{exposureSocketMarker}})
	if err != nil {
		t.Fatalf("frame socket: %v", err)
	}
	defer c.CloseNow()
	if err := tf.st.OperatorTokenDefSetRetiredAt(ctx, "def_alice", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("after the retirement the frame socket read %v, want a close with 1008", err)
	}
	time.Sleep(400 * time.Millisecond)
	if resp, _ := frameGet(t, ts, m.FramePath, nil); resp.StatusCode == http.StatusOK {
		t.Errorf("past the cache window after the retirement = %d, want a refusal", resp.StatusCode)
	}
}

// The recheck cache is at most 30 seconds.
func TestExposureFrame_RecheckCacheIsAtMostThirtySeconds(t *testing.T) {
	var c exposureFrameCache
	if got := c.window(); got <= 0 || got > 30*time.Second {
		t.Errorf("the default recheck window = %v, want (0, 30s]", got)
	}
}

// G35: unexpose closes the frame, as it closes the page.
func TestExposureFrame_RefusedOnceThePageCloses(t *testing.T) {
	tf, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	if _, err := tf.f.st.WorkspaceExposureClose(context.Background(), "acme", testExposureID, lifecycle.SurfaceUnexposed, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp, _ := frameGet(t, ts, m.FramePath, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("after unexpose = %d, want 404", resp.StatusCode)
	}
	wantNoReferrer(t, "a closed page's 404", resp.Header)
}

// G35: a capability is never a bearer, on any route; and the frame path
// takes the capability alone, never beside a header.
func TestExposureFrame_IsNeverABearer(t *testing.T) {
	tf, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	frameCap := capOf(t, m.FramePath)
	before := tf.f.seenCount()
	for _, path := range []string{
		exposurePrefix + testExposureID + "/index.html",
		exposurePrefix + testExposureID,
		"/v1/workspaces/exposures",
		framePrefix + testExposureID,
		leasePrefix + testExposureID,
		m.FramePath,
	} {
		method := "GET"
		if strings.HasPrefix(path, framePrefix) || strings.HasPrefix(path, leasePrefix) {
			method = "POST"
		}
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(`{"ttl_s": 60}`))
		req.Header.Set("Authorization", "Bearer "+frameCap)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with the capability as bearer = %d, want 401", method, path, resp.StatusCode)
		}
	}
	// A real bearer beside the frame path is refused too: the path is
	// the capability's alone, and never reaches the application.
	if resp, _ := frameGet(t, ts, m.FramePath, map[string]string{"Authorization": "Bearer alice-token"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the frame path with a bearer header = %d, want 401", resp.StatusCode)
	}
	if tf.f.seenCount() != before {
		t.Error("the application was reached")
	}
}

// RFC HI §1: the header form gains Referrer-Policy too, on its answers and
// its refusals.
func TestExposureRoute_EveryAnswerHasNoReferrer(t *testing.T) {
	_, ts := frameServer(t)
	resp, _ := frameGet(t, ts, pagePath, map[string]string{"Authorization": "Bearer alice-token"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("header form = %d", resp.StatusCode)
	}
	wantNoReferrer(t, "header form", resp.Header)
	resp, _ = frameGet(t, ts, pagePath, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credential = %d", resp.StatusCode)
	}
	wantNoReferrer(t, "the middleware's refusal", resp.Header)
	if resp.Header.Get("Content-Security-Policy") != exposurePolicy {
		t.Errorf("the middleware's refusal: Content-Security-Policy = %q", resp.Header.Get("Content-Security-Policy"))
	}
}

// G35: the capability is in no log line at any level: a mint, uses, a
// socket, refusals of a tampered and an aged capability. Not parallel: it
// swaps the default logger.
func TestExposureFrame_NeverLogged(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tf, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	frameCap := capOf(t, m.FramePath)
	frameGet(t, ts, m.FramePath+"index.html", nil)
	frameGet(t, ts, m.FramePath+"nope", nil)
	frameGet(t, ts, framePathFor(testExposureID, frameCap+"x"), nil)
	frameGet(t, ts, framePathFor(exposureB, frameCap), nil)
	frameGet(t, ts, framePathFor(testExposureID, reframe(t, tf, frameCap, func(c *ticketClaims) {
		c.Iat = time.Now().Add(-11 * time.Minute).UnixMilli()
	})), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+m.FramePath+"ws", &websocket.DialOptions{
		Subprotocols: []string{exposureSocketMarker}}); err == nil {
		c.CloseNow()
	}
	logs := buf.String()
	_, payload, _ := strings.Cut(frameCap, ".")
	if strings.Contains(logs, frameCap) || strings.Contains(logs, payload) {
		t.Errorf("a log line carries the capability:\n%s", logs)
	}
	if !strings.Contains(logs, "page frame refused") {
		t.Errorf("the refusals were not logged with a reason:\n%s", logs)
	}
}

// RFC HI §1: the panic-recovery log line records the path with the
// capability redacted.
func TestRecoveryMiddleware_RedactsAFrameCapability(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	h := recoveryMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	const frameCap = "lxf1.c2VjcmV0LXBheWxvYWQ.c2lnbmF0dXJl"
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", framePathFor(testExposureID, frameCap)+"app.js", nil))
	logs := buf.String()
	if strings.Contains(logs, frameCap) || strings.Contains(logs, "c2VjcmV0LXBheWxvYWQ") {
		t.Errorf("the panic log carries the capability: %s", logs)
	}
	if !strings.Contains(logs, exposurePrefix+testExposureID+"/~f/") || !strings.Contains(logs, "/app.js") {
		t.Errorf("the panic log lost the rest of the path: %s", logs)
	}
}

func TestRedactExposureFrame(t *testing.T) {
	for in, want := range map[string]string{
		"/v1/workspaces/exposures/abc/~f/lxf1.x.y/a/b": "/v1/workspaces/exposures/abc/~f/[redacted]/a/b",
		"/v1/workspaces/exposures/abc/~f/lxf1.x.y":     "/v1/workspaces/exposures/abc/~f/[redacted]",
		"/v1/workspaces/exposures/abc/index.html":      "/v1/workspaces/exposures/abc/index.html",
		"/v1/runs/~f/x": "/v1/runs/~f/x",
	} {
		if got := redactExposureFrame(in); got != want {
			t.Errorf("redactExposureFrame(%q) = %q, want %q", in, got, want)
		}
	}
}

// A frame's first use is audited by the capability's jti, once across
// requests, beside the mint.
func TestExposureFrame_MintAndFirstUseAreAuditedByJTI(t *testing.T) {
	tf, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	frameGet(t, ts, m.FramePath, nil)
	tf.s.frameCreds.setWindow(-1) // no cache: every request re-resolves
	frameGet(t, ts, m.FramePath, nil)
	key, _ := tf.s.clientToolsTicketKey()
	c, _ := verifyTicket(key, exposureFrameFormat, capOf(t, m.FramePath))
	fresh, err := tf.st.AuthAssertionUseOnce(context.Background(), exposureFrameFirstUseGate, c.JTI, time.Now().Add(time.Hour), time.Now())
	if err != nil || fresh {
		t.Errorf("the first use was not recorded under the jti (fresh=%v err=%v)", fresh, err)
	}
}

// frameDo sends method to path with hdr and no credential beyond what hdr
// carries, never following a redirect.
func frameDo(t *testing.T, ts *httptest.Server, method, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// The owner's ruling on RFC HI P3 (option a): a frame's page runs in an
// opaque origin, so its fetch is cross-origin. Every answer under the frame
// prefix, refusals included, carries Access-Control-Allow-Origin: *, and
// none ever carries Access-Control-Allow-Credentials, whatever the
// application sends. The header form and every other route are unchanged.
func TestExposureFrame_AllowsAnyOriginOnFrameAnswersOnly(t *testing.T) {
	_, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	frameCap := capOf(t, m.FramePath)
	wantCORS := func(what string, resp *http.Response, want string) {
		t.Helper()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("%s (%d): Access-Control-Allow-Origin = %q, want %q", what, resp.StatusCode, got, want)
		}
		if v := resp.Header.Values("Access-Control-Allow-Credentials"); len(v) != 0 {
			t.Errorf("%s: Access-Control-Allow-Credentials = %v, want none", what, v)
		}
	}
	null := map[string]string{"Origin": "null"}
	if resp := frameDo(t, ts, "GET", m.FramePath+"index.html", null); resp.StatusCode != http.StatusOK {
		t.Fatalf("frame GET = %d", resp.StatusCode)
	} else {
		wantCORS("a frame answer", resp, "*")
	}
	// The application's own CORS headers never pass (hostile sends
	// Access-Control-Allow-Origin: * and -Credentials: true).
	wantCORS("a frame answer of a hostile application", frameDo(t, ts, "GET", m.FramePath+"hostile", null), "*")
	wantCORS("a frame redirect", frameDo(t, ts, "GET", m.FramePath+"redirect", null), "*")
	parts := strings.Split(frameCap, ".")
	wantCORS("a refused capability", frameDo(t, ts, "GET", framePathFor(testExposureID, parts[0]+"."+parts[1]+".AAAA"), null), "*")

	bearer := map[string]string{"Origin": "https://board.example", "Authorization": "Bearer alice-token"}
	if resp := frameDo(t, ts, "GET", pagePath, bearer); resp.StatusCode != http.StatusOK {
		t.Fatalf("header form = %d", resp.StatusCode)
	} else {
		wantCORS("a header-form answer", resp, "")
	}
	wantCORS("a header-form answer of a hostile application", frameDo(t, ts, "GET", exposurePrefix+testExposureID+"/hostile", bearer), "")
	wantCORS("a header-form refusal", frameDo(t, ts, "GET", pagePath, null), "")
	wantCORS("the page's state", frameDo(t, ts, "GET", exposurePrefix+testExposureID, bearer), "")
	wantCORS("the frame mint", frameDo(t, ts, "POST", framePrefix+testExposureID, bearer), "")
}

// A preflight carries no credential, but its address carries the
// capability: the route checks it exactly as for any frame request, and
// then answers the preflight itself; the application never sees it.
func TestExposureFrame_AnswersAPreflightItself(t *testing.T) {
	tf, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	frameCap := capOf(t, m.FramePath)
	pre := map[string]string{"Origin": "null", "Access-Control-Request-Method": "POST",
		"Access-Control-Request-Headers": "content-type, x-app"}
	before := tf.f.seenCount()
	resp := frameDo(t, ts, "OPTIONS", m.FramePath+"api/data", pre)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("preflight Access-Control-Allow-Origin = %q, want *", got)
	}
	if v := resp.Header.Values("Access-Control-Allow-Credentials"); len(v) != 0 {
		t.Errorf("preflight Access-Control-Allow-Credentials = %v, want none", v)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("preflight Access-Control-Allow-Methods = %q, want POST in it", got)
	}
	if got := strings.ToLower(resp.Header.Get("Access-Control-Allow-Headers")); !strings.Contains(got, "content-type") || !strings.Contains(got, "x-app") {
		t.Errorf("preflight Access-Control-Allow-Headers = %q, want content-type and x-app", got)
	}
	wantNoReferrer(t, "preflight", resp.Header)
	if tf.f.seenCount() != before {
		t.Error("the preflight reached the application")
	}
	// A credential is never allowed through: the frame path refuses one.
	resp = frameDo(t, ts, "OPTIONS", m.FramePath+"api/data", map[string]string{"Origin": "null",
		"Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "authorization"})
	if strings.Contains(strings.ToLower(resp.Header.Get("Access-Control-Allow-Headers")), "authorization") {
		t.Errorf("a preflight allowed the Authorization header: %q", resp.Header.Get("Access-Control-Allow-Headers"))
	}

	refused := map[string]string{
		"a tampered capability": framePathFor(testExposureID, frameCap+"x") + "api/data",
		"another page's":        framePathFor(exposureB, frameCap) + "api/data",
		"an aged one, no lease": framePathFor(testExposureID, reframe(t, tf, frameCap, func(c *ticketClaims) { c.Iat = time.Now().Add(-11 * time.Minute).UnixMilli() })) + "api/data",
		"past its hard end": framePathFor(testExposureID, reframe(t, tf, frameCap, func(c *ticketClaims) {
			c.Iat = time.Now().Add(-2 * time.Hour).UnixMilli()
			c.Exp = time.Now().Add(-time.Minute).UnixMilli()
		})) + "api/data",
	}
	for what, path := range refused {
		if resp := frameDo(t, ts, "OPTIONS", path, pre); resp.StatusCode < 400 {
			t.Errorf("a preflight with %s = %d, want a refusal", what, resp.StatusCode)
		}
	}
	if _, err := tf.f.st.WorkspaceExposureClose(context.Background(), "acme", testExposureID, lifecycle.SurfaceUnexposed, time.Now()); err != nil {
		t.Fatal(err)
	}
	if resp := frameDo(t, ts, "OPTIONS", m.FramePath+"api/data", pre); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a preflight on a closed page = %d, want 404", resp.StatusCode)
	}
	if tf.f.seenCount() != before {
		t.Error("a refused preflight reached the application")
	}
}

// An OPTIONS that is not a preflight (no Access-Control-Request-Method) is
// the application's, through a frame as through the header form; and the
// header form's preflight-looking OPTIONS is the application's too.
func TestExposureFrame_PlainOptionsReachesTheApplication(t *testing.T) {
	tf, ts := frameServer(t)
	m := mustFrame(t, ts, "alice-token", testExposureID)
	before := tf.f.seenCount()
	frameDo(t, ts, "OPTIONS", m.FramePath+"api/data", nil)
	frameDo(t, ts, "OPTIONS", pagePath, map[string]string{"Authorization": "Bearer alice-token",
		"Origin": "https://board.example", "Access-Control-Request-Method": "POST"})
	if got := tf.f.seenCount() - before; got != 2 {
		t.Errorf("the application saw %d OPTIONS requests, want 2", got)
	}
}
