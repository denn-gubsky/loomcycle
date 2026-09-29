package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// snapshotConnector answers the snapshot tools with fixed connector results.
type snapshotConnector struct {
	*mockConnector
	desc    connector.SnapshotDescriptor
	restore connector.RestoreSnapshotResult
}

func (c *snapshotConnector) CreateSnapshot(context.Context, connector.CreateSnapshotRequest) (connector.SnapshotDescriptor, error) {
	return c.desc, nil
}

func (c *snapshotConnector) RestoreSnapshot(context.Context, connector.RestoreSnapshotRequest) (connector.RestoreSnapshotResult, error) {
	return c.restore, nil
}

// The MCP snapshot tools serialize the connector's results, so the restore
// counter map and the capture warnings reach an MCP client with no MCP-side
// mapping — this pins that they do.
func TestSnapshotTools_CarryRestoredMapAndCaptureWarnings(t *testing.T) {
	env := &handlerEnv{connector: &snapshotConnector{
		mockConnector: &mockConnector{},
		desc:          connector.SnapshotDescriptor{SnapshotID: "snap_1", Warnings: []string{"hook_defs gate: body.headers.X-Api-Key holds a literal value"}},
		restore:       connector.RestoreSnapshotResult{Restored: map[string]int{"hook_defs": 2, "mcp_server_defs_activated": 1}},
	}}

	res, err := handleCreateSnapshot(context.Background(), env, json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("create_snapshot: %v %+v", err, res)
	}
	var desc struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &desc); err != nil {
		t.Fatalf("create_snapshot result: %v (%s)", err, res.Content[0].Text)
	}
	if len(desc.Warnings) != 1 {
		t.Errorf("create_snapshot warnings = %q, want the capture's one", desc.Warnings)
	}

	res, err = handleRestoreSnapshot(context.Background(), env, json.RawMessage(`{"snapshot_id":"snap_1"}`))
	if err != nil || res.IsError {
		t.Fatalf("restore_snapshot: %v %+v", err, res)
	}
	var out struct {
		Restored map[string]int `json:"restored"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("restore_snapshot result: %v (%s)", err, res.Content[0].Text)
	}
	if out.Restored["hook_defs"] != 2 || out.Restored["mcp_server_defs_activated"] != 1 {
		t.Errorf("restore_snapshot restored = %v, want the connector's map", out.Restored)
	}
}
