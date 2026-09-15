package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Before this change an upstream server's structuredContent was dropped at
// decode: the struct had no field for it, so a peer that said "transient,
// retryable, wait 5s" reached the model as the bare sentence. These pin the
// retention — and the limits on it.

func TestUpstreamStructured_ReachesTheModel(t *testing.T) {
	structured := json.RawMessage(`{"errorCategory":"transient","isRetryable":true,"retryAfterSeconds":5,"description":"Order DB under load."}`)
	got := withUpstreamStructured("Service temporarily unavailable", structured)

	for _, want := range []string{
		"Service temporarily unavailable", // the original text survives
		"errorCategory",
		"transient",
		"isRetryable",
		"retryAfterSeconds",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("model text is missing %q:\n%s", want, got)
		}
	}
	// Attributed, so the model can weigh a peer's claim as a peer's claim.
	if !strings.Contains(got, "server-reported") {
		t.Errorf("peer data is not attributed:\n%s", got)
	}
}

func TestUpstreamStructured_AbsentChangesNothing(t *testing.T) {
	const text = "ordinary successful output"
	for _, empty := range []json.RawMessage{nil, {}} {
		if got := withUpstreamStructured(text, empty); got != text {
			t.Errorf("text altered when no structuredContent present: %q", got)
		}
	}
}

// A malformed payload must not corrupt what the model sees. Forwarding
// unparseable bytes would be worse than dropping them.
func TestUpstreamStructured_MalformedIsDropped(t *testing.T) {
	const text = "Service temporarily unavailable"
	got := withUpstreamStructured(text, json.RawMessage(`{not json`))
	if got != text {
		t.Errorf("malformed payload leaked into model text: %q", got)
	}
}

// Indentation is the peer's choice and the model's cost, so it is compacted.
func TestUpstreamStructured_IsCompacted(t *testing.T) {
	pretty := json.RawMessage("{\n    \"errorCategory\":   \"transient\",\n    \"isRetryable\": true\n}")
	got := withUpstreamStructured("t", pretty)
	if strings.Contains(got, "\n    ") {
		t.Errorf("payload was not compacted:\n%s", got)
	}
	if !strings.Contains(got, `{"errorCategory":"transient","isRetryable":true}`) {
		t.Errorf("compacted form unexpected:\n%s", got)
	}
}

// The decode side: the field is retained on the struct rather than discarded.
func TestUpstreamStructured_SurvivesDecode(t *testing.T) {
	raw := []byte(`{"isError":true,"content":[{"type":"text","text":"boom"}],"structuredContent":{"errorCategory":"business"}}`)
	var res CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.StructuredContent) == 0 {
		t.Fatal("structuredContent was dropped at decode")
	}
	if !strings.Contains(string(res.StructuredContent), "business") {
		t.Errorf("retained payload wrong: %s", res.StructuredContent)
	}
}

// Round-trip: a result we did not classify must re-encode without the key, so
// proxying someone else's plain result stays byte-clean.
func TestUpstreamStructured_AbsentKeyDoesNotReappearOnEncode(t *testing.T) {
	raw := []byte(`{"content":[{"type":"text","text":"ok"}]}`)
	var res CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "structuredContent") {
		t.Errorf("empty key reappeared on encode: %s", out)
	}
}

// --- the wiring, not just the helper ---

// structuredCaller answers tools/call with a structuredContent-bearing result,
// so Execute's real path can be driven end to end.
type structuredCaller struct{ structured string }

func (c *structuredCaller) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return json.RawMessage(`{"protocolVersion":"` + ProtocolVersion +
			`","serverInfo":{"name":"s","version":"0"},"capabilities":{}}`), nil
	case "tools/list":
		return json.RawMessage(`{"tools":[{"name":"probe","description":"d","inputSchema":{"type":"object"}}]}`), nil
	case "tools/call":
		return json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"Service temporarily unavailable"}],` +
			`"structuredContent":` + c.structured + `}`), nil
	}
	return nil, errors.New("unexpected method " + method)
}
func (c *structuredCaller) Notify(context.Context, string, any) error { return nil }
func (c *structuredCaller) Healthy() bool                             { return true }

// TestUpstreamStructured_ExecuteWiresItThrough covers the CALL SITE, not the
// helper. Testing withUpstreamStructured alone passed even with the call
// deleted from Execute — the helper stayed correct and did nothing.
func TestUpstreamStructured_ExecuteWiresItThrough(t *testing.T) {
	caller := &structuredCaller{structured: `{"errorCategory":"transient","isRetryable":true,"retryAfterSeconds":5}`}
	pool := NewPool(func(_, _ string) (Caller, error) { return caller, nil }, nil, nil)

	tool := NewTool(pool, "peer", ToolDescriptor{Name: "probe", Description: "d"})
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Error("IsError lost in translation")
	}
	if !strings.Contains(res.Text, "Service temporarily unavailable") {
		t.Errorf("original text lost: %q", res.Text)
	}
	for _, want := range []string{"errorCategory", "transient", "retryAfterSeconds", "server-reported"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("Execute did not carry %q to the model — the call site is not wired:\n%s", want, res.Text)
		}
	}
	// The peer's claim must never be promoted into OUR classification: a
	// mounted server must not be able to talk this runtime into retrying.
	if res.Error != nil {
		t.Errorf("upstream category was promoted into tools.Result.Error (%+v) — "+
			"a peer's claim is data, not this runtime's finding", res.Error)
	}
}
