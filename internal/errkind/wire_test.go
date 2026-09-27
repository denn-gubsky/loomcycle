package errkind_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/errkind"
)

// Info serializes in the shape its consumers declare — @loomcycle/client's
// ErrorInfo, the gRPC ErrorInfo — with the call format under
// correct_call_format.
func TestInfo_MarshalsTheWireShape(t *testing.T) {
	d := 1500 * time.Millisecond
	raw, err := json.Marshal(errkind.Info{
		Category:    errkind.CategoryValidation,
		Description: "Pass only this tool's own arguments.",
		CallFormat:  &errkind.CallFormat{Tool: "Document", Op: "create_chunk", Example: json.RawMessage(`{"op":"create_chunk"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if got["category"] != "validation" || got["is_retryable"] != false || got["description"] == nil {
		t.Errorf("wire = %s", raw)
	}
	if cf, _ := got["correct_call_format"].(map[string]any); cf["tool"] != "Document" || cf["op"] != "create_chunk" {
		t.Errorf("correct_call_format = %v", got["correct_call_format"])
	}
	if _, ok := got["retry_after_ms"]; ok {
		t.Errorf("a non-retryable failure carries a backoff: %s", raw)
	}

	raw, _ = json.Marshal(errkind.Info{Category: errkind.CategoryTransient, Retryable: true, RetryAfter: &d})
	_ = json.Unmarshal(raw, &got)
	if got["retry_after_ms"] != float64(1500) || got["is_retryable"] != true {
		t.Errorf("retryable wire = %s", raw)
	}
}

// It round-trips — a persisted event is read back on replay — and it still
// reads the Go-field-name shape events persisted before the wire shape existed.
func TestInfo_RoundTripsAndReadsTheLegacyShape(t *testing.T) {
	d := 5 * time.Second
	in := errkind.Info{Category: errkind.CategoryTransient, Retryable: true, Description: "wait", RetryAfter: &d,
		CallFormat: &errkind.CallFormat{Tool: "Path", Op: "mv"}}
	raw, _ := json.Marshal(in)
	var back errkind.Info
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Category != in.Category || !back.Retryable || back.Description != "wait" ||
		back.RetryAfter == nil || *back.RetryAfter != d || back.CallFormat == nil || back.CallFormat.Op != "mv" {
		t.Errorf("round-trip = %+v", back)
	}

	var legacy errkind.Info
	if err := json.Unmarshal([]byte(`{"Category":"business","Retryable":false,"Description":"budget","RetryAfter":null}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Category != errkind.CategoryBusiness || legacy.Description != "budget" {
		t.Errorf("legacy shape read as %+v", legacy)
	}
}
