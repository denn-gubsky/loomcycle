package http

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runstate"
)

// streamFixture extends systemChannelFixture with a runstate.Bus
// installed. Returns the server, the bus, and a cleanup func.
func streamFixture(t *testing.T) (*Server, *runstate.Bus, func()) {
	t.Helper()
	srv, _, cleanup := systemChannelFixture(t)
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	return srv, bus, cleanup
}

// readSSEFrame parses one event frame from a bufio.Reader. Returns
// (eventName, data, ok). Blocks until a complete frame arrives or the
// reader returns an error.
func readSSEFrame(t *testing.T, r *bufio.Reader) (string, string, bool) {
	t.Helper()
	var ev, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", "", false
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if ev != "" || data != "" {
				return ev, data, true
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			ev = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			if data != "" {
				data += "\n"
			}
			data += strings.TrimPrefix(line, "data: ")
		}
		// `:` comment lines (keepalives) ignored
	}
}

// openStream starts an httptest.NewServer-backed SSE connection and
// returns a bufio.Reader over the response body plus a cancel func.
// The caller must call cancel() to clean up.
func openStream(t *testing.T, srv *Server, userID, queryString string) (*bufio.Reader, context.CancelFunc, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(srv.Mux())
	target := ts.URL + "/v1/users/" + userID + "/agents/stream"
	if queryString != "" {
		target += "?" + queryString
	}
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() {
		_ = resp.Body.Close()
		ts.Close()
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return bufio.NewReader(resp.Body), cancel, ts
}

func TestStreamUserAgents_EmitsStreamOpenAndRunStateEvents(t *testing.T) {
	srv, bus, cleanup := streamFixture(t)
	defer cleanup()

	reader, cancel, _ := openStream(t, srv, "user-a", "")
	defer cancel()

	ev, data, ok := readSSEFrame(t, reader)
	if !ok {
		t.Fatal("no stream_open frame")
	}
	if ev != "stream_open" {
		t.Fatalf("first frame event = %q, want stream_open; data=%s", ev, data)
	}
	var openPayload map[string]any
	if err := json.Unmarshal([]byte(data), &openPayload); err != nil {
		t.Fatalf("stream_open decode: %v", err)
	}
	if openPayload["user_id"] != "user-a" {
		t.Errorf("stream_open user_id = %v, want user-a", openPayload["user_id"])
	}

	bus.Publish(runstate.RunStateEvent{
		RunID: "r1", Agent: "researcher", UserID: "user-a", Status: "running",
	})

	ev, data, ok = readSSEFrame(t, reader)
	if !ok {
		t.Fatal("no run_state frame after publish")
	}
	if ev != "run_state" {
		t.Fatalf("event = %q, want run_state; data=%s", ev, data)
	}
	var evt runstate.RunStateEvent
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		t.Fatalf("run_state decode: %v", err)
	}
	if evt.RunID != "r1" || evt.Status != "running" || evt.Agent != "researcher" {
		t.Errorf("wrong run_state payload: %+v", evt)
	}
}

func TestStreamUserAgents_FiltersByStatus(t *testing.T) {
	srv, bus, cleanup := streamFixture(t)
	defer cleanup()

	reader, cancel, _ := openStream(t, srv, "user-a", "status=completed")
	defer cancel()

	// drain stream_open
	if _, _, ok := readSSEFrame(t, reader); !ok {
		t.Fatal("no stream_open")
	}

	bus.Publish(runstate.RunStateEvent{RunID: "r1", UserID: "user-a", Status: "running"})
	bus.Publish(runstate.RunStateEvent{RunID: "r2", UserID: "user-a", Status: "completed"})

	ev, data, ok := readSSEFrame(t, reader)
	if !ok {
		t.Fatal("no event after publishes")
	}
	if ev != "run_state" {
		t.Fatalf("event = %q, want run_state", ev)
	}
	var evt runstate.RunStateEvent
	_ = json.Unmarshal([]byte(data), &evt)
	if evt.RunID != "r2" || evt.Status != "completed" {
		t.Errorf("filter let wrong event through: %+v", evt)
	}
}

func TestStreamUserAgents_FiltersByAgent(t *testing.T) {
	srv, bus, cleanup := streamFixture(t)
	defer cleanup()

	reader, cancel, _ := openStream(t, srv, "user-a", "agent=writer")
	defer cancel()

	if _, _, ok := readSSEFrame(t, reader); !ok {
		t.Fatal("no stream_open")
	}

	bus.Publish(runstate.RunStateEvent{RunID: "r1", UserID: "user-a", Agent: "reader", Status: "running"})
	bus.Publish(runstate.RunStateEvent{RunID: "r2", UserID: "user-a", Agent: "writer", Status: "running"})

	ev, data, ok := readSSEFrame(t, reader)
	if !ok {
		t.Fatal("no event")
	}
	var evt runstate.RunStateEvent
	_ = json.Unmarshal([]byte(data), &evt)
	if ev != "run_state" || evt.Agent != "writer" {
		t.Errorf("agent filter wrong: ev=%q evt=%+v", ev, evt)
	}
}

func TestStreamUserAgents_OtherUsersEventsNotDelivered(t *testing.T) {
	srv, bus, cleanup := streamFixture(t)
	defer cleanup()

	reader, cancel, _ := openStream(t, srv, "user-a", "")
	defer cancel()

	if _, _, ok := readSSEFrame(t, reader); !ok {
		t.Fatal("no stream_open")
	}

	// Event for a different user.
	bus.Publish(runstate.RunStateEvent{RunID: "r1", UserID: "user-b", Status: "running"})

	doneFrame := make(chan struct{})
	go func() {
		_, _, _ = readSSEFrame(t, reader)
		close(doneFrame)
	}()
	select {
	case <-doneFrame:
		t.Error("user-a received event for user-b")
	case <-time.After(200 * time.Millisecond):
		// expected — no event delivered
	}
}

func TestStreamUserAgents_RequiresBearer(t *testing.T) {
	srv, _, cleanup := streamFixture(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/v1/users/user-a/agents/stream", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestStreamUserAgents_503WhenBusUnwired(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	// Note: no SetRunStateBus call.

	req := authedRequest("GET", "/v1/users/user-a/agents/stream", nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestParseStreamFilter_AcceptsCommaSeparated(t *testing.T) {
	req := parseStreamFilter("user-a", map[string][]string{"status": {"running,completed,failed"}})
	if req.UserID != "user-a" {
		t.Errorf("user_id = %q, want user-a", req.UserID)
	}
	if len(req.Statuses) != 3 {
		t.Fatalf("got %d statuses, want 3: %v", len(req.Statuses), req.Statuses)
	}
	want := map[string]bool{"running": true, "completed": true, "failed": true}
	for _, s := range req.Statuses {
		if !want[s] {
			t.Errorf("status %q not expected", s)
		}
	}
}

func TestParseStreamFilter_EmptyMeansNoFilter(t *testing.T) {
	req := parseStreamFilter("user-a", map[string][]string{})
	if len(req.Statuses) != 0 {
		t.Errorf("expected empty statuses, got %v", req.Statuses)
	}
	if req.Agent != "" {
		t.Errorf("expected empty agent, got %q", req.Agent)
	}
}
