package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestListChannels_ReturnsDeclaredWithZeroCounts(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()

	req := authedRequest("GET", "/v1/_channels", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp channelsListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Channels) != 2 {
		t.Fatalf("got %d channels, want 2 (the two declared in fixture): %+v", len(resp.Channels), resp.Channels)
	}
	names := map[string]ChannelDescriptor{}
	for _, c := range resp.Channels {
		names[c.Name] = c
	}
	if d, ok := names["_system/alarms/critical"]; !ok || d.MessageCount != 0 {
		t.Errorf("_system/alarms/critical: %+v", d)
	}
	if d := names["_system/heartbeat-1m"]; d.Period != "1m" || d.Publisher != "system" {
		t.Errorf("heartbeat-1m metadata wrong: %+v", d)
	}
}

func TestListChannels_PopulatesCountsAfterPublish(t *testing.T) {
	srv, s, cleanup := systemChannelFixture(t)
	defer cleanup()

	// Publish two messages to the declared channel via the store
	// directly (the admin POST is exercised elsewhere).
	for i := 0; i < 2; i++ {
		if _, _, err := s.ChannelPublish(t.Context(), store.ChannelMessage{
			Channel: "_system/alarms/critical",
			Scope:   store.MemoryScopeGlobal,
			ScopeID: "global",
			Payload: []byte(`{"severity":"critical"}`),
		}, 1000); err != nil {
			t.Fatalf("ChannelPublish: %v", err)
		}
	}

	req := authedRequest("GET", "/v1/_channels", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp channelsListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range resp.Channels {
		if c.Name == "_system/alarms/critical" {
			if c.MessageCount != 2 {
				t.Errorf("message_count = %d, want 2", c.MessageCount)
			}
			if c.OldestVisibleAt.IsZero() || c.NewestVisibleAt.IsZero() {
				t.Errorf("visible_at bounds should be populated: %+v", c)
			}
			return
		}
	}
	t.Fatal("alarms/critical channel not present in response")
}

func TestListChannels_RequiresBearer(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/v1/_channels", nil)
	// no Authorization header
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
