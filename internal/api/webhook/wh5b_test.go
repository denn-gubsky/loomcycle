package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// waitForHooks polls the fake store until it has recorded at least the
// expected number of channel publishes + memory sets, or the deadline
// elapses. The on_complete dispatch runs in the spawn goroutine AFTER the
// 202 is returned, so a test cannot assume the hooks fired by the time
// doPost returns.
func waitForHooks(t *testing.T, st *fakeWebhookStore, wantChan, wantMem int) ([]store.ChannelMessage, []memorySetCall) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		cp, ms := st.snapshotHooks()
		if len(cp) >= wantChan && len(ms) >= wantMem {
			return cp, ms
		}
		if time.Now().After(deadline) {
			t.Fatalf("on_complete hooks not fired in time: got %d channel / %d memory, want %d / %d",
				len(cp), len(ms), wantChan, wantMem)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestReceiver_OnCompleteChannelPublish_FiresAfterSpawnedRun(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"do it","user":"u-7"}`)

	wh := config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "researcher",
		Auth:     config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		PayloadMapping: map[string]string{
			"goal":    "$.goal",
			"user_id": "$.user",
		},
		OnComplete: []config.ScheduledRunHook{
			{Kind: "channel.publish", Channel: "results", Payload: map[string]any{"k": "v"}},
		},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	st := &fakeWebhookStore{existing: map[string]store.Run{}}
	rec := newTestReceiverWithStore(t, map[string]config.Webhook{"gh": wh}, fr, st,
		map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	if w := doPost(rec, "gh", body, h); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	cp, _ := waitForHooks(t, st, 1, 0)
	msg := cp[0]
	if msg.Channel != "results" {
		t.Errorf("channel = %q, want results", msg.Channel)
	}
	// userID was projected (u-7) → user scope.
	if msg.Scope != store.MemoryScopeUser || msg.ScopeID != "u-7" {
		t.Errorf("scope = %q/%q, want user/u-7", msg.Scope, msg.ScopeID)
	}
	var payload map[string]any
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["webhook_name"] != "gh" || payload["run_id"] != "run-1" || payload["agent_id"] != "agent-1" {
		t.Errorf("payload missing traceability fields: %+v", payload)
	}
}

func TestReceiver_OnCompleteMemorySet_FiresAfterSpawnedRun(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"do it"}`)

	wh := config.Webhook{
		Enabled:        true,
		Delivery:       "spawn",
		Agent:          "researcher",
		Auth:           config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		PayloadMapping: map[string]string{"goal": "$.goal"},
		OnComplete: []config.ScheduledRunHook{
			{Kind: "memory.set", Scope: "agent", Key: "last_seen", Payload: map[string]any{"n": 1}},
		},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	st := &fakeWebhookStore{existing: map[string]store.Run{}}
	rec := newTestReceiverWithStore(t, map[string]config.Webhook{"gh": wh}, fr, st,
		map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	if w := doPost(rec, "gh", body, h); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	_, ms := waitForHooks(t, st, 0, 1)
	set := ms[0]
	// agent scope keys on the webhook NAME (scheduler-parity choice).
	if set.scope != store.MemoryScopeAgent || set.scopeID != "gh" {
		t.Errorf("scope = %q/%q, want agent/gh", set.scope, set.scopeID)
	}
	if set.key != "last_seen" {
		t.Errorf("key = %q, want last_seen", set.key)
	}
}

func TestReceiver_OnCompleteMCPCall_LogsAndSkips(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"do it"}`)

	var logged []string
	wh := config.Webhook{
		Enabled:        true,
		Delivery:       "spawn",
		Agent:          "researcher",
		Auth:           config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		PayloadMapping: map[string]string{"goal": "$.goal"},
		OnComplete: []config.ScheduledRunHook{
			{Kind: "mcp.call", Server: "s", Tool: "t"},
			// A trailing channel.publish proves the loop CONTINUES past the
			// skipped mcp.call rather than aborting.
			{Kind: "channel.publish", Channel: "results"},
		},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	st := &fakeWebhookStore{existing: map[string]store.Run{}}
	rec := New(Deps{
		Cfg:          &config.Config{Webhooks: map[string]config.Webhook{"gh": wh}},
		Store:        st,
		Runner:       fr,
		EnvAllowlist: map[string]bool{"WH_SECRET": true},
		Now:          fixedClock(now),
		Getenv:       mapGetenv(map[string]string{"WH_SECRET": secret}),
		Logf:         func(f string, a ...any) { logged = append(logged, f) },
	})

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	if w := doPost(rec, "gh", body, h); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	// The channel.publish after the skipped mcp.call must still fire.
	waitForHooks(t, st, 1, 0)

	foundSkipLog := false
	for _, l := range logged {
		if strings.Contains(l, "mcp.call not wired") {
			foundSkipLog = true
		}
	}
	if !foundSkipLog {
		t.Errorf("expected an mcp.call-not-wired log line; got %v", logged)
	}
}

func TestReceiver_RecentDeliveries_ReturnsVerdictsNewestFirstCapped(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	wh := config.Webhook{
		Enabled:        true,
		Delivery:       "spawn",
		Agent:          "researcher",
		Auth:           config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery-Id"},
		PayloadMapping: map[string]string{"goal": "$.goal"},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil,
		map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	// One accepted delivery, then a tampered (bad-sig) one.
	good := []byte(`{"goal":"a"}`)
	hGood := http.Header{}
	hGood.Set("X-Hub-Signature-256", githubSig(secret, good))
	hGood.Set("X-Delivery-Id", "d-1")
	if w := doPost(rec, "gh", good, hGood); w.Code != http.StatusAccepted {
		t.Fatalf("good delivery status = %d, want 202", w.Code)
	}

	bad := []byte(`{"goal":"b"}`)
	hBad := http.Header{}
	hBad.Set("X-Hub-Signature-256", githubSig(secret, good)) // sig over `good`, body is `bad`
	if w := doPost(rec, "gh", bad, hBad); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad delivery status = %d, want 401", w.Code)
	}

	// Pass-through adminAuth (gating is wired+tested at the http-server layer).
	mux := http.NewServeMux()
	rec.MountAdmin(mux, func(h http.Handler) http.Handler { return h })
	req := httptest.NewRequest(http.MethodGet, "/v1/_webhooks/gh/recent-deliveries?limit=10", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("recent-deliveries status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		WebhookName string           `json:"webhook_name"`
		Deliveries  []deliveryRecord `json:"deliveries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.WebhookName != "gh" {
		t.Errorf("webhook_name = %q, want gh", resp.WebhookName)
	}
	if len(resp.Deliveries) != 2 {
		t.Fatalf("got %d deliveries, want 2", len(resp.Deliveries))
	}
	// Newest-first: the bad-sig reject was recorded last.
	if resp.Deliveries[0].Verdict != verdictRejectedSig {
		t.Errorf("newest verdict = %q, want %s", resp.Deliveries[0].Verdict, verdictRejectedSig)
	}
	if resp.Deliveries[1].Verdict != verdictAccepted {
		t.Errorf("oldest verdict = %q, want %s", resp.Deliveries[1].Verdict, verdictAccepted)
	}
	if resp.Deliveries[1].RunID != "run-1" {
		t.Errorf("accepted run_id = %q, want run-1", resp.Deliveries[1].RunID)
	}
}

func TestReceiver_RecentDeliveries_UnknownName_404(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rec := newTestReceiver(t, map[string]config.Webhook{}, &fakeRunner{}, nil, nil, nil, now)
	mux := http.NewServeMux()
	rec.MountAdmin(mux, func(h http.Handler) http.Handler { return h })
	req := httptest.NewRequest(http.MethodGet, "/v1/_webhooks/never-seen/recent-deliveries", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestReceiver_Test_ValidSig_ReturnsPreviewWithoutCredentialValuesAndNoRun(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"do the thing","user":"u-9","tok":"PAYLOAD-SECRET-VALUE"}`)

	wh := config.Webhook{
		Enabled:                true,
		Delivery:               "spawn",
		Agent:                  "researcher",
		Auth:                   config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		UserCredentialsFromEnv: map[string]string{"API_KEY": "ENV_CRED"},
		PayloadMapping: map[string]string{
			"goal":                 "$.goal",
			"user_id":              "$.user",
			"user_credentials.TOK": "$.tok",
		},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	env := map[string]string{"WH_SECRET": secret, "ENV_CRED": "ENV-SECRET-VALUE"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, env,
		[]string{"WH_SECRET", "ENV_CRED"}, now)

	mux := http.NewServeMux()
	rec.MountAdmin(mux, func(h http.Handler) http.Handler { return h })
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/gh/test", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fr.called {
		t.Fatal("dry-run /test must NOT invoke the runner")
	}

	// Credential VALUES must never appear anywhere in the response body.
	raw := w.Body.String()
	if strings.Contains(raw, "PAYLOAD-SECRET-VALUE") || strings.Contains(raw, "ENV-SECRET-VALUE") {
		t.Fatalf("credential VALUE leaked in /test response: %s", raw)
	}

	var resp testResult
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.WouldAccept || resp.Verdict != verdictAccepted {
		t.Errorf("would_accept=%v verdict=%q, want true/%s", resp.WouldAccept, resp.Verdict, verdictAccepted)
	}
	if resp.RunInputPreview.Agent != "researcher" || resp.RunInputPreview.UserID != "u-9" ||
		resp.RunInputPreview.Goal != "do the thing" {
		t.Errorf("preview = %+v", resp.RunInputPreview)
	}
	// Credential KEYS are present (the operator confirms wiring) — values not.
	keys := map[string]bool{}
	for _, k := range resp.RunInputPreview.CredentialKeys {
		keys[k] = true
	}
	if !keys["API_KEY"] || !keys["TOK"] {
		t.Errorf("credential_keys = %v, want API_KEY + TOK present", resp.RunInputPreview.CredentialKeys)
	}
}

func TestReceiver_Test_BadSig_WouldAcceptFalse(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	signed := []byte(`{"goal":"a"}`)
	tampered := []byte(`{"goal":"b"}`)

	wh := config.Webhook{
		Enabled:        true,
		Delivery:       "spawn",
		Agent:          "researcher",
		Auth:           config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		PayloadMapping: map[string]string{"goal": "$.goal"},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil,
		map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	mux := http.NewServeMux()
	rec.MountAdmin(mux, func(h http.Handler) http.Handler { return h })
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/gh/test", strings.NewReader(string(tampered)))
	req.Header.Set("X-Hub-Signature-256", githubSig(secret, signed)) // sig over the wrong body
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp testResult
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.WouldAccept {
		t.Errorf("would_accept = true on bad sig, want false")
	}
	if resp.Verdict != verdictRejectedSig {
		t.Errorf("verdict = %q, want %s", resp.Verdict, verdictRejectedSig)
	}
	if fr.called {
		t.Fatal("dry-run /test must NOT invoke the runner even on bad sig")
	}
}
