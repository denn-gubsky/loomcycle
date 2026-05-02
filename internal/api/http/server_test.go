package http

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// stubProvider replays a fixed event sequence, ignoring the request entirely.
type stubProvider struct{ events []providers.Event }

func (s *stubProvider) ID() string { return "stub" }
func (s *stubProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (s *stubProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
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
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem)

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

	// Read SSE frames; expect started, text, usage, done.
	got := readEvents(t, resp.Body)
	wantTypes := []string{"started", "text", "usage", "done"}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d frames %v, want %d %v", len(got), got, len(wantTypes), wantTypes)
	}
	for i, w := range wantTypes {
		if got[i] != w {
			t.Errorf("frame %d: got %q want %q", i, got[i], w)
		}
	}
}

func TestHealthz(t *testing.T) {
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100}}
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second))
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	cfg := &config.Config{
		Agents:      map[string]config.AgentDef{"default": {Model: "x"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	cfg.Env.AuthToken = "secret"
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second))
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
	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(1, 1, time.Second))
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
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second))
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
func (n *nonFlushingWriter) WriteHeader(s int)             { n.status = s }
func (n *nonFlushingWriter) Write(b []byte) (int, error)   { return n.body.Write(b) }

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
	srv := New(cfg, &stubResolver{p: &stubProvider{}}, nil, concurrency.New(1, 1, time.Second))

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

// readEvents parses SSE frames and returns the event-type per frame in order.
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
