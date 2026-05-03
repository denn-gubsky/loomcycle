package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearchRefusesWithoutAPIKey(t *testing.T) {
	s := &WebSearch{}
	res, err := s.Execute(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "BRAVE_API_KEY") {
		t.Errorf("expected api-key refusal, got %q", res.Text)
	}
}

func TestWebSearchRefusesEmptyQuery(t *testing.T) {
	s := &WebSearch{APIKey: "k"}
	res, err := s.Execute(context.Background(), json.RawMessage(`{"query":"   "}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "query") {
		t.Errorf("expected query-required, got %q", res.Text)
	}
}

// Plumb a fake Brave server. Verifies: token header is sent, count
// comes from max_results, response is rendered in the documented
// "[N] Title — URL\n   snippet" shape.
func TestWebSearchSuccessfulQuery(t *testing.T) {
	var seenToken, seenQuery, seenCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenToken = r.Header.Get("X-Subscription-Token")
		seenQuery = r.URL.Query().Get("q")
		seenCount = r.URL.Query().Get("count")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"<strong>Foo</strong>","url":"https://foo.example/","description":"about foo"},
			{"title":"Bar","url":"https://bar.example/","description":"about bar"}
		]}}`)
	}))
	defer srv.Close()

	s := &WebSearch{APIKey: "secret123", Endpoint: srv.URL}
	body, _ := json.Marshal(map[string]any{"query": "what is foo", "max_results": 2})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}

	if seenToken != "secret123" {
		t.Errorf("token header = %q, want secret123", seenToken)
	}
	if seenQuery != "what is foo" {
		t.Errorf("q = %q", seenQuery)
	}
	if seenCount != "2" {
		t.Errorf("count = %q, want 2", seenCount)
	}

	want := "[1] Foo — https://foo.example/\n   about foo\n[2] Bar — https://bar.example/\n   about bar"
	if res.Text != want {
		t.Errorf("rendered output mismatch:\n got: %q\nwant: %q", res.Text, want)
	}
}

// Hard ceiling: caller asks for 999 results, we cap at 25.
func TestWebSearchHardMaxResultsCeiling(t *testing.T) {
	var seenCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCount = r.URL.Query().Get("count")
		fmt.Fprint(w, `{"web":{"results":[]}}`)
	}))
	defer srv.Close()

	s := &WebSearch{APIKey: "k", Endpoint: srv.URL}
	body, _ := json.Marshal(map[string]any{"query": "x", "max_results": 999})
	_, _ = s.Execute(context.Background(), body)
	if seenCount != "25" {
		t.Errorf("count = %q, want 25 (hard ceiling)", seenCount)
	}
}

// Snippet truncation at 256 chars so a long Brave description can't blow
// the model's context window.
// Defence in depth: even if Brave returns more results than we asked
// for (server bug or unexpected response shape), the rendering loop
// must still cap at max. Without this clamp a misbehaving search
// backend could blow the model's context window.
func TestWebSearchClampsBraveOverflow(t *testing.T) {
	// Hand-craft a response with 30 results.
	var sb strings.Builder
	sb.WriteString(`{"web":{"results":[`)
	for i := 0; i < 30; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"title":"R%d","url":"https://r%d/","description":"d"}`, i, i)
	}
	sb.WriteString(`]}}`)
	resp := sb.String()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, resp)
	}))
	defer srv.Close()

	s := &WebSearch{APIKey: "k", Endpoint: srv.URL}
	body, _ := json.Marshal(map[string]any{"query": "x", "max_results": 9999})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	// The rendered output should have exactly 25 lines that start with "[" —
	// one per result.
	count := strings.Count(res.Text, "\n[")
	// Number of [N] markers = newline-prefixed [ + the very first one.
	if !strings.HasPrefix(res.Text, "[") {
		t.Fatalf("output should start with [1] marker; got %q", res.Text[:min(len(res.Text), 80)])
	}
	count++
	if count != 25 {
		t.Errorf("rendered %d result blocks, want 25 (hard cap on Brave-side overflow)", count)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestWebSearchTruncatesLongSnippet(t *testing.T) {
	long := strings.Repeat("x", 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"web":{"results":[{"title":"T","url":"https://t/","description":%q}]}}`, long)
	}))
	defer srv.Close()

	s := &WebSearch{APIKey: "k", Endpoint: srv.URL}
	body, _ := json.Marshal(map[string]string{"query": "x"})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, strings.Repeat("x", 256)+"…") {
		t.Errorf("snippet not truncated to 256 chars + ellipsis: %q", res.Text)
	}
}

func TestWebSearchSurfacesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":"slow down"}`)
	}))
	defer srv.Close()

	s := &WebSearch{APIKey: "k", Endpoint: srv.URL}
	body, _ := json.Marshal(map[string]string{"query": "x"})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "429") {
		t.Errorf("expected 429 surfaced, got %q", res.Text)
	}
}

// Per-run host narrowing — drop mode (default when AllowedHosts is set).
// Brave returns three results across two hosts; AllowedHosts permits
// only one host. The other host's result must be omitted, and indices
// must renumber so the model sees [1] not [3].
func TestWebSearchAllowedHostsDropMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"A","url":"https://allowed.example/a","description":"da"},
			{"title":"B","url":"https://blocked.example/b","description":"db"},
			{"title":"C","url":"https://api.allowed.example/c","description":"dc"}
		]}}`)
	}))
	defer srv.Close()

	s := &WebSearch{
		APIKey:       "k",
		Endpoint:     srv.URL,
		AllowedHosts: []string{"allowed.example"},
		// FilterMode unset → defaults to drop.
	}
	body, _ := json.Marshal(map[string]string{"query": "x"})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if strings.Contains(res.Text, "blocked.example") {
		t.Errorf("blocked host leaked into results: %q", res.Text)
	}
	if !strings.Contains(res.Text, "allowed.example/a") || !strings.Contains(res.Text, "allowed.example/c") {
		t.Errorf("allowed hosts missing: %q", res.Text)
	}
	// Renumbering: only two results survive — should be [1] and [2], not [1] and [3].
	if !strings.HasPrefix(res.Text, "[1]") {
		t.Errorf("first result should be [1]; got prefix %q", res.Text[:min(len(res.Text), 50)])
	}
	if !strings.Contains(res.Text, "[2] C") {
		t.Errorf("second result should be [2] C; got %q", res.Text)
	}
	if strings.Contains(res.Text, "[3]") {
		t.Errorf("indices should renumber; saw [3] in %q", res.Text)
	}
}

// Per-run host narrowing — keep mode (caller filters downstream).
// Brave's full result set comes through unchanged; the model receives
// every URL so it can reason about them. The contract: AllowedHosts
// is informational here, NOT enforced.
func TestWebSearchAllowedHostsKeepMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"A","url":"https://allowed.example/a","description":"da"},
			{"title":"B","url":"https://blocked.example/b","description":"db"}
		]}}`)
	}))
	defer srv.Close()

	s := &WebSearch{
		APIKey:       "k",
		Endpoint:     srv.URL,
		AllowedHosts: []string{"allowed.example"},
		FilterMode:   WebSearchFilterKeep,
	}
	body, _ := json.Marshal(map[string]string{"query": "x"})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "blocked.example") {
		t.Errorf("keep mode should return non-matching results; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "allowed.example") {
		t.Errorf("matching result missing in keep mode: %q", res.Text)
	}
}

// Edge: AllowedHosts set, drop mode, ALL Brave results filtered out.
// Should return "no results" (matching the empty-Brave-response shape).
func TestWebSearchAllowedHostsDropAllFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"A","url":"https://blocked.example/a","description":"d"}
		]}}`)
	}))
	defer srv.Close()

	s := &WebSearch{APIKey: "k", Endpoint: srv.URL, AllowedHosts: []string{"only.example"}}
	body, _ := json.Marshal(map[string]string{"query": "x"})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if res.Text != "no results" {
		t.Errorf("text = %q, want 'no results'", res.Text)
	}
}

func TestWebSearchEmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"web":{"results":[]}}`)
	}))
	defer srv.Close()

	s := &WebSearch{APIKey: "k", Endpoint: srv.URL}
	body, _ := json.Marshal(map[string]string{"query": "obscure"})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("empty results should not error; got %q", res.Text)
	}
	if res.Text != "no results" {
		t.Errorf("text = %q, want 'no results'", res.Text)
	}
}
