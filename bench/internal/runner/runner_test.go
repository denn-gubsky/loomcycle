package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestClient_Initialize verifies the client extracts the session ID
// header and enables runEvents when the server echoes the capability.
func TestClient_Initialize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
		}
		var frame map[string]any
		_ = json.NewDecoder(r.Body).Decode(&frame)
		if method, _ := frame["method"].(string); method != "initialize" {
			// notifications/initialized: respond 204 empty.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-fixture-001")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      frame["id"],
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "test", "version": "0.0.0"},
				"capabilities": map[string]any{
					"loomcycle": map[string]any{"runEvents": true},
				},
			},
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-bearer", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if c.SessionID() != "sess-fixture-001" {
		t.Errorf("session id = %q, want %q", c.SessionID(), "sess-fixture-001")
	}
	if !c.runEvents {
		t.Error("runEvents capability was not picked up from response")
	}
}

// TestClient_SpawnRunSSE_DecodesNotifsAndFinalFrame parses an SSE
// stream that interleaves a tool_call notification with the final
// tools/call response and verifies both are captured.
func TestClient_SpawnRunSSE_DecodesNotifsAndFinalFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var frame map[string]any
		_ = json.NewDecoder(r.Body).Decode(&frame)
		method, _ := frame["method"].(string)
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-fixture-002")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": frame["id"],
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"serverInfo":      map[string]any{"name": "test"},
					"capabilities": map[string]any{
						"loomcycle": map[string]any{"runEvents": true},
					},
				},
			})
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusNoContent)
			return
		case "tools/call":
			// Emit SSE: one notification, then the final result frame.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			notif := `{"jsonrpc":"2.0","method":"notifications/loomcycle/run_event","params":{"run_id":"r1","agent_id":"a1","event":{"type":"tool_call","tool_use":{"id":"t1","name":"mcp__jobs__getAgentContext","input":{"user_id":"x"}}}}}`
			final := `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"{\"agent_id\":\"a1\",\"run_id\":\"r1\",\"status\":\"completed\",\"final_text\":\"all done\"}"}]}}`
			_, _ = io.WriteString(w, "data: "+notif+"\n\n")
			flusher.Flush()
			_, _ = io.WriteString(w, "data: "+final+"\n\n")
			flusher.Flush()
			return
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	result, err := c.SpawnRun(ctx, SpawnRunArgs{
		Agent:    "bench-agent",
		Segments: []PromptSegment{UserTextSegment("hi")},
	})
	if err != nil {
		t.Fatalf("SpawnRun: %v", err)
	}
	if result.FinalText != "all done" {
		t.Errorf("final_text = %q, want 'all done'", result.FinalText)
	}
	if result.Status != "completed" {
		t.Errorf("status = %q, want 'completed'", result.Status)
	}
	if len(result.Events) != 1 {
		t.Fatalf("expected 1 captured event, got %d", len(result.Events))
	}
	if result.Events[0].ToolUse == nil || result.Events[0].ToolUse.Name != "mcp__jobs__getAgentContext" {
		t.Errorf("tool_use missing or wrong name: %+v", result.Events[0].ToolUse)
	}
}

// TestClient_RegisterAgent_SendsBearerAndSessionHeader.
func TestClient_RegisterAgent_SendsBearerAndSessionHeader(t *testing.T) {
	gotAuth := ""
	gotSession := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var frame map[string]any
		_ = json.NewDecoder(r.Body).Decode(&frame)
		method, _ := frame["method"].(string)
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-fixture-003")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": frame["id"],
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"serverInfo":      map[string]any{"name": "test"},
					"capabilities":    map[string]any{},
				},
			})
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusNoContent)
			return
		case "tools/call":
			gotAuth = r.Header.Get("Authorization")
			gotSession = r.Header.Get("Mcp-Session-Id")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": frame["id"],
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": `{"name":"bench-agent","ttl_seconds":7200}`},
					},
				},
			})
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, "secret-bearer", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	err := c.RegisterAgent(ctx, RegisterAgentArgs{
		Name: "bench-agent", SystemPrompt: "sp", Tools: []string{"Read"},
	})
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if !strings.Contains(gotAuth, "secret-bearer") {
		t.Errorf("Authorization header missing bearer; got %q", gotAuth)
	}
	if gotSession != "sess-fixture-003" {
		t.Errorf("Mcp-Session-Id header = %q, want %q", gotSession, "sess-fixture-003")
	}
}
