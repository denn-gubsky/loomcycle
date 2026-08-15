package docremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_DoSuccessAndRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Op string `json:"op"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		if in.Op == "boom" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "tool": "Document", "error": "nope"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"document_id": "d1"})
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	raw, err := c.Do(context.Background(), map[string]any{"op": "get_document", "path": "/x"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	var m map[string]any
	if uerr := json.Unmarshal(raw, &m); uerr != nil || m["document_id"] != "d1" {
		t.Errorf("Do result = %s (err %v)", raw, uerr)
	}

	_, err = c.Do(context.Background(), map[string]any{"op": "boom"})
	if err == nil || !strings.Contains(err.Error(), "tool_refused") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("refusal err = %v, want a tool_refused error carrying the peer message", err)
	}
}

func TestClient_New_Validates(t *testing.T) {
	if _, err := New(Options{HTTPClient: http.DefaultClient}); err == nil {
		t.Errorf("New without base_url should error")
	}
	if _, err := New(Options{BaseURL: "http://peer"}); err == nil {
		t.Errorf("New without HTTPClient should error")
	}
}
