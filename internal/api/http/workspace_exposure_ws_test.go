package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// RFC GW §5.3, P9 plan §3.3, PR 5: WebSocket through an exposure.

// wsSeen is what the application's socket handshake carried.
type wsSeen struct {
	mu                                  sync.Mutex
	host, origin, protos, authz, cookie string
	n                                   int
}

// echoSocket is an application socket that offers protocols, echoes every
// message, closes with 4001 "bye" on "close", and sets headers on its 101
// that must never reach the client.
func echoSocket(seen *wsSeen, protocols ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		seen.mu.Lock()
		seen.n++
		seen.host, seen.origin = r.Host, r.Header.Get("Origin")
		seen.protos, seen.authz, seen.cookie = r.Header.Get("Sec-WebSocket-Protocol"), r.Header.Get("Authorization"), r.Header.Get("Cookie")
		seen.mu.Unlock()
		w.Header().Set("Set-Cookie", "sid=s3cret")
		w.Header().Set("Clear-Site-Data", `"*"`)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: protocols, InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(64 << 20)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if string(data) == "close" {
				_ = c.Close(4001, "bye")
				return
			}
			if err := c.Write(context.Background(), typ, data); err != nil {
				return
			}
		}
	}
}

func (s *wsSeen) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// socketServer is the route behind a real server, as p (nil: no
// principal); cookie marks the request as a Web UI cookie's.
func socketServer(t *testing.T, f *exposureFixture, p *auth.Principal, cookie bool) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if p != nil {
			ctx = auth.WithPrincipal(ctx, *p)
		}
		if cookie {
			ctx = withUIAuth(ctx, uiAuth{})
		}
		f.mux.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// dialPage opens the page's socket as a browser would: the marker, the
// bearer as a subprotocol, and the application's own protocols.
func dialPage(t *testing.T, ts *httptest.Server, path string, extra ...string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+path, &websocket.DialOptions{
		Subprotocols: append([]string{exposureSocketMarker, "bearer.lc_test"}, extra...),
		HTTPHeader:   http.Header{"Origin": {"https://board.example"}},
	})
}

var socketPath = exposurePrefix + testExposureID + "/ws"

// Only the workspace's own user opens the page's socket: another user,
// another tenant, an admin, the legacy token and no principal get the
// unknown id's 404 before any upgrade, and the application sees no
// handshake. A Web UI cookie is refused, so a page of another site cannot
// open the socket with the user's cookie (cross-site WebSocket hijacking).
func TestExposureSocket_RefusesEveryCallerButTheWorkspacesOwnUser(t *testing.T) {
	f := newExposureFixture(t)
	seen := &wsSeen{}
	f.wsApp = echoSocket(seen)
	for name, p := range map[string]*auth.Principal{"another user": bob, "another tenant": aliceElse,
		"an admin": adminBob, "the legacy token": legacyRoot, "no principal": nil} {
		_, resp, err := dialPage(t, socketServer(t, f, p, false), socketPath)
		if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: err %v, status %v; want a 404 and no socket", name, err, resp)
		}
	}
	_, resp, err := dialPage(t, socketServer(t, f, alice, true), socketPath)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a cookie: err %v, status %v; want 401", err, resp)
	}
	if n := seen.count(); n != 0 {
		t.Fatalf("the application saw %d handshakes from refused callers", n)
	}
	c, _, err := dialPage(t, socketServer(t, f, alice, false), socketPath)
	if err != nil {
		t.Fatalf("the workspace's own user: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// Messages cross both ways, as they are; the application chooses among its
// own protocols and never sees the marker, the bearer, a cookie or an
// Authorization header; Host and Origin pass as sent; and nothing of the
// application's 101 reaches the client but the protocol it chose.
func TestExposureSocket_CarriesMessagesAndNoCredential(t *testing.T) {
	f := newExposureFixture(t)
	seen := &wsSeen{}
	f.wsApp = echoSocket(seen, "chat.v1")
	ts := socketServer(t, f, alice, false)
	c, resp, err := dialPage(t, ts, socketPath, "chat.v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if c.Subprotocol() != "chat.v1" {
		t.Errorf("subprotocol = %q, want the application's choice", c.Subprotocol())
	}
	for _, h := range []string{"Set-Cookie", "Clear-Site-Data"} {
		if v := resp.Header.Values(h); len(v) != 0 {
			t.Errorf("the application's %s reached the client: %v", h, v)
		}
	}
	ctx := context.Background()
	for _, m := range []struct {
		typ  websocket.MessageType
		data string
	}{{websocket.MessageText, "hello"}, {websocket.MessageBinary, "\x00\x01\xff"}} {
		if err := c.Write(ctx, m.typ, []byte(m.data)); err != nil {
			t.Fatal(err)
		}
		typ, data, err := c.Read(ctx)
		if err != nil || typ != m.typ || string(data) != m.data {
			t.Errorf("echo of %q = %v %q %v", m.data, typ, data, err)
		}
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.protos != "chat.v1" || seen.authz != "" || seen.cookie != "" {
		t.Errorf("the application saw protocols %q authorization %q cookie %q", seen.protos, seen.authz, seen.cookie)
	}
	if !strings.HasPrefix(seen.host, "127.0.0.1:") || seen.origin != "https://board.example" {
		t.Errorf("the application saw host %q origin %q, want the client's", seen.host, seen.origin)
	}
}

// An application that chooses no protocol is answered with the marker, so a
// browser that offered it connects (spike D's echo rule).
func TestExposureSocket_AnswersTheMarkerWhenTheApplicationChoosesNone(t *testing.T) {
	f := newExposureFixture(t)
	f.wsApp = echoSocket(&wsSeen{})
	c, _, err := dialPage(t, socketServer(t, f, alice, false), socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if c.Subprotocol() != exposureSocketMarker {
		t.Errorf("subprotocol = %q, want the marker", c.Subprotocol())
	}
}

// The application is dialled first: a port no server of the workspace
// listens on itself (the worker's #2917 check) is a plain 502, never a
// half-open socket.
func TestExposureSocket_APortNotServedIsAPlainBadGateway(t *testing.T) {
	f := newExposureFixture(t)
	f.dialErr = &dispatch.OpError{Class: workspace.ClassPortNotServed, Detail: "not listened on by a server of this workspace itself"}
	_, resp, err := dialPage(t, socketServer(t, f, alice, false), socketPath)
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Errorf("err %v, status %v; want a 502 and no socket", err, resp)
	}
}

// A close code and its reason cross as they are.
func TestExposureSocket_CarriesTheCloseCode(t *testing.T) {
	f := newExposureFixture(t)
	f.wsApp = echoSocket(&wsSeen{})
	c, _, err := dialPage(t, socketServer(t, f, alice, false), socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx := context.Background()
	_ = c.Write(ctx, websocket.MessageText, []byte("close"))
	_, _, err = c.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != 4001 || ce.Reason != "bye" {
		t.Errorf("close = %v, want 4001 bye", err)
	}
}

// An open socket closes when its exposure does, found at the next recheck:
// unexpose, and the workspace's release. (The operator's switch is boot
// configuration; a restart ends every socket.)
func TestExposureSocket_ClosesWhenItsExposureCloses(t *testing.T) {
	ctx := context.Background()
	for name, close := range map[string]func(f *exposureFixture){
		"unexpose": func(f *exposureFixture) { _, _ = f.st.WorkspaceExposureDelete(ctx, "acme", testExposureID) },
		"release": func(f *exposureFixture) {
			_, _ = f.st.WorkspaceSetState(ctx, "acme", "ws_1", "ready", "releasing")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newExposureFixture(t)
			f.wsApp = echoSocket(&wsSeen{})
			f.recheck = 50 * time.Millisecond
			f.rebuild()
			c, _, err := dialPage(t, socketServer(t, f, alice, false), socketPath)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			close(f)
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if _, _, err := c.Read(rctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Errorf("after %s the socket read %v, want a close with 1008", name, err)
			}
		})
	}
}

// The bounds: a message over max_message_bytes closes the socket with 1009;
// a socket counts against max_connections while it is open; one quiet for
// the idle timeout, and one open past its lifetime, is closed.
func TestExposureSocket_KeepsItsBounds(t *testing.T) {
	ctx := context.Background()
	t.Run("message size", func(t *testing.T) {
		f := newExposureFixture(t)
		f.wsApp = echoSocket(&wsSeen{})
		f.lim.MaxMessageBytes = 1024
		f.rebuild()
		c, _, err := dialPage(t, socketServer(t, f, alice, false), socketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		_ = c.Write(ctx, websocket.MessageBinary, make([]byte, 4096))
		if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
			t.Errorf("a message over the bound: %v, want 1009", err)
		}
	})
	t.Run("connections", func(t *testing.T) {
		f := newExposureFixture(t)
		f.wsApp = echoSocket(&wsSeen{})
		f.lim.MaxConnections = 1
		f.rebuild()
		ts := socketServer(t, f, alice, false)
		c, _, err := dialPage(t, ts, socketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		if _, resp, err := dialPage(t, ts, socketPath); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("a second socket past max_connections 1: %v %v, want 429", err, resp)
		}
	})
	for name, set := range map[string]func(*exposeRouteLimits){
		"idle":     func(l *exposeRouteLimits) { l.SocketIdle = 200 * time.Millisecond },
		"lifetime": func(l *exposeRouteLimits) { l.SocketLifetime = 300 * time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			f := newExposureFixture(t)
			f.wsApp = echoSocket(&wsSeen{})
			set(&f.lim)
			f.rebuild()
			c, _, err := dialPage(t, socketServer(t, f, alice, false), socketPath)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if name == "lifetime" {
				// Busy, so only the lifetime can end it.
				go func() {
					for rctx.Err() == nil {
						if c.Write(rctx, websocket.MessageText, []byte("x")) != nil {
							return
						}
						time.Sleep(20 * time.Millisecond)
					}
				}()
			}
			for {
				_, _, err := c.Read(rctx)
				if err == nil {
					continue
				}
				if st := websocket.CloseStatus(err); st != websocket.StatusGoingAway {
					t.Errorf("%s: the socket ended with %v, want a close with 1001", name, err)
				}
				break
			}
		})
	}
}
