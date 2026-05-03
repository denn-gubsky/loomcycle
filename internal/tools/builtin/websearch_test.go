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
