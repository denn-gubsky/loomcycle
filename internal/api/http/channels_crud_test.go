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
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// channelCRUDFixture builds a Server wired against an in-memory sqlite
// store + a non-system "team-updates" channel (scope=global) + a per-user
// "inbox" channel (scope=user). Covers both the admin and per-user
// URL families.
func channelCRUDFixture(t *testing.T) (*Server, store.Store, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Channels: map[string]config.Channel{
			"team-updates": {Scope: "global", Semantic: "broadcast", MaxMessages: 100},
			"inbox":        {Scope: "user", Semantic: "broadcast", MaxMessages: 100},
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
	return srv, s, func() { _ = s.Close() }
}

// TestAdminChannelPublish_HappyPath verifies a bearer-authed POST to
// the admin publish endpoint stores the message + returns the
// expected wire shape.
func TestAdminChannelPublish_HappyPath(t *testing.T) {
	srv, s, cleanup := channelCRUDFixture(t)
	defer cleanup()

	body := `{"payload":{"event":"hello","ts":"2026-05-22T12:00:00Z"}}`
	req := authedRequest("POST", "/v1/_channels/team-updates/publish", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp connector.ChannelPublishResult
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.MsgID, "msg_") {
		t.Errorf("MsgID = %q, want msg_ prefix", resp.MsgID)
	}
	if resp.Channel != "team-updates" {
		t.Errorf("Channel = %q, want team-updates", resp.Channel)
	}

	// Verify the row landed at scope=global, scope_id="".
	rows, err := s.ChannelPeek(t.Context(), "team-updates", store.MemoryScopeGlobal, "", "", 10)
	if err != nil {
		t.Fatalf("ChannelPeek: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != resp.MsgID {
		t.Errorf("store rows = %+v, want one matching msg_id", rows)
	}
}

// TestAdminChannelPublish_RefusesUndeclared ensures channels not in
// `cfg.Channels` are rejected at the HTTP boundary with 404 (same
// posture as the in-band tool's resolveChannel).
func TestAdminChannelPublish_RefusesUndeclared(t *testing.T) {
	srv, _, cleanup := channelCRUDFixture(t)
	defer cleanup()

	body := `{"payload":{}}`
	req := authedRequest("POST", "/v1/_channels/not-declared/publish", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (channel_not_declared); body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminChannelPublish_RequiresBearer guards the auth middleware
// wiring on the new route.
func TestAdminChannelPublish_RequiresBearer(t *testing.T) {
	srv, _, cleanup := channelCRUDFixture(t)
	defer cleanup()

	req := httptest.NewRequest("POST", "/v1/_channels/team-updates/publish", strings.NewReader(`{"payload":{}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminChannelSubscribe_HappyPath publishes then subscribes; the
// batch comes back with the published message + a next_cursor that
// matches the auto-commit advance.
func TestAdminChannelSubscribe_HappyPath(t *testing.T) {
	srv, _, cleanup := channelCRUDFixture(t)
	defer cleanup()

	// Publish via the HTTP path so the bus.Notify wire is exercised.
	pubBody := `{"payload":{"event":"first"}}`
	pubReq := authedRequest("POST", "/v1/_channels/team-updates/publish", strings.NewReader(pubBody))
	pubRec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusOK {
		t.Fatalf("publish prereq failed: %d %s", pubRec.Code, pubRec.Body.String())
	}

	// Subscribe — no wait_ms, so this is a one-shot read.
	subReq := authedRequest("POST", "/v1/_channels/team-updates/subscribe", strings.NewReader(`{}`))
	subRec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(subRec, subReq)

	if subRec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", subRec.Code, subRec.Body.String())
	}
	var resp connector.ChannelSubscribeResult
	if err := json.NewDecoder(subRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("Messages count = %d, want 1; resp=%+v", len(resp.Messages), resp)
	}
	if resp.NextCursor == "" {
		t.Error("NextCursor empty — expected auto-commit advance")
	}
	if !strings.Contains(string(resp.Messages[0].Value), "first") {
		t.Errorf("Messages[0].Value = %s", string(resp.Messages[0].Value))
	}
}

// TestAdminChannelPeek_DoesNotAdvanceCursor ensures peek is non-
// destructive: a subsequent subscribe still returns the same message.
func TestAdminChannelPeek_DoesNotAdvanceCursor(t *testing.T) {
	srv, _, cleanup := channelCRUDFixture(t)
	defer cleanup()

	// Seed one message.
	pubReq := authedRequest("POST", "/v1/_channels/team-updates/publish", strings.NewReader(`{"payload":{"k":"v"}}`))
	pubRec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(pubRec, pubReq)

	// Peek twice — both should return the message.
	for i := 0; i < 2; i++ {
		req := authedRequest("GET", "/v1/_channels/team-updates/peek?max_messages=5", nil)
		rec := httptest.NewRecorder()
		srv.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("peek %d: status = %d, body=%s", i, rec.Code, rec.Body.String())
		}
		var resp connector.ChannelPeekResult
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if len(resp.Messages) != 1 {
			t.Errorf("peek %d: got %d messages, want 1", i, len(resp.Messages))
		}
	}
}

// TestUserChannelPublish_ScopesByPath verifies the per-user route
// stores a message at scope=user with scope_id derived from the URL
// path — not from any body field a caller could forge.
func TestUserChannelPublish_ScopesByPath(t *testing.T) {
	srv, s, cleanup := channelCRUDFixture(t)
	defer cleanup()

	body := `{"payload":{"to":"alice","subject":"hi"}}`
	req := authedRequest("POST", "/v1/users/alice/channels/inbox/publish", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Check scope=user, scope_id="alice".
	rows, err := s.ChannelPeek(t.Context(), "inbox", store.MemoryScopeUser, "alice", "", 10)
	if err != nil {
		t.Fatalf("ChannelPeek scope=user alice: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("scope=user alice rows = %d, want 1", len(rows))
	}

	// And the global scope row should NOT have this message.
	rowsG, _ := s.ChannelPeek(t.Context(), "inbox", store.MemoryScopeGlobal, "", "", 10)
	if len(rowsG) != 0 {
		t.Errorf("scope=global rows = %d, want 0 (per-user routes must scope by URL path)", len(rowsG))
	}
}

// TestUserChannelPublish_RefusesInvalidUserID guards the user_id
// regex check we apply at handler entry.
func TestUserChannelPublish_RefusesInvalidUserID(t *testing.T) {
	srv, _, cleanup := channelCRUDFixture(t)
	defer cleanup()

	req := authedRequest("POST", "/v1/users/alice@bob/channels/inbox/publish", strings.NewReader(`{"payload":{}}`))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (invalid_user_id)", rec.Code)
	}
}

// TestAdminChannelAck_CursorRegression maps the typed store error to
// HTTP 409 — the wire contract for monotonic-cursor violations. Uses
// an explicit older cursor (encoded at unix epoch) because cur_0 /
// empty cursor are treated as "no-op" by the store, not regression.
func TestAdminChannelAck_CursorRegression(t *testing.T) {
	srv, _, cleanup := channelCRUDFixture(t)
	defer cleanup()

	// Publish + subscribe (auto-commits) to advance the cursor.
	pubReq := authedRequest("POST", "/v1/_channels/team-updates/publish", strings.NewReader(`{"payload":{"x":1}}`))
	srv.Mux().ServeHTTP(httptest.NewRecorder(), pubReq)
	subReq := authedRequest("POST", "/v1/_channels/team-updates/subscribe", strings.NewReader(`{}`))
	subRec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(subRec, subReq)

	// Build a cursor at unix epoch — guaranteed older than the
	// just-committed one (which encodes "now-ish").
	older := store.EncodeChannelCursor(time.Unix(0, 0), "msg_000000000000000000000000")
	ackBody := `{"cursor":"` + older + `"}`
	ackReq := authedRequest("POST", "/v1/_channels/team-updates/ack", strings.NewReader(ackBody))
	ackRec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(ackRec, ackReq)

	if ackRec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (channel_cursor_regression); body=%s", ackRec.Code, ackRec.Body.String())
	}
}

// TestAdminChannelSubscribe_ZeroWaitMSReturnsImmediately confirms that
// wait_ms=0 (the poll-and-return shape) does NOT block waiting for new
// messages. Without this guard, a future regression that always called
// Bus.Wait could hang n8n workers indefinitely on empty channels.
func TestAdminChannelSubscribe_ZeroWaitMSReturnsImmediately(t *testing.T) {
	srv, _, cleanup := channelCRUDFixture(t)
	defer cleanup()

	start := time.Now()
	req := authedRequest("POST", "/v1/_channels/team-updates/subscribe", strings.NewReader(`{"wait_ms":0}`))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Empty channel + wait_ms=0 → must return well under the
	// ChannelsLongPollCapMS (1000ms in this fixture). A real return
	// is sub-millisecond; we give 250ms of headroom for slow CI.
	if elapsed > 250*time.Millisecond {
		t.Errorf("wait_ms=0 on empty channel took %v — should return immediately", elapsed)
	}
	var resp connector.ChannelSubscribeResult
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Messages) != 0 {
		t.Errorf("Messages = %d, want 0 (empty channel)", len(resp.Messages))
	}
}

// TestAdminChannelSubscribe_LongPollCappedToOperatorLimit pins that a
// caller-requested wait_ms exceeding the operator's configured
// ChannelsLongPollCapMS is clamped to the cap. Fixture sets the cap
// to 1000ms; we request 999999ms (~16 minutes) and assert the call
// returns within ~1500ms (the cap plus generous slack).
func TestAdminChannelSubscribe_LongPollCappedToOperatorLimit(t *testing.T) {
	srv, _, cleanup := channelCRUDFixture(t)
	defer cleanup()

	start := time.Now()
	req := authedRequest("POST", "/v1/_channels/team-updates/subscribe", strings.NewReader(`{"wait_ms":999999}`))
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// The fixture's ChannelsLongPollCapMS = 1000ms. With 1500ms of
	// slack the test stays robust on slow CI while failing loudly if
	// the cap is bypassed (a regression would block for ~999s).
	if elapsed > 1500*time.Millisecond {
		t.Errorf("subscribe wait_ms=999999 took %v — operator cap (1000ms) not enforced", elapsed)
	}
	var resp connector.ChannelSubscribeResult
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Messages) != 0 {
		t.Errorf("Messages = %d on empty channel, want 0", len(resp.Messages))
	}
}
