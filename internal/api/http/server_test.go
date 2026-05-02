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
