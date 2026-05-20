package http

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// stubProvider replays a fixed event sequence, ignoring the request entirely.
// If preEvents are set and req.OnEvent is non-nil, they are fired
// synchronously through the OnEvent callback before the channel events —
// simulating a driver that emitted EventRetry during a 429 sleep.
type stubProvider struct {
	events    []providers.Event
	preEvents []providers.Event
}

func (s *stubProvider) ID() string                    { return "stub" }
func (s *stubProvider) Probe(_ context.Context) error { return nil }
func (s *stubProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (s *stubProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (s *stubProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	if req.OnEvent != nil {
		for _, ev := range s.preEvents {
			req.OnEvent(ev)
		}
	}
	ch := make(chan providers.Event, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

type stubResolver struct{ p providers.Provider }

func (r *stubResolver) Get(_ string) (providers.Provider, error) { return r.p, nil }

func TestHandleRunsSSE(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model", AllowedTools: []string{"Read"}, SystemPrompt: "be brief"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode for tests
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "hello"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 1}},
	}}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, nil)

	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}

	// Read SSE frames. v0.4 announces agent_id in a side-channel
	// "agent" frame before the loop emits its first model event;
	// agent → started → text → usage → done.
	got := readEvents(t, resp.Body)
	wantTypes := []string{"agent", "started", "text", "usage", "done"}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d frames %v, want %d %v", len(got), got, len(wantTypes), wantTypes)
	}
	for i, w := range wantTypes {
		if got[i] != w {
			t.Errorf("frame %d: got %q want %q", i, got[i], w)
		}
	}
}

// v0.3.2 end-to-end: a driver that fires EventRetry through req.OnEvent
// (as our drivers do during a 429 sleep) must surface as `event: retry`
// over SSE, ahead of the main response events. Adapter UIs read this to
// show "waiting on rate limit" live.
func TestHandleRunsSSEEmitsRetryFrame(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model", AllowedTools: []string{"Read"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	provider := &stubProvider{
		preEvents: []providers.Event{{
			Type: providers.EventRetry,
			Retry: &providers.RetryInfo{
				Provider: "stub",
				Attempt:  1,
				WaitMs:   12345,
				Reason:   "retry-after header",
			},
		}},
		events: []providers.Event{
			{Type: providers.EventText, Text: "hi"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, nil)

	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	frames := readFrames(t, resp.Body)

	// Must see a retry frame somewhere in the stream, with the full payload
	// the adapter UI needs.
	var retry *sseFrame
	for i := range frames {
		if frames[i].Event == "retry" {
			retry = &frames[i]
			break
		}
	}
	if retry == nil {
		var types []string
		for _, f := range frames {
			types = append(types, f.Event)
		}
		t.Fatalf("no retry frame in stream, got events: %v", types)
	}
	r, ok := retry.Data["retry"].(map[string]any)
	if !ok {
		t.Fatalf("retry frame has no .retry object: %#v", retry.Data)
	}
	if r["provider"] != "stub" {
		t.Errorf("retry.provider = %v, want stub", r["provider"])
	}
	if r["attempt"].(float64) != 1 {
		t.Errorf("retry.attempt = %v, want 1", r["attempt"])
	}
	if r["wait_ms"].(float64) != 12345 {
		t.Errorf("retry.wait_ms = %v, want 12345", r["wait_ms"])
	}
	if r["reason"] != "retry-after header" {
		t.Errorf("retry.reason = %v, want 'retry-after header'", r["reason"])
	}
}

func TestHealthz(t *testing.T) {
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100}}
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	// v0.8.21: response is JSON {ok, metrics_enabled}. metrics
	// sampler isn't wired in this test, so metrics_enabled must
	// be false — the Activity Monitor UI keys off this flag to
	// render its "metrics off" empty state immediately, without
	// probing /v1/_metrics and getting 503.
	var body struct {
		OK             bool `json:"ok"`
		MetricsEnabled bool `json:"metrics_enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK {
		t.Errorf("ok = %v, want true", body.OK)
	}
	if body.MetricsEnabled {
		t.Errorf("metrics_enabled = %v, want false (sampler not wired in test)", body.MetricsEnabled)
	}
}

func TestAuthRequired(t *testing.T) {
	cfg := &config.Config{
		Agents:      map[string]config.AgentDef{"default": {Model: "x"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	cfg.Env.AuthToken = "secret"
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Regression: bodies larger than the cap are rejected, not silently read into
// memory. We send valid JSON whose `text` field is large enough to cross the
// 1 MiB cap — without MaxBytesReader, the decoder happily reads it all and
// returns 200. With MaxBytesReader, the decoder fails mid-parse.
func TestRunsRejectsOversizedBody(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventDone, StopReason: "end_turn"},
	}}
	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(1, 1, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// 2 MiB of "x" inside a valid JSON string — guaranteed to cross 1 MiB cap.
	huge := strings.Repeat("x", 2<<20)
	body := `{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"` + huge + `"}]}]}`

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	// MaxBytesReader returns 400 (decoder sees a "http: request body too large"
	// error mid-parse) or 413. Anything 2xx means the cap was bypassed.
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = 200 — oversized body was accepted (MaxBytesReader missing)")
	}
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 400 or 413", resp.StatusCode)
	}
}

// Regression: requests with the wrong Content-Type get 415, not a confusing
// JSON parse error. Empty Content-Type is allowed (curl POST without -H).
func TestRunsRejectsWrongContentType(t *testing.T) {
	cfg := &config.Config{
		Agents:      map[string]config.AgentDef{"default": {Model: "x"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/x-www-form-urlencoded", strings.NewReader(`agent=default`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415 for non-JSON content type", resp.StatusCode)
	}
}

// Regression: a panic in any /v1 handler returns 500 to the caller and
// keeps the process alive. Without recoveryMiddleware, the test would
// terminate with the panic propagating through the test harness.
func TestRecoveryMiddlewareCatchesPanic(t *testing.T) {
	cfg := &config.Config{
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), nil)

	// Wrap a handler that always panics with the same recoveryMiddleware
	// the server uses. Verifies the middleware itself, independent of the
	// /v1/runs path's other validations.
	mux := http.NewServeMux()
	mux.Handle("/panic", recoveryMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("synthetic panic for test")
	})))
	_ = srv // keep srv referenced; not used here directly

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/panic")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (panic should have been recovered)", resp.StatusCode)
	}
}

// nonFlushingWriter implements http.ResponseWriter but NOT http.Flusher.
// httptest's writers all implement Flusher, so we need this to exercise the
// "writer cannot stream" fallback path.
type nonFlushingWriter struct {
	header http.Header
	status int
	body   strings.Builder
}

func (n *nonFlushingWriter) Header() http.Header {
	if n.header == nil {
		n.header = make(http.Header)
	}
	return n.header
}
func (n *nonFlushingWriter) WriteHeader(s int)           { n.status = s }
func (n *nonFlushingWriter) Write(b []byte) (int, error) { return n.body.Write(b) }

// Regression: when the ResponseWriter doesn't support flushing, the handler
// must refuse cleanly with 500 instead of silently buffering every SSE frame
// until handler return.
func TestRunsRejectsNonFlushableWriter(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	srv := New(cfg, &stubResolver{p: &stubProvider{}}, nil, concurrency.New(1, 1, time.Second), nil)

	w := &nonFlushingWriter{}
	body := strings.NewReader(`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`)
	req := httptest.NewRequest("POST", "/v1/runs", body)
	req.Header.Set("Content-Type", "application/json")
	srv.Mux().ServeHTTP(w, req)

	if w.status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.status)
	}
	if !strings.Contains(w.body.String(), "streaming") {
		t.Errorf("body should mention streaming: %q", w.body.String())
	}
}

// Regression: when a Store is wired, /v1/runs records the session+run+events
// so a follow-up GetTranscript returns the full transcript. Also verifies
// the SSE stream announces the new session_id as a "session" frame so the
// caller can address continuation requests.
func TestRunsPersistsToStore(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "hello"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 1}},
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	// The first frame must announce the session ID.
	if !strings.Contains(bodyStr, "event: session") {
		t.Errorf("missing session announcement in SSE stream:\n%s", bodyStr)
	}

	// Pull the session ID out of the announcement frame.
	sessionID := extractSessionID(bodyStr)
	if sessionID == "" {
		t.Fatalf("could not parse session_id from stream:\n%s", bodyStr)
	}

	transcript, err := st.GetTranscript(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Loop emits at least started, text, usage, done — same as v0.2 SSE.
	if len(transcript) < 4 {
		t.Errorf("transcript len = %d, want >= 4 (started/text/usage/done)", len(transcript))
	}
	wantTypes := map[string]bool{"started": true, "text": true, "done": true}
	for _, ev := range transcript {
		delete(wantTypes, ev.Type)
	}
	if len(wantTypes) > 0 {
		t.Errorf("missing event types in transcript: %v", wantTypes)
	}
}

// Regression: GET /v1/sessions/{id}/transcript returns the persisted events
// of a session that's already been run via POST /v1/runs. Tests the full
// chain: post → record → read back.
func TestTranscriptEndpointReturnsEvents(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "hi"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()

	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Create a session by running once.
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sessionID := extractSessionID(string(body))
	if sessionID == "" {
		t.Fatalf("no session id in stream:\n%s", string(body))
	}

	// Now read transcript.
	tResp, err := http.Get(ts.URL + "/v1/sessions/" + sessionID + "/transcript")
	if err != nil {
		t.Fatal(err)
	}
	defer tResp.Body.Close()
	if tResp.StatusCode != 200 {
		t.Fatalf("transcript status = %d", tResp.StatusCode)
	}

	var got transcriptResponse
	if err := json.NewDecoder(tResp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Session.ID != sessionID {
		t.Errorf("session.id = %q, want %q", got.Session.ID, sessionID)
	}
	if len(got.Events) < 4 {
		t.Errorf("events len = %d, want >=4", len(got.Events))
	}
	// The text event must round-trip its Text field through the typed decode.
	var foundText bool
	for _, ev := range got.Events {
		if ev.Type == "text" && ev.Event.Text == "hi" {
			foundText = true
			break
		}
	}
	if !foundText {
		t.Errorf("no text event with Text=\"hi\" in transcript: %+v", got.Events)
	}
}

// Regression: GET on an unknown session returns 404, not 500.
func TestTranscriptEndpoint404OnUnknownSession(t *testing.T) {
	cfg := &config.Config{
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/sessions/s_nope/transcript")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// Regression: the transcript endpoint requires a Store to be configured.
// Without one, return 404 with a clear message rather than panicking.
func TestTranscriptEndpoint404WhenStoreNotConfigured(t *testing.T) {
	cfg := &config.Config{
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/sessions/s_anything/transcript")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// Regression: POST /v1/sessions/{id}/messages replays the prior transcript
// and runs the model with both the old conversation and the new user input.
// The provider stub records the messages it receives; the second request
// must see the first request's user message + assistant reply + the new
// user message.
func TestMessagesEndpointReplaysAndContinues(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	// First call returns "first reply"; second call returns "second reply".
	provider := &callableProvider{}
	provider.call = func(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
		// Note: callableProvider.Call has already appended req to .requests
		// under .mu; we read the index without re-locking.
		idx := len(provider.requests) - 1
		text := "first reply"
		if idx == 1 {
			text = "second reply"
		}
		evs := []providers.Event{
			{Type: providers.EventText, Text: text},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 2}},
		}
		ch := make(chan providers.Event, len(evs))
		for _, e := range evs {
			ch <- e
		}
		close(ch)
		return ch, nil
	}

	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()

	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// First call: fresh session.
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hello"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sessionID := extractSessionID(string(body))
	if sessionID == "" {
		t.Fatalf("no session id from first call:\n%s", string(body))
	}

	// Second call: continuation on same session.
	resp2, err := http.Post(ts.URL+"/v1/sessions/"+sessionID+"/messages", "application/json", strings.NewReader(
		`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"and again"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("status = %d, body=%s", resp2.StatusCode, string(body2))
	}
	if !strings.Contains(string(body2), "second reply") {
		t.Errorf("missing second-call reply in stream:\n%s", string(body2))
	}

	// Verify the second provider call carried the prior conversation.
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 2 {
		t.Fatalf("provider got %d calls, want 2", len(provider.requests))
	}
	secondMsgs := provider.requests[1].Messages
	// Expected (at minimum): user("hello"), assistant("first reply"), user("and again").
	if len(secondMsgs) < 3 {
		t.Fatalf("second call carried %d messages, want >=3: %+v", len(secondMsgs), secondMsgs)
	}
	// First message: user with "hello".
	if secondMsgs[0].Role != "user" || !containsText(secondMsgs[0].Content, "hello") {
		t.Errorf("first replayed message: %+v", secondMsgs[0])
	}
	// Some message in the middle: assistant with "first reply".
	var foundAsst bool
	for _, m := range secondMsgs[1 : len(secondMsgs)-1] {
		if m.Role == "assistant" && containsText(m.Content, "first reply") {
			foundAsst = true
		}
	}
	if !foundAsst {
		t.Errorf("missing assistant reply in replay: %+v", secondMsgs)
	}
	// Last message: user with "and again".
	last := secondMsgs[len(secondMsgs)-1]
	if last.Role != "user" || !containsText(last.Content, "and again") {
		t.Errorf("last message: %+v", last)
	}
}

// v0.3.2: two concurrent POSTs to the same session must serialize at the
// session level. The second is fast-failed with 409 / session_busy
// because blocking on an open SSE handler would hold the second HTTP
// connection for the full duration of the first run, and a second
// transcript replay overlapping the first would corrupt history.
func TestPerSessionLockRejectsConcurrentMessages(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	// Provider is non-blocking by default; the test swaps in a blocking
	// implementation after the seed call completes.
	provider := &callableProvider{}
	provider.call = func(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
		ch := make(chan providers.Event, 2)
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
		close(ch)
		return ch, nil
	}

	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()

	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Seed: a fresh run to obtain a session id. Non-blocking.
	resp0, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	body0, _ := io.ReadAll(resp0.Body)
	resp0.Body.Close()
	sessionID := extractSessionID(string(body0))
	if sessionID == "" {
		t.Fatalf("no session id from seed call:\n%s", string(body0))
	}

	// Now swap in a blocking provider that holds until release2 is closed.
	// Signal entry via a closed channel so the test waits deterministically
	// rather than spinning on a length read.
	release2 := make(chan struct{})
	entered := make(chan struct{}, 1)
	provider.call = func(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		ch := make(chan providers.Event, 2)
		go func() {
			defer close(ch)
			<-release2
			ch <- providers.Event{Type: providers.EventText, Text: "ok"}
			ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
		}()
		return ch, nil
	}

	// Fire two concurrent POSTs to the same session.
	type result struct {
		status int
		body   string
	}
	first := make(chan result, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/v1/sessions/"+sessionID+"/messages", "application/json", strings.NewReader(
			`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"q1"}]}]}`,
		))
		if err != nil {
			first <- result{status: -1, body: err.Error()}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		first <- result{status: resp.StatusCode, body: string(b)}
	}()

	// Wait deterministically for the first request to enter the provider
	// (and thus hold the session lock).
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release2)
		t.Fatal("first continuation never reached the provider")
	}

	// Second continuation while the first is mid-call.
	resp2, err := http.Post(ts.URL+"/v1/sessions/"+sessionID+"/messages", "application/json", strings.NewReader(
		`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"q2"}]}]}`,
	))
	if err != nil {
		close(release2)
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusConflict {
		close(release2)
		<-first
		t.Fatalf("second status = %d, want 409; body=%s", resp2.StatusCode, string(body2))
	}
	if !strings.Contains(string(body2), `"code":"session_busy"`) {
		close(release2)
		<-first
		t.Fatalf("second body missing session_busy code: %s", string(body2))
	}

	// Let the first complete, then verify a follow-up succeeds (lock released).
	close(release2)
	got := <-first
	if got.status != 200 {
		t.Fatalf("first call failed: status=%d body=%s", got.status, got.body)
	}

	resp3, err := http.Post(ts.URL+"/v1/sessions/"+sessionID+"/messages", "application/json", strings.NewReader(
		`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"q3"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		b, _ := io.ReadAll(resp3.Body)
		t.Fatalf("post-release status = %d, body=%s", resp3.StatusCode, string(b))
	}
}

// v0.3.2: the per-session lock must be scoped per-session, not global.
// Two concurrent POSTs to DIFFERENT sessions must both succeed.
func TestPerSessionLockDoesNotAffectOtherSessions(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &callableProvider{}
	provider.call = func(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
		ch := make(chan providers.Event, 2)
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
		close(ch)
		return ch, nil
	}

	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()

	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Two fresh runs → two distinct session IDs.
	mkSession := func() string {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
		))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return extractSessionID(string(b))
	}
	s1, s2 := mkSession(), mkSession()
	if s1 == "" || s2 == "" || s1 == s2 {
		t.Fatalf("bad session ids: %q %q", s1, s2)
	}

	// Concurrent continuation on each — both must succeed.
	var wg sync.WaitGroup
	results := make([]int, 2)
	for i, sid := range []string{s1, s2} {
		wg.Add(1)
		go func(i int, sid string) {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/sessions/"+sid+"/messages", "application/json", strings.NewReader(
				`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
			))
			if err != nil {
				results[i] = -1
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			results[i] = resp.StatusCode
		}(i, sid)
	}
	wg.Wait()
	for i, st := range results {
		if st != 200 {
			t.Errorf("session %d status = %d, want 200", i, st)
		}
	}
}

// v0.3.2 review fix: unknown session IDs must NOT grow sessionLocks.
// Previously trySessionLock ran before existence validation, so a caller
// spamming /v1/sessions/<random>/messages or /v1/runs with an unknown
// session_id could leak a *sync.Mutex per request indefinitely.
func TestUnknownSessionIDDoesNotGrowLockTable(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()

	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Hammer /messages with 50 unknown ids.
	for i := 0; i < 50; i++ {
		resp, err := http.Post(ts.URL+"/v1/sessions/s_unknown_"+fmt.Sprint(i)+"/messages", "application/json", strings.NewReader(
			`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
		))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("messages unknown #%d: status = %d, want 404", i, resp.StatusCode)
		}
	}

	// Hammer /runs with 50 unknown session_ids.
	for i := 0; i < 50; i++ {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"default","session_id":"s_unknown_runs_`+fmt.Sprint(i)+`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
		))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("runs unknown #%d: status = %d, want 404", i, resp.StatusCode)
		}
	}

	if got := srv.lockedSessionCount(); got != 0 {
		t.Fatalf("sessionLocks grew to %d entries from 100 unknown-id requests; want 0", got)
	}
}

// v0.3.2: handleRuns also takes the lock when SessionID is set.
// Reusing an explicit SessionID concurrently must fast-fail with 409.
func TestPerSessionLockAppliesToRunsWithSessionID(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	// Gate which call blocks by inspecting the request itself rather than
	// reading provider.requests' length racily. Calls whose user message
	// contains "BLOCK_HERE" hold until release is closed; everything else
	// completes immediately. entered signals first blocking entry.
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	provider := &callableProvider{}
	provider.call = func(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
		shouldBlock := requestContains(req, "BLOCK_HERE")
		if shouldBlock {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		ch := make(chan providers.Event, 2)
		go func() {
			defer close(ch)
			if shouldBlock {
				<-release
			}
			ch <- providers.Event{Type: providers.EventText, Text: "ok"}
			ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
		}()
		return ch, nil
	}

	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()

	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Seed.
	resp0, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	body0, _ := io.ReadAll(resp0.Body)
	resp0.Body.Close()
	sessionID := extractSessionID(string(body0))
	if sessionID == "" {
		t.Fatalf("no session id from seed:\n%s", string(body0))
	}

	// Concurrent /v1/runs reusing the same session_id: first blocks; second 409s.
	first := make(chan int, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"default","session_id":"`+sessionID+`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"q1 BLOCK_HERE"}]}]}`,
		))
		if err != nil {
			first <- -1
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		first <- resp.StatusCode
	}()

	// Wait deterministically for the blocking call to enter the provider.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("first run never reached provider")
	}

	resp2, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","session_id":"`+sessionID+`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"q2"}]}]}`,
	))
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusConflict {
		close(release)
		<-first
		t.Fatalf("second status = %d, want 409; body=%s", resp2.StatusCode, string(body2))
	}
	if !strings.Contains(string(body2), `"code":"session_busy"`) {
		close(release)
		<-first
		t.Fatalf("second body missing session_busy: %s", string(body2))
	}
	close(release)
	if got := <-first; got != 200 {
		t.Errorf("first status = %d, want 200", got)
	}
}

// Per-agent default-deny invariant: an agent declared with no
// allowed_tools in YAML must see ZERO tools at the dispatcher,
// even though several built-ins are registered server-wide. The
// model's req.Tools should be empty. This is the security floor —
// turning off filterTools' empty-list early-return would silently
// expose every registered tool to every agent.
func TestAgentWithNoAllowedToolsSeesZeroTools(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			// AllowedTools omitted — default-deny.
			"locked": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &callableProvider{}
	provider.call = func(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
		ch := make(chan providers.Event, 2)
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
		close(ch)
		return ch, nil
	}
	// Server-wide tool inventory has FakeTool registered — the agent
	// must NOT see it because its allowed_tools is empty.
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{&fakeBuiltinTool{}}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"locked","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) == 0 {
		t.Fatal("provider was never called")
	}
	if got := len(provider.requests[0].Tools); got != 0 {
		var names []string
		for _, ts := range provider.requests[0].Tools {
			names = append(names, ts.Name)
		}
		t.Errorf("agent with no allowed_tools saw %d tools (%v); want 0", got, names)
	}
}

// Asymmetric: an agent that DOES allow a tool sees it. Pairs with the
// previous test as the positive case — confirms the dispatcher is
// actually plumbed (so a "0 tools" pass isn't accidentally because the
// pipeline is broken).
func TestAgentWithExplicitAllowSeeTool(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"open": {Model: "stub-model", AllowedTools: []string{"FakeTool"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &callableProvider{}
	provider.call = func(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
		ch := make(chan providers.Event, 2)
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
		close(ch)
		return ch, nil
	}
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{&fakeBuiltinTool{}}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"open","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) == 0 || len(provider.requests[0].Tools) != 1 {
		t.Fatalf("agent with FakeTool allowed should see 1 tool; got %d", len(provider.requests[0].Tools))
	}
	if provider.requests[0].Tools[0].Name != "FakeTool" {
		t.Errorf("tool name = %q, want FakeTool", provider.requests[0].Tools[0].Name)
	}
}

// Per-request host narrowing — empty array → deny all. The model
// invokes HTTP; the wrapped tool's allowlist is empty, so the call
// refuses BEFORE any DNS or dial. We inspect the tool_result text in
// the second provider call to confirm the refusal happened with the
// "not in allowlist" message (which only fires when the wrapped
// allowlist is what blocked the call).
func TestPerRequestAllowedHostsEmptyDeniesNetworkCalls(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"net": {Model: "stub-model", AllowedTools: []string{"HTTP"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &callableProvider{}
	provider.call = func(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
		idx := len(provider.requests) - 1
		var evs []providers.Event
		switch idx {
		case 0:
			// First turn: emit a tool_call so the loop executes HTTP.
			evs = []providers.Event{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
					ID: "h1", Name: "HTTP",
					Input: json.RawMessage(`{"method":"GET","url":"https://operator.example/"}`),
				}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			}
		default:
			// Second turn: just close out so the run finishes.
			evs = []providers.Event{
				{Type: providers.EventText, Text: "ok"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			}
		}
		ch := make(chan providers.Event, len(evs))
		for _, e := range evs {
			ch <- e
		}
		close(ch)
		return ch, nil
	}

	// Register a real HTTP tool with operator's allowlist permitting
	// operator.example. Without per-request narrowing the call would
	// attempt to dial (and fail with DNS NXDOMAIN, not a refusal).
	httpTool := &builtin.HTTP{HostAllowlist: []string{"operator.example"}}
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{httpTool}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// allowed_hosts: [] → deny-all narrowing. The wrapped HTTP tool
	// has an empty allowlist, so even operator.example is refused.
	body := `{"agent":"net","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}],"allowed_hosts":[]}`
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) < 2 {
		t.Fatalf("provider got %d calls, want >=2 (tool_use + continuation)", len(provider.requests))
	}
	// The second call's messages must include a tool_result whose Text
	// reflects the empty-allowlist refusal.
	contMsgs := provider.requests[1].Messages
	var toolResult string
	for _, m := range contMsgs {
		for _, c := range m.Content {
			if c.Type == "tool_result" {
				toolResult = c.Text
			}
		}
	}
	if toolResult == "" {
		t.Fatalf("no tool_result in second call's messages: %s", describeMessages(contMsgs))
	}
	if !strings.Contains(toolResult, "empty host allowlist") && !strings.Contains(toolResult, "not in allowlist") {
		t.Errorf("tool_result didn't show allowlist refusal: %q", toolResult)
	}
}

// Conversely: nil allowed_hosts (omitted from request) → operator's
// list applies. The same scenario as above but with allowed_hosts
// missing should reach the dial layer (and fail with a DNS error,
// distinct from the allowlist message).
func TestPerRequestAllowedHostsNilLeavesOperatorListIntact(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"net": {Model: "stub-model", AllowedTools: []string{"HTTP"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &callableProvider{}
	provider.call = func(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
		idx := len(provider.requests) - 1
		var evs []providers.Event
		switch idx {
		case 0:
			evs = []providers.Event{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
					ID: "h1", Name: "HTTP",
					Input: json.RawMessage(`{"method":"GET","url":"https://operator.example/"}`),
				}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			}
		default:
			evs = []providers.Event{
				{Type: providers.EventText, Text: "ok"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			}
		}
		ch := make(chan providers.Event, len(evs))
		for _, e := range evs {
			ch <- e
		}
		close(ch)
		return ch, nil
	}

	httpTool := &builtin.HTTP{HostAllowlist: []string{"operator.example"}}
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{httpTool}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// allowed_hosts omitted → operator's list applies; URL passes
	// hostAllowed and proceeds to the dial (which fails with DNS).
	body := `{"agent":"net","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) < 2 {
		t.Fatalf("provider got %d calls, want >=2", len(provider.requests))
	}
	var toolResult string
	for _, m := range provider.requests[1].Messages {
		for _, c := range m.Content {
			if c.Type == "tool_result" {
				toolResult = c.Text
			}
		}
	}
	if toolResult == "" {
		t.Fatal("no tool_result in second call's messages")
	}
	// Negative: should NOT be the empty-allowlist message — caller's
	// nil allowed_hosts means no narrowing, so the operator's
	// [operator.example] applies and the call passes hostAllowed.
	if strings.Contains(toolResult, "empty host allowlist") {
		t.Errorf("nil allowed_hosts should NOT trigger empty-allowlist refusal; got %q", toolResult)
	}
	// Positive: the result must include "request:" — the only error
	// path that prefix appears on (httptool.go's "request: " + err)
	// fires AFTER hostAllowed accepts and dialContext starts dialing.
	// Without this assertion a future regression that rejects in some
	// other way would still pass the negative check.
	if !strings.Contains(toolResult, "request:") {
		t.Errorf("tool_result should contain 'request:' (proves call reached dial layer); got %q", toolResult)
	}
}

// callableProvider lets a test inject a Call function. Reuses stubProvider's
// ID/Capabilities trivially.
type callableProvider struct {
	call     func(context.Context, providers.Request) (<-chan providers.Event, error)
	requests []providers.Request
	mu       sync.Mutex
}

func (c *callableProvider) ID() string                    { return "stub" }
func (c *callableProvider) Probe(_ context.Context) error { return nil }
func (c *callableProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (c *callableProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (c *callableProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	return c.call(ctx, req)
}

func containsText(blocks []providers.ContentBlock, want string) bool {
	for _, b := range blocks {
		if b.Type == "text" && strings.Contains(b.Text, want) {
			return true
		}
	}
	return false
}

// requestContains scans the request's user messages for a marker string.
// Tests use this to gate per-call behaviour deterministically (instead of
// reading provider.requests' length, which races with concurrent appends).
func requestContains(req providers.Request, marker string) bool {
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		if containsText(m.Content, marker) {
			return true
		}
	}
	return false
}

// Regression: replay must reconstruct messages in valid Anthropic/OpenAI
// order even when the original run had tool calls. The bug was that
// "usage" was treated as a flush boundary, but the loop emits usage BEFORE
// tool_result within an iteration — so a multi-iteration tool-call run
// would replay as [user, assistant(text+tool_use), assistant(text), user(tool_result)],
// which both providers 400 on (two assistant turns back-to-back, tool_result
// orphaned from its tool_use).
//
// Correct shape: [user, assistant(tool_use), user(tool_result), assistant(text), user(new)].
func TestMessagesEndpointReplaysToolCallsCorrectly(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model", AllowedTools: []string{"FakeTool"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	// Provider call sequence:
	//   call 0: emit tool_call → done(stop=tool_use)   ← loop will execute the tool
	//   call 1: emit text "thanks" → done(stop=end_turn)
	//   call 2: continuation → emit "second reply" → done(stop=end_turn)
	provider := &callableProvider{}
	provider.call = func(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
		idx := len(provider.requests) - 1
		var evs []providers.Event
		switch idx {
		case 0:
			evs = []providers.Event{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
					ID: "call_a", Name: "FakeTool", Input: json.RawMessage(`{}`),
				}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			}
		case 1:
			evs = []providers.Event{
				{Type: providers.EventText, Text: "thanks"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			}
		default:
			evs = []providers.Event{
				{Type: providers.EventText, Text: "second"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			}
		}
		ch := make(chan providers.Event, len(evs))
		for _, e := range evs {
			ch <- e
		}
		close(ch)
		return ch, nil
	}

	// Wire a real Read-tool-style stub that always succeeds with "TOOL OK".
	type fakeTool struct{}
	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{&fakeBuiltinTool{}}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	_ = fakeTool{} // keep the named type referenced

	// First run — uses the tool.
	resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hello"}]}],"allowed_tools":["FakeTool"]}`,
	))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sessionID := extractSessionID(string(body))
	if sessionID == "" {
		t.Fatalf("no session id from first run:\n%s", string(body))
	}

	// Continuation. Provider call 2 receives the replayed history.
	resp2, _ := http.Post(ts.URL+"/v1/sessions/"+sessionID+"/messages", "application/json", strings.NewReader(
		`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"more"}]}]}`,
	))
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) < 3 {
		t.Fatalf("provider got %d calls, want >=3", len(provider.requests))
	}
	contMsgs := provider.requests[2].Messages
	// Must be: user(hello), assistant(tool_use call_a), user(tool_result call_a), assistant(thanks), user(more)
	if len(contMsgs) != 5 {
		t.Fatalf("continuation messages = %d, want 5; got: %s", len(contMsgs), describeMessages(contMsgs))
	}
	checks := []struct {
		role    string
		hasType string
		text    string
	}{
		{"user", "text", "hello"},
		{"assistant", "tool_use", ""},
		{"user", "tool_result", ""},
		{"assistant", "text", "thanks"},
		{"user", "text", "more"},
	}
	for i, want := range checks {
		got := contMsgs[i]
		if got.Role != want.role {
			t.Errorf("msg %d: role = %q, want %q. full sequence:\n%s", i, got.Role, want.role, describeMessages(contMsgs))
			break
		}
		var found bool
		for _, c := range got.Content {
			if c.Type == want.hasType {
				found = true
				if want.text != "" && c.Text != want.text {
					t.Errorf("msg %d: text = %q, want %q", i, c.Text, want.text)
				}
				break
			}
		}
		if !found {
			t.Errorf("msg %d: no content block of type %q. full sequence:\n%s", i, want.hasType, describeMessages(contMsgs))
		}
	}
}

func describeMessages(msgs []providers.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		fmt.Fprintf(&b, "  [%d] %s: ", i, m.Role)
		for _, c := range m.Content {
			fmt.Fprintf(&b, "(%s ", c.Type)
			if c.Text != "" {
				fmt.Fprintf(&b, "text=%q ", c.Text)
			}
			if c.ToolName != "" {
				fmt.Fprintf(&b, "name=%s ", c.ToolName)
			}
			if c.ToolUseID != "" {
				fmt.Fprintf(&b, "id=%s ", c.ToolUseID)
			}
			b.WriteString(") ")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// fakeBuiltinTool is a Read-shaped tool that always succeeds, for the
// continuation+tool-replay test. Lives next to that test only.
type fakeBuiltinTool struct{}

func (fakeBuiltinTool) Name() string                 { return "FakeTool" }
func (fakeBuiltinTool) Description() string          { return "succeeds" }
func (fakeBuiltinTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (fakeBuiltinTool) Execute(ctx context.Context, in json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: "TOOL OK"}, nil
}

// Regression: continuation against an unknown session returns 404.
func TestMessagesEndpoint404OnUnknownSession(t *testing.T) {
	cfg := &config.Config{
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	st, _ := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/sessions/s_nope/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// extractSessionID pulls the session_id payload from the first SSE frame
// whose event-name is "session". Returns "" if not found.
func extractSessionID(body string) string {
	const marker = "event: session\ndata: "
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	tail := body[i+len(marker):]
	end := strings.Index(tail, "\n")
	if end < 0 {
		return ""
	}
	// data is JSON: {"type":"session","text":"s_..."}
	var ev struct{ Text string }
	_ = json.Unmarshal([]byte(tail[:end]), &ev)
	return ev.Text
}

// readEvents parses SSE frames and returns the event-type per frame in order.
// sseFrame is one parsed SSE frame: event type + decoded JSON data.
type sseFrame struct {
	Event string
	Data  map[string]any
}

func readFrames(t *testing.T, r io.Reader) []sseFrame {
	t.Helper()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var frames []sseFrame
	var current sseFrame
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if current.Event != "" {
				frames = append(frames, current)
				current = sseFrame{}
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			current.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			_ = json.Unmarshal([]byte(payload), &current.Data)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if current.Event != "" {
		frames = append(frames, current)
	}
	return frames
}

func readEvents(t *testing.T, r io.Reader) []string {
	t.Helper()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var types []string
	var current string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if current != "" {
				types = append(types, current)
				current = ""
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			current = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if current != "" {
		types = append(types, current)
	}
	return types
}

// TestRunRequest_UserBearerValidation guards the v0.8.x per-run MCP
// bearer charset and length check at the HTTP boundary. Charset:
// [A-Za-z0-9._\-+/=], length 16..512. Empty string is accepted
// (backwards compat — static-bearer setups omit the field).
func TestRunRequest_UserBearerValidation(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model", AllowedTools: []string{"Read"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn"},
	}}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	valid16 := strings.Repeat("a", 16)
	valid512 := strings.Repeat("a", 512)
	cases := []struct {
		name       string
		userBearer string
		want400    bool
	}{
		{"empty_omitted_OK", "", false},
		{"valid_16_char", valid16, false},
		{"valid_512_char", valid512, false},
		{"valid_jwt_shape", "eyJhbGciOi.payload-part.signature_part", false},
		{"valid_with_slash_plus_eq", "abc+def/ghi=ABC=", false},
		{"too_short_15", strings.Repeat("a", 15), true},
		{"too_long_513", strings.Repeat("a", 513), true},
		{"illegal_space", "valid_token_with space_inside", true},
		{"illegal_exclaim", "token!!!!!!!!!!!!!!!!", true},
		{"illegal_unicode", "тест" + strings.Repeat("a", 12), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"agent":"default","user_bearer":%q,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
				tc.userBearer,
			)
			resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			gotBad := resp.StatusCode == http.StatusBadRequest
			if gotBad != tc.want400 {
				slurp, _ := io.ReadAll(resp.Body)
				t.Errorf("status=%d want400=%v body=%s", resp.StatusCode, tc.want400, slurp)
				return
			}
			if tc.want400 {
				slurp, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(slurp), "user_bearer") {
					t.Errorf("400 body should mention user_bearer, got %q", slurp)
				}
			}
		})
	}
}
