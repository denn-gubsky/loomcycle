package llamacpp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// bodies serves a llama-server stand-in that answers every chat completion
// with an empty stop, and records each request body.
func bodies(t *testing.T) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	got := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		got <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func callOnce(t *testing.T, p providers.Provider, ctx context.Context) {
	t.Helper()
	ch, err := p.Call(ctx, providers.Request{
		Model:    "qwen3-8b",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}
}

// With options.pin_slots, a call carrying its lane slot is sent pinned to the
// llama-server slot of the same number, with its prompt cached there; a call
// no lane granted is sent unpinned. Without the option nothing is pinned.
func TestNewFromOptions_PinSlotsSendsTheLaneSlotAsIDSlot(t *testing.T) {
	srv, got := bodies(t)
	pinned, err := newFromOptions(providers.DriverOptions{BaseURL: srv.URL + "/v1", Options: map[string]any{"pin_slots": true}})
	if err != nil {
		t.Fatal(err)
	}

	callOnce(t, pinned, providers.WithLaneSlot(context.Background(), 2))
	body := <-got
	if body["id_slot"] != float64(2) || body["cache_prompt"] != true {
		t.Fatalf("pinned call body id_slot=%v cache_prompt=%v, want 2 and true", body["id_slot"], body["cache_prompt"])
	}
	if body["model"] != "qwen3-8b" {
		t.Errorf("the rest of the body was lost: model=%v", body["model"])
	}

	callOnce(t, pinned, context.Background())
	if body := <-got; body["id_slot"] != nil || body["cache_prompt"] != nil {
		t.Errorf("a call with no lane slot was pinned: %v", body)
	}

	plain, err := newFromOptions(providers.DriverOptions{BaseURL: srv.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	callOnce(t, plain, providers.WithLaneSlot(context.Background(), 1))
	if body := <-got; body["id_slot"] != nil {
		t.Errorf("pinning without options.pin_slots: %v", body)
	}
}
