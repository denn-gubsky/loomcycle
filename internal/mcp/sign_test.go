package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSign_PrefixedHex(t *testing.T) {
	h := Sign(MCPServerContent{Name: "n8n-mailgun"})
	if !strings.HasPrefix(h, "sha256:") {
		t.Errorf("hash %q missing sha256: prefix", h)
	}
	if got := len(h); got != 71 {
		t.Errorf("hash length = %d, want 71 (sha256: + 64 hex chars)", got)
	}
	if strings.ContainsAny(h[7:], "ABCDEF") {
		t.Errorf("hash %q has uppercase hex; want lowercase", h)
	}
}

func TestSign_Deterministic(t *testing.T) {
	c := MCPServerContent{
		Name:      "n8n-mailgun",
		Transport: "streamable-http",
		URL:       "https://n8n.example.com/mcp/abc",
		Headers:   map[string]string{"Authorization": "Bearer ${LOOMCYCLE_N8N_TOKEN}"},
	}
	if Sign(c) != Sign(c) {
		t.Error("non-deterministic")
	}
}

func TestSign_URLChangeMovesHash(t *testing.T) {
	a := Sign(MCPServerContent{Name: "x", URL: "https://a.example/mcp"})
	b := Sign(MCPServerContent{Name: "x", URL: "https://b.example/mcp"})
	if a == b {
		t.Error("URL change didn't move the hash")
	}
}

func TestSign_TransportChangeMovesHash(t *testing.T) {
	a := Sign(MCPServerContent{Name: "x", Transport: "http"})
	b := Sign(MCPServerContent{Name: "x", Transport: "streamable-http"})
	if a == b {
		t.Error("transport change didn't move the hash")
	}
}

func TestSign_HeaderChangeMovesHash(t *testing.T) {
	a := Sign(MCPServerContent{Name: "x", Headers: map[string]string{"Authorization": "Bearer A"}})
	b := Sign(MCPServerContent{Name: "x", Headers: map[string]string{"Authorization": "Bearer B"}})
	if a == b {
		t.Error("header value change didn't move the hash")
	}
}

func TestSign_HeaderKeysSortedDeterministically(t *testing.T) {
	// Go's encoding/json sorts map keys; declaring them in different
	// orders must produce the same hash.
	a := Sign(MCPServerContent{
		Name: "x",
		Headers: map[string]string{
			"Authorization": "Bearer X",
			"X-Trace-ID":    "abc",
		},
	})
	b := Sign(MCPServerContent{
		Name: "x",
		Headers: map[string]string{
			"X-Trace-ID":    "abc",
			"Authorization": "Bearer X",
		},
	})
	if a != b {
		t.Errorf("header-map ordering leaked into hash: %s vs %s", a, b)
	}
}

func TestSign_NilEqualsEmptyHeaders(t *testing.T) {
	a := Sign(MCPServerContent{Name: "x", Headers: nil})
	b := Sign(MCPServerContent{Name: "x", Headers: map[string]string{}})
	if a != b {
		t.Errorf("nil vs empty headers differ: %s vs %s", a, b)
	}
}

func TestSign_TrailingWhitespaceNormalisedInDescription(t *testing.T) {
	a := Sign(MCPServerContent{Name: "x", Description: "Mailgun via n8n"})
	b := Sign(MCPServerContent{Name: "x", Description: "Mailgun via n8n\n\n  "})
	if a != b {
		t.Errorf("trailing whitespace caused drift: %s vs %s", a, b)
	}
}

func TestSign_CRLFNormalisedInDescription(t *testing.T) {
	a := Sign(MCPServerContent{Name: "x", Description: "line1\nline2"})
	b := Sign(MCPServerContent{Name: "x", Description: "line1\r\nline2"})
	if a != b {
		t.Errorf("CRLF vs LF differ: %s vs %s", a, b)
	}
}

func TestSign_InternalWhitespacePreserved(t *testing.T) {
	a := Sign(MCPServerContent{Name: "x", Description: "para1\n\npara2"})
	b := Sign(MCPServerContent{Name: "x", Description: "para1\npara2"})
	if a == b {
		t.Error("internal blank line was stripped; description content was lost")
	}
}

func TestFromOverlay_ParsesValidJSON(t *testing.T) {
	overlay := json.RawMessage(`{"name":"x","transport":"http","url":"https://x.example/mcp","headers":{"X-Token":"abc"}}`)
	c, err := FromOverlay(overlay)
	if err != nil {
		t.Fatalf("FromOverlay: %v", err)
	}
	if c.Name != "x" || c.Transport != "http" || c.URL != "https://x.example/mcp" || c.Headers["X-Token"] != "abc" {
		t.Errorf("FromOverlay lost data: %+v", c)
	}
}

func TestFromOverlay_RejectsMalformed(t *testing.T) {
	_, err := FromOverlay(json.RawMessage(`{`))
	if err == nil {
		t.Error("malformed JSON should error")
	}
}

func TestFromOverlay_EmptyInputZeroValue(t *testing.T) {
	c, err := FromOverlay(nil)
	if err != nil {
		t.Errorf("nil overlay should not error: %v", err)
	}
	if c.Name != "" {
		t.Errorf("nil overlay should yield zero value: %+v", c)
	}
}

func TestSign_KnownVector(t *testing.T) {
	// A pin — if anyone changes the canonical encoding, this test
	// catches the silent break. Update only with intent: bump a
	// version field on every existing row + re-run the backfill.
	c := MCPServerContent{
		Name:      "n8n-mailgun",
		Transport: "streamable-http",
		URL:       "https://n8n.example.com/mcp/abc",
		Headers: map[string]string{
			"Authorization": "Bearer ${LOOMCYCLE_N8N_TOKEN}",
		},
		Description: "n8n workflow exposing Mailgun send via MCP Server Trigger",
	}
	const want = "sha256:7da12c1e79db6d6e5e5dd323f121a1be1606ef454b3a9dc91b101880ac663e2a"
	got := Sign(c)
	if got != want {
		t.Errorf("canonical encoding drift: got %s, want %s — update only with intent (bump every existing row + re-backfill)", got, want)
	}
}
