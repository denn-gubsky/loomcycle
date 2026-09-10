package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// channelHoldFixture is channelCRUDFixture with one held channel and one
// ordinary one, and — importantly — the SystemPublisher wired the way main.go
// wires it, HoldFn included. Without that wiring the hold is a setting nothing
// reads, which is precisely the regression these tests guard.
func channelHoldFixture(t *testing.T) (*Server, store.Store, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Channels: map[string]config.Channel{
			"gate": {Scope: "global", Semantic: "queue", MaxMessages: 100, Hold: true},
			"open": {Scope: "global", Semantic: "queue", MaxMessages: 100},
		},
		Env: config.Env{
			AuthToken:             "test-token",
			ChannelsMaxValueBytes: 64 * 1024,
			ChannelsLongPollCapMS: 1000,
		},
	}
	hookReg := hooks.NewRegistry()
	bus := channels.NewBus()
	sched := channels.NewScheduler(bus, 100)
	srv := &Server{
		cfgHolder:      config.NewHolder(cfg),
		store:          s,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
	srv.SetSystemPublisher(&channels.StorePublisher{
		Store: s, Bus: bus, Scheduler: sched, HoldFn: srv.ChannelHeld,
	})
	srv.SetChannelBus(bus)
	return srv, s, func() { _ = s.Close() }
}

func postJSON(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = authedRequest("POST", path, nil)
	} else {
		r = authedRequest("POST", path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, r)
	return rec
}

func peekCount(t *testing.T, srv *Server, channel string) int {
	t.Helper()
	req := authedRequest("GET", "/v1/_channels/"+channel+"/peek?max_messages=50", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("peek %s: status %d (%s)", channel, rec.Code, rec.Body.String())
	}
	var out connector.ChannelPeekResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("peek decode: %v", err)
	}
	return len(out.Messages)
}

// The admin publish route honours a yaml `hold:` — stored, reported as held,
// not delivered. The control channel in the same fixture proves the route
// otherwise delivers.
func TestChannelHold_AdminPublishIsHeldNotDelivered(t *testing.T) {
	srv, _, cleanup := channelHoldFixture(t)
	defer cleanup()

	rec := postJSON(t, srv, "/v1/_channels/gate/publish", `{"payload":{"n":1}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: status %d (%s)", rec.Code, rec.Body.String())
	}
	var pub connector.ChannelPublishResult
	if err := json.Unmarshal(rec.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode publish: %v", err)
	}
	if !pub.Held {
		t.Errorf("publish to a hold: channel must report held:true, got %+v", pub)
	}
	if pub.VisibleAt != "" {
		t.Errorf("a held publish must not promise a visible_at, got %q", pub.VisibleAt)
	}
	if n := peekCount(t, srv, "gate"); n != 0 {
		t.Errorf("held message readable: %d, want 0", n)
	}

	if rec := postJSON(t, srv, "/v1/_channels/open/publish", `{"payload":{"n":1}}`); rec.Code != http.StatusOK {
		t.Fatalf("control publish: status %d (%s)", rec.Code, rec.Body.String())
	}
	if n := peekCount(t, srv, "open"); n != 1 {
		t.Errorf("control channel delivered %d, want 1", n)
	}
}

// POST /v1/_channels/{name}/release hands over the oldest N and says what is
// left. Allowed on a yaml channel — releasing moves messages, it does not
// mutate the definition.
func TestChannelHold_ReleaseEndpointDeliversOldestFirst(t *testing.T) {
	srv, _, cleanup := channelHoldFixture(t)
	defer cleanup()

	for _, n := range []string{"1", "2", "3"} {
		if rec := postJSON(t, srv, "/v1/_channels/gate/publish", `{"payload":{"n":`+n+`}}`); rec.Code != http.StatusOK {
			t.Fatalf("publish %s: status %d (%s)", n, rec.Code, rec.Body.String())
		}
	}

	// Empty body = release one.
	rec := postJSON(t, srv, "/v1/_channels/gate/release", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("release: status %d (%s)", rec.Code, rec.Body.String())
	}
	var out connector.ChannelReleaseResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode release: %v", err)
	}
	if out.ReleasedCount != 1 || out.StillHeld != 2 {
		t.Fatalf("released %d leaving %d, want 1 leaving 2", out.ReleasedCount, out.StillHeld)
	}
	if n := peekCount(t, srv, "gate"); n != 1 {
		t.Errorf("after one release, %d readable, want 1", n)
	}

	rec = postJSON(t, srv, "/v1/_channels/gate/release", `{"count":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("release rest: status %d (%s)", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.ReleasedCount != 2 || out.StillHeld != 0 {
		t.Errorf("drain released %d leaving %d, want 2 leaving 0", out.ReleasedCount, out.StillHeld)
	}
	if n := peekCount(t, srv, "gate"); n != 3 {
		t.Errorf("after draining, %d readable, want 3", n)
	}
}

// An undeclared channel 404s rather than silently releasing nothing — the same
// answer publish/subscribe give, so a typo is a typo everywhere.
func TestChannelHold_ReleaseUndeclaredChannelIs404(t *testing.T) {
	srv, _, cleanup := channelHoldFixture(t)
	defer cleanup()
	rec := postJSON(t, srv, "/v1/_channels/ghost/release", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("release on an undeclared channel: status %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

// The hold is a property of the CHANNEL, so a deliver_at cannot escape it: a
// caller asking for delivery in two seconds still waits for a release.
func TestChannelHold_DeliverAtDoesNotEscapeTheHold(t *testing.T) {
	srv, _, cleanup := channelHoldFixture(t)
	defer cleanup()

	rec := postJSON(t, srv, "/v1/_channels/gate/publish",
		`{"payload":{"n":1},"deliver_at":"2000-01-01T00:00:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: status %d (%s)", rec.Code, rec.Body.String())
	}
	var pub connector.ChannelPublishResult
	_ = json.Unmarshal(rec.Body.Bytes(), &pub)
	if !pub.Held {
		t.Errorf("a past deliver_at escaped the hold: %+v", pub)
	}
	if n := peekCount(t, srv, "gate"); n != 0 {
		t.Errorf("held message with a past deliver_at was delivered: %d", n)
	}
}

// A RUNTIME-declared channel holds too. This is the path the canvas uses —
// a workflow declares its own channels through the substrate, not through
// operator yaml — so the hold has to survive create → ChannelGet → publish,
// not just the yaml map.
func TestChannelHold_RuntimeDeclaredChannelHoldsAndReleases(t *testing.T) {
	srv, _, cleanup := channelHoldFixture(t)
	defer cleanup()

	rec := postJSON(t, srv, "/v1/_channels",
		`{"name":"wave-in","scope":"global","semantic":"queue","hold":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d (%s)", rec.Code, rec.Body.String())
	}
	var desc connector.ChannelDescriptor
	if err := json.Unmarshal(rec.Body.Bytes(), &desc); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !desc.Hold {
		t.Errorf("create response dropped hold: %+v", desc)
	}

	if rec := postJSON(t, srv, "/v1/_channels/wave-in/publish", `{"payload":{"n":1}}`); rec.Code != http.StatusOK {
		t.Fatalf("publish: status %d (%s)", rec.Code, rec.Body.String())
	}
	if n := peekCount(t, srv, "wave-in"); n != 0 {
		t.Errorf("runtime hold channel delivered %d, want 0", n)
	}

	rec = postJSON(t, srv, "/v1/_channels/wave-in/release", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("release: status %d (%s)", rec.Code, rec.Body.String())
	}
	if n := peekCount(t, srv, "wave-in"); n != 1 {
		t.Errorf("after release, %d readable, want 1", n)
	}
}
