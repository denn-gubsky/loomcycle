package clienttools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ridCtx stamps a RunIdentity so toolAdapter.Execute resolves the routing key.
func ridCtx(tenant, subject string) context.Context {
	return tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		TenantID: tenant, UserID: subject, RootRunID: "r1", AgentID: "a1",
	})
}

func TestToolAdapter_ExecuteDelegates(t *testing.T) {
	reg := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	var c *Conn
	// The fake client replies ok, echoing the input as the output.
	conn, dereg, _ := reg.Register(key, []ToolSchema{{
		Name: "browser.read_page", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`),
	}}, echoSender(&c, ""))
	c = conn
	defer dereg()

	cands := Candidates(reg, key, time.Second)
	if len(cands) != 1 {
		t.Fatalf("Candidates = %d, want 1", len(cands))
	}
	tool := cands[0]
	if tool.Name() != "client:browser.read_page" {
		t.Errorf("Name = %q, want client:browser.read_page", tool.Name())
	}
	res, err := tool.Execute(ridCtx("t1", "u1"), json.RawMessage(`{"q":"hi"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError || res.Text != `{"q":"hi"}` {
		t.Errorf("result = %+v, want the echoed input", res)
	}
}

func TestToolAdapter_RendersStringOutput(t *testing.T) {
	reg := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	var c *Conn
	// Reply with a JSON *string* output — the adapter unwraps it to plain text.
	send := func(_ context.Context, v any) error {
		f := v.(InvokeFrame)
		go c.DeliverResult(ResultFrame{Type: FrameResult, CallID: f.CallID, OK: true, Output: json.RawMessage(`"the page title"`)})
		return nil
	}
	conn, dereg, _ := reg.Register(key, []ToolSchema{{Name: "browser.title"}}, send)
	c = conn
	defer dereg()

	res, _ := Candidates(reg, key, time.Second)[0].Execute(ridCtx("t1", "u1"), nil)
	if res.IsError || res.Text != "the page title" {
		t.Errorf("string output should unwrap to plain text; got %+v", res)
	}
}

func TestToolAdapter_NoConnection(t *testing.T) {
	reg := NewRegistry(0)
	// No connection for this principal → a clear tool error, not a hang.
	tool := toolAdapter{reg: reg, schema: ToolSchema{Name: "browser.read_page"}, timeout: time.Second}
	res, err := tool.Execute(ridCtx("t1", "u1"), nil)
	if err != nil {
		t.Fatalf("Execute returned a hard error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "no client connection") {
		t.Errorf("want a clear no-connection tool error, got %+v", res)
	}
}

func TestToolAdapter_ClientError(t *testing.T) {
	reg := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	var c *Conn
	send := func(_ context.Context, v any) error {
		f := v.(InvokeFrame)
		go c.DeliverResult(ResultFrame{Type: FrameResult, CallID: f.CallID, OK: false, Error: "element not found"})
		return nil
	}
	conn, dereg, _ := reg.Register(key, []ToolSchema{{Name: "browser.click"}}, send)
	c = conn
	defer dereg()

	res, _ := Candidates(reg, key, time.Second)[0].Execute(ridCtx("t1", "u1"), nil)
	if !res.IsError || res.Text != "element not found" {
		t.Errorf("client ok:false should surface as a tool error with the client's message; got %+v", res)
	}
}

func TestToolAdapter_Timeout(t *testing.T) {
	reg := NewRegistry(0)
	key := PrincipalKey{"t1", "u1"}
	silent := func(context.Context, any) error { return nil }
	_, dereg, _ := reg.Register(key, []ToolSchema{{Name: "fs.read"}}, silent)
	defer dereg()

	tool := toolAdapter{reg: reg, schema: ToolSchema{Name: "fs.read"}, timeout: 30 * time.Millisecond}
	res, _ := tool.Execute(ridCtx("t1", "u1"), nil)
	if !res.IsError || !strings.Contains(res.Text, "timed out") {
		t.Errorf("want a timeout tool error, got %+v", res)
	}
}
