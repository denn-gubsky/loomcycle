package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeRunner records the RunInput it was driven with and invokes
// OnRegistered so the async-spawn path returns a run id.
type fakeRunner struct {
	mu      sync.Mutex
	lastIn  runner.RunInput
	called  bool
	runErr  error // when set, returned BEFORE OnRegistered (setup-error path)
	runID   string
	agentID string
}

func (f *fakeRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	f.mu.Lock()
	f.lastIn = in
	f.called = true
	f.mu.Unlock()
	if f.runErr != nil {
		return f.runErr // setup-time failure: return before OnRegistered
	}
	if cb.OnRegistered != nil {
		cb.OnRegistered(f.agentID, f.runID, "sess-1", "")
	}
	return nil
}

func (f *fakeRunner) input() runner.RunInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIn
}

// fakePublisher records channel publishes.
type fakePublisher struct {
	mu      sync.Mutex
	channel string
	payload json.RawMessage
	called  bool
}

func (p *fakePublisher) Publish(ctx context.Context, channel string, scope store.MemoryScope, scopeID string,
	payload json.RawMessage, deliverAt time.Time, publishedByUserID string, maxMessages, defaultTTLSeconds int,
) (store.ChannelMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.channel = channel
	p.payload = payload
	p.called = true
	return store.ChannelMessage{ID: "msg-1"}, nil
}

func (p *fakePublisher) PublishNow(ctx context.Context, channel string, scope store.MemoryScope, scopeID string,
	payload json.RawMessage, publishedByUserID string, maxMessages, defaultTTLSeconds int,
) (store.ChannelMessage, error) {
	return p.Publish(ctx, channel, scope, scopeID, payload, time.Time{}, publishedByUserID, maxMessages, defaultTTLSeconds)
}

// newTestReceiver wires a Receiver against a yaml-only config (no store),
// a fixed clock, and a map-backed getenv.
func newTestReceiver(t *testing.T, webhooks map[string]config.Webhook, fr runner.Runner, fp *fakePublisher, env map[string]string, allow []string, now time.Time) *Receiver {
	t.Helper()
	cfg := &config.Config{Webhooks: webhooks}
	al := make(map[string]bool, len(allow))
	for _, n := range allow {
		al[n] = true
	}
	d := Deps{
		Cfg:          cfg,
		Runner:       fr,
		EnvAllowlist: al,
		Now:          fixedClock(now),
		Getenv:       mapGetenv(env),
	}
	if fp != nil {
		d.Publisher = fp
	}
	return New(d)
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func doPost(rec *Receiver, name string, body []byte, hdr http.Header) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	rec.Mount(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/"+name, bytesReader(body))
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestReceiver_SpawnMode_ValidGitHubSig_Builds202AndRunInput(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"do the thing","user":"u-9","tok":"payload-cred"}`)

	wh := config.Webhook{
		Enabled:                true,
		Delivery:               "spawn",
		Agent:                  "researcher",
		Auth:                   config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		UserCredentialsFromEnv: map[string]string{"API_KEY": "ENV_CRED", "TOK": "ENV_TOK"},
		PayloadMapping: map[string]string{
			"goal":                 "$.goal",
			"user_id":              "$.user",
			"user_credentials.TOK": "$.tok",
		},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	env := map[string]string{"WH_SECRET": secret, "ENV_CRED": "env-api-key", "ENV_TOK": "env-tok-value"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, env, []string{"WH_SECRET", "ENV_CRED", "ENV_TOK"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "gh", body, h)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if !fr.called {
		t.Fatal("runner was not invoked")
	}
	in := fr.input()
	if in.Agent != "researcher" {
		t.Errorf("agent = %q, want researcher", in.Agent)
	}
	if in.UserID != "u-9" {
		t.Errorf("user_id = %q, want u-9", in.UserID)
	}
	// The goal is external webhook payload → untrusted-block (fenced in
	// <untrusted> tags by the loop), NOT trusted-text. This is the
	// prompt-injection boundary for webhook-triggered runs.
	if len(in.Segments) != 1 || len(in.Segments[0].Content) != 1 ||
		in.Segments[0].Content[0].Text != "do the thing" ||
		in.Segments[0].Content[0].Type != "untrusted-block" ||
		in.Segments[0].Content[0].Kind != "webhook_payload" {
		t.Errorf("segment not built from mapped goal as untrusted-block: %+v", in.Segments)
	}
	// Env-resolved credential survives.
	if in.UserCredentials["API_KEY"] != "env-api-key" {
		t.Errorf("API_KEY cred = %q, want env-api-key", in.UserCredentials["API_KEY"])
	}
	// Payload overlay WINS over env for the same key.
	if in.UserCredentials["TOK"] != "payload-cred" {
		t.Errorf("TOK cred = %q, want payload-cred (payload wins)", in.UserCredentials["TOK"])
	}
}

func TestReceiver_TamperedBody_401(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	signed := []byte(`{"goal":"a"}`)
	tampered := []byte(`{"goal":"b"}`)
	wh := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x",
		Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, signed))
	w := doPost(rec, "gh", tampered, h)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if fr.called {
		t.Fatal("runner invoked on bad signature")
	}
}

func TestReceiver_ReplayWithinTTL_401(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"a"}`)
	wh := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x",
		Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery"}}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	h.Set("X-Delivery", "dup-1")

	if w := doPost(rec, "gh", body, h); w.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", w.Code)
	}
	if w := doPost(rec, "gh", body, h); w.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", w.Code)
	}
}

func TestReceiver_UnresolvableSecret_503(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"a"}`)
	wh := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x",
		Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	// WH_SECRET NOT in allowlist.
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": "shhh"}, []string{}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig("shhh", body))
	w := doPost(rec, "gh", body, h)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["error"] != "secret_unresolvable" || got["secret_env"] != "WH_SECRET" {
		t.Errorf("body = %v, want secret_unresolvable + secret_env=WH_SECRET", got)
	}
}

func TestReceiver_RateLimited_429WithRetryAfter(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	wh := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x",
		Auth:      config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		RateLimit: config.WebhookRateLimit{RequestsPerMinute: 60, Burst: 1}}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	// Distinct bodies so dedup doesn't intercept the second request first.
	mk := func(i string) (*http.Header, []byte) {
		b := []byte(`{"goal":"` + i + `"}`)
		h := http.Header{}
		h.Set("X-Hub-Signature-256", githubSig(secret, b))
		return &h, b
	}
	h1, b1 := mk("one")
	if w := doPost(rec, "gh", b1, *h1); w.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", w.Code)
	}
	h2, b2 := mk("two")
	w := doPost(rec, "gh", b2, *h2)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header on 429")
	}
}

func TestReceiver_ChannelMode_PublishesRawPayload(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"event":"ping"}`)
	wh := config.Webhook{Enabled: true, Delivery: "channel", Channel: "_system/ingress",
		Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}}
	fp := &fakePublisher{}
	rec := newTestReceiver(t, map[string]config.Webhook{"ch": wh}, &fakeRunner{}, fp, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "ch", body, h)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if !fp.called {
		t.Fatal("publisher not invoked")
	}
	if fp.channel != "_system/ingress" {
		t.Errorf("channel = %q, want _system/ingress", fp.channel)
	}
	if string(fp.payload) != string(body) {
		t.Errorf("payload = %s, want raw body %s", fp.payload, body)
	}
}

func TestReceiver_UnknownWebhook_404(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rec := newTestReceiver(t, map[string]config.Webhook{}, &fakeRunner{}, nil, nil, nil, now)
	w := doPost(rec, "nope", []byte(`{}`), http.Header{})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// busRunner publishes a terminal run-state event after OnRegistered so the
// ?sync path observes completion. It subscribes-side races are avoided
// because spawnSync subscribes BEFORE invoking RunOnce.
type busRunner struct {
	bus     *runstate.Bus
	runID   string
	agentID string
}

func (r *busRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	if cb.OnRegistered != nil {
		cb.OnRegistered(r.agentID, r.runID, "sess-1", "")
	}
	// Simulate the loop reaching a terminal state.
	r.bus.Publish(runstate.RunStateEvent{
		RunID:   r.runID,
		AgentID: r.agentID,
		UserID:  in.UserID,
		Status:  string(store.RunCompleted),
	})
	return nil
}

func TestReceiver_SyncMode_BlocksUntilTerminal_200(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"g","user":"u-7"}`)
	wh := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x",
		Auth:         config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		SyncResponse: config.WebhookSyncResponse{Enabled: true, TimeoutMs: 2000},
		PayloadMapping: map[string]string{
			"goal":    "$.goal",
			"user_id": "$.user",
		}}
	bus := runstate.NewBus()
	br := &busRunner{bus: bus, runID: "run-9", agentID: "agent-9"}
	cfg := &config.Config{Webhooks: map[string]config.Webhook{"gh": wh}}
	rec := New(Deps{
		Cfg:          cfg,
		Runner:       br,
		RunStateBus:  bus,
		EnvAllowlist: map[string]bool{"WH_SECRET": true},
		Now:          fixedClock(now),
		Getenv:       mapGetenv(map[string]string{"WH_SECRET": secret}),
	})

	mux := http.NewServeMux()
	rec.Mount(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/gh?sync=true", bytesReader(body))
	req.Header.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["status"] != "completed" || got["run_id"] != "run-9" {
		t.Errorf("body = %v, want status=completed run_id=run-9", got)
	}
}

func TestReceiver_SpawnSetupError_503(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"a"}`)
	wh := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "ghost",
		Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}}
	fr := &fakeRunner{runErr: runner.ErrUnknownAgent}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "gh", body, h)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown agent setup error)", w.Code)
	}
}
