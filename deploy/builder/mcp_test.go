package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestHandler(t *testing.T, fr *fakeRunner) *MCPHandler {
	t.Helper()
	cfg := testCfg()
	cfg.AuthToken = "secret-token"
	d := NewDispatcher(cfg, NewEngine(cfg, fr), NewStore(cfg.SessionIdleTTL, cfg.SessionMaxTTL))
	return NewMCPHandler(cfg, d, "test")
}

func post(h http.Handler, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestMCP_AuthRequired(t *testing.T) {
	h := newTestHandler(t, &fakeRunner{})
	if rr := post(h, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); rr.Code != http.StatusUnauthorized {
		t.Errorf("no bearer: got %d want 401", rr.Code)
	}
	if rr := post(h, "wrong", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer: got %d want 401", rr.Code)
	}
	if rr := post(h, "secret-token", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); rr.Code != http.StatusOK {
		t.Errorf("right bearer: got %d want 200", rr.Code)
	}
}

func TestMCP_Initialize(t *testing.T) {
	h := newTestHandler(t, &fakeRunner{})
	rr := post(h, "secret-token", `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("initialize: %d", rr.Code)
	}
	if rr.Header().Get("Mcp-Session-Id") == "" {
		t.Errorf("initialize should set an Mcp-Session-Id header")
	}
	var resp rpcResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != 7 {
		t.Errorf("response id = %d want 7 (must echo request id)", resp.ID)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %q want %q", res.ProtocolVersion, protocolVersion)
	}
	if res.ServerInfo.Name != "loomcycle-builder" {
		t.Errorf("serverInfo.name = %q", res.ServerInfo.Name)
	}
}

func TestMCP_ToolsList(t *testing.T) {
	h := newTestHandler(t, &fakeRunner{})
	rr := post(h, "secret-token", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var resp rpcResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"sandbox_open": false, "sandbox_exec": false, "sandbox_write": false, "sandbox_read": false, "sandbox_close": false, "sandbox_list": false}
	for _, tl := range res.Tools {
		if _, ok := want[tl.Name]; ok {
			want[tl.Name] = true
		}
		if len(tl.InputSchema) == 0 {
			t.Errorf("tool %s has empty inputSchema", tl.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tools/list missing %s", name)
		}
	}
}

func TestMCP_ToolsCall_Open(t *testing.T) {
	// fake podman run succeeds.
	h := newTestHandler(t, &fakeRunner{})
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sandbox_open","arguments":{}}}`
	rr := post(h, "secret-token", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call: %d", rr.Code)
	}
	var resp rpcResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.ID != 3 {
		t.Errorf("id echo: %d", resp.ID)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) == 0 || res.Content[0].Type != "text" {
		t.Fatalf("unexpected call result: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "session_id") {
		t.Errorf("open text should carry a session_id: %q", res.Content[0].Text)
	}
}

func TestMCP_Notification_202(t *testing.T) {
	h := newTestHandler(t, &fakeRunner{})
	rr := post(h, "secret-token", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rr.Code != http.StatusAccepted {
		t.Errorf("notification: got %d want 202", rr.Code)
	}
}

func TestMCP_UnknownMethod(t *testing.T) {
	h := newTestHandler(t, &fakeRunner{})
	rr := post(h, "secret-token", `{"jsonrpc":"2.0","id":9,"method":"no/such"}`)
	var resp rpcResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("unknown method should return -32601, got %+v", resp.Error)
	}
}
