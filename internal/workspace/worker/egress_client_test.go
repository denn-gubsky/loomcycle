package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workerlink"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// The bearers of the round trip. Neither is a real token.
const (
	egressWorkerToken = "tok-egress-worker"
	// egressSecret is the route's credential: it must reach the upstream and
	// nothing the worker receives.
	egressSecret = "tok-route-secret-7e1d-never-on-the-worker"
)

// rowStore is the dispatcher's view of the workspaces table, in memory.
type rowStore struct {
	mu   sync.Mutex
	rows []store.WorkspaceRow
}

func (s *rowStore) WorkspaceListByWorker(_ context.Context, workerID string, states ...store.WorkspaceState) ([]store.WorkspaceRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.WorkspaceRow
	for _, r := range s.rows {
		if r.WorkerID != workerID {
			continue
		}
		ok := len(states) == 0
		for _, st := range states {
			ok = ok || r.State == st
		}
		if ok {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *rowStore) WorkspaceDelete(context.Context, string, string) (bool, error) { return false, nil }
func (s *rowStore) WorkspaceCountActiveByWorker(context.Context, string) (int, error) {
	return 0, nil
}
func (s *rowStore) WorkspaceSetState(context.Context, string, string, store.WorkspaceState, store.WorkspaceState) (bool, error) {
	return false, nil
}
func (s *rowStore) GetRun(_ context.Context, id string) (store.Run, error) {
	return store.Run{}, &store.ErrNotFound{Kind: "run", ID: id}
}
func (s *rowStore) WorkspaceTouchByWorker(context.Context, string) (int, error) { return 0, nil }

// routeCreds resolves egressSecret for one credential, for targets under
// allowed, as credential.Engine does.
type routeCreds struct{ allowed []string }

func (c routeCreds) ResolveFor(_ context.Context, _, _, _, name, target string) (credential.Resolved, bool, error) {
	if name != "api_token" {
		return credential.Resolved{}, false, nil
	}
	if !credential.URLAllowed(&c.allowed, target) {
		return credential.Resolved{}, false, credential.ErrURLNotAllowed
	}
	return credential.Resolved{Value: egressSecret, AllowedURLs: &c.allowed}, true, nil
}

// tap records every message the worker's connection receives.
type tap struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (t *tap) interceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	s, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		return nil, err
	}
	return tappedStream{ClientStream: s, t: t}, nil
}

type tappedStream struct {
	grpc.ClientStream
	t *tap
}

func (s tappedStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if pm, ok := m.(proto.Message); ok && err == nil {
		b, _ := proto.Marshal(pm)
		s.t.mu.Lock()
		s.t.msgs = append(s.t.msgs, b)
		s.t.mu.Unlock()
	}
	return err
}

func (t *tap) contains(secret string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range t.msgs {
		if bytes.Contains(b, []byte(secret)) {
			return true
		}
	}
	return false
}

// egressRig is a dispatcher whose egress goes through a real proxy to a
// local TLS upstream, and a worker attached to it whose egress handler a
// session's requests are served by.
type egressRig struct {
	d        *dispatch.Dispatcher
	w        *worker
	st       *rowStore
	up       *httptest.Server
	upHits   atomic.Int32
	upAuth   atomic.Value
	upBodies atomic.Value
	tap      *tap
	session  *httptest.Server
}

func newEgressRig(t *testing.T, answer func(egress.Request) (*egress.Response, error)) *egressRig {
	t.Helper()
	g := &egressRig{st: &rowStore{}, tap: &tap{}}
	g.up = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.upHits.Add(1)
		g.upAuth.Store(r.Header.Get("Authorization"))
		b, _ := io.ReadAll(r.Body)
		g.upBodies.Store(string(b))
		w.Header().Set("Set-Cookie", "session=upstream")
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, "answer to "+r.Method+" "+r.URL.RequestURI())
	}))
	t.Cleanup(g.up.Close)
	allowed := []string{g.up.URL + "/api"}
	proxy, err := egress.New(egress.Options{
		Credentials:          routeCreds{allowed: allowed},
		PrivateHostAllowlist: []string{"127.0.0.1/32"},
		TLSConfig:            &tls.Config{RootCAs: g.up.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS12},
	})
	if err != nil {
		t.Fatal(err)
	}
	routes := []egress.Route{
		{Name: "api", URL: g.up.URL + "/api/", Methods: []string{"GET", "POST"}, Credential: "api_token"},
		{Name: "open", URL: g.up.URL + "/open/"},
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g.d = dispatch.New(dispatch.Options{})
	g.d.Activate(g.st, func(_ context.Context, bearer string) (auth.Principal, bool) {
		if bearer != egressWorkerToken {
			return auth.Principal{}, false
		}
		return auth.Principal{TenantID: "acme", Subject: "worker", Scopes: []string{auth.ScopeWorkersAttach}, TokenDefID: "def-w"}, true
	})
	// The gate the runtime wires (lifecycle.Manager.Egress) in small: the
	// grant is the row's tenant and run and the routes its row names.
	g.d.SetEgress(func(ctx context.Context, row store.WorkspaceRow, req egress.Request) (*egress.Response, error) {
		if answer != nil {
			return answer(req)
		}
		gr := egress.Grant{TenantID: row.TenantID, UserID: row.UserID, RunID: row.OwnerRunID, WorkspaceID: row.ID}
		for _, rt := range routes {
			for _, name := range row.Egress {
				if rt.Name == name {
					gr.Routes = append(gr.Routes, rt)
				}
			}
		}
		return proxy.Do(ctx, gr, req)
	})
	go func() { _ = g.d.Serve(l) }()
	t.Cleanup(g.d.Stop)

	conn, err := grpc.NewClient(l.Addr().String(), append(workerlink.DialOptions(),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithStreamInterceptor(g.tap.interceptor))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	g.w = &worker{opt: Options{Token: egressWorkerToken, ID: "w1", Root: t.TempDir(), Engine: workspace.EngineHost, Capacity: 4, IdleStreams: 1},
		log: slog.Default(), engine: &engine.Host{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = g.w.session(ctx, conn, slog.Default())
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for g.w.links.pick() == nil || len(g.d.Workers()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker did not attach")
		}
		time.Sleep(5 * time.Millisecond)
	}
	g.st.mu.Lock()
	g.st.rows = append(g.st.rows,
		store.WorkspaceRow{ID: "ws_a", TenantID: "acme", UserID: "alice", WorkerID: "tenant:acme/w1", OwnerRunID: "run-a", State: store.WorkspaceReady, Egress: []string{"api"}},
		store.WorkspaceRow{ID: "ws_b", TenantID: "acme", UserID: "bob", WorkerID: "tenant:acme/w1", OwnerRunID: "run-b", State: store.WorkspaceReady, Egress: []string{"open"}},
	)
	g.st.mu.Unlock()
	// The session's endpoint as PR 4b serves it: one per workspace.
	g.session = httptest.NewServer(g.w.egressHandler("ws_a"))
	t.Cleanup(g.session.Close)
	return g
}

// rawRequest sends a request line exactly as written, so the path's escapes
// reach the worker unchanged, and returns the answer.
func rawRequest(t *testing.T, srv *httptest.Server, method, target, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Opaque = target
	if body == "" {
		req.Body = nil
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// A session's request goes worker → dispatcher → proxy → upstream, with the
// route's credential added in the core: the upstream sees it, and nothing
// the worker's connection received carries it. The answer comes back
// cleaned (no Set-Cookie).
func TestEgressHandler_RoundTripCarriesTheCredentialOnlyPastTheCore(t *testing.T) {
	g := newEgressRig(t, nil)
	resp, body := rawRequest(t, g.session, http.MethodPost, "/api/deals/%41?since=2026", `{"n":1}`)
	if resp.StatusCode != http.StatusOK || body != "answer to POST /api/deals/%41?since=2026" {
		t.Fatalf("answer = %d %q", resp.StatusCode, body)
	}
	if got, _ := g.upAuth.Load().(string); got != "Bearer "+egressSecret {
		t.Fatalf("the upstream saw Authorization %q", got)
	}
	if got, _ := g.upBodies.Load().(string); got != `{"n":1}` {
		t.Errorf("the upstream got body %q", got)
	}
	if resp.Header.Get("X-Upstream") != "yes" || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("answer headers = %v", resp.Header)
	}
	if g.tap.contains(egressSecret) {
		t.Fatal("the route's credential reached the worker")
	}
	for name, vals := range resp.Header {
		for _, v := range vals {
			if strings.Contains(v, egressSecret) {
				t.Fatalf("the session's answer carries the credential in %s", name)
			}
		}
	}
}

// A route the workspace's row does not name is refused before any network
// call, though another workspace on the same worker holds it; the session
// reads the refusal as an HTTP 403.
func TestEgressHandler_ARouteNotOnTheRowIsRefusedBeforeAnyNetworkCall(t *testing.T) {
	g := newEgressRig(t, nil)
	resp, body := rawRequest(t, g.session, http.MethodGet, "/open/x", "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"open"`) {
		t.Fatalf("answer = %d %q, want a 403 naming the route", resp.StatusCode, body)
	}
	if n := g.upHits.Load(); n != 0 {
		t.Fatalf("the upstream was reached %d times", n)
	}
}

// What the handler refuses itself, before the core: a target that is not
// /<route>/<path>, a malformed route name, a proxy-form request.
func TestEgressHandler_RefusesWhatIsNoRouteRequestLocally(t *testing.T) {
	g := newEgressRig(t, nil)
	for _, target := range []string{"/", "/api", "/API/x", "/../api/x", "/api%2fx/y", "http://egress/api/x"} {
		resp, _ := rawRequest(t, g.session, http.MethodGet, target, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", target, resp.StatusCode)
		}
	}
	if n := g.upHits.Load(); n != 0 {
		t.Fatalf("the upstream was reached %d times", n)
	}
}

// An answer the core cuts midway ends the session's connection, so its
// client sees an error rather than a short answer that looks whole.
func TestEgressHandler_ACutAnswerBreaksTheSessionsRead(t *testing.T) {
	g := newEgressRig(t, func(egress.Request) (*egress.Response, error) {
		return &egress.Response{Status: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(io.MultiReader(
			strings.NewReader("partial"), errReader{errors.New("upstream went away")}))}, nil
	})
	req, err := http.NewRequest(http.MethodGet, g.session.URL+"/api/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.session.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("a cut answer read as whole")
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
