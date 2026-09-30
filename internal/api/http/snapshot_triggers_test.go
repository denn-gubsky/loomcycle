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
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// Every restore call site wires the same options: the authoring validators of
// the webhook and A2A sections — so a peer endpoint the target would not let
// an author create is not restored — and the two checks behind the
// missing-credential scan.

// restoreWarnings restores through one transport and returns the counts and
// the warnings.
func restoreWarnings(t *testing.T, srv *Server, via string, envelope []byte) (map[string]int, []string) {
	t.Helper()
	if via == "connector" {
		res, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: envelope})
		if err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		return res.Restored, res.Warnings
	}
	body, _ := json.Marshal(map[string]any{"json": json.RawMessage(envelope)})
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
	return resp.Restored, resp.Warnings
}

func triggerSource(t *testing.T) store.Store {
	t.Helper()
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	put := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	webhook := func(defID, name string, body string) {
		_, err := src.SnapshotRestoreWebhookDef(ctx, store.WebhookDefRow{DefID: defID, Name: name, Version: 1, CreatedAt: now, Definition: json.RawMessage(body)})
		put(err)
		_, err = src.SnapshotRestoreWebhookDefActive(ctx, store.WebhookDefActiveEntry{Name: name, DefID: defID, PromotedAt: now})
		put(err)
	}
	peer := func(defID, name string, body string) {
		_, err := src.SnapshotRestoreA2AAgentDef(ctx, store.A2AAgentDefRow{DefID: defID, Name: name, Version: 1, CreatedAt: now, Definition: json.RawMessage(body)})
		put(err)
		_, err = src.SnapshotRestoreA2AAgentDefActive(ctx, store.A2AAgentDefActiveEntry{Name: name, DefID: defID, PromotedAt: now})
		put(err)
	}
	webhook("wh_ok", "gh", `{"delivery":"spawn","agent":"a","enabled":true,"auth":{"kind":"hmac","signing_secret_env":"P3_TEST_UNSET_SECRET"},
		"user_credentials":{"slack":"p3-literal","jobs":"$cred:p3-jobs"}}`)
	// Planted directly into the source store, as a hand-edited or older
	// database could hold them: bodies the authoring tool would refuse.
	webhook("wh_bad", "bad-auth", `{"delivery":"spawn","agent":"a","auth":{"kind":"hmac","signing_secret_env":"lower-case-name"}}`)
	peer("aad_ok", "peer-ok", `{"endpoint":"https://peer.example/a2a","binding":"jsonrpc"}`)
	peer("aad_scheme", "peer-file", `{"agent_card_url":"file:///etc/passwd"}`)
	peer("aad_metadata", "peer-meta", `{"endpoint":"grpc://169.254.169.254:443","binding":"grpc"}`)
	card := func(defID, name string, body string) {
		_, err := src.SnapshotRestoreA2AServerCardDef(ctx, store.A2AServerCardDefRow{DefID: defID, Name: name, Version: 1, CreatedAt: now, Definition: json.RawMessage(body)})
		put(err)
	}
	card("ascd_ok", "card-ok", `{"name":"card-ok","exposed_agents":[{"agent_name":"helper"}]}`)
	card("ascd_bad", "card-bad", `{"name":"card-bad","exposed_agents":[]}`)
	return src
}

func TestRestore_EveryCallSiteRevalidatesAndScansTriggerDefs(t *testing.T) {
	for _, via := range []string{"connector", "http"} {
		t.Run(via, func(t *testing.T) {
			src := triggerSource(t)
			_, envelope, err := snapshot.Capture(context.Background(), src, snapshot.CaptureOptions{})
			if err != nil {
				t.Fatal(err)
			}
			srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
			srv.SetCredKeyable(func(_ context.Context, tenant, agent, user, name string) bool { return false })

			restored, warnings := restoreWarnings(t, srv, via, envelope)
			has := func(parts ...string) bool {
				for _, w := range warnings {
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
			if restored["webhook_defs"] != 1 || restored["a2a_agent_defs"] != 1 || restored["a2a_server_card_defs"] != 1 ||
				restored["defs_disabled_for_credentials"] != 1 {
				t.Errorf("restored = %v; want the valid webhook and peer only, the webhook disabled", restored)
			}
			for _, refused := range []struct{ where, why string }{
				{"webhook_def bad-auth", "not a valid env-var name"},
				{"a2a_agent_def peer-file", "http or https"},
				{"a2a_agent_def peer-meta", "private/loopback/link-local"},
				{"a2a_server_card_def card-bad", "exposed_agents"},
			} {
				if !has(refused.where, "not restored", refused.why) {
					t.Errorf("no warning refusing %s (%s): %v", refused.where, refused.why, warnings)
				}
			}
			for _, id := range []string{"aad_scheme", "aad_metadata"} {
				if _, err := srv.store.A2AAgentDefGet(context.Background(), id); err == nil {
					t.Errorf("the refused peer %s was written", id)
				}
			}
			if !has("missing credential", "webhook_def gh", "P3_TEST_UNSET_SECRET") ||
				!has("missing credential", "webhook_def gh", "$cred:p3-jobs") {
				t.Errorf("the scan did not name the unset env var and the unresolved $cred: %v", warnings)
			}
			if has("not checked") {
				t.Errorf("a call site left a scan check unwired: %v", warnings)
			}
			for _, w := range warnings {
				if strings.Contains(w, "p3-literal") {
					t.Errorf("a warning carries the literal credential: %s", w)
				}
			}
		})
	}
}
