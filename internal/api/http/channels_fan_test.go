package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// channelFanFixture builds a Server with three global channels c1/c2/c3
// for the RFC S client-twin fan-in (await) / fan-out (broadcast) tests.
func channelFanFixture(t *testing.T) (*Server, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Channels: map[string]config.Channel{
			"c1": {Scope: "global", Semantic: "queue", MaxMessages: 100},
			"c2": {Scope: "global", Semantic: "queue", MaxMessages: 100},
			"c3": {Scope: "global", Semantic: "queue", MaxMessages: 100},
		},
		Env: config.Env{
			AuthToken:             "test-token",
			ChannelsMaxValueBytes: 64 * 1024,
			ChannelsLongPollCapMS: 2000,
		},
	}
	hookReg := hooks.NewRegistry()
	bus := channels.NewBus()
	sched := channels.NewScheduler(bus, 100)
	srv := &Server{
		cfg:            cfg,
		store:          s,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
	srv.SetSystemPublisher(&channels.StorePublisher{Store: s, Bus: bus, Scheduler: sched})
	srv.SetChannelBus(bus)
	return srv, func() { _ = s.Close() }
}

func doJSON(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := authedRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)
	return rec
}

func TestChannelBroadcast_HTTP_FansOut(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	rec := doJSON(t, srv, "POST", "/v1/_channels/_broadcast", `{"channels":["c1","c2","c3"],"payload":{"go":1}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("broadcast: status %d (%s)", rec.Code, rec.Body.String())
	}
	var out connector.ChannelBroadcastResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Published != 3 || out.Failed != 0 {
		t.Errorf("published=%d failed=%d, want 3/0", out.Published, out.Failed)
	}
	// Each channel actually received it (peek via the admin route).
	for _, ch := range []string{"c1", "c2", "c3"} {
		prec := doJSON(t, srv, "GET", "/v1/_channels/"+ch+"/peek?from_cursor=cur_0&max_messages=10", "")
		var pk connector.ChannelPeekResult
		_ = json.NewDecoder(prec.Body).Decode(&pk)
		if len(pk.Messages) != 1 {
			t.Errorf("%s has %d messages, want 1", ch, len(pk.Messages))
		}
	}
}

func TestChannelBroadcast_HTTP_AtomicRefusal(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	// "ghost" is undeclared → 404, and NOTHING published (atomic pre-flight).
	rec := doJSON(t, srv, "POST", "/v1/_channels/_broadcast", `{"channels":["c1","ghost"],"payload":{"x":1}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("broadcast w/ undeclared channel: status %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	prec := doJSON(t, srv, "GET", "/v1/_channels/c1/peek?from_cursor=cur_0", "")
	var pk connector.ChannelPeekResult
	_ = json.NewDecoder(prec.Body).Decode(&pk)
	if len(pk.Messages) != 0 {
		t.Errorf("c1 got %d messages — broadcast must be all-or-nothing", len(pk.Messages))
	}
}

func TestChannelAwait_HTTP_AtLeastSync(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	doJSON(t, srv, "POST", "/v1/_channels/c1/publish", `{"payload":{"a":1}}`)
	doJSON(t, srv, "POST", "/v1/_channels/c2/publish", `{"payload":{"b":1}}`)

	rec := doJSON(t, srv, "POST", "/v1/_channels/_await", `{"channels":["c1","c2","c3"],"mode":"at_least","n":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("await: status %d (%s)", rec.Code, rec.Body.String())
	}
	var out connector.ChannelAwaitResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Satisfied || out.TimedOut {
		t.Errorf("satisfied=%v timed_out=%v, want true/false", out.Satisfied, out.TimedOut)
	}
	if out.TotalMessages < 2 {
		t.Errorf("total_messages=%d, want >=2", out.TotalMessages)
	}
	if len(out.Fired) != 2 {
		t.Errorf("fired=%v, want 2", out.Fired)
	}
}

func TestChannelAwait_HTTP_AllTimeout(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	doJSON(t, srv, "POST", "/v1/_channels/c1/publish", `{"payload":{"a":1}}`)
	// c2/c3 stay empty; mode=all can't be satisfied → timeout (NOT an error).
	start := time.Now()
	rec := doJSON(t, srv, "POST", "/v1/_channels/_await", `{"channels":["c1","c2","c3"],"mode":"all","wait_ms":200}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("await timeout must be 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Errorf("returned too fast (%v) — should wait ~200ms", time.Since(start))
	}
	var out connector.ChannelAwaitResult
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out.Satisfied || !out.TimedOut {
		t.Errorf("satisfied=%v timed_out=%v, want false/true", out.Satisfied, out.TimedOut)
	}
}

// A publish that lands mid-await wakes it well under the wait_ms budget —
// exercises the connector's register-before-read + select loop over the
// shared bus.
func TestChannelAwait_HTTP_LongPollWake(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	type res struct {
		code    int
		elapsed time.Duration
		out     connector.ChannelAwaitResult
	}
	done := make(chan res, 1)
	go func() {
		start := time.Now()
		rec := doJSON(t, srv, "POST", "/v1/_channels/_await", `{"channels":["c1","c2"],"mode":"any","wait_ms":5000}`)
		var out connector.ChannelAwaitResult
		_ = json.NewDecoder(rec.Body).Decode(&out)
		done <- res{rec.Code, time.Since(start), out}
	}()

	time.Sleep(50 * time.Millisecond)
	doJSON(t, srv, "POST", "/v1/_channels/c2/publish", `{"payload":{"woke":1}}`)

	select {
	case r := <-done:
		if r.code != http.StatusOK || !r.out.Satisfied {
			t.Fatalf("await: code=%d satisfied=%v", r.code, r.out.Satisfied)
		}
		if r.elapsed > 3*time.Second {
			t.Errorf("await took %v — should wake on publish, not wait 5s", r.elapsed)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("await did not return — wake missed")
	}
}
