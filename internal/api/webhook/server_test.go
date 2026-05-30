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

// fakeWebhookStore satisfies lookup.WebhookStore for the RFC H Decision
// 10 "Layer 2" durable-dedup tests. WebhookDefGetActive always misses (so
// the yaml cfg path resolves the Def); RunByIdempotencyKey returns a
// preconfigured run for a matching key.
type fakeWebhookStore struct {
	existing  map[string]store.Run // key -> run returned by RunByIdempotencyKey
	lookupErr error
	calls     int
}

func (f *fakeWebhookStore) WebhookDefGetActive(_ context.Context, name string) (store.WebhookDefRow, error) {
	return store.WebhookDefRow{}, &store.ErrNotFound{Kind: "webhook_def", ID: name}
}

func (f *fakeWebhookStore) RunByIdempotencyKey(_ context.Context, key string) (store.Run, bool, error) {
	f.calls++
	if f.lookupErr != nil {
		return store.Run{}, false, f.lookupErr
	}
	r, ok := f.existing[key]
	return r, ok, nil
}

// newTestReceiverWithStore mirrors newTestReceiver but wires a store so
// the Layer-2 dedup path is exercised. A fixed DeliveryIDHeader lets the
// test control the delivery id (= idempotency key) deterministically.
func newTestReceiverWithStore(t *testing.T, webhooks map[string]config.Webhook, fr runner.Runner, st *fakeWebhookStore, env map[string]string, allow []string, now time.Time) *Receiver {
	t.Helper()
	cfg := &config.Config{Webhooks: webhooks}
	al := make(map[string]bool, len(allow))
	for _, n := range allow {
		al[n] = true
	}
	return New(Deps{
		Cfg:          cfg,
		Store:        st,
		Runner:       fr,
		EnvAllowlist: al,
		Now:          fixedClock(now),
		Getenv:       mapGetenv(env),
	})
}

// TestReceiver_Layer2Dedup_ExistingRunReturnedWithoutSpawn pins the RFC H
// Decision 10 BEFORE-spawn check: a delivery whose id already has a run
// (e.g. a redelivery past the in-memory Layer-1 TTL) returns the existing
// run id as 202 WITHOUT invoking the runner.
func TestReceiver_Layer2Dedup_ExistingRunReturnedWithoutSpawn(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"do it"}`)
	const did = "delivery-abc"

	wh := config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "researcher",
		Auth: config.WebhookAuth{
			Kind: "hmac", Header: "X-Hub-Signature-256",
			SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery-Id",
		},
		PayloadMapping: map[string]string{"goal": "$.goal"},
	}
	fr := &fakeRunner{runID: "run-fresh", agentID: "agent-fresh"}
	st := &fakeWebhookStore{existing: map[string]store.Run{did: {ID: "run-existing", AgentID: "agent-existing"}}}
	rec := newTestReceiverWithStore(t, map[string]config.Webhook{"gh": wh}, fr, st, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	h.Set("X-Delivery-Id", did)
	w := doPost(rec, "gh", body, h)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if fr.called {
		t.Error("runner WAS invoked; Layer-2 dedup should have short-circuited the spawn")
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["run_id"] != "run-existing" {
		t.Errorf("run_id = %q, want run-existing (the deduped run)", got["run_id"])
	}
	if got["deduped"] != "true" {
		t.Errorf("deduped = %q, want true", got["deduped"])
	}
}

// TestReceiver_Layer2Dedup_ConcurrentRaceResolvesToWinner pins the
// concurrent-race path: the BEFORE-spawn check misses (no run yet), but a
// racing request won the CreateRun insert, so this request's RunOnce
// returns ErrDuplicateIdempotencyKey. The receiver re-looks-up the winner
// and returns it as 202 — NOT a 503.
func TestReceiver_Layer2Dedup_ConcurrentRaceResolvesToWinner(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"do it"}`)
	const did = "delivery-race"

	wh := config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "researcher",
		Auth: config.WebhookAuth{
			Kind: "hmac", Header: "X-Hub-Signature-256",
			SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery-Id",
		},
		PayloadMapping: map[string]string{"goal": "$.goal"},
	}
	// fakeRunner returns the dup sentinel BEFORE OnRegistered (setup-time),
	// modelling the loser of the insert race.
	fr := &fakeRunner{runErr: store.ErrDuplicateIdempotencyKey}
	// The store's BEFORE-spawn lookup misses first, then HITS on the
	// re-lookup after the dup error. Pre-seed the winner so the re-lookup
	// resolves it. (The BEFORE-check and the re-lookup both call the same
	// fake; pre-seeding means the BEFORE-check would also hit — so instead
	// we leave existing empty and flip it via a tiny indirection.)
	st := &raceStore{winner: store.Run{ID: "run-winner", AgentID: "agent-winner"}, key: did}
	rec := New(Deps{
		Cfg:          &config.Config{Webhooks: map[string]config.Webhook{"gh": wh}},
		Store:        st,
		Runner:       fr,
		EnvAllowlist: map[string]bool{"WH_SECRET": true},
		Now:          fixedClock(now),
		Getenv:       mapGetenv(map[string]string{"WH_SECRET": secret}),
	})

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	h.Set("X-Delivery-Id", did)
	w := doPost(rec, "gh", body, h)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (race resolved to winner); body=%s", w.Code, w.Body.String())
	}
	if !fr.called {
		t.Error("runner should have been invoked (this is the insert-race loser path)")
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["run_id"] != "run-winner" {
		t.Errorf("run_id = %q, want run-winner (re-lookup after dup)", got["run_id"])
	}
	if got["deduped"] != "true" {
		t.Errorf("deduped = %q, want true", got["deduped"])
	}
}

// raceStore models the insert-race timeline: the FIRST RunByIdempotencyKey
// (the BEFORE-spawn check) misses; subsequent calls (the post-dup
// re-lookup) hit the winner. This reproduces the window where two requests
// both pass the BEFORE-check and only one wins the unique-index insert.
type raceStore struct {
	winner store.Run
	key    string
	calls  int
}

func (s *raceStore) WebhookDefGetActive(_ context.Context, name string) (store.WebhookDefRow, error) {
	return store.WebhookDefRow{}, &store.ErrNotFound{Kind: "webhook_def", ID: name}
}

func (s *raceStore) RunByIdempotencyKey(_ context.Context, key string) (store.Run, bool, error) {
	s.calls++
	if s.calls == 1 {
		return store.Run{}, false, nil // BEFORE-spawn check: no run yet
	}
	if key == s.key {
		return s.winner, true, nil // re-lookup after the dup error
	}
	return store.Run{}, false, nil
}
