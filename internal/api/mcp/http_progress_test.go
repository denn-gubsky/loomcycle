package mcp

import (
	"bytes"
	"strings"
	"testing"
)

// TestHTTPTransport_SpawnRunsWithProgressTokenStreamsProgress: over the HTTP
// transport (what the thin client proxies to), a spawn_runs call that carries
// a progress token is answered as an event stream, so its progress
// notifications reach the client ahead of the result (RFC GK D7). Without a
// token the reply stays a single JSON body.
func TestHTTPTransport_SpawnRunsWithProgressTokenStreamsProgress(t *testing.T) {
	ts, _, _ := httpTestServer(t, nil)
	resp, _ := postFrame(t, ts.URL, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	sessionID := resp.Header.Get("Mcp-Session-Id")

	spawns := `[{"agent":"qa","segments":[{"role":"user","content":[{"type":"trusted-text","text":"a"}]}]},` +
		`{"agent":"qa","segments":[{"role":"user","content":[{"type":"trusted-text","text":"b"}]}]}]`

	resp, body := postFrame(t, ts.URL, sessionID,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_runs","_meta":{"progressToken":"p-9"},"arguments":{"spawns":`+spawns+`}}}`)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream; body=%s", ct, body)
	}
	// The opening notification, one per settled child, then the result.
	if got := bytes.Count(body, []byte(`"method":"notifications/progress"`)); got < 3 {
		t.Errorf("progress notifications = %d, want at least 3; body=%s", got, body)
	}
	if !bytes.Contains(body, []byte(`"progressToken":"p-9"`)) || !bytes.Contains(body, []byte(`2 of 2 runs settled`)) {
		t.Errorf("progress lacks the token or the final count; body=%s", body)
	}
	if !bytes.Contains(body, []byte(`"id":2`)) {
		t.Errorf("no result frame; body=%s", body)
	}

	resp, body = postFrame(t, ts.URL, sessionID,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"spawn_runs","arguments":{"spawns":`+spawns+`}}}`)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" || bytes.Contains(body, []byte("notifications/progress")) {
		t.Errorf("without a token: Content-Type %q, body %s; want a single JSON result", ct, body)
	}
}
