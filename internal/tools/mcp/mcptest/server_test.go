package mcptest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
)

// TestMCPTestServer_HandshakeAndCallSucceeds drives the real loomcycle
// MCP HTTP client against the test server to prove the handshake
// (initialize → notifications/initialized → tools/list) works and a
// tool call with a correct bearer is reported as matched.
func TestMCPTestServer_HandshakeAndCallSucceeds(t *testing.T) {
	srv := NewServer(t)

	// Wire the loomcycle MCP HTTP client with a credential-substituted
	// Authorization header. The bearer value resolves at request build
	// time against tools.RunIdentity(ctx).UserCredentials.
	c, err := mcphttp.New(mcphttp.Config{
		URL: srv.URL,
		Headers: map[string]string{
			"Authorization": "Bearer ${run.credentials.user_token}",
		},
	})
	if err != nil {
		t.Fatalf("mcphttp.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		AgentID:         "a_test",
		UserCredentials: map[string]string{"user_token": "u_alpha"},
	})

	// The first call against a fresh client triggers the handshake.
	_, err = loommcp.CallTool(ctx, c, "check_user", json.RawMessage(`{"user_id":"u_alpha"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if got := srv.ToolCalls(); got != 1 {
		t.Errorf("ToolCalls = %d, want 1", got)
	}
	if got := srv.MatchedBearers(); got != 1 {
		t.Errorf("MatchedBearers = %d, want 1 (bearer should have matched user_id)", got)
	}
	if got := srv.MismatchedBearers(); got != 0 {
		t.Errorf("MismatchedBearers = %d, want 0", got)
	}
	log := srv.CallLog()
	if len(log) != 1 || log[0].UserID != "u_alpha" || !log[0].Matched {
		t.Errorf("unexpected call log entry: %+v", log)
	}
}

// TestMCPTestServer_BearerMismatch verifies the negative path —
// when the inbound bearer DOESN'T match the user_id, the server
// counts it as a mismatch but still returns a 200 OK response so
// the test driver sees `ok: false` in the tool body.
func TestMCPTestServer_BearerMismatch(t *testing.T) {
	srv := NewServer(t)

	c, err := mcphttp.New(mcphttp.Config{
		URL: srv.URL,
		Headers: map[string]string{
			"Authorization": "Bearer ${run.credentials.user_token}",
		},
	})
	if err != nil {
		t.Fatalf("mcphttp.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Bearer is "u_alpha" but the tool is called with user_id=u_beta.
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		AgentID:         "a_test",
		UserCredentials: map[string]string{"user_token": "u_alpha"},
	})

	if _, err := loommcp.CallTool(ctx, c, "check_user", json.RawMessage(`{"user_id":"u_beta"}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if got := srv.MismatchedBearers(); got != 1 {
		t.Errorf("MismatchedBearers = %d, want 1 (u_alpha bearer with u_beta user_id should be reported as a mismatch)", got)
	}
}

// TestMCPTestServer_WithToolName verifies the override option used by
// the compound test to spin up two distinct servers with two distinct
// tool names.
func TestMCPTestServer_WithToolName(t *testing.T) {
	srv := NewServer(t, WithToolName("custom_check"))

	c, err := mcphttp.New(mcphttp.Config{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer u_alpha"},
	})
	if err != nil {
		t.Fatalf("mcphttp.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})

	// Calling under the custom name should succeed.
	if _, err := loommcp.CallTool(ctx, c, "custom_check", json.RawMessage(`{"user_id":"u_alpha"}`)); err != nil {
		t.Fatalf("CallTool(custom_check): %v", err)
	}
	if got := srv.ToolCalls(); got != 1 {
		t.Errorf("custom_check ToolCalls = %d, want 1", got)
	}
}
