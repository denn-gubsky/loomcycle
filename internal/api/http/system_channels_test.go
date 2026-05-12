package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// systemChannelFixture builds a Server wired against an in-memory
// sqlite store + a Bus + SystemPublisher + the operator-declared
// `_system/alarms/critical` channel. Token is fixed for the tests
// so they can set the Authorization header.
func systemChannelFixture(t *testing.T) (*Server, store.Store, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Channels: map[string]config.Channel{
			"_system/alarms/critical": {
				Scope:       "global",
				Semantic:    "broadcast",
				DefaultTTL:  86400,
				MaxMessages: 1000,
				// Publisher is empty — admin endpoint accepts; agents
				// would also be allowed (but they hit the `_system/`
				// prefix refusal at the tool layer).
			},
			"_system/heartbeat-1m": {
				Scope:     "global",
				Semantic:  "broadcast",
				Publisher: "system",
				Period:    "1m",
			},
		},
		Env: config.Env{
			AuthToken:             "test-token",
			ChannelsMaxValueBytes: 64 * 1024,
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
	srv.SetSystemPublisher(&channels.StorePublisher{
		Store:     s,
		Bus:       bus,
		Scheduler: sched,
	})
	return srv, s, func() { _ = s.Close() }
}

// authedRequest builds an HTTP request with the test bearer attached.
func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestSystemChannelPublish_HappyPath(t *testing.T) {
	srv, s, cleanup := systemChannelFixture(t)
	defer cleanup()

	body := `{"payload": {"severity": "critical", "msg": "disk full"}}`
	req := authedRequest("POST", "/v1/_channels/_system/alarms/critical", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp["channel"] != "_system/alarms/critical" {
		t.Errorf("response channel = %v, want _system/alarms/critical", resp["channel"])
	}
	if id, _ := resp["msg_id"].(string); !strings.HasPrefix(id, "msg_") {
		t.Errorf("response msg_id = %v, want msg_ prefix", resp["msg_id"])
	}

	// Verify the message landed in storage with the admin sentinel.
	msgs, _, err := s.ChannelSubscribe(context.Background(),
		"_system/alarms/critical", store.MemoryScopeGlobal, "", "", 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("readback: msgs=%d err=%v", len(msgs), err)
	}
	if msgs[0].PublishedByUserID != "_admin" {
		t.Errorf("PublishedByUserID = %q, want _admin", msgs[0].PublishedByUserID)
	}
}

func TestSystemChannelPublish_RejectsNonSystemPrefix(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()

	body := `{"payload": {}}`
	req := authedRequest("POST", "/v1/_channels/regular-channel", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestSystemChannelPublish_RejectsUndeclaredChannel(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()

	body := `{"payload": {}}`
	req := authedRequest("POST", "/v1/_channels/_system/not-declared", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSystemChannelPublish_DeferredVisibleAtFutureDated(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()

	deferTo := time.Now().Add(60 * time.Second).UTC().Format(time.RFC3339Nano)
	body := `{"payload": {"k":"v"}, "deliver_at": "` + deferTo + `"}`
	req := authedRequest("POST", "/v1/_channels/_system/alarms/critical", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if _, ok := resp["visible_at"]; !ok {
		t.Errorf("response missing visible_at on deferred publish: %v", resp)
	}
}

func TestSystemChannelPublish_MissingPayloadRefused(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()

	body := `{}` // no payload
	req := authedRequest("POST", "/v1/_channels/_system/alarms/critical", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// PR 2 review fix: `{"payload": null}` must be rejected. Without the
// fix the JSON-null literal decoded into json.RawMessage as 4-byte
// `"null"`, passing both `len > 0` and `json.Valid` — silently
// storing a null payload despite the "payload is required" contract.
func TestSystemChannelPublish_NullPayloadRefused(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()

	body := `{"payload": null}`
	req := authedRequest("POST", "/v1/_channels/_system/alarms/critical", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing_payload") {
		t.Errorf("response should signal missing_payload; got %s", rec.Body.String())
	}
}

func TestSystemChannelPublish_UnauthorizedWithoutBearer(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()

	body := `{"payload": {}}`
	req := httptest.NewRequest("POST", "/v1/_channels/_system/alarms/critical", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSystemChannelPublish_503WithoutSystemPublisher(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := &config.Config{
		Channels: map[string]config.Channel{
			"_system/foo": {Scope: "global"},
		},
		Env: config.Env{AuthToken: "test-token"},
	}
	srv := &Server{
		cfg:            cfg,
		store:          s,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hooks.NewRegistry(),
		hookDispatcher: hooks.NewDispatcher(hooks.NewRegistry(), nil),
		sem:            concurrency.New(8, 16, 30000),
		// systemPublisher intentionally left nil.
	}

	body := `{"payload": {}}`
	req := authedRequest("POST", "/v1/_channels/_system/foo", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
