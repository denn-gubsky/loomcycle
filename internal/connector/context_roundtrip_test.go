package connector

import (
	"encoding/json"
	"testing"
)

// The per-run `context` override (RFC CR) must parse off the wire into
// SpawnRunRequest.Context — the JSON tag is the one thing that silently breaks
// the MCP / connector transport parity if it drifts. This pins the field name +
// that the nested state_schema survives.
func TestSpawnRunRequest_ContextRoundTrips(t *testing.T) {
	raw := `{
		"agent": "runner",
		"context": {
			"mode": "auto",
			"keep_last_n": 4,
			"on_invalid_patch": "fail",
			"state_schema": {"type": "object", "properties": {"count": {"type": "integer"}}}
		}
	}`
	var req SpawnRunRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Context == nil {
		t.Fatal("context did not parse into SpawnRunRequest.Context (json tag drift?)")
	}
	if req.Context.Mode == nil || *req.Context.Mode != "auto" {
		t.Errorf("context.mode = %v, want auto", req.Context.Mode)
	}
	if req.Context.KeepLastN == nil || *req.Context.KeepLastN != 4 {
		t.Errorf("context.keep_last_n = %v, want 4", req.Context.KeepLastN)
	}
	if req.Context.OnInvalidPatch == nil || *req.Context.OnInvalidPatch != "fail" {
		t.Errorf("context.on_invalid_patch = %v, want fail", req.Context.OnInvalidPatch)
	}
	if _, ok := req.Context.StateSchema["properties"]; !ok {
		t.Errorf("nested state_schema did not survive: %v", req.Context.StateSchema)
	}
	// And the whole thing must validate.
	if err := req.Context.Validate(); err != nil {
		t.Errorf("parsed context should validate: %v", err)
	}
}
