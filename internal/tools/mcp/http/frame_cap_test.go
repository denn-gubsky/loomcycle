package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The response body was read whole, however large; the server decided how
// much the runtime held. It is now capped, and a response over the cap is an
// error that says so, rather than a truncated body parsed as JSON.
func TestClient_ResponseOverTheFrameCapIsRefused(t *testing.T) {
	big := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"text": big}})
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _ := New(Config{URL: srv.URL, MaxResponseBytes: 1024})
	if _, err := c.Call(ctx, "tools/call", map[string]any{}); err == nil || !strings.Contains(err.Error(), "1024-byte limit") {
		t.Fatalf("err = %v, want the frame-limit refusal", err)
	}
	c, _ = New(Config{URL: srv.URL, MaxResponseBytes: 1 << 20})
	if _, err := c.Call(ctx, "tools/call", map[string]any{}); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
}
