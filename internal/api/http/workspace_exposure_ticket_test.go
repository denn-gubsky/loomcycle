package http

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC GW P9 PR 5, D-P9-42 as ruled 2026-11-25: a one-time ticket opens a
// page's socket, for a browser in strict sign-in mode, as RFC GJ's ticket
// opens the client-tools socket.

const (
	exposureB = "bbbbbbbbbbbbbbbbbbbbbbbb" // alice's second page
	exposureC = "cccccccccccccccccccccccc" // bob's page
)

type ticketFixture struct {
	f  *exposureFixture
	s  *Server
	st store.Store
	ts *httptest.Server
	ws *wsSeen
}

// newTicketFixture: alice's page A (the fixture's) and B, bob's page C, and
// the route and the ticket route behind the real auth middleware, with
// tickets on; strict sets strict sign-in mode.
func newTicketFixture(t *testing.T, strict bool, recheck time.Duration) *ticketFixture {
	t.Helper()
	f := newExposureFixture(t)
	seen := &wsSeen{}
	f.wsApp = echoSocket(seen)
	ctx := context.Background()
	for _, w := range []struct{ ws, user, ex string }{{"ws_2", "alice", exposureB}, {"ws_3", "bob", exposureC}} {
		if _, err := f.st.WorkspaceCreate(ctx, store.WorkspaceRow{ID: w.ws, TenantID: "acme", UserID: w.user, Name: "web",
			WorkerID: "wk1", Engine: "host", OwnerRunID: "run_" + w.ws, RootRunID: "run_" + w.ws, State: store.WorkspaceReady}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.WorkspaceExposureCreate(ctx, store.WorkspaceExposure{ID: w.ex, TenantID: "acme", UserID: w.user,
			WorkspaceID: w.ws, Port: 8000, Kind: store.WorkspaceExposurePage}, 2); err != nil {
			t.Fatal(err)
		}
	}
	s, st := tokenAuthServer(t, "legacy-secret")
	for _, who := range []string{"alice", "bob"} {
		if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
			DefID: "def_" + who, Name: who, TenantID: "acme", Subject: who,
			TokenHash: auth.HashToken(testTokenPepper, who+"-token"), AllowedScopes: []string{auth.ScopeRunsRead},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if h, err := s.hasher(); err == nil {
		s.SetTokenHasher(h)
	}
	s.cfg().Env.SecretKey = ticketTestSecretKey
	if strict {
		s.cfg().ResolvedAuth = strictPolicy()
	}
	s.SetWorkspaceExposures(testExposures{Manager: f.m, f: f}, config.WorkspaceExposeConfig{MaxConnections: 8,
		RequestsPerSecond: 100, BytesPerSecond: 1 << 30, MaxMessageBytes: 1 << 20, IdleTimeoutS: 600, SocketLifetimeS: 3600})
	s.exposures.socketRecheck = recheck
	mux := http.NewServeMux()
	mux.Handle(exposureRoutePattern, s.authMiddleware(http.HandlerFunc(s.handleWorkspaceExposure)))
	mux.Handle("POST /v1/workspaces/exposure-tickets/{id}", s.authMiddleware(http.HandlerFunc(s.handleExposureTicket)))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &ticketFixture{f: f, s: s, st: st, ts: ts, ws: seen}
}

// mint asks for a ticket for exposure id with bearer.
func (tf *ticketFixture) mint(t *testing.T, bearer, id string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", tf.ts.URL+"/v1/workspaces/exposure-tickets/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Ticket string `json:"ticket"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Ticket
}

func (tf *ticketFixture) mustMint(t *testing.T, bearer, id string) string {
	t.Helper()
	code, ticket := tf.mint(t, bearer, id)
	if code != http.StatusOK || ticket == "" {
		t.Fatalf("mint for %s: %d", id, code)
	}
	return ticket
}

// dial opens page id's socket offering the marker and the given entries.
func (tf *ticketFixture) dial(t *testing.T, id string, entries ...string) (*websocket.Conn, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(tf.ts.URL, "http")+exposurePrefix+id+"/ws", &websocket.DialOptions{
		Subprotocols: append([]string{exposureSocketMarker}, entries...)})
	if err != nil {
		if resp == nil {
			t.Fatalf("dial: %v", err)
		}
		return nil, resp.StatusCode
	}
	t.Cleanup(func() { c.CloseNow() })
	return c, http.StatusSwitchingProtocols
}

// In strict mode a ticket opens the page's socket, once: the same ticket is
// refused the second time. The ticket is never offered to the application.
// Without a ticket, strict mode still refuses a bearer subprotocol.
func TestExposureTicket_OpensTheSocketOnceInStrictMode(t *testing.T) {
	tf := newTicketFixture(t, true, time.Hour)
	ticket := tf.mustMint(t, "alice-token", testExposureID)
	c, code := tf.dial(t, testExposureID, "ticket."+ticket)
	if c == nil {
		t.Fatalf("a fresh ticket: %d, want the socket", code)
	}
	ctx := context.Background()
	if err := c.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := c.Read(ctx); err != nil || string(data) != "hi" {
		t.Fatalf("echo: %q %v", data, err)
	}
	tf.ws.mu.Lock()
	protos := tf.ws.protos
	tf.ws.mu.Unlock()
	if strings.Contains(protos, "ticket.") {
		t.Errorf("the application was offered the ticket: %q", protos)
	}
	if _, code := tf.dial(t, testExposureID, "ticket."+ticket); code != http.StatusUnauthorized {
		t.Errorf("a ticket used twice = %d, want 401", code)
	}
	if _, code := tf.dial(t, testExposureID, "bearer.alice-token"); code != http.StatusUnauthorized {
		t.Errorf("strict mode with a bearer subprotocol = %d, want 401", code)
	}
}

// A ticket past its 30 seconds is refused; the same ticket within them
// still opens the socket.
func TestExposureTicket_ExpiredRefused(t *testing.T) {
	tf := newTicketFixture(t, true, time.Hour)
	ticket := tf.mustMint(t, "alice-token", testExposureID)
	key, err := tf.s.clientToolsTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	c, reason := verifyTicket(key, exposureTicketFormat, ticket)
	if reason != "" {
		t.Fatalf("verify own ticket: %s", reason)
	}
	c.Exp = time.Now().Add(-time.Second).UnixMilli()
	expired, err := signTicket(key, exposureTicketFormat, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, code := tf.dial(t, testExposureID, "ticket."+expired); code != http.StatusUnauthorized {
		t.Errorf("an expired ticket = %d, want 401", code)
	}
	if c, code := tf.dial(t, testExposureID, "ticket."+ticket); c == nil {
		t.Errorf("the live ticket = %d, want the socket", code)
	}
}

// A ticket opens only the page it was minted for, even another page of the
// same user, and never a client-tools socket's ticket (another format).
func TestExposureTicket_BoundToItsPage(t *testing.T) {
	tf := newTicketFixture(t, true, time.Hour)
	ticket := tf.mustMint(t, "alice-token", testExposureID)
	if _, code := tf.dial(t, exposureB, "ticket."+ticket); code != http.StatusUnauthorized {
		t.Errorf("A's ticket on B = %d, want 401", code)
	}
	key, err := tf.s.clientToolsTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := verifyTicket(key, ticketFormat, ticket); reason == "" {
		t.Error("a page ticket verifies as a client-tools ticket")
	}
}

// Another user can neither mint a ticket for alice's page (the unknown
// id's 404) nor open it with a ticket for a page of their own.
func TestExposureTicket_AnotherUsersRefused(t *testing.T) {
	tf := newTicketFixture(t, true, time.Hour)
	if code, _ := tf.mint(t, "bob-token", testExposureID); code != http.StatusNotFound {
		t.Errorf("bob minting for alice's page = %d, want 404", code)
	}
	bobs := tf.mustMint(t, "bob-token", exposureC)
	if _, code := tf.dial(t, testExposureID, "ticket."+bobs); code != http.StatusUnauthorized {
		t.Errorf("bob's ticket on alice's page = %d, want 401", code)
	}
	if tf.ws.count() != 0 {
		t.Errorf("the application saw %d handshakes", tf.ws.count())
	}
}

// A ticket is minted only by a request its bearer header authenticated: a
// Web UI cookie mints none, so a page of another site cannot have one
// minted in the user's name.
func TestExposureTicket_NeverMintedByACookie(t *testing.T) {
	tf := newTicketFixture(t, true, time.Hour)
	r := httptest.NewRequest("POST", "/v1/workspaces/exposure-tickets/"+testExposureID, nil)
	r.SetPathValue("id", testExposureID)
	r = r.WithContext(withUIAuth(auth.WithPrincipal(r.Context(), *alice), uiAuth{}))
	w := httptest.NewRecorder()
	tf.s.handleExposureTicket(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a cookie-authenticated mint = %d, want 401", w.Code)
	}
}

// A socket a ticket opened re-checks the credential that minted it, without
// spending the ticket again: it stays open across rechecks, and closes with
// 1008 once that token is retired.
func TestExposureTicket_SocketFollowsTheMintingCredential(t *testing.T) {
	tf := newTicketFixture(t, true, 100*time.Millisecond)
	c, code := tf.dial(t, testExposureID, "ticket."+tf.mustMint(t, "alice-token", testExposureID))
	if c == nil {
		t.Fatalf("dial: %d", code)
	}
	ctx := context.Background()
	time.Sleep(500 * time.Millisecond)
	if err := c.Write(ctx, websocket.MessageText, []byte("still")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := c.Read(ctx); err != nil || string(data) != "still" {
		t.Fatalf("after rechecks the socket = %q %v, want it open", data, err)
	}
	if err := tf.st.OperatorTokenDefSetRetiredAt(ctx, "def_alice", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(rctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("after the minting token was retired the socket read %v, want 1008", err)
	}
}

// The ticket appears in no log line at any level — through a mint, an
// accepted handshake, a replay and a tampered try — while the refusals still
// say why. Not parallel: it swaps the default logger.
func TestExposureTicket_NeverLogged(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tf := newTicketFixture(t, true, time.Hour)
	ticket := tf.mustMint(t, "alice-token", testExposureID)
	if c, code := tf.dial(t, testExposureID, "ticket."+ticket); c == nil {
		t.Fatalf("dial: %d", code)
	}
	tf.dial(t, testExposureID, "ticket."+ticket)
	tf.dial(t, testExposureID, "ticket."+ticket+"x")

	logs := buf.String()
	parts := strings.Split(ticket, ".")
	for name, secret := range map[string]string{"ticket": ticket, "payload": parts[1], "mac": parts[2]} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs hold the ticket's %s", name)
		}
	}
	for _, want := range []string{"reason=spent", "reason=bad_signature", "page socket ticket accepted"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q", want)
		}
	}
}
