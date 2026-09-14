package mcp

import (
	"encoding/json"
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
