package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// The restore response's `restored` map carries every counter, including the
// def counts no typed field reports; the create response's `warnings` carry
// the capture's findings by location only (RFC DP §5.3).

// captureEveryDefKind builds an envelope holding one skill, team, hook and
// channel def — the counts that were invisible on every transport.
func captureEveryDefKind(t *testing.T) []byte {
	t.Helper()
	ctx := context.Background()
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	if _, err := src.SkillDefCreate(ctx, store.SkillDefRow{DefID: "sdf_w", Name: "wire-skill", Definition: json.RawMessage(`{"body":"b"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.TeamDefCreate(ctx, store.TeamDefRow{DefID: "tdf_w", Name: "wire-team", Definition: json.RawMessage(`{"states":[]}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.HookDefCreate(ctx, store.HookDefRow{DefID: "hdf_w", Name: "wire-hook",
		Definition: json.RawMessage(`{"event":"pre","body":{"kind":"http","url":"https://hooks.example.test/w"}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := src.ChannelsCreate(ctx, store.ChannelRow{Name: "wire-chan", Scope: "tenant", Semantic: "queue"}); err != nil {
		t.Fatal(err)
	}
	_, raw, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var invisibleDefCounts = []string{"skill_defs", "team_defs", "hook_defs", "channel_defs"}

func TestRestoreSnapshot_RestoredMapCarriesEveryDefCount(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	body, _ := json.Marshal(map[string]any{"json": json.RawMessage(captureEveryDefKind(t))})
	req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
	req.SetPathValue("id", "inline")
	rec := httptest.NewRecorder()
	srv.handleRestoreSnapshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Restored map[string]int `json:"restored"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, k := range invisibleDefCounts {
		if resp.Restored[k] != 1 {
			t.Errorf("restored[%q] = %d, want 1 (body %s)", k, resp.Restored[k], rec.Body.String())
		}
	}
	if _, ok := resp.Restored["mcp_server_defs_activated"]; !ok {
		t.Error("restored lacks mcp_server_defs_activated")
	}
}

func TestConnectorRestoreSnapshot_RestoredMapCarriesEveryDefCount(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	res, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: captureEveryDefKind(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range invisibleDefCounts {
		if res.Restored[k] != 1 {
			t.Errorf("Restored[%q] = %d, want 1 (%v)", k, res.Restored[k], res.Restored)
		}
	}
}

const markWireHeader = "dp1awire-literal-bearer-0123456789"

func seedLiteralHeaderDef(t *testing.T, st store.Store) {
	t.Helper()
	def := json.RawMessage(`{"transport":"streamable-http","url":"https://mcp.example.test/mcp","headers":{"Authorization":"Bearer ` + markWireHeader + `"}}`)
	if _, err := st.MCPServerDefCreate(context.Background(), store.MCPServerDefRow{DefID: "mcp_wire", Name: "wire-mcp", Definition: def}); err != nil {
		t.Fatal(err)
	}
}

func assertFindingWarning(t *testing.T, where string, warnings []string) {
	t.Helper()
	joined := strings.Join(warnings, "\n")
	if len(warnings) != 1 || !strings.Contains(joined, "wire-mcp") || !strings.Contains(joined, "headers.Authorization") {
		t.Errorf("%s warnings = %q; want one naming wire-mcp's headers.Authorization", where, warnings)
	}
	if strings.Contains(joined, markWireHeader[:len(markWireHeader)/2]) {
		t.Errorf("%s warnings carry the header value: %q", where, warnings)
	}
}

func TestCreateSnapshot_ResponseCarriesCaptureWarnings(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	seedLiteralHeaderDef(t, st)

	rec := httptest.NewRecorder()
	srv.handleCreateSnapshot(rec, httptest.NewRequest("POST", "/v1/_snapshots", bytes.NewReader([]byte(`{}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp snapshotCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	assertFindingWarning(t, "HTTP create", resp.Warnings)
}

func TestConnectorCreateSnapshot_DescriptorCarriesCaptureWarnings(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	seedLiteralHeaderDef(t, st)

	desc, err := srv.CreateSnapshot(context.Background(), connector.CreateSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertFindingWarning(t, "connector create", desc.Warnings)

	// A listing cannot recompute them; it leaves the field empty.
	list, err := srv.ListSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list {
		if len(d.Warnings) != 0 {
			t.Errorf("listed descriptor %s carries warnings %q", d.SnapshotID, d.Warnings)
		}
	}
}
