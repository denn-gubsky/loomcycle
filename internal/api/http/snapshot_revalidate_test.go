package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/mcptest"
)

// The def sections snapshots carried first are re-validated on restore with
// the validators the call sites wire — this host's allowlists, its stdio
// opt-in, its code-hook runner, its HookDefs. A row an author here could not
// create is skipped with a warning; it never reaches the store, so the
// post-restore refresh never makes a refused MCP server dialable.

// captureWithSource builds an envelope from a store of its own that plant
// fills.
func captureWithSource(t *testing.T, plant func(ctx context.Context, src store.Store)) []byte {
	t.Helper()
	ctx := context.Background()
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	plant(ctx, src)
	_, raw, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func plantMCPDef(t *testing.T, ctx context.Context, s store.Store, id, name, url string) {
	t.Helper()
	if _, err := s.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: id, TenantID: "acme", Name: name, Definition: mcpDefJSON(url)}); err != nil {
		t.Fatal(err)
	}
	if err := s.MCPServerDefSetActive(ctx, "acme", name, id, ""); err != nil {
		t.Fatal(err)
	}
}

func restoreOverHTTP(t *testing.T, srv *Server, raw []byte) snapshotRestoreResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"json": json.RawMessage(raw)})
	req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
	req.SetPathValue("id", "inline")
	rec := httptest.NewRecorder()
	srv.handleRestoreSnapshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp snapshotRestoreResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func warned(ws []string, parts ...string) bool {
	for _, w := range ws {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(w, p)
		}
		if all {
			return true
		}
	}
	return false
}

// V12: an MCP server def whose host is not on THIS host's allowlist is not
// restored, its pointer is refused, and it is never dialable; the valid def
// beside it lands and is live. Both call sites.
func TestRestoreSnapshot_MCPServerDefOffTheTargetsAllowlistIsNotRestored(t *testing.T) {
	upstream := mcptest.NewServer(t)
	raw := captureWithSource(t, func(ctx context.Context, src store.Store) {
		plantMCPDef(t, ctx, src, "mcp_ok", "restored", upstream.URL)
		plantMCPDef(t, ctx, src, "mcp_meta", "metadata", "http://169.254.169.254/latest/meta-data/")
		plantMCPDef(t, ctx, src, "mcp_evil", "exfil", "https://user:hunter2@evil.example.net/mcp")
	})
	for _, via := range []string{"http", "connector"} {
		t.Run(via, func(t *testing.T) {
			h := newMCPRefreshHarness(t)
			var counts map[string]int
			var warnings []string
			if via == "http" {
				resp := restoreOverHTTP(t, h.srv, raw)
				counts, warnings = resp.Restored, resp.Warnings
			} else {
				res, err := h.srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: raw})
				if err != nil {
					t.Fatal(err)
				}
				counts, warnings = res.Restored, res.Warnings
			}

			if _, err := h.dial(t, "acme", "restored"); err != nil {
				t.Errorf("the valid def is not live after the restore: %v", err)
			}
			ctx := context.Background()
			var nf *store.ErrNotFound
			for _, d := range []struct{ id, name, host string }{
				{"mcp_meta", "metadata", "169.254.169.254"},
				{"mcp_evil", "exfil", "evil.example.net"},
			} {
				if _, err := h.st.MCPServerDefGet(ctx, d.id); !errors.As(err, &nf) {
					t.Errorf("%s is in the store (err = %v)", d.id, err)
				}
				if _, err := h.st.MCPServerDefGetActive(ctx, "acme", d.name); !errors.As(err, &nf) {
					t.Errorf("%s has an active pointer (err = %v)", d.name, err)
				}
				if _, ok := h.reg.Get("acme", d.name); ok {
					t.Errorf("%s is in the live registry", d.name)
				}
				if _, err := h.dial(t, "acme", d.name); err == nil {
					t.Errorf("%s is dialable", d.name)
				}
				if !warned(warnings, "mcp_server_def acme/"+d.name, "not restored", d.host, "LOOMCYCLE_HTTP_HOST_ALLOWLIST") {
					t.Errorf("no warning names %s and why: %v", d.name, warnings)
				}
			}
			if counts["mcp_server_defs_refused"] != 2 || counts["mcp_server_defs"] != 1 || counts["mcp_server_defs_activated"] != 1 {
				t.Errorf("restored map = %v; want 2 refused, 1 restored and 1 activated", counts)
			}
			if counts["active_pointers_refused"] != 2 {
				t.Errorf("active_pointers_refused = %d, want 2", counts["active_pointers_refused"])
			}
			for _, w := range warnings {
				if strings.Contains(w, "hunter2") {
					t.Errorf("a warning carries the url's password: %q", w)
				}
				if strings.Contains(w, "without re-validation") {
					t.Errorf("a production call site left a section unvalidated: %q", w)
				}
			}
		})
	}
}

// The hook, channel, agent and team defs go through their authoring rules on
// the target: a forbidden header, a code body on a host without code hooks, a
// channel whose hook names a HookDef that is not here, an inline hook header
// with a line break. What passes lands.
func TestRestoreSnapshot_HookChannelAgentAndTeamDefsAreRevalidated(t *testing.T) {
	now := time.Now().UTC()
	raw := captureWithSource(t, func(ctx context.Context, src store.Store) {
		mustOK := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		hook := func(id, name, def string) {
			_, err := src.HookDefCreate(ctx, store.HookDefRow{DefID: id, TenantID: "acme", Name: name, Version: 1, Definition: json.RawMessage(def), CreatedAt: now})
			mustOK(err)
			mustOK(src.HookDefSetActive(ctx, "acme", name, id, ""))
		}
		hook("hd_gate", "gate", `{"event":"channel_publish","body":{"kind":"http","url":"https://hooks.example.com/gate"},"fail_mode":"open"}`)
		hook("hd_host", "hosty", `{"event":"pre","body":{"kind":"http","url":"https://hooks.example.com/h","headers":{"Host":"internal"}},"fail_mode":"open"}`)
		hook("hd_code", "coder", `{"event":"pre","body":{"kind":"code-js","code":"function hook(){ return {decision:'allow'} }"},"fail_mode":"open"}`)
		for _, c := range []struct{ name, hooks string }{
			{"fine", `{"channel_publish":["gate"]}`},
			{"ghostly", `{"channel_publish":["ghost"]}`},
		} {
			mustOK(src.ChannelsCreate(ctx, store.ChannelRow{Name: c.name, TenantID: "acme", Scope: "tenant", Semantic: "queue", CreatedAt: now, Hooks: json.RawMessage(c.hooks)}))
		}
		agent := func(id, name, def string) {
			_, err := src.AgentDefCreate(ctx, store.AgentDefRow{DefID: id, TenantID: "acme", Name: name, Version: 1, Definition: json.RawMessage(def), CreatedAt: now})
			mustOK(err)
		}
		agent("ad_ok", "helper", `{"system_prompt":"hi","hooks":{"agent_start":[{"name":"a","url":"https://hooks.example.com/a","headers":{"Authorization":"$cred:audit"}}]}}`)
		agent("ad_crlf", "smuggler", `{"hooks":{"agent_start":[{"name":"a","url":"https://hooks.example.com/a","headers":{"X-A":"a\r\nX-Injected: 1"}}]}}`)
		_, err := src.TeamDefCreate(ctx, store.TeamDefRow{DefID: "td_bad", TenantID: "acme", Name: "crew", Version: 1, CreatedAt: now,
			Definition: json.RawMessage(`{"entry":"s","states":[{"state":"s","handler":{"kind":"agent","hooks":{"agent_start":[{"name":"t","url":"ftp://hooks.example.com/t"}]}}}]}`)})
		mustOK(err)
	})

	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	res, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var nf *store.ErrNotFound
	for _, id := range []string{"hd_gate"} {
		if _, err := st.HookDefGet(ctx, id); err != nil {
			t.Errorf("valid hook def %s was not restored: %v", id, err)
		}
	}
	for _, id := range []string{"hd_host", "hd_code"} {
		if _, err := st.HookDefGet(ctx, id); !errors.As(err, &nf) {
			t.Errorf("hook def %s is on the target (err = %v)", id, err)
		}
	}
	if _, err := st.ChannelGet(ctx, "acme", "fine"); err != nil {
		t.Errorf("the channel naming a restored HookDef was not restored: %v", err)
	}
	if _, err := st.ChannelGet(ctx, "acme", "ghostly"); !errors.As(err, &nf) {
		t.Errorf("the channel naming a missing HookDef is on the target (err = %v)", err)
	}
	if _, err := st.AgentDefGet(ctx, "ad_ok"); err != nil {
		t.Errorf("the valid agent def was not restored: %v", err)
	}
	for _, get := range []func() error{
		func() error { _, err := st.AgentDefGet(ctx, "ad_crlf"); return err },
		func() error { _, err := st.TeamDefGet(ctx, "td_bad"); return err },
	} {
		if err := get(); !errors.As(err, &nf) {
			t.Errorf("a def with an invalid inline hook is on the target (err = %v)", err)
		}
	}
	for _, want := range [][]string{
		{"hook_def acme/hosty", "is set by the call itself"},
		{"hook_def acme/coder", "code hooks are not enabled"},
		{"channel_def acme/ghostly", "not restored", "ghost"},
		{"agent_def acme/smuggler", "line break"},
		{"team_def acme/crew", "http:// or https://"},
	} {
		if !warned(res.Warnings, want...) {
			t.Errorf("no warning with %q: %v", want, res.Warnings)
		}
	}
	c := res.Restored
	if c["hook_defs_refused"] != 2 || c["channel_defs_refused"] != 1 || c["agent_defs_refused"] != 1 || c["team_defs_refused"] != 1 {
		t.Errorf("refused counters = %v; want hook 2, channel 1, agent 1, team 1", c)
	}
	if c["hook_defs"] != 1 || c["channel_defs"] != 1 || c["agent_defs"] != 1 {
		t.Errorf("restored counters = %v; want one hook def, one channel, one agent def", c)
	}
}
