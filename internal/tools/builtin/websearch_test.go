package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/search"
)

// braveWS builds a WebSearch whose only provider is Brave, pointed at a test
// server — the single-provider shape that exercises the render/clamp/filter
// behaviour the pre-RFC-BB tests covered. hostKey is the operator Brave key
// (resolved via ResolveKeyOrOperator, so a ctx CredentialDef can override it).
func braveWS(t *testing.T, endpoint, hostKey string) *WebSearch {
	t.Helper()
	reg, err := search.BuildRegistry([]search.ProviderSpec{
		{ID: "brave", Options: []search.Option{search.WithEndpoint(endpoint)}},
	})
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	return &WebSearch{
		Registry: reg,
		Resolver: search.NewResolver([]string{"brave"}),
		HostKeys: map[string]string{"brave": hostKey},
	}
}

func TestWebSearchRefusesWhenUnconfigured(t *testing.T) {
	s := &WebSearch{} // nil registry
	res, err := s.Execute(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "not configured") {
		t.Errorf("expected not-configured refusal, got %q", res.Text)
	}
}

func TestWebSearchRefusesWithoutKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"web":{"results":[]}}`)
	}))
	defer srv.Close()
	s := braveWS(t, srv.URL, "") // configured, but no operator key + no tenant cred
	res, err := s.Execute(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "no keyable") {
		t.Errorf("expected no-keyable refusal, got %q", res.Text)
	}
}

func TestWebSearchRefusesEmptyQuery(t *testing.T) {
	s := &WebSearch{} // the empty-query check precedes the registry check
	res, err := s.Execute(context.Background(), json.RawMessage(`{"query":"   "}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "query") {
		t.Errorf("expected query-required, got %q", res.Text)
	}
}

// Plumb a fake Brave server. Verifies: token header is sent (resolved from the
// operator key), count comes from max_results, response is rendered in the
// documented "[N] Title — URL\n   snippet" shape.
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

	s := braveWS(t, srv.URL, "secret123")
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

	s := braveWS(t, srv.URL, "k")
	body, _ := json.Marshal(map[string]any{"query": "x", "max_results": 999})
	_, _ = s.Execute(context.Background(), body)
	if seenCount != "25" {
		t.Errorf("count = %q, want 25 (hard ceiling)", seenCount)
	}
}

// Defence in depth: even if the provider returns more results than asked, the
// rendering loop must still cap at max.
func TestWebSearchClampsProviderOverflow(t *testing.T) {
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

	s := braveWS(t, srv.URL, "k")
	body, _ := json.Marshal(map[string]any{"query": "x", "max_results": 9999})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	count := strings.Count(res.Text, "\n[")
	if !strings.HasPrefix(res.Text, "[") {
		t.Fatalf("output should start with [1] marker; got %q", res.Text[:min(len(res.Text), 80)])
	}
	count++
	if count != 25 {
		t.Errorf("rendered %d result blocks, want 25 (hard cap on provider-side overflow)", count)
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

	s := braveWS(t, srv.URL, "k")
	body, _ := json.Marshal(map[string]string{"query": "x"})
	res, err := s.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, strings.Repeat("x", 256)+"…") {
		t.Errorf("snippet not truncated to 256 chars + ellipsis: %q", res.Text)
	}
}

// A provider error is surfaced (after the fallback circuit exhausts the cascade).
func TestWebSearchSurfacesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":"slow down"}`)
	}))
	defer srv.Close()

	s := braveWS(t, srv.URL, "k")
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
func TestWebSearchAllowedHostsDropMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"A","url":"https://allowed.example/a","description":"da"},
			{"title":"B","url":"https://blocked.example/b","description":"db"},
			{"title":"C","url":"https://api.allowed.example/c","description":"dc"}
		]}}`)
	}))
	defer srv.Close()

	s := braveWS(t, srv.URL, "k")
	s.AllowedHosts = []string{"allowed.example"} // FilterMode unset → drop
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
func TestWebSearchAllowedHostsKeepMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"A","url":"https://allowed.example/a","description":"da"},
			{"title":"B","url":"https://blocked.example/b","description":"db"}
		]}}`)
	}))
	defer srv.Close()

	s := braveWS(t, srv.URL, "k")
	s.AllowedHosts = []string{"allowed.example"}
	s.FilterMode = WebSearchFilterKeep
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

// AllowedHosts set, drop mode, ALL results filtered out → "no results".
func TestWebSearchAllowedHostsDropAllFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"A","url":"https://blocked.example/a","description":"d"}
		]}}`)
	}))
	defer srv.Close()

	s := braveWS(t, srv.URL, "k")
	s.AllowedHosts = []string{"only.example"}
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

	s := braveWS(t, srv.URL, "k")
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
