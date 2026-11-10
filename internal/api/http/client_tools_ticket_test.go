package http

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/clienttools"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC GJ: a one-time ticket for the client-tools WebSocket. Each test names
// the acceptance item it pins.

var ticketTestSecretKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

// enableTickets turns on the client-tool host and the ticket key.
func enableTickets(s *Server) *clienttools.Registry {
	s.cfg().Env.SecretKey = ticketTestSecretKey
	s.cfg().Env.ClientToolMaxBytes = 1 << 20
	reg := clienttools.NewRegistry(0)
	s.clientTools = reg
	return reg
}

// strictAppFixture is a strict-mode server with a signed-in app (RFC FL)
// whose access token belongs to google:100 of tenant acme, served over its
// real mux.
func strictAppFixture(t *testing.T) (*appFixture, *clienttools.Registry, *httptest.Server, string) {
	t.Helper()
	f := newAppFixture(t, "tenant")
	reg := enableTickets(f.s)
	at, _ := f.signIn(t)
	f.s.cfg().ResolvedAuth.Mode = config.AuthModeStrict
	ts := httptest.NewServer(f.s.Mux())
	t.Cleanup(ts.Close)
	return f, reg, ts, at
}

// mintTicket POSTs the mint route with a bearer and returns status and body.
func mintTicket(t *testing.T, ts *httptest.Server, bearer string) (int, map[string]string) {
	t.Helper()
	code, body := bearerCall(t, ts, bearer, http.MethodPost, clientToolsTicketPath, "")
	var out map[string]string
	_ = json.Unmarshal([]byte(body), &out)
	return code, out
}

func mustTicket(t *testing.T, ts *httptest.Server, bearer string) string {
	t.Helper()
	code, out := mintTicket(t, ts, bearer)
	if code != http.StatusOK || out["ticket"] == "" {
		t.Fatalf("mint = %d %v", code, out)
	}
	exp, err := time.Parse(time.RFC3339Nano, out["expires_at"])
	if err != nil || exp.Sub(time.Now()) > clientToolsTicketTTL || exp.Before(time.Now()) {
		t.Fatalf("expires_at = %q (%v), want within %s", out["expires_at"], err, clientToolsTicketTTL)
	}
	return out["ticket"]
}

// dialTicket opens the client-tools socket with a ticket subprotocol entry.
func dialTicket(ctx context.Context, url, ticket string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, wsURL(url)+"/v1/client-tools", &websocket.DialOptions{
		Subprotocols: []string{clientToolSubprotocol, ticketSubprotocolPrefix + ticket},
		HTTPHeader:   hdr,
	})
}

// helloTool registers one tool on c and waits for hello_ok.
func helloTool(t *testing.T, ctx context.Context, c *websocket.Conn, tool string) {
	t.Helper()
	b, _ := json.Marshal(clienttools.HelloFrame{Type: clienttools.FrameHello,
		Tools: []clienttools.ToolSchema{{Name: tool, Description: "test"}}})
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if _, data, err := c.Read(ctx); err != nil || clienttools.TypeOf(data) != clienttools.FrameHelloOK {
		t.Fatalf("hello_ok: err=%v frame=%s", err, data)
	}
}

func refusedStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// Acceptance: strict mode + an app token — the socket opens with a ticket,
// is filed under that principal, and a client tool call for that user is
// routed to it. The bearer subprotocol stays refused in strict mode.
func TestClientToolsTicket_StrictAppTokenOpensSocketUnderItsPrincipal(t *testing.T) {
	f, reg, ts, at := strictAppFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticket := mustTicket(t, ts, at)
	// An extension's Origin: a ticket socket skips the same-origin check, as
	// a bearer socket does.
	c, _, err := dialTicket(ctx, ts.URL, ticket, http.Header{"Origin": {"chrome-extension://abcdefghijklmnop"}})
	if err != nil {
		t.Fatalf("dial with a ticket in strict mode: %v", err)
	}
	defer c.CloseNow()
	if c.Subprotocol() != clientToolSubprotocol {
		t.Errorf("negotiated subprotocol = %q, want %q (the ticket is never echoed)", c.Subprotocol(), clientToolSubprotocol)
	}
	helloTool(t, ctx, c, "browser_read_page")
	key := clienttools.PrincipalKey{Tenant: "acme", Subject: "google:100"}
	if got := len(reg.Provides(key)); got != 1 {
		t.Fatalf("tools filed under the app token's principal = %d, want 1", got)
	}

	// A run of that user calls client__browser_read_page: it reaches this socket.
	res := make(chan clienttools.ResultFrame, 1)
	go func() {
		r, err := reg.Invoke(ctx, key, "browser_read_page", json.RawMessage(`{}`), clienttools.InvokeMeta{RunID: "run-1"})
		if err != nil {
			t.Errorf("invoke: %v", err)
		}
		res <- r
	}()
	_, data, err := c.Read(ctx)
	if err != nil || clienttools.TypeOf(data) != clienttools.FrameInvoke {
		t.Fatalf("invoke frame: err=%v frame=%s", err, data)
	}
	var inv clienttools.InvokeFrame
	_ = json.Unmarshal(data, &inv)
	reply, _ := json.Marshal(clienttools.ResultFrame{Type: clienttools.FrameResult, CallID: inv.CallID, OK: true, Output: json.RawMessage(`"page"`)})
	if err := c.Write(ctx, websocket.MessageText, reply); err != nil {
		t.Fatal(err)
	}
	if r := <-res; !r.OK || string(r.Output) != `"page"` {
		t.Errorf("routed result = %+v", r)
	}

	ev := lastEvent(t, f.sink, "client_tools_ticket_issued")
	if ev.AuthClient != "board" || ev.ActorTenant != "acme" || ev.ActorSubject != "google:100" || ev.AuthRecordID == "" {
		t.Errorf("mint audit = %+v", ev)
	}
	if b, _ := json.Marshal(f.sink.events); bytes.Contains(b, []byte(ticket)) {
		t.Error("the audit trail holds the ticket")
	}

	// Strict mode still never reads a bearer from the subprotocol.
	if _, resp, err := websocket.Dial(ctx, wsURL(ts.URL)+"/v1/client-tools", &websocket.DialOptions{
		Subprotocols: []string{clientToolSubprotocol, "bearer." + at},
	}); err == nil || refusedStatus(resp) != http.StatusUnauthorized {
		t.Errorf("strict bearer subprotocol: err=%v status=%d, want 401", err, refusedStatus(resp))
	}
}

// Acceptance: a spent ticket is refused at the handshake.
func TestClientToolsTicket_SpentTicketRefused(t *testing.T) {
	_, _, ts, at := strictAppFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticket := mustTicket(t, ts, at)
	c, _, err := dialTicket(ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("first use: %v", err)
	}
	c.CloseNow()
	if _, resp, err := dialTicket(ctx, ts.URL, ticket, nil); err == nil || refusedStatus(resp) != http.StatusUnauthorized {
		t.Errorf("replay: err=%v status=%d, want 401", err, refusedStatus(resp))
	}
}

// Acceptance: a ticket presented after expires_at is refused, even with a
// valid signature.
func TestClientToolsTicket_ExpiredTicketRefused(t *testing.T) {
	f, _, ts, at := strictAppFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticket := mustTicket(t, ts, at)
	key, err := f.s.clientToolsTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	c, reason := verifyTicket(key, ticket)
	if reason != "" {
		t.Fatalf("verify own ticket: %s", reason)
	}
	c.Exp = time.Now().Add(-time.Second).UnixMilli()
	expired, err := signTicket(key, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, resp, err := dialTicket(ctx, ts.URL, expired, nil); err == nil || refusedStatus(resp) != http.StatusUnauthorized {
		t.Errorf("expired: err=%v status=%d, want 401", err, refusedStatus(resp))
	}
	// The untouched ticket still works: the refusal was the expiry.
	if c, _, err := dialTicket(ctx, ts.URL, ticket, nil); err != nil {
		t.Errorf("the live ticket: %v", err)
	} else {
		c.CloseNow()
	}
}

// Acceptance: a ticket is spent once across replicas — two servers sharing
// one store. Minted on one, used on the other, refused back on the first.
func TestClientToolsTicket_SpentOnOneReplicaRefusedOnAnother(t *testing.T) {
	f, _, ts1, at := strictAppFixture(t)
	s2 := &Server{cfgHolder: f.s.cfgHolder, store: f.st, tokenAudit: f.sink, clientTools: clienttools.NewRegistry(0)}
	mux2 := http.NewServeMux()
	mux2.Handle("GET /v1/client-tools", s2.authMiddleware(http.HandlerFunc(s2.handleClientTools)))
	ts2 := httptest.NewServer(mux2)
	t.Cleanup(ts2.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticket := mustTicket(t, ts1, at)
	c, _, err := dialTicket(ctx, ts2.URL, ticket, nil)
	if err != nil {
		t.Fatalf("a ticket minted on replica 1 must open replica 2: %v", err)
	}
	c.CloseNow()
	if _, resp, err := dialTicket(ctx, ts1.URL, ticket, nil); err == nil || refusedStatus(resp) != http.StatusUnauthorized {
		t.Errorf("replay on replica 1: err=%v status=%d, want 401", err, refusedStatus(resp))
	}
}

// A ticket whose MAC (or payload) was altered is refused.
func TestClientToolsTicket_TamperedTicketRefused(t *testing.T) {
	_, _, ts, at := strictAppFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticket := mustTicket(t, ts, at)
	parts := strings.Split(ticket, ".")
	flip := func(s string) string {
		b := []byte(s)
		if b[len(b)-2] == 'A' {
			b[len(b)-2] = 'B'
		} else {
			b[len(b)-2] = 'A'
		}
		return string(b)
	}
	for name, bad := range map[string]string{
		"mac":     parts[0] + "." + parts[1] + "." + flip(parts[2]),
		"payload": parts[0] + "." + flip(parts[1]) + "." + parts[2],
	} {
		if _, resp, err := dialTicket(ctx, ts.URL, bad, nil); err == nil || refusedStatus(resp) != http.StatusUnauthorized {
			t.Errorf("tampered %s: err=%v status=%d, want 401", name, err, refusedStatus(resp))
		}
	}
	// The tampered tries spent nothing: the real ticket still opens.
	if c, _, err := dialTicket(ctx, ts.URL, ticket, nil); err != nil {
		t.Errorf("the untampered ticket: %v", err)
	} else {
		c.CloseNow()
	}
}

// Acceptance: a ticket is not a bearer — Authorization: Bearer <ticket> on an
// HTTP route is refused, and so is the ticket as a `bearer.` subprotocol
// entry in token mode. A `ticket.` entry on any other route is ignored.
func TestClientToolsTicket_NotAcceptedAsABearerOrOnOtherRoutes(t *testing.T) {
	f, _, ts, at := strictAppFixture(t)
	ticket := mustTicket(t, ts, at)
	for _, path := range []string{"/v1/_me", "/v1/runs"} {
		if code, _ := bearerCall(t, ts, ticket, http.MethodGet, path, ""); code != http.StatusUnauthorized {
			t.Errorf("Bearer <ticket> on %s = %d, want 401", path, code)
		}
	}
	if _, ok := f.s.ResolvePrincipal(context.Background(), ticket); ok {
		t.Error("the ticket resolved as a bearer (the gRPC/MCP path)")
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Protocol", clientToolSubprotocol+", "+ticketSubprotocolPrefix+ticket)
	if code, reached := through(f.s, req); code != http.StatusUnauthorized || reached {
		t.Errorf("ticket on another route's upgrade = %d reached=%v, want 401", code, reached)
	}
	// Token mode: the ticket as a bearer. subprotocol entry is no bearer either.
	f.s.cfg().ResolvedAuth.Mode = config.AuthModeToken
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, resp, err := websocket.Dial(ctx, wsURL(ts.URL)+"/v1/client-tools", &websocket.DialOptions{
		Subprotocols: []string{clientToolSubprotocol, "bearer." + ticket},
	}); err == nil || refusedStatus(resp) != http.StatusUnauthorized {
		t.Errorf("bearer.<ticket>: err=%v status=%d, want 401", err, refusedStatus(resp))
	}
	// None of the refusals above spent the ticket; it still opens the socket.
	if c, _, err := dialTicket(ctx, ts.URL, ticket, nil); err != nil {
		t.Errorf("the ticket on its own route: %v", err)
	} else {
		c.CloseNow()
	}
}

// Acceptance: a ticket minted by a token that is then retired is refused at
// the handshake.
func TestClientToolsTicket_MintingTokenRetiredBeforeUseRefused(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	enableTickets(s)
	s.cfg().ResolvedAuth = strictPolicy()
	seedToken(t, st, "lct_member", "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	live := mustTicket(t, ts, "lct_member")
	if c, _, err := dialTicket(ctx, ts.URL, live, nil); err != nil {
		t.Fatalf("precondition: a member token's ticket opens the socket: %v", err)
	} else {
		c.CloseNow()
	}
	ticket := mustTicket(t, ts, "lct_member")
	if err := st.OperatorTokenDefSetRetiredAt(ctx, "def_alice", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, resp, err := dialTicket(ctx, ts.URL, ticket, nil); err == nil || refusedStatus(resp) != http.StatusUnauthorized {
		t.Errorf("retired token's ticket: err=%v status=%d, want 401", err, refusedStatus(resp))
	}
}

// Acceptance: a ticket never carries more than its credential. The digest the
// handshake compares changes with any widening of scopes, restrictions or
// authority mode, so a credential that changed since the mint is refused,
// never upgraded.
func TestTicketAuthority_ChangesWithAnyWidening(t *testing.T) {
	base := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		Restrictions: auth.Restrictions{Agents: []string{"allowed"}}}
	a0, err := ticketAuthority(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered := base
	reordered.Scopes = []string{auth.ScopeRunsRead, auth.ScopeRunsCreate}
	if a, _ := ticketAuthority(reordered); a != a0 {
		t.Error("scope order changed the digest")
	}
	wider := base
	wider.Scopes = append(slices.Clone(base.Scopes), auth.ScopeAdmin)
	unrestricted := base
	unrestricted.Restrictions = auth.Restrictions{}
	inherit := base
	inherit.InheritsAuthority = true
	for name, p := range map[string]auth.Principal{"wider scopes": wider, "restrictions lifted": unrestricted, "inherit": inherit} {
		if a, _ := ticketAuthority(p); a == a0 {
			t.Errorf("%s: same digest", name)
		}
	}
}

// mustTicketFor mints through the handler for a principal stamped directly.
func mustTicketFor(t *testing.T, s *Server, p auth.Principal) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, clientToolsTicketPath, nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	rec := httptest.NewRecorder()
	s.handleClientToolsTicket(rec, r)
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out["ticket"] == "" {
		t.Fatalf("mint = %d %s", rec.Code, rec.Body.String())
	}
	return out["ticket"]
}

// The mint route is gated as the socket is: a token limited to certain
// agents is refused with the socket's code, and one without runs:create
// with the scope refusal the socket gives.
func TestClientToolsTicket_MintGatedLikeTheSocket(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	enableTickets(s)
	ctx := context.Background()
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{DefID: "def_restricted", Name: "restricted",
		TenantID: "acme", Subject: "rob", TokenHash: auth.HashToken(testTokenPepper, "lct_restricted"),
		AllowedScopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, RestrictionsJSON: `{"agents":["allowed"]}`}); err != nil {
		t.Fatal(err)
	}
	seedToken(t, st, "lct_reader", "acme", "rita", []string{auth.ScopeRunsRead}, time.Time{})
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	for _, tok := range []string{"lct_restricted", "lct_reader"} {
		mintCode, mintBody := bearerCall(t, ts, tok, http.MethodPost, clientToolsTicketPath, "")
		req := httptest.NewRequest(http.MethodGet, "/v1/client-tools", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		s.authMiddleware(passthroughHandler(new(bool))).ServeHTTP(rec, req)
		if mintCode != rec.Code || mintCode != http.StatusForbidden || errCodeOf(mintBody) != errCodeOf(rec.Body.String()) {
			t.Errorf("%s: mint = %d %s, socket = %d %s; want the same 403", tok, mintCode, mintBody, rec.Code, rec.Body.String())
		}
	}
}

func errCodeOf(body string) string {
	var out struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.Code
}

// Without LOOMCYCLE_SECRET_KEY there are no tickets (503 ticket_unavailable);
// with the client-tool host off the mint answers as the socket does; in open
// mode the socket needs no credential, so there is nothing to mint.
func TestClientToolsTicket_UnavailableAnswers(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	enableTickets(s)
	seedToken(t, st, "lct_member", "acme", "alice", []string{auth.ScopeRunsCreate}, time.Time{})
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)

	s.cfg().Env.SecretKey = ""
	if code, body := bearerCall(t, ts, "lct_member", http.MethodPost, clientToolsTicketPath, ""); code != http.StatusServiceUnavailable || errCodeOf(body) != "ticket_unavailable" {
		t.Errorf("no secret key = %d %s, want 503 ticket_unavailable", code, body)
	}
	s.cfg().Env.SecretKey = ticketTestSecretKey
	reg := s.clientTools
	s.clientTools = nil
	if code, body := bearerCall(t, ts, "lct_member", http.MethodPost, clientToolsTicketPath, ""); code != http.StatusServiceUnavailable || errCodeOf(body) != "client_tools_unavailable" {
		t.Errorf("host off = %d %s, want 503 client_tools_unavailable", code, body)
	}
	s.clientTools = reg

	open, _ := tokenAuthServer(t, "")
	enableTickets(open)
	rec := httptest.NewRecorder()
	open.authMiddleware(http.HandlerFunc(open.handleClientToolsTicket)).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, clientToolsTicketPath, nil))
	if rec.Code != http.StatusServiceUnavailable || errCodeOf(rec.Body.String()) != "ticket_unavailable" {
		t.Errorf("open mode = %d %s, want 503 ticket_unavailable", rec.Code, rec.Body.String())
	}
}

// D4: a Web UI session mints too (CSRF applies), and its ticket stands only
// while the session does.
func TestClientToolsTicket_SessionMintedTicketFollowsTheSession(t *testing.T) {
	s, st, _ := sessionTestServer(t, "legacy-secret")
	enableTickets(s)
	seedToken(t, st, "lct_member", "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})
	cookie := mustSession(t, s, "lct_member")
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mint := func(csrf bool) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+clientToolsTicketPath, nil)
		req.AddCookie(cookie)
		req.Header.Set("User-Agent", testUA)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if csrf {
			req.Header.Set(uiRequestHeader, "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]string
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out["ticket"]
	}
	if code, _ := mint(false); code != http.StatusForbidden {
		t.Errorf("session mint without the CSRF header = %d, want 403", code)
	}
	code, ticket := mint(true)
	if code != http.StatusOK || ticket == "" {
		t.Fatalf("session mint = %d", code)
	}
	c, _, err := dialTicket(ctx, ts.URL, ticket, http.Header{"Origin": {"chrome-extension://abcdefghijklmnop"}})
	if err != nil {
		t.Fatalf("session ticket from another origin: %v", err)
	}
	c.CloseNow()

	_, ticket = mint(true)
	id, _ := base64.RawURLEncoding.DecodeString(cookie.Value)
	if _, err := st.UISessionRevoke(ctx, sessionHash(id), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, resp, err := dialTicket(ctx, ts.URL, ticket, nil); err == nil || refusedStatus(resp) != http.StatusUnauthorized {
		t.Errorf("ended session's ticket: err=%v status=%d, want 401", err, refusedStatus(resp))
	}
}

// Acceptance: the ticket appears in no log line at any level — through a
// mint, an accepted handshake, a replay and a tampered try — while the
// refusals still say why. Not parallel: it swaps the default logger.
func TestClientToolsTicket_NeverLogged(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f, _, ts, at := strictAppFixture(t)
	f.s.cfg().Env.AuthVerbose = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticket := mustTicket(t, ts, at)
	c, _, err := dialTicket(ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.CloseNow()
	_, _, _ = dialTicket(ctx, ts.URL, ticket, nil)
	_, _, _ = dialTicket(ctx, ts.URL, ticket+"x", nil)
	_, _ = bearerCall(t, ts, ticket, http.MethodGet, "/v1/_me", "")

	logs := buf.String()
	parts := strings.Split(ticket, ".")
	for name, secret := range map[string]string{"ticket": ticket, "payload": parts[1], "mac": parts[2]} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs hold the ticket's %s", name)
		}
	}
	for _, want := range []string{"reason=spent", "reason=bad_signature", "client-tools ticket accepted"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}
