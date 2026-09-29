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
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// confinedTriggerServer is a server whose one agent, "target", is what a
// trigger fires, over a provider that records the identity and operator-key
// permission of every call.
func confinedTriggerServer(t *testing.T) (*Server, *storesqlite.Store, *identityRecordingProvider, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"target": {Provider: "scripted", Model: "stub-model", SystemPrompt: "you run when triggered", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "trigger_fire.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &identityRecordingProvider{scriptedProvider: &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "fired"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	return srv, st, prov, cfg
}

// confinedAuthorCtx is the ctx a resumed confined run presents: its bits
// restored from the row onto the run identity, and no principal.
func confinedAuthorCtx(tenant string) context.Context {
	return tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		AgentID: "a_confined", TenantID: tenant, UserID: "alice",
		OperatorKeyRestricted: true, Isolated: true,
	})
}

// assertFiredConfined waits for the trigger's run to call the provider and
// checks every call was denied the operator's key and ran isolated.
func assertFiredConfined(t *testing.T, prov *identityRecordingProvider) {
	t.Helper()
	waitFor(t, "the trigger to fire", func() bool {
		prov.mu.Lock()
		defer prov.mu.Unlock()
		return len(prov.seen) > 0
	})
	if prov.lastOpKeyAllowed.Load() {
		t.Error("the fired run's provider call was allowed the operator's key")
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	for i, id := range prov.seen {
		if !id.OperatorKeyRestricted || !id.Isolated {
			t.Errorf("fired call %d ran with restricted=%v isolated=%v, want both", i, id.OperatorKeyRestricted, id.Isolated)
		}
	}
}

// A schedule authored under a confined run's identity — the shape a resumed
// run presents, with its bits restored from the row and no principal on ctx —
// fires confined: the fired run is denied the operator's provider key and stays
// isolated. The stored flag alone is not the claim; the fire is.
func TestScheduleDef_FireOfAConfinedRunsScheduleIsConfined(t *testing.T) {
	srv, st, prov, cfg := confinedTriggerServer(t)
	srv.SetScheduleDefTool(&builtin.ScheduleDef{Store: st, Cfg: cfg})

	ctx := tools.WithScheduleDefPolicy(confinedAuthorCtx("acme"), tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
	res, err := srv.ScheduleDef(ctx, json.RawMessage(`{"op":"create","name":"laundered","overlay":{"agent":"target","schedule":"0 9 * * 1","user_id":"alice","prompt":[{"role":"user","content":[{"type":"trusted-text","text":"tick"}]}]}}`))
	if err != nil || res.IsError {
		t.Fatalf("create: %v %s", err, res.Text)
	}
	var created struct {
		DefID string `json:"def_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &created); err != nil || created.DefID == "" {
		t.Fatalf("create result %s: %v", res.Text, err)
	}
	// Due now rather than next Monday.
	if err := st.ScheduleRunStateSeed(context.Background(), created.DefID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	sched := scheduler.New(scheduler.Config{TickInterval: 20 * time.Millisecond}, st, srv, nil, nil, t.Logf)
	sched.Start(context.Background())
	defer sched.Stop()
	assertFiredConfined(t, prov)
}

// The same for a webhook: authored under a confined run's identity, a delivery
// to it spawns a confined run.
func TestWebhookDef_DeliveryToAConfinedRunsWebhookIsConfined(t *testing.T) {
	srv, st, prov, cfg := confinedTriggerServer(t)
	srv.SetWebhookDefTool(&builtin.WebhookDef{Store: st, Cfg: cfg})

	ctx := tools.WithWebhookDefPolicy(confinedAuthorCtx(""), tools.WebhookDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
	res, err := srv.WebhookDef(ctx, json.RawMessage(`{"op":"create","name":"laundered","overlay":{"enabled":true,"delivery":"spawn","agent":"target","auth":{"kind":"hmac","header":"X-Test-Signature","signing_secret_env":"LOOMCYCLE_TEST_WH_SECRET"},"payload_mapping":{"goal":"$.goal"}}}`))
	if err != nil || res.IsError {
		t.Fatalf("create: %v %s", err, res.Text)
	}

	const secret = "test-webhook-secret"
	rec := webhook.New(webhook.Deps{
		Store: st, Cfg: cfg, Runner: srv,
		EnvAllowlist: map[string]bool{"LOOMCYCLE_TEST_WH_SECRET": true},
		Getenv: func(k string) string {
			if k == "LOOMCYCLE_TEST_WH_SECRET" {
				return secret
			}
			return ""
		},
	})
	mux := http.NewServeMux()
	rec.Mount(mux)
	body := []byte(`{"goal":"handle the delivery"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/laundered", bytes.NewReader(body))
	req.Header.Set("X-Test-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("delivery status = %d: %s", w.Code, w.Body.String())
	}
	var accepted struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil || accepted.RunID == "" {
		t.Fatalf("delivery response %s: %v", w.Body.String(), err)
	}
	assertFiredConfined(t, prov)
	// The run is detached from the delivery; let it finish before the store closes.
	waitFor(t, "the delivered run to finish", func() bool {
		run, err := st.GetRun(context.Background(), accepted.RunID)
		return err == nil && run.Status != store.RunRunning
	})
}
