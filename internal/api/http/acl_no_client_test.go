package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FY Part E: a source_cidr blocklist names client addresses. A run with
// no client by nature — a schedule's fire, a resume — has no address to
// match, so the blocklist does not refuse it; a webhook delivery has its
// sender's address, so the blocklist does.

const blocklistRun = `[{"effect":"deny","actions":["run"],"resources":["agent:worker"],"when":{"source_cidr":["198.51.100.0/24"]}}]`

func (e *aclResumeEnv) bindBlocklist(t *testing.T) {
	t.Helper()
	if _, err := e.st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "blocklist", Rules: json.RawMessage(blocklistRun)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, e.st, "blocklist")
}

func deliverSignedFrom(mux *http.ServeMux, name, remote string) *httptest.ResponseRecorder {
	body := []byte(`{"goal":"handle the delivery"}`)
	mac := hmac.New(sha256.New, []byte(ownerWebhookSecret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/acme/"+name, bytes.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("X-Test-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestSourceCIDR_ABlocklistDoesNotRefuseAScheduledFire(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	defID := e.createScheduleAsDev(t, "nightly", "worker")
	e.bindBlocklist(t)
	e.startScheduler(t)
	e.waitScheduleStatus(t, defID, "completed")
}

func TestSourceCIDR_ABlocklistRefusesAWebhookDeliveryFromItsRange(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn(), endTurn()}})
	mux := e.createWebhookAsDev(t, "hooked", "worker")
	e.bindBlocklist(t)
	if w := deliverSignedFrom(mux, "hooked", "198.51.100.7:4000"); w.Code != http.StatusForbidden {
		t.Errorf("a delivery from the blocked range: %d %s, want 403", w.Code, w.Body.String())
	}
	if w := deliverSignedFrom(mux, "hooked", "203.0.113.9:4000"); w.Code != http.StatusAccepted {
		t.Errorf("a delivery from elsewhere: %d %s, want 202", w.Code, w.Body.String())
	}
}

func TestSourceCIDR_ABlocklistDoesNotNarrowAResumedRun(t *testing.T) {
	e := keyspaceWriteFixture(t, authz.ModeCompat, `[{"effect":"deny","actions":["write"],"resources":["memory:tenant"],"when":{"source_cidr":["198.51.100.0/24"]}}]`,
		[][]providers.Event{
			toolCallTurn("tu_probe", "Probe", `{}`), endTurn(),
			toolCallTurn("tu_resumed_set", "Memory", `{"op":"set","scope":"tenant","key":"after-resume","value":"x"}`), endTurn(),
		})
	run := e.run(t, "a_keeper_blocklist")
	resumeAndFinish(t, e.srv, run)
	if res := resumedToolResult(t, e.srv, run, "tu_resumed_set"); strings.Contains(res, "read-only") {
		t.Errorf("a blocklist naming other addresses made the resumed run read-only: %s", res)
	}
}

// A run a client started keeps that client's address: a resume re-stamps the
// address recorded with the run, so a deny naming the client's range holds
// after the resume as it held at the start.
func TestSourceCIDR_AResumedRunKeepsItsClientsAddress(t *testing.T) {
	e := keyspaceWriteFixture(t, authz.ModeCompat, `[{"effect":"deny","actions":["write"],"resources":["memory:tenant"],"when":{"source_cidr":["127.0.0.0/8","::1/128"]}}]`,
		[][]providers.Event{
			toolCallTurn("tu_probe", "Probe", `{}`), endTurn(),
			toolCallTurn("tu_resumed_set", "Memory", `{"op":"set","scope":"tenant","key":"after-resume","value":"x"}`), endTurn(),
		})
	run := e.run(t, "a_keeper_client")
	waitFor(t, "the live probe", func() bool { return len(e.probe.calls()) >= 1 })
	if live := e.probe.calls()[0]; !slices.Equal(live.Memory.ReadOnlyScopes, []string{"tenant"}) {
		t.Fatalf("live policy: read-only %v; the deny on the client's range did not hold at the start", live.Memory.ReadOnlyScopes)
	}
	if rec, ok := decodeRunConfig(run.RunConfig); !ok || rec.ACLOrigin == nil || rec.ACLOrigin.SourceIP == "" {
		t.Fatalf("the run did not record its client's address: %s", run.RunConfig)
	}
	resumeAndFinish(t, e.srv, run)
	if res := resumedToolResult(t, e.srv, run, "tu_resumed_set"); !strings.Contains(res, "read-only") {
		t.Errorf("the resumed run wrote the scope its client's range is denied: %s", res)
	}
}

// A client request recorded before its address was kept is unknown, not
// clientless: a deny on an address range holds for it.
func TestAClientOriginWithNoRecordedAddressIsUnknown(t *testing.T) {
	ctx := (&aclOriginRecord{Transport: "http"}).withOriginClient(context.Background())
	if authz.ContextFrom(ctx).NoClient {
		t.Error("a client request with no recorded address was taken for a run with no client")
	}
	if !authz.ContextFrom((&aclOriginRecord{Transport: "schedule"}).withOriginClient(context.Background())).NoClient {
		t.Error("a schedule's fire was not taken for a run with no client")
	}
	if ip := authz.ContextFrom((&aclOriginRecord{Transport: "http", SourceIP: "198.51.100.7"}).withOriginClient(context.Background())).SourceIP; ip.String() != "198.51.100.7" {
		t.Errorf("the recorded address was not re-stamped: %v", ip)
	}
}
